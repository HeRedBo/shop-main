package worker

import (
	"context"
	"sync"

	"shop/pkg/global"

	"github.com/IBM/sarama"
)

const (
	// defaultTaskBufferSize tasks channel 默认缓冲大小，用于背压控制
	defaultTaskBufferSize = 1000
)

// Consumer 封装 Kafka ConsumerGroup 的消费逻辑
// 采用双缓冲架构：Poller（拉取消息）和 Worker Pool（处理消息）分离
// 慢处理不会阻塞 Kafka 拉取，避免 rebalance
type Consumer struct {
	group       sarama.ConsumerGroup
	topics      []string
	registry    *Registry
	tasks       chan *Event          // 有界缓冲 channel，背压机制
	middlewares []Middleware         // 全局中间件链
	workerWg    sync.WaitGroup       // 等待 Worker Pool goroutine 退出
	consumeWg   sync.WaitGroup       // 等待 consumeLoop goroutine 退出
	ctx         context.Context
	cancel      context.CancelFunc
	concurrency int                  // Worker Pool goroutine 数量
}

// NewConsumer 创建一个 Kafka Consumer
// group: sarama ConsumerGroup 实例
// topics: 订阅的 topic 列表
// registry: Handler 注册表
// concurrency: Worker Pool 并发数
func NewConsumer(group sarama.ConsumerGroup, topics []string, registry *Registry, concurrency int) *Consumer {
	ctx, cancel := context.WithCancel(context.Background())
	return &Consumer{
		group:       group,
		topics:      topics,
		registry:    registry,
		tasks:       make(chan *Event, defaultTaskBufferSize),
		middlewares: []Middleware{RecoveryMiddleware(), LoggingMiddleware()},
		ctx:         ctx,
		cancel:      cancel,
		concurrency: concurrency,
	}
}

// SetMiddlewares 设置全局中间件链，覆盖默认中间件
func (c *Consumer) SetMiddlewares(middlewares ...Middleware) {
	c.middlewares = middlewares
}

// Start 启动消费循环
// 1. 启动 Worker Pool（N 个 goroutine 从 tasks channel 读取并处理）
// 2. 启动 ConsumerGroup.Consume() 循环（持续拉取消息）
func (c *Consumer) Start() {
	// 启动 Worker Pool
	for i := 0; i < c.concurrency; i++ {
		c.workerWg.Add(1)
		go c.workerLoop(i)
	}
	global.LOG.Infof("[worker] Worker Pool 已启动，并发数: %d", c.concurrency)

	// 启动 ConsumerGroup 消费循环（独立 goroutine）
	c.consumeWg.Add(1)
	go c.consumeLoop()
	global.LOG.Infof("[worker] Kafka Consumer 已启动，topics: %v", c.topics)
}

// Stop 优雅关闭
// 1. cancel context 停止拉取新消息
// 2. 等待 consumeLoop 退出，确保不再有新的消息写入 tasks channel
// 3. close(tasks) 通知 Worker 处理完剩余消息后退出
// 4. 等待所有 Worker 完成
func (c *Consumer) Stop() {
	global.LOG.Info("[worker] Consumer 开始关闭...")

	// 1. 取消 context，停止拉取新消息
	c.cancel()

	// 2. 等待 consumeLoop 退出，确保不再有新的消息写入 tasks channel
	c.consumeWg.Wait()

	// 3. 关闭 tasks channel，通知 Worker 处理完剩余消息后退出
	close(c.tasks)

	// 4. 等待所有 Worker 完成
	c.workerWg.Wait()

	global.LOG.Info("[worker] Consumer 已关闭")
}

// consumeLoop ConsumerGroup 消费循环
// 每次 session 结束（rebalance）后自动重新加入消费组
func (c *Consumer) consumeLoop() {
	defer c.consumeWg.Done()

	for {
		select {
		case <-c.ctx.Done():
			global.LOG.Info("[worker] consumeLoop 收到取消信号，退出消费循环")
			return
		default:
			// Consume 会阻塞直到 session 结束（rebalance 或错误）
			if err := c.group.Consume(c.ctx, c.topics, c); err != nil {
				global.LOG.Errorf("[worker] ConsumerGroup.Consume error: %v", err)
			}
			// session 结束，检查是否需要重新加入
			if c.ctx.Err() != nil {
				return
			}
			global.LOG.Info("[worker] Consumer session 结束，准备重新加入消费组")
		}
	}
}

// workerLoop Worker Pool 中的单个 Worker goroutine
// 从 tasks channel 读取 Event，通过中间件链包装后执行 Handler
func (c *Consumer) workerLoop(id int) {
	defer c.workerWg.Done()
	defer func() {
		if r := recover(); r != nil {
			global.LOG.Errorf("[worker] Worker-%d panic recovered: %v", id, r)
		}
	}()

	global.LOG.Infof("[worker] Worker-%d 已启动", id)

	for event := range c.tasks {
		c.processEvent(event)
	}

	global.LOG.Infof("[worker] Worker-%d 已退出", id)
}

// processEvent 处理单条事件
func (c *Consumer) processEvent(event *Event) {
	// 根据 topic 查找 Handler
	handler, ok := c.registry.GetHandler(event.Topic)
	if !ok {
		global.LOG.Warnf("[worker] 未找到 topic[%s] 的 Handler，跳过消息 offset=%d partition=%d",
			event.Topic, event.Offset, event.Partition)
		return
	}

	// 通过中间件链包装 Handler
	wrappedHandler := Chain(handler, c.middlewares...)

	// 执行处理
	if err := wrappedHandler.Handle(c.ctx, event); err != nil {
		global.LOG.Errorf("[worker] 处理消息失败 topic=%s offset=%d partition=%d err=%v",
			event.Topic, event.Offset, event.Partition, err)
	}
}

// ─────────────────────────────────────────────────────────────
// 实现 sarama.ConsumerGroupHandler 接口
// ─────────────────────────────────────────────────────────────

// Setup 在新的 consumer session 开始时调用，可用于初始化资源
func (c *Consumer) Setup(session sarama.ConsumerGroupSession) error {
	global.LOG.Infof("[worker] Consumer session 建立，claims: %v", session.Claims())
	return nil
}

// Cleanup 在 consumer session 结束时调用，可用于清理资源
func (c *Consumer) Cleanup(session sarama.ConsumerGroupSession) error {
	global.LOG.Info("[worker] Consumer session 结束")
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

			// 发送到 tasks channel（背压：channel 满时阻塞，不会丢失消息）
			select {
			case c.tasks <- event:
				// 发送成功，标记消息（注意：这里先 mark 再处理，依赖 Handler 幂等）
				// 如果需要严格 at-least-once，应在 processEvent 成功后再 mark
				session.MarkMessage(msg, "")
			case <-c.ctx.Done():
				return nil
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
