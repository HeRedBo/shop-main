package worker

import (
	"context"
	"fmt"
	"sync"
	"time"

	"shop/conf"
	"shop/pkg/global"

	"github.com/IBM/sarama"
)

// Engine 管理所有 Worker 组件的生命周期
// 负责创建 Consumer、管理注册表、协调启动和关闭
// 支持多 Consumer 模式：每个 HandlerConfig 对应一个独立的 Consumer 实例
type Engine struct {
	// ProcessManager 管理异构 Process（Redis 等多队列）
	processManager *ProcessManager

	// DelayScheduler 基于 Redis Sorted Set 的延迟调度器（主调度路径）
	delayScheduler *DelayScheduler

	// 保留原有字段（向后兼容）
	runners  []Runnable  // Kafka/Redis 等多种任务实例
	registry *Registry
	relay    *Relay
	config   conf.WorkerConfig
	ctx      context.Context
	cancel   context.CancelFunc
	mu       sync.Mutex
	started  bool
}

// NewEngine 根据 WorkerConfig 创建 Worker 引擎
func NewEngine(config conf.WorkerConfig) *Engine {
	ctx, cancel := context.WithCancel(context.Background())
	registry := NewRegistry()
	return &Engine{
		processManager: NewProcessManager(registry),
		registry:       registry,
		config:         config,
		ctx:            ctx,
		cancel:         cancel,
	}
}

// AddProcess 注册一个 Process 到 Engine（用于多队列模式，如 Redis Stream）
func (e *Engine) AddProcess(p *Process) {
	e.processManager.AddProcess(p)
}

// GetRegistry 返回 Handler 注册表，供外部注册 Handler
func (e *Engine) GetRegistry() *Registry {
	return e.registry
}

// Start 启动引擎
// 1. 创建 sarama ConsumerGroup
// 2. 根据配置创建 Consumer（新模式：per-handler 独立 Consumer；旧模式：单 Consumer 订阅所有 topics）
// 3. 创建并启动 Relay（Outbox 扫描投递）
func (e *Engine) Start() error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.started {
		return fmt.Errorf("worker engine already started")
	}

	if !e.config.Enabled {
		Logger().Info("[worker] Worker 未启用，跳过启动")
		return nil
	}

	// 校验配置
	if e.config.GroupId == "" {
		return fmt.Errorf("worker group-id is required")
	}

	// 创建 sarama ConsumerGroup 配置
	saramaConfig := sarama.NewConfig()
	saramaConfig.Consumer.Group.Rebalance.GroupStrategies = []sarama.BalanceStrategy{sarama.NewBalanceStrategyRoundRobin()}
	saramaConfig.Consumer.Offsets.Initial = sarama.OffsetNewest
	saramaConfig.Consumer.Offsets.AutoCommit.Enable = false // 手动提交 offset

	// 解析全局 handler-timeout 配置
	handlerTimeout := ParseHandlerTimeout(e.config.HandlerTimeout)

	if len(e.config.Handlers) > 0 {
		// 新模式：per-Handler 独立 Consumer
		for _, h := range e.config.Handlers {
			config := ConsumerConfig{
				Topic:          h.Topic,
				Concurrency:    h.Concurrency,
				BufferSize:     h.BufferSize,
				Ordered:        h.Ordered,
				HandlerTimeout: handlerTimeout,
			}

			// 使用全局默认值兜底
			if config.Concurrency <= 0 {
				config.Concurrency = e.config.DefaultConcurrency
				if config.Concurrency <= 0 {
					config.Concurrency = e.config.Concurrency
					if config.Concurrency <= 0 {
						config.Concurrency = defaultConcurrency
					}
				}
			}
			if config.BufferSize <= 0 {
				config.BufferSize = e.config.DefaultBufferSize
				if config.BufferSize <= 0 {
					config.BufferSize = defaultTaskBufferSize
				}
			}

			// 为每个 Consumer 创建独立的 ConsumerGroup
			consumerGroup, err := sarama.NewConsumerGroup(global.CONFIG.Kafka.Hosts, e.config.GroupId, saramaConfig)
			if err != nil {
				return fmt.Errorf("创建 ConsumerGroup 失败 (topic=%s): %w", h.Topic, err)
			}

			consumer := NewConsumerWithConfig(consumerGroup, config, e.registry)
			e.runners = append(e.runners, consumer)

			Logger().Infof("[worker] 创建 Consumer: topic=%s, concurrency=%d, buffer-size=%d",
				h.Topic, config.Concurrency, config.BufferSize)
		}
	} else {
		// 旧模式：向后兼容，单 Consumer 订阅所有 topics
		if len(e.config.Topics) == 0 {
			return fmt.Errorf("worker topics is required (no handlers configured)")
		}

		concurrency := e.config.Concurrency
		if concurrency <= 0 {
			concurrency = defaultConcurrency
			Logger().Warn("[worker] concurrency 未配置或为 0，使用默认值 1")
		}

		group, err := sarama.NewConsumerGroup(global.CONFIG.Kafka.Hosts, e.config.GroupId, saramaConfig)
		if err != nil {
			return fmt.Errorf("创建 ConsumerGroup 失败: %w", err)
		}

		consumer := NewConsumerWithConfig(group, ConsumerConfig{
			Topics:         e.config.Topics,
			Concurrency:    concurrency,
			HandlerTimeout: handlerTimeout,
		}, e.registry)
		e.runners = append(e.runners, consumer)

		Logger().Infof("[worker] 创建 Consumer（兼容模式）: topics=%v, concurrency=%d",
			e.config.Topics, concurrency)
	}

	// 启动所有 Runner
	for _, r := range e.runners {
		if err := r.Start(e.ctx); err != nil {
			return fmt.Errorf("启动 Runner[%s] 失败: %w", r.Name(), err)
		}
	}

	// 创建并启动 Relay（Outbox 扫描投递）
	relayConfig := ParseRelayConfig(e.config)
	e.relay = NewRelay(relayConfig)
	if err := e.relay.Start(e.ctx); err != nil {
		return fmt.Errorf("启动 Relay 失败: %w", err)
	}

	// 创建并启动 DelayScheduler（Redis Sorted Set 延迟调度，主调度路径）
	// Redis 可用时启用，否则完全依赖 DB Relay 兜底
	if global.RedisClient != nil {
		e.delayScheduler = NewDelayScheduler(global.RedisClient)
		if err := e.delayScheduler.Start(e.ctx); err != nil {
			return fmt.Errorf("启动 DelayScheduler 失败: %w", err)
		}
	}

	// 启动 ProcessManager 中注册的额外 Process（Redis 等异构队列）
	// 注意：Kafka Consumer 已在上面通过 e.runners 直接启动
	// ProcessManager 管理的是通过 AddProcess 注册的异构 Process
	if err := e.processManager.Start(); err != nil {
		return fmt.Errorf("ProcessManager 启动失败: %w", err)
	}

	e.started = true

	Logger().Infof("[worker] Engine 已启动，group-id=%s, runners=%d",
		e.config.GroupId, len(e.runners))

	return nil
}

// Stop 优雅关闭引擎
// 有序关闭：Relay → DelayScheduler → ProcessManager → Cancel Context → Runners
// 注意：ConsumerGroup 连接由 Consumer.Stop() 内部自行关闭，无需 Engine 层单独管理
func (e *Engine) Stop() error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if !e.started {
		return nil
	}

	Logger().Info("[worker] Engine 开始关闭...")

	// 1. 停止 Relay
	if e.relay != nil {
		if err := e.relay.Stop(); err != nil {
			Logger().Errorf("[worker] Relay 关闭失败: %v", err)
		}
	}

	// 1.5. 停止 DelayScheduler
	if e.delayScheduler != nil {
		if err := e.delayScheduler.Stop(); err != nil {
			Logger().Errorf("[worker] DelayScheduler 关闭失败: %v", err)
		}
	}

	// 2. 关闭 ProcessManager 中的 Process（异构队列，如 Redis）
	if e.processManager != nil {
		e.processManager.Stop()
	}

	// 3. 取消 Engine context，通知所有 Consumer 的 consumeLoop 准备退出
	e.cancel()

	// 4. 停止所有 Runner（Consumer.Stop() 内部会等待 Worker 处理完剩余消息并关闭 ConsumerGroup 连接）
	for _, r := range e.runners {
		if err := r.Stop(); err != nil {
			Logger().Errorf("[worker] Runner[%s] 关闭失败: %v", r.Name(), err)
		}
	}

	e.started = false
	Logger().Info("[worker] Engine 已关闭")

	return nil
}

// ParseHandlerTimeout 解析 handler-timeout 配置，失败则返回默认值 30s
func ParseHandlerTimeout(timeoutStr string) time.Duration {
	if timeoutStr == "" {
		return defaultTimeout
	}
	d, err := time.ParseDuration(timeoutStr)
	if err != nil {
		Logger().Warnf("[worker] handler-timeout 解析失败，使用默认值 30s: %v", err)
		return defaultTimeout
	}
	if d <= 0 {
		return defaultTimeout
	}
	return d
}

// ParseRelayConfig 从 WorkerConfig 解析 Relay 配置（导出供 main.go 等外部包使用）
func ParseRelayConfig(cfg conf.WorkerConfig) WorkerRelayConfig {
	pollInterval := 5 * time.Second
	if cfg.PollInterval != "" {
		if d, err := time.ParseDuration(cfg.PollInterval); err == nil {
			pollInterval = d
		} else {
			Logger().Warnf("[worker] poll-interval 解析失败，使用默认值 5s: %v", err)
		}
	}

	retryBaseDelay := 1 * time.Second
	if cfg.RetryBaseDelay != "" {
		if d, err := time.ParseDuration(cfg.RetryBaseDelay); err == nil {
			retryBaseDelay = d
		} else {
			Logger().Warnf("[worker] retry-base-delay 解析失败，使用默认值 1s: %v", err)
		}
	}

	leaseTimeout := 60 * time.Second
	if cfg.RelayLeaseTimeout != "" {
		if d, err := time.ParseDuration(cfg.RelayLeaseTimeout); err == nil {
			leaseTimeout = d
		} else {
			Logger().Warnf("[worker] relay-lease-timeout 解析失败，使用默认值 60s: %v", err)
		}
	}

	batchSize := cfg.RelayBatchSize
	if batchSize <= 0 {
		batchSize = 100
	}

	maxRetries := cfg.MaxRetries
	if maxRetries <= 0 {
		maxRetries = 5
	}

	cleanupDays := cfg.RelayCleanupDays
	if cleanupDays <= 0 {
		cleanupDays = 7
	}

	return WorkerRelayConfig{
		PollInterval:   pollInterval,
		BatchSize:      batchSize,
		LeaseTimeout:   leaseTimeout,
		MaxRetries:     maxRetries,
		RetryBaseDelay: retryBaseDelay,
		CleanupDays:    cleanupDays,
	}
}
