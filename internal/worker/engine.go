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
	consumers []*Consumer
	registry  *Registry
	relay     *Relay
	config    conf.WorkerConfig
	ctx       context.Context
	cancel    context.CancelFunc
	mu        sync.Mutex
	started   bool
}

// NewEngine 根据 WorkerConfig 创建 Worker 引擎
func NewEngine(config conf.WorkerConfig) *Engine {
	ctx, cancel := context.WithCancel(context.Background())
	return &Engine{
		registry: NewRegistry(),
		config:   config,
		ctx:      ctx,
		cancel:   cancel,
	}
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
		global.LOG.Info("[worker] Worker 未启用，跳过启动")
		return nil
	}

	// 校验配置
	if e.config.GroupId == "" {
		return fmt.Errorf("worker group-id is required")
	}

	// 创建 sarama ConsumerGroup（所有 Consumer 共享同一个 group）
	saramaConfig := sarama.NewConfig()
	saramaConfig.Consumer.Group.Rebalance.GroupStrategies = []sarama.BalanceStrategy{sarama.NewBalanceStrategyRoundRobin()}
	saramaConfig.Consumer.Offsets.Initial = sarama.OffsetNewest
	saramaConfig.Consumer.Offsets.AutoCommit.Enable = false // 手动提交 offset

	group, err := sarama.NewConsumerGroup(global.CONFIG.Kafka.Hosts, e.config.GroupId, saramaConfig)
	if err != nil {
		return fmt.Errorf("创建 ConsumerGroup 失败: %w", err)
	}

	// 解析全局 handler-timeout 配置
	handlerTimeout := parseHandlerTimeout(e.config.HandlerTimeout)

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

			consumer := NewConsumerWithConfig(group, config, e.registry)
			e.consumers = append(e.consumers, consumer)

			global.LOG.Infof("[worker] 创建 Consumer: topic=%s, concurrency=%d, buffer-size=%d",
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
			global.LOG.Warn("[worker] concurrency 未配置或为 0，使用默认值 1")
		}

		consumer := NewConsumerWithConfig(group, ConsumerConfig{
			Topics:         e.config.Topics,
			Concurrency:    concurrency,
			HandlerTimeout: handlerTimeout,
		}, e.registry)
		e.consumers = append(e.consumers, consumer)

		global.LOG.Infof("[worker] 创建 Consumer（兼容模式）: topics=%v, concurrency=%d",
			e.config.Topics, concurrency)
	}

	// 启动所有 Consumer
	for _, c := range e.consumers {
		c.Start()
	}

	// 创建并启动 Relay（Outbox 扫描投递）
	relayConfig := parseRelayConfig(e.config)
	e.relay = NewRelay(relayConfig)
	e.relay.Start()

	e.started = true

	global.LOG.Infof("[worker] Engine 已启动，group-id=%s, consumers=%d",
		e.config.GroupId, len(e.consumers))

	return nil
}

// Stop 优雅关闭引擎
// 有序关闭：Relay → Consumers → ConsumerGroup
func (e *Engine) Stop() error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if !e.started {
		return nil
	}

	global.LOG.Info("[worker] Engine 开始关闭...")

	// 1. 停止 Relay
	if e.relay != nil {
		e.relay.Stop()
	}

	// 2. 停止所有 Consumer（停拉取 + 等 Worker 处理完）
	for _, c := range e.consumers {
		c.Stop()
	}

	// 3. 取消 Engine context
	e.cancel()

	// 4. 关闭底层 ConsumerGroup 连接
	// 所有 Consumer 共享同一个 ConsumerGroup，只需关闭一次
	// 取第一个 Consumer 的 group 引用即可
	if len(e.consumers) > 0 && e.consumers[0].group != nil {
		if err := e.consumers[0].group.Close(); err != nil {
			global.LOG.Errorf("[worker] ConsumerGroup 关闭失败: %v", err)
			return err
		}
	}

	e.started = false
	global.LOG.Info("[worker] Engine 已关闭")

	return nil
}

// parseHandlerTimeout 解析 handler-timeout 配置，失败则返回默认值 30s
func parseHandlerTimeout(timeoutStr string) time.Duration {
	if timeoutStr == "" {
		return defaultTimeout
	}
	d, err := time.ParseDuration(timeoutStr)
	if err != nil {
		global.LOG.Warnf("[worker] handler-timeout 解析失败，使用默认值 30s: %v", err)
		return defaultTimeout
	}
	if d <= 0 {
		return defaultTimeout
	}
	return d
}

// parseRelayConfig 从 WorkerConfig 解析 Relay 配置
func parseRelayConfig(cfg conf.WorkerConfig) WorkerRelayConfig {
	pollInterval := 5 * time.Second
	if cfg.PollInterval != "" {
		if d, err := time.ParseDuration(cfg.PollInterval); err == nil {
			pollInterval = d
		} else {
			global.LOG.Warnf("[worker] poll-interval 解析失败，使用默认值 5s: %v", err)
		}
	}

	retryBaseDelay := 1 * time.Second
	if cfg.RetryBaseDelay != "" {
		if d, err := time.ParseDuration(cfg.RetryBaseDelay); err == nil {
			retryBaseDelay = d
		} else {
			global.LOG.Warnf("[worker] retry-base-delay 解析失败，使用默认值 1s: %v", err)
		}
	}

	leaseTimeout := 60 * time.Second
	if cfg.RelayLeaseTimeout != "" {
		if d, err := time.ParseDuration(cfg.RelayLeaseTimeout); err == nil {
			leaseTimeout = d
		} else {
			global.LOG.Warnf("[worker] relay-lease-timeout 解析失败，使用默认值 60s: %v", err)
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
