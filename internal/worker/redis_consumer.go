package worker

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/go-redis/redis/v7"
)

// RedisConsumerConfig Redis Stream 消费者配置
type RedisConsumerConfig struct {
	StreamKey      string        // Redis Stream key（如 "delay-order-queue"）
	ConsumerGroup  string        // 消费组名称（如 "shop-delay-group"）
	ConsumerName   string        // 消费者名称（如 "shop-worker-1"）
	Concurrency    int           // Worker Pool 并发数
	BufferSize     int           // tasks channel 缓冲大小
	BlockTimeout   time.Duration // XReadGroup BLOCK 超时（默认 5s）
	HandlerTimeout time.Duration // 单条消息处理超时
	MaxRetries     int           // Pending 消息最大重试次数
}

// RedisStreamConsumer 基于 Redis Stream 的消费者实现
// 使用 XReadGroup 消费消息，支持消费组、ACK、Pending 列表
// 采用与 Kafka Consumer 相同的双缓冲架构（Poller + Worker Pool）
type RedisStreamConsumer struct {
	config      RedisConsumerConfig
	registry    *Registry
	client      *redis.Client
	tasks       chan *redisConsumeTask
	middlewares []Middleware
	workerWg    sync.WaitGroup
	consumeWg   sync.WaitGroup
	ctx         context.Context
	cancel      context.CancelFunc
}

// redisConsumeTask Redis Stream 消费任务
type redisConsumeTask struct {
	msg      *Message // 统一消息模型
	streamID string   // Redis Stream entry ID（用于 XACK）
}

// 编译期检查：RedisStreamConsumer 实现 Runnable 接口
var _ Runnable = (*RedisStreamConsumer)(nil)

// NewRedisStreamConsumer 创建 Redis Stream 消费者
func NewRedisStreamConsumer(
	client *redis.Client,
	config RedisConsumerConfig,
	registry *Registry,
) *RedisStreamConsumer {
	// 默认值处理
	if config.Concurrency <= 0 {
		config.Concurrency = defaultConcurrency
	}
	if config.BufferSize <= 0 {
		config.BufferSize = defaultTaskBufferSize
	}
	if config.BlockTimeout <= 0 {
		config.BlockTimeout = 5 * time.Second
	}
	if config.HandlerTimeout <= 0 {
		config.HandlerTimeout = defaultTimeout
	}
	if config.MaxRetries <= 0 {
		config.MaxRetries = 5
	}

	ctx, cancel := context.WithCancel(context.Background())
	return &RedisStreamConsumer{
		config:      config,
		registry:    registry,
		client:      client,
		tasks:       make(chan *redisConsumeTask, config.BufferSize),
		middlewares: []Middleware{RecoveryMiddleware(), LoggingMiddleware()},
		ctx:         ctx,
		cancel:      cancel,
	}
}

// Name 返回消费者名称
func (c *RedisStreamConsumer) Name() string {
	return fmt.Sprintf("redis:%s", c.config.StreamKey)
}

// Start 启动 Redis Stream 消费循环
func (c *RedisStreamConsumer) Start(ctx context.Context) error {
	if ctx != nil {
		c.ctx = ctx
	}

	// 1. 确保消费组已创建
	if err := c.ensureConsumerGroup(); err != nil {
		return fmt.Errorf("创建 Redis 消费组失败: %w", err)
	}

	// 2. 启动 Worker Pool（双缓冲模式，与 Kafka Consumer 一致）
	for i := 0; i < c.config.Concurrency; i++ {
		c.workerWg.Add(1)
		go c.redisWorkerLoop(i)
	}
	Logger().Infof("[redis-consumer] [%s] Worker Pool 已启动，并发数: %d, 缓冲大小: %d",
		c.Name(), c.config.Concurrency, c.config.BufferSize)

	// 3. 启动 Poller（从 Redis Stream 拉取消息）
	c.consumeWg.Add(1)
	go c.redisConsumeLoop()
	Logger().Infof("[redis-consumer] [%s] Redis Stream Consumer 已启动，stream=%s, group=%s",
		c.Name(), c.config.StreamKey, c.config.ConsumerGroup)

	return nil
}

// Stop 优雅关闭
func (c *RedisStreamConsumer) Stop() error {
	Logger().Infof("[redis-consumer] [%s] Consumer 开始关闭...", c.Name())

	c.cancel()
	c.consumeWg.Wait()
	close(c.tasks)
	c.workerWg.Wait()

	Logger().Infof("[redis-consumer] [%s] Consumer 已关闭", c.Name())
	return nil
}

// ensureConsumerGroup 确保 Redis 消费组已创建
// go-redis v7 没有 XGroupCreateMkStream，需先确保 Stream 存在
func (c *RedisStreamConsumer) ensureConsumerGroup() error {
	// 先尝试直接创建消费组（Stream 已存在的情况）
	err := c.client.XGroupCreate(c.config.StreamKey, c.config.ConsumerGroup, "0").Err()
	if err == nil {
		return nil
	}

	// 如果消费组已存在（BUSYGROUP），忽略错误
	if err.Error() == "BUSYGROUP Consumer Group name already exists" {
		return nil
	}

	// Stream 可能不存在（NOGROUP 错误），先创建 Stream
	// 使用 XAdd 写入一条占位消息以确保 Stream 存在
	dummyID, addErr := c.client.XAdd(&redis.XAddArgs{
		Stream: c.config.StreamKey,
		Values: map[string]interface{}{"__init__": "1"},
	}).Result()
	if addErr != nil {
		return fmt.Errorf("创建 Stream 失败: %w", addErr)
	}

	// 删除占位消息
	c.client.XDel(c.config.StreamKey, dummyID)

	// 重新创建消费组
	err = c.client.XGroupCreate(c.config.StreamKey, c.config.ConsumerGroup, "0").Err()
	if err != nil && err.Error() != "BUSYGROUP Consumer Group name already exists" {
		return err
	}
	return nil
}

// redisConsumeLoop Poller 循环：从 Redis Stream 拉取消息并分发到 Worker Pool
func (c *RedisStreamConsumer) redisConsumeLoop() {
	defer c.consumeWg.Done()

	for {
		select {
		case <-c.ctx.Done():
			return
		default:
		}

		// XReadGroup: 从 Stream 读取新消息（">" 表示只读取新消息）
		streams, err := c.client.XReadGroup(&redis.XReadGroupArgs{
			Group:    c.config.ConsumerGroup,
			Consumer: c.config.ConsumerName,
			Streams:  []string{c.config.StreamKey, ">"},
			Count:    int64(c.config.BufferSize),
			Block:    c.config.BlockTimeout,
		}).Result()

		if err != nil {
			if err == redis.Nil {
				continue // 无新消息，继续等待
			}
			// 判断是否为 context 取消
			if c.ctx.Err() != nil {
				return
			}
			Logger().Errorf("[redis-consumer] [%s] XReadGroup 失败: %v", c.Name(), err)
			time.Sleep(1 * time.Second) // 错误退避
			continue
		}

		for _, stream := range streams {
			for _, message := range stream.Messages {
				// 构造统一 Message 对象
				msg := c.parseStreamMessage(&message)

				// 分发到 Worker Pool（背压控制）
				select {
				case c.tasks <- &redisConsumeTask{msg: msg, streamID: message.ID}:
				case <-c.ctx.Done():
					return
				}
			}
		}
	}
}

// redisWorkerLoop Worker 循环：从 tasks channel 读取并处理消息
func (c *RedisStreamConsumer) redisWorkerLoop(id int) {
	defer c.workerWg.Done()
	defer func() {
		if r := recover(); r != nil {
			Logger().Errorf("[redis-consumer] Worker-%d panic recovered: %v", id, r)
		}
	}()

	Logger().Infof("[redis-consumer] [%s] Worker-%d 已启动", c.Name(), id)

	for task := range c.tasks {
		c.processRedisEvent(task)
	}

	Logger().Infof("[redis-consumer] [%s] Worker-%d 已退出", c.Name(), id)
}

// processRedisEvent 处理单条 Redis Stream 消息
func (c *RedisStreamConsumer) processRedisEvent(task *redisConsumeTask) {
	handler, ok := c.registry.GetHandler(task.msg.Topic)
	if !ok {
		Logger().Warnf("[redis-consumer] [%s] 未找到 topic[%s] 的 Handler，跳过消息 id=%s",
			c.Name(), task.msg.Topic, task.streamID)
		// 找不到 Handler，ACK 消息避免重复投递
		c.client.XAck(c.config.StreamKey, c.config.ConsumerGroup, task.streamID)
		return
	}

	wrappedHandler := Chain(handler, c.middlewares...)

	timeout := c.config.HandlerTimeout
	ctx, cancel := context.WithTimeout(c.ctx, timeout)
	defer cancel()

	if err := wrappedHandler.Handle(ctx, task.msg); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			Logger().Errorf("[redis-consumer] [%s] 处理消息超时 topic=%s id=%s timeout=%v",
				c.Name(), task.msg.Topic, task.streamID, timeout)
		} else {
			Logger().Errorf("[redis-consumer] [%s] 处理消息失败 topic=%s id=%s err=%v",
				c.Name(), task.msg.Topic, task.streamID, err)
		}
		// 处理失败，不 ACK → Redis Pending 列表会保留该消息
		// 后续可通过 XCLAIM 重新分配
	} else {
		// 处理成功，ACK 消息
		if err := c.client.XAck(c.config.StreamKey, c.config.ConsumerGroup, task.streamID).Err(); err != nil {
			Logger().Errorf("[redis-consumer] [%s] XACK 失败 id=%s: %v", c.Name(), task.streamID, err)
		}
	}
}

// parseStreamMessage 将 Redis Stream Message 转换为统一 Message 对象
func (c *RedisStreamConsumer) parseStreamMessage(streamMsg *redis.XMessage) *Message {
	// Redis Stream 的 Values 是 map[string]interface{}
	values := streamMsg.Values

	msg := &Message{
		Source:    "redis",
		Topic:     c.config.StreamKey, // 使用 Stream key 作为 Topic
		Key:       getStringValue(values, "key"),
		Value:     []byte(getStringValue(values, "value")),
		Headers:   make(map[string]string),
		RawOffset: streamMsg.ID,
	}

	// 提取 headers
	if headers, ok := values["headers"].(map[string]interface{}); ok {
		for k, v := range headers {
			msg.Headers[k] = fmt.Sprintf("%v", v)
		}
	}

	// 解析 EventType
	if et, exists := msg.Headers["event_type"]; exists {
		msg.EventType = et
	}

	return msg
}

// getStringValue 从 map 中安全获取 string 值
func getStringValue(m map[string]interface{}, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
		return fmt.Sprintf("%v", v)
	}
	return ""
}
