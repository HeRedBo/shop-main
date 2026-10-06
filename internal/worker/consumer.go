package worker

import (
	"context"
	"hash/fnv"
	"strings"
	"sync"
	"time"

	"shop/pkg/global"

	"github.com/IBM/sarama"
)

const (
	// defaultTaskBufferSize tasks channel 默认缓冲大小，用于背压控制
	defaultTaskBufferSize = 1000
	// defaultConcurrency 默认 Worker Pool 并发数
	defaultConcurrency = 1
)

// ConsumerConfig 单个 Consumer 的配置
type ConsumerConfig struct {
	Topic          string        // 单个 Topic（向后兼容，与 Topics 二选一）
	Topics         []string      // 多个 Topic（优先级高于 Topic）
	Concurrency    int           // Worker Pool 并发数（0 则使用默认值 1）
	BufferSize     int           // tasks channel 缓冲大小（0 则使用默认值 1000）
	Ordered        bool          // 是否有序消费（同 key 路由到同 worker）
	HandlerTimeout time.Duration // 单条消息处理超时（0 则使用默认值 30s）
}

// resolvedTopics 返回实际监听的 topic 列表
// Topics 优先，若为空则回退到 Topic
func (cfg *ConsumerConfig) resolvedTopics() []string {
	if len(cfg.Topics) > 0 {
		return cfg.Topics
	}
	if cfg.Topic != "" {
		return []string{cfg.Topic}
	}
	return nil
}

// Name 返回 Consumer 的标识名称，用于日志区分
// 单 topic 返回 topic 名，多 topic 返回逗号拼接
func (cfg *ConsumerConfig) Name() string {
	tps := cfg.resolvedTopics()
	if len(tps) == 0 {
		return "<no-topic>"
	}
	return strings.Join(tps, ",")
}

// defaultTimeout 默认单条消息处理超时时间
const defaultTimeout = 30 * time.Second

// Consumer 封装 Kafka ConsumerGroup 的消费逻辑
// 采用双缓冲架构：Poller（拉取消息）和 Worker Pool（处理消息）分离
// 每个 Consumer 绑定一个或多个 Topic，拥有独立的 tasks channel 和 Worker Pool
// 慢处理不会阻塞 Kafka 拉取，避免 rebalance
type Consumer struct {
	group        sarama.ConsumerGroup
	config       ConsumerConfig         // 单 Consumer 配置
	registry     *Registry
	tasks        chan *Event            // 有界缓冲 channel，背压机制（无序模式）
	ordered      bool                   // 是否有序消费
	orderedChs   []chan *Event          // 有序模式：每个 Worker 的专属 channel
	middlewares  []Middleware           // 全局中间件链
	workerWg     sync.WaitGroup         // 等待 Worker Pool goroutine 退出
	consumeWg    sync.WaitGroup         // 等待 consumeLoop goroutine 退出
	handlerTimeout time.Duration        // 单条消息处理超时
	ctx          context.Context
	cancel       context.CancelFunc
}

// NewConsumer 创建一个 Kafka Consumer（旧版 API，向后兼容）
// group: sarama ConsumerGroup 实例
// topics: 订阅的 topic 列表
// registry: Handler 注册表
// concurrency: Worker Pool 并发数
func NewConsumer(group sarama.ConsumerGroup, topics []string, registry *Registry, concurrency int) *Consumer {
	return NewConsumerWithConfig(group, ConsumerConfig{
		Topics:      topics,
		Concurrency: concurrency,
	}, registry)
}

// NewConsumerWithConfig 创建一个 Kafka Consumer（新版 API，支持 per-topic 独立配置）
// group: sarama ConsumerGroup 实例
// config: Consumer 配置（Topic/Topics、Concurrency、BufferSize）
// registry: Handler 注册表
func NewConsumerWithConfig(group sarama.ConsumerGroup, config ConsumerConfig, registry *Registry) *Consumer {
	// 默认值处理
	bufferSize := config.BufferSize
	if bufferSize <= 0 {
		bufferSize = defaultTaskBufferSize
	}
	concurrency := config.Concurrency
	if concurrency <= 0 {
		concurrency = defaultConcurrency
	}
	handlerTimeout := config.HandlerTimeout
	if handlerTimeout <= 0 {
		handlerTimeout = defaultTimeout
	}

	ctx, cancel := context.WithCancel(context.Background())
	return &Consumer{
		group: group,
		config: ConsumerConfig{
			Topic:          config.Topic,
			Topics:         config.Topics,
			Concurrency:    concurrency,
			BufferSize:     bufferSize,
			Ordered:        config.Ordered,
			HandlerTimeout: handlerTimeout,
		},
		registry:       registry,
		tasks:          make(chan *Event, bufferSize),
		ordered:        config.Ordered,
		middlewares:    []Middleware{RecoveryMiddleware(), LoggingMiddleware()},
		handlerTimeout: handlerTimeout,
		ctx:            ctx,
		cancel:         cancel,
	}
}

// SetMiddlewares 设置全局中间件链，覆盖默认中间件
func (c *Consumer) SetMiddlewares(middlewares ...Middleware) {
	c.middlewares = middlewares
}

// Start 启动消费循环
// 根据 ordered 字段选择启动模式：无序模式（共享 channel）或有序模式（独立 channel）
func (c *Consumer) Start() {
	name := c.config.Name()
	topics := c.config.resolvedTopics()

	if c.ordered {
		// 有序模式：每个 Worker 拥有独立 channel，通过 key hash 路由
		c.orderedChs = make([]chan *Event, c.config.Concurrency)
		for i := 0; i < c.config.Concurrency; i++ {
			c.orderedChs[i] = make(chan *Event, c.config.BufferSize)
			c.workerWg.Add(1)
			go c.orderedWorkerLoop(i, c.orderedChs[i])
		}
		global.LOG.Infof("[worker] [%s] Ordered Worker Pool 已启动，并发数: %d, 缓冲大小: %d（有序模式）",
			name, c.config.Concurrency, c.config.BufferSize)
	} else {
		// 无序模式：所有 Worker 共享同一个 tasks channel
		for i := 0; i < c.config.Concurrency; i++ {
			c.workerWg.Add(1)
			go c.workerLoop(i)
		}
		global.LOG.Infof("[worker] [%s] Worker Pool 已启动，并发数: %d, 缓冲大小: %d",
			name, c.config.Concurrency, c.config.BufferSize)
	}

	// 启动 ConsumerGroup 消费循环（独立 goroutine）
	c.consumeWg.Add(1)
	go c.consumeLoop()
	global.LOG.Infof("[worker] [%s] Kafka Consumer 已启动，topics: %v", name, topics)
}

// Stop 优雅关闭
// 1. cancel context 停止拉取新消息
// 2. 等待 consumeLoop 退出，确保不再有新的消息写入 tasks channel
// 3. close(tasks) 通知 Worker 处理完剩余消息后退出
// 4. 等待所有 Worker 完成
func (c *Consumer) Stop() {
	global.LOG.Infof("[worker] [%s] Consumer 开始关闭...", c.config.Name())

	// 1. 取消 context，停止拉取新消息
	c.cancel()

	// 2. 等待 consumeLoop 退出，确保不再有新的消息写入 tasks channel
	c.consumeWg.Wait()

	// 3. 关闭 channel，通知 Worker 处理完剩余消息后退出
	if c.ordered {
		for _, ch := range c.orderedChs {
			close(ch)
		}
	} else {
		close(c.tasks)
	}

	// 4. 等待所有 Worker 完成
	c.workerWg.Wait()

	global.LOG.Infof("[worker] [%s] Consumer 已关闭", c.config.Name())
}

// consumeLoop ConsumerGroup 消费循环
// 每次 session 结束（rebalance）后自动重新加入消费组
func (c *Consumer) consumeLoop() {
	defer c.consumeWg.Done()
	name := c.config.Name()
	topics := c.config.resolvedTopics()

	for {
		select {
		case <-c.ctx.Done():
			global.LOG.Infof("[worker] [%s] consumeLoop 收到取消信号，退出消费循环", name)
			return
		default:
			// Consume 会阻塞直到 session 结束（rebalance 或错误）
			if err := c.group.Consume(c.ctx, topics, c); err != nil {
				global.LOG.Errorf("[worker] [%s] ConsumerGroup.Consume error: %v", name, err)
			}
			// session 结束，检查是否需要重新加入
			if c.ctx.Err() != nil {
				return
			}
			global.LOG.Infof("[worker] [%s] Consumer session 结束，准备重新加入消费组", name)
		}
	}
}

// workerLoop Worker Pool 中的单个 Worker goroutine（无序模式）
// 从 tasks channel 读取 Event，通过中间件链包装后执行 Handler
func (c *Consumer) workerLoop(id int) {
	defer c.workerWg.Done()
	defer func() {
		if r := recover(); r != nil {
			global.LOG.Errorf("[worker] Worker-%d panic recovered: %v", id, r)
		}
	}()

	global.LOG.Infof("[worker] [%s] Worker-%d 已启动", c.config.Name(), id)

	for event := range c.tasks {
		c.processEvent(event)
	}

	global.LOG.Infof("[worker] [%s] Worker-%d 已退出", c.config.Name(), id)
}

// orderedWorkerLoop 有序模式下的 Worker goroutine
// 从专属 channel 读取 Event，保证同 key 的消息始终由同一个 Worker 处理
func (c *Consumer) orderedWorkerLoop(id int, ch <-chan *Event) {
	defer c.workerWg.Done()
	defer func() {
		if r := recover(); r != nil {
			global.LOG.Errorf("[worker] [%s] OrderedWorker-%d panic recovered: %v", c.config.Name(), id, r)
		}
	}()

	global.LOG.Infof("[worker] [%s] OrderedWorker-%d 已启动（有序模式）", c.config.Name(), id)

	for event := range ch {
		c.processEvent(event)
	}

	global.LOG.Infof("[worker] [%s] OrderedWorker-%d 已退出", c.config.Name(), id)
}

// routeKey 根据 key 计算路由目标 Worker 索引
// 使用 FNV-1a 哈希算法，保证同 key 始终路由到同一 Worker
func routeKey(key string, count int) int {
	if key == "" || count <= 0 {
		return 0
	}
	h := fnv.New32a()
	h.Write([]byte(key))
	return int(h.Sum32()) % count
}

// processEvent 处理单条事件
func (c *Consumer) processEvent(event *Event) {
	// 根据 topic 查找 Handler
	handler, ok := c.registry.GetHandler(event.Topic)
	if !ok {
		global.LOG.Warnf("[worker] [%s] 未找到 topic[%s] 的 Handler，跳过消息 offset=%d partition=%d",
			c.config.Name(), event.Topic, event.Offset, event.Partition)
		return
	}

	// 通过中间件链包装 Handler
	wrappedHandler := Chain(handler, c.middlewares...)

	// 设置超时，防止慢处理占满 Worker 槽位
	timeout := c.handlerTimeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(c.ctx, timeout)
	defer cancel()

	// 执行处理
	if err := wrappedHandler.Handle(ctx, event); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			global.LOG.Errorf("[worker] [%s] 处理消息超时 topic=%s offset=%d partition=%d timeout=%v",
				c.config.Name(), event.Topic, event.Offset, event.Partition, timeout)
		} else {
			global.LOG.Errorf("[worker] [%s] 处理消息失败 topic=%s offset=%d partition=%d err=%v",
				c.config.Name(), event.Topic, event.Offset, event.Partition, err)
		}
	}
}

// ─────────────────────────────────────────────────────────────
// 实现 sarama.ConsumerGroupHandler 接口
// ─────────────────────────────────────────────────────────────

// Setup 在新的 consumer session 开始时调用，可用于初始化资源
func (c *Consumer) Setup(session sarama.ConsumerGroupSession) error {
	global.LOG.Infof("[worker] [%s] Consumer session 建立，claims: %v", c.config.Name(), session.Claims())
	return nil
}

// Cleanup 在 consumer session 结束时调用，可用于清理资源
func (c *Consumer) Cleanup(session sarama.ConsumerGroupSession) error {
	global.LOG.Infof("[worker] [%s] Consumer session 结束", c.config.Name())
	return nil
}

// ConsumeClaim 处理某个 partition 的消息
// 从 claim.Messages() 读取消息，构造 Event 发送到 tasks channel
// 处理成功后标记消息 offset
func (c *Consumer) ConsumeClaim(session sarama.ConsumerGroupSession, claim sarama.ConsumerGroupClaim) error {
	for {
		select {
		case msg, ok := <-claim.Messages():
			if !ok {
				// channel 关闭，partition 消费结束
				return nil
			}

			// 构造 Event 对象
			event := &Event{
				Topic:     msg.Topic,
				Key:       string(msg.Key),
				Value:     msg.Value,
				Headers:   extractHeaders(msg.Headers),
				Offset:    msg.Offset,
				Partition: msg.Partition,
			}

			// 尝试从 header 中解析 EventType
			if et, exists := event.Headers["event_type"]; exists {
				event.EventType = et
			}

			// 发送到 worker channel
			if c.ordered {
				// 有序模式：按 key hash 路由到专属 Worker channel
				idx := routeKey(event.Key, len(c.orderedChs))
				select {
				case c.orderedChs[idx] <- event:
					session.MarkMessage(msg, "")
				case <-c.ctx.Done():
					return nil
				}
			} else {
				// 无序模式：发送到共享 tasks channel
				select {
				case c.tasks <- event:
					session.MarkMessage(msg, "")
				case <-c.ctx.Done():
					return nil
				}
			}

		case <-c.ctx.Done():
			return nil
		}
	}
}

// extractHeaders 将 sarama 的 RecordHeader 列表转换为 map
func extractHeaders(headers []*sarama.RecordHeader) map[string]string {
	if len(headers) == 0 {
		return nil
	}
	m := make(map[string]string, len(headers))
	for _, h := range headers {
		m[string(h.Key)] = string(h.Value)
	}
	return m
}
