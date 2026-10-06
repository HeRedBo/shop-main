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
type Engine struct {
	consumer    *Consumer
	registry    *Registry
	relay       *Relay
	config      conf.WorkerConfig
	ctx         context.Context
	cancel      context.CancelFunc
	mu          sync.Mutex
	started     bool
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
// 2. 创建 Consumer 并启动消费循环和 Worker Pool
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
	if len(e.config.Topics) == 0 {
		return fmt.Errorf("worker topics is required")
	}

	concurrency := e.config.Concurrency
	if concurrency <= 0 {
		concurrency = 1
		global.LOG.Warn("[worker] concurrency 未配置或为 0，使用默认值 1")
	}

	// 创建 sarama ConsumerGroup
	saramaConfig := sarama.NewConfig()
	saramaConfig.Consumer.Group.Rebalance.GroupStrategies = []sarama.BalanceStrategy{sarama.NewBalanceStrategyRoundRobin()}
	saramaConfig.Consumer.Offsets.Initial = sarama.OffsetNewest
	saramaConfig.Consumer.Offsets.AutoCommit.Enable = false // 手动提交 offset

	group, err := sarama.NewConsumerGroup(global.CONFIG.Kafka.Hosts, e.config.GroupId, saramaConfig)
	if err != nil {
		return fmt.Errorf("创建 ConsumerGroup 失败: %w", err)
	}

	// 创建 Consumer
	e.consumer = NewConsumer(group, e.config.Topics, e.registry, concurrency)

	// 创建并启动 Relay（Outbox 扫描投递）
	relayConfig := parseRelayConfig(e.config)
	e.relay = NewRelay(relayConfig)
	e.relay.Start()

	// 启动
	e.consumer.Start()
	e.started = true

	global.LOG.Infof("[worker] Engine 已启动，group-id=%s, topics=%v, concurrency=%d",
		e.config.GroupId, e.config.Topics, concurrency)

	return nil
}

// Stop 优雅关闭引擎
// 有序关闭：停 Consumer → 等处理完成 → 关闭 ConsumerGroup
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

	// 2. 停止 Consumer（停拉取 + 等 Worker 处理完）
	if e.consumer != nil {
		e.consumer.Stop()
	}

	// 3. 取消 Engine context
	e.cancel()

	// 4. 关闭底层 ConsumerGroup 连接
	if e.consumer != nil && e.consumer.group != nil {
		if err := e.consumer.group.Close(); err != nil {
			global.LOG.Errorf("[worker] ConsumerGroup 关闭失败: %v", err)
			return err
		}
	}

	e.started = false
	global.LOG.Info("[worker] Engine 已关闭")

	return nil
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
