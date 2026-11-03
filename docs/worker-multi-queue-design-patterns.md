# Worker 多队列架构设计模式与实现详解

## 一、设计哲学

### 1.1 从"Kafka 专用"到"队列无关"

#### 原有架构的 6 个 Kafka 耦合点

在多队列架构改造之前，Worker 模块是一个 Kafka 专用消费引擎，存在以下 6 个硬编码耦合点：

| 耦合点 | 位置 | 具体表现 |
|--------|------|----------|
| **Consumer 层** | `internal/worker/consumer.go` | 直接持有 `sarama.ConsumerGroup`，无法替换为其他队列 |
| **Event 模型** | `internal/worker/message.go` (原) | 包含 `Offset`/`Partition` 等 Kafka 特有字段 |
| **ACK 机制** | `internal/worker/consumer.go` | 与 `sarama.Session.MarkMessage()` 绑定 |
| **Relay 层** | `internal/worker/relay.go` (原) | 硬编码调用 Kafka Producer 投递 |
| **Outbox 层** | `internal/worker/outbox.go` (原) | 使用 `Topic`/`PartitionKey` 术语，无队列类型区分 |
| **Engine 层** | `internal/worker/engine.go` (原) | 只创建 `sarama.ConsumerGroup`，无扩展点 |

#### 为什么要抽象：开闭原则

抽象的核心驱动力是**开闭原则（Open/Closed Principle）**——对扩展开放，对修改封闭。

当需要引入 Redis Stream 作为新的队列实现时，如果核心逻辑（消费、投递、消息模型）都硬编码了 Kafka 细节，那么每新增一种队列类型都需要修改大量已有代码。这不仅增加引入 Bug 的风险，也违背了模块化设计的基本原则。

通过在关键边界引入接口抽象，使得：
- **新增队列类型**（如 RocketMQ、RabbitMQ）只需新增接口实现，不改动调度逻辑
- **业务 Handler** 完全不感知底层队列差异
- **测试** 可以针对接口 Mock，无需启动真实队列

#### 抽象的粒度选择

一个关键的设计决策是：**在 Consumer/Producer 层抽象，而不在消息中间件层抽象**。

```
❌ 过度抽象：定义 MessageQueue 接口包含 Connect/Disconnect/Subscribe/Publish/Ack...
   → 不同队列的语义差异巨大（Kafka 的 Partition vs Redis 的 Stream），强行统一会丢失特性

✅ 适度抽象：只在 Runnable（任务生命周期）和 Message（消息模型）层抽象
   → Consumer 内部各自实现拉取逻辑，对外暴露统一的 Start/Stop 接口
   → Handler 接收统一的 Message 对象，不关心消息来自 Kafka 还是 Redis
```

这种粒度选择遵循了**接口隔离原则（ISP）**——客户端不应该被迫依赖它不使用的接口。

### 1.2 Runnable 通用任务抽象

#### 灵感来源

Runnable 接口的设计灵感来自两个经典接口：

- **Java `Runnable`**：`public interface Runnable { void run(); }` —— 最小化的任务契约
- **Go `io.Reader`**：`type Reader interface { Read(p []byte) (n int, err error) }` —— 用接口描述能力而非实现

两者共同的设计哲学：**接口应该描述"能做什么"，而非"怎么做"**。

#### 设计理念

```go
// internal/worker/runnable.go
type Runnable interface {
    Name() string                  // 标识（用于日志和监控）
    Start(ctx context.Context) error // 启动（ctx 控制生命周期）
    Stop() error                   // 关闭（优雅退出）
}
```

三个方法构成了一个**最小化的守护任务契约**：
- `Name()` — 可观测性基础，所有日志和监控都依赖任务标识
- `Start(ctx)` — 通过 context 实现生命周期控制，收到取消信号后应停止运行
- `Stop()` — 优雅关闭，等待当前处理完毕后再返回

#### 与 Hyperf Process 模型的对齐

| 概念 | Hyperf (PHP) | Shop-Main (Go) | 职责 |
|------|-------------|----------------|------|
| **运行单元** | Process（独立进程） | Process（goroutine） | 封装 Runnable + 状态信息 |
| **行为契约** | — | Runnable 接口 | 定义 Start/Stop 生命周期 |
| **生命周期管理** | ProcessManager | ProcessManager | 统一编排启动/关闭/状态 |

Hyperf 的 Process 是操作系统级进程（fork），而 Shop-Main 的 Process 是 goroutine 级任务单元。两者的共同点是：**ProcessManager 不关心 Process 内部做什么，只管理其生命周期**。

#### 核心价值

Runnable 接口的引入，使得以下异构任务可以被统一管理：

- `Consumer` — Kafka 消费者（双缓冲架构）
- `RedisStreamConsumer` — Redis Stream 消费者（双缓冲架构）
- 未来可扩展：监控上报任务、健康检查任务、定时同步任务等

### 1.3 接口隔离原则的应用

多队列架构中，接口隔离原则（ISP）体现在三个层次：

**层次一：任务生命周期抽象 — Runnable**

```go
// internal/worker/runnable.go
type Runnable interface {
    Name() string
    Start(ctx context.Context) error
    Stop() error
}
```

最小化任务契约，不包含任何队列特有概念。Kafka Consumer 和 Redis Stream Consumer 都实现此接口，ProcessManager 只依赖此接口编排生命周期。

**层次二：生产者抽象 — QueueProducer**

```go
// internal/worker/queue_producer.go
type QueueProducer interface {
    Produce(ctx context.Context, msg *Message) error
    ProduceBatch(ctx context.Context, msgs []*Message) error
    Close() error
}
```

屏蔽不同队列的投递差异，业务层通过此接口发送消息，无需关心底层是 Kafka 的 `ProducerMessage` 还是 Redis 的 `XADD`。

**层次三：业务处理抽象 — Handler**

```go
// internal/worker/handler.go
type Handler interface {
    Handle(ctx context.Context, msg *Message) error
}
```

业务处理器只关心"收到一条消息并处理"，不感知消息来自哪个队列。`*Message` 是统一消息模型，包含 `Source` 字段区分来源。

---

## 二、核心设计模式详解

### 2.1 Strategy 模式（策略模式）

**在架构中的应用位置**：Runnable 接口 + KafkaConsumer / RedisStreamConsumer 实现

**解决的具体问题**：不同队列类型的消费逻辑差异巨大（Kafka 用 `ConsumerGroup`，Redis 用 `XReadGroup`），但上层 Handler 和 ProcessManager 不关心这些差异。Strategy 模式将这些差异封装在具体实现中，对上层透明。

**代码实现**：

```go
// 接口定义 — internal/worker/runnable.go
type Runnable interface {
    Name() string
    Start(ctx context.Context) error
    Stop() error
}

// Kafka 实现 — internal/worker/consumer.go
type Consumer struct {
    group    sarama.ConsumerGroup
    config   ConsumerConfig
    registry *Registry
    tasks    chan *consumeTask
    // ... Kafka 特有字段
}
var _ Runnable = (*Consumer)(nil) // 编译期接口检查

// Redis 实现 — internal/worker/redis_consumer.go
type RedisStreamConsumer struct {
    client   *redis.Client
    config   RedisConsumerConfig
    registry *Registry
    tasks    chan *redisConsumeTask
    // ... Redis 特有字段
}
var _ Runnable = (*RedisStreamConsumer)(nil) // 编译期接口检查
```

ProcessManager 只持有 `Runnable` 接口引用，调用方式完全一致：

```go
// internal/worker/process_manager.go — Start()
for _, p := range pm.processes {
    if err := p.Runner.Start(pm.ctx); err != nil {
        pm.stopStartedProcesses(p)
        return fmt.Errorf("启动 Process[%s] 失败: %w", p.Name, err)
    }
    p.Status = ProcessStatusRunning
}
```

**模式效果**：
- ProcessManager 只依赖 `Runnable` 接口，新增队列类型（如 RocketMQ）只需新增实现类
- 各实现内部可自由选择最优策略（Kafka 用 ConsumerGroup，Redis 用 XReadGroup）
- 编译期 `var _ Runnable = (*T)(nil)` 检查确保接口实现正确性

### 2.2 Factory 模式（工厂模式）

**在架构中的应用位置**：`Engine.Start()` 根据配置创建不同类型的 Runnable

**解决的具体问题**：Consumer 的创建逻辑复杂——Kafka Consumer 需要 `sarama.Config`、`ConsumerGroup`、`ConsumerConfig` 等多个参数；Redis Consumer 需要独立的 `*redis.Client`（因为 cache 封装层不支持 Pub/Sub）。Factory 模式将创建与使用分离。

**代码实现**：

```go
// internal/worker/engine.go — Engine.Start() 中的工厂逻辑

// Kafka Consumer 创建：需要 sarama ConsumerGroup + ConsumerConfig
for _, h := range e.config.Handlers {
    config := ConsumerConfig{
        Topic:          h.Topic,
        Concurrency:    h.Concurrency,
        BufferSize:     h.BufferSize,
        Ordered:        h.Ordered,
        HandlerTimeout: handlerTimeout,
    }
    // 为每个 Consumer 创建独立的 ConsumerGroup
    consumerGroup, err := sarama.NewConsumerGroup(
        global.CONFIG.Kafka.Hosts, e.config.GroupId, saramaConfig,
    )
    consumer := NewConsumerWithConfig(consumerGroup, config, e.registry)
    e.runners = append(e.runners, consumer)
}

// Redis Consumer 通过 AddProcess 注册（外部工厂创建后注入）
// 在 cmd/worker/main.go 中：
redisConsumer := worker.NewRedisStreamConsumer(redisClient, redisConfig, registry)
process := worker.NewProcess("redis:delay-order", "redis", redisConsumer)
engine.AddProcess(process)
```

**模式效果**：
- Engine 不需要知道 Consumer 的内部构造细节
- 新增队列类型时，只需新增工厂函数，Engine 代码不变
- 创建失败时可以优雅回滚（关闭已创建的 ConsumerGroup）

### 2.3 Observer 模式（观察者模式）

**在架构中的应用位置**：GORM Observer → event_outbox → Relay

**解决的具体问题**：业务代码不应该关心事件发送逻辑。当订单创建、商品更新等操作发生时，系统需要自动捕获这些事件并写入 Outbox 表。Observer 模式通过模型回调实现"零侵入"的事件捕获。

**代码实现**：

```go
// 业务层观察者 — internal/observers/order_observer.go
type OrderObserver struct{}

func (o *OrderObserver) ObserveModel() string {
    return "store_order"
}

// AfterCreate 回调：订单创建时自动写入 Outbox
func (o *OrderObserver) AfterCreate(tx *gorm.DB, model interface{}) error {
    order, ok := model.(*models.StoreOrder)
    if !ok {
        return nil
    }
    // 写入 Kafka Outbox（与业务操作在同一事务中）
    return worker.WriteKafkaOutbox(
        tx,
        order.OrderId,
        "order.created",
        worker.TopicOrderEvents,
        order.OrderId,
        order,
    )
}

// 延迟队列场景 — internal/service/order_service/order.go
// CreateOrder 中调用 DelayQueue.Push 写入延迟消息
func (s *OrderService) CreateOrder(tx *gorm.DB, order *models.StoreOrder) error {
    // ... 业务逻辑 ...

    // 写入延迟 Outbox（30 分钟后超时取消检查）
    return worker.WriteDelayOutbox(
        tx,
        order.OrderId,
        "order.timeout_cancel",
        worker.TopicDelayOrderTimeout,
        order.OrderId,
        worker.OrderEventPayload{OrderId: order.OrderId, Action: "timeout_cancel"},
        30*time.Minute,
    )
}

// Relay 层 — internal/worker/relay.go
// 扫描 outbox 表，按 queue_type 分发投递到 Kafka 或 Redis
func (r *Relay) scanAndDeliver() {
    r.scanAndDeliverByType(models.QueueTypeKafka, true)   // 即时消息
    r.scanAndDeliverByType(models.QueueTypeRedis, false)  // 延迟消息
}
```

**模式效果**：
- 业务代码零侵入事件发送逻辑
- 新增事件类型只需在 Observer 中添加回调方法
- 模型变更自动捕获，不会遗漏

### 2.4 State Machine（状态机模式）

**在架构中的应用位置**：`event_outbox.status` 字段流转

**解决的具体问题**：Outbox 记录有明确的生命周期（WAIT → SENDING → SENT/DEAD），需要防止非法状态转换（如 SENT 不能回退到 WAIT）。状态机模式通过 `UPDATE WHERE status=X` 原子操作保证状态转换的合法性。

**代码实现**：

```
状态流转图：

WAIT ──抢占成功──▶ SENDING ──投递成功──▶ SENT
  │                   │
  │                   └──超过重试──▶ DEAD (移入 event_dead_letter)
  │                   └──投递失败──▶ WAIT (retry_count+1, 指数退避)
  │
  └──被其他实例抢占──▶ (跳过，不变更状态)

租约回收：
SENDING ──超时(sending_at > LeaseTimeout)──▶ WAIT (重置)
```

关键代码——原子抢占（`internal/worker/relay.go` 的 `deliverOne()`）：

```go
// 原子抢占：UPDATE status=SENDING WHERE id=? AND status=WAIT
// 利用数据库行锁保证多实例环境下只有一個 Relay 能抢占成功
result := global.Db.Model(&models.EventOutbox{}).
    Where("id = ? AND status = ?", outbox.Id, models.OutboxStatusWait).
    Updates(map[string]interface{}{
        "status":     models.OutboxStatusSending,
        "sending_at": time.Now(),
    })
if result.RowsAffected == 0 {
    // 已被其他实例抢占，跳过
    return
}

// 投递成功后更新为 SENT
global.Db.Model(&models.EventOutbox{}).
    Where("id = ? AND status = ?", outbox.Id, models.OutboxStatusSending).
    Updates(map[string]interface{}{
        "status":     models.OutboxStatusSent,
        "sending_at": nil,
        "error_msg":  "",
    })
```

投递失败时的重试逻辑（`handleDeliveryFailure`）：

```go
func (r *Relay) handleDeliveryFailure(outbox *models.EventOutbox, errMsg string) {
    newRetryCount := outbox.RetryCount + 1
    if newRetryCount >= outbox.MaxRetries {
        // 超过最大重试次数 → 移入死信表
        r.moveToDeadLetter(outbox, errMsg)
        return
    }
    // 保持 WAIT 状态，retry_count+1，记录错误信息
    global.Db.Model(&models.EventOutbox{}).
        Where("id = ? AND status = ?", outbox.Id, models.OutboxStatusSending).
        Updates(map[string]interface{}{
            "status":      models.OutboxStatusWait,
            "sending_at":  nil,
            "retry_count": newRetryCount,
            "error_msg":   errMsg,
        })
}
```

**模式效果**：
- 状态转换规则明确，非法转换被 `WHERE status=X` 条件阻止
- 多实例环境下的并发安全通过数据库原子操作保证
- 租约回收机制防止 SENDING 状态永久卡住

### 2.5 Chain of Responsibility（责任链模式）

**在架构中的应用位置**：Middleware 链

**解决的具体问题**：横切关注点（Recovery、Logging、Timeout）需要可插拔，不影响业务 Handler。责任链模式将多个中间件串联，每个中间件可以选择性地处理消息、传递给下一个节点、或中断链。

**代码实现**：

```go
// internal/worker/middleware.go
type Middleware func(Handler) Handler

// Chain 将多个 Middleware 串联，最后一个为实际 Handler
// 执行顺序：middlewares[0] → middlewares[1] → ... → h
func Chain(h Handler, middlewares ...Middleware) Handler {
    // 从后往前包装，确保执行顺序从左到右
    for i := len(middlewares) - 1; i >= 0; i-- {
        h = middlewares[i](h)
    }
    return h
}

// 内置中间件工厂函数
func RecoveryMiddleware() Middleware   { /* panic 恢复 */ }
func LoggingMiddleware() Middleware   { /* 日志记录 */ }
func TimeoutMiddleware(timeout time.Duration) Middleware { /* 超时控制 */ }
```

在 Consumer 中的使用方式：

```go
// internal/worker/consumer.go — processEvent()
wrappedHandler := Chain(handler, c.middlewares...)

// 设置超时，防止慢处理占满 Worker 槽位
ctx, cancel := context.WithTimeout(c.ctx, c.handlerTimeout)
defer cancel()

if err := wrappedHandler.Handle(ctx, task.msg); err != nil {
    // 处理失败逻辑...
}
```

**模式效果**：
- 中间件可独立开发、测试和替换
- 新增横切关注点（如 Metrics、Tracing）只需新增 Middleware，不改 Handler
- 执行顺序可控，从外到内依次经过每个中间件

### 2.6 Producer-Consumer 模式（双缓冲架构）

**在架构中的应用位置**：KafkaConsumer 和 RedisStreamConsumer 都采用的 Poller + Worker Pool 架构

**解决的具体问题**：慢处理不会阻塞拉取，拉取速率和处理速率完全解耦。这是整个 Worker 架构的核心设计。

**代码实现**：

```
双缓冲架构数据流：

消息队列 (Kafka/Redis)
    │
    ▼
┌──────────────────────────────────────────┐
│  Poller (1 goroutine)                    │
│  Kafka: ConsumeClaim → 构造 Message      │
│  Redis: XReadGroup BLOCK → 构造 Message  │
│                                          │
│  写入 ▼                                  │
│  ┌─────────────────────────────┐         │
│  │  tasks channel (有界缓冲)    │ ← 背压  │
│  │  默认大小: 1000              │         │
│  └─────────────────────────────┘         │
│               │                          │
│  Worker Pool  ▼ (N goroutines)           │
│  ┌─────┐ ┌─────┐ ┌─────┐ ┌─────┐       │
│  │ W-0 │ │ W-1 │ │ W-2 │ │ W-N │       │
│  └──┬──┘ └──┬──┘ └──┬──┘ └──┬──┘       │
│     └───────┴───────┴───────┘            │
│                 │                        │
│                 ▼                        │
│         Handler.Handle()                 │
│         (业务处理逻辑)                    │
└──────────────────────────────────────────┘
```

Kafka Consumer 的 Poller（`internal/worker/consumer.go`）：

```go
// ConsumeClaim — Poller 端：从 Kafka 拉取消息，写入 tasks channel
func (c *Consumer) ConsumeClaim(session sarama.ConsumerGroupSession,
    claim sarama.ConsumerGroupClaim) error {
    for {
        select {
        case kafkaMsg, ok := <-claim.Messages():
            if !ok { return nil }
            msg := &Message{
                Source: "kafka", Topic: kafkaMsg.Topic,
                Key: string(kafkaMsg.Key), Value: kafkaMsg.Value,
                Headers: extractHeaders(kafkaMsg.Headers),
                RawOffset: kafkaMsg.Offset,
            }
            // 发送到 worker channel（背压：channel 满时阻塞）
            select {
            case c.tasks <- &consumeTask{msg: msg, session: session, raw: kafkaMsg}:
            case <-c.ctx.Done():
                return nil
            }
        case <-c.ctx.Done():
            return nil
        }
    }
}
```

Redis Stream Consumer 的 Poller（`internal/worker/redis_consumer.go`）：

```go
// redisConsumeLoop — Poller 端：从 Redis Stream 拉取消息
func (c *RedisStreamConsumer) redisConsumeLoop() {
    defer c.consumeWg.Done()
    for {
        select {
        case <-c.ctx.Done(): return
        default:
        }
        streams, err := c.client.XReadGroup(&redis.XReadGroupArgs{
            Group: c.config.ConsumerGroup, Consumer: c.config.ConsumerName,
            Streams: []string{c.config.StreamKey, ">"},
            Count: int64(c.config.BufferSize), Block: c.config.BlockTimeout,
        }).Result()
        // ... 错误处理 ...
        for _, stream := range streams {
            for _, message := range stream.Messages {
                msg := c.parseStreamMessage(&message)
                select {
                case c.tasks <- &redisConsumeTask{msg: msg, streamID: message.ID}:
                case <-c.ctx.Done(): return
                }
            }
        }
    }
}
```

**模式效果**：
- **慢处理隔离**：慢处理占用 Worker goroutine，不影响 Poller 继续拉取
- **背压机制**：tasks channel 满时 Poller 自然阻塞，不会无限堆积内存
- **独立调优**：拉取并发（Poller）和处理并发（Worker Pool）可分别控制
- **架构一致性**：Kafka 和 Redis 两种队列采用相同的双缓冲模式

### 2.7 Transactional Outbox（事务发件箱）

**在架构中的应用位置**：`event_outbox` 表 + Relay + DelayScheduler

**解决的具体问题**：分布式系统中的 Dual Write Problem——业务操作（写数据库）和事件发送（写消息队列）无法在同一个事务中完成。如果先写库再发队列，发队列失败则事件丢失；如果先发队列再写库，写库失败则产生幽灵事件。

**核心思想**：把"跨系统的一致性问题"转化为"单个数据库的事务问题"——降维打击。

```go
// internal/worker/outbox.go — WriteOutbox()
// 在业务事务中调用，保证业务数据与事件记录原子性
func WriteOutbox(tx *gorm.DB, aggregateId, eventType, topic,
    partitionKey string, payload interface{},
    queueType string, executeAt *time.Time) error {

    payloadJSON, _ := json.Marshal(payload)
    outbox := models.EventOutbox{
        EventId: uuid.New().String(), AggregateId: aggregateId,
        EventType: eventType, Topic: topic,
        PartitionKey: partitionKey, Payload: string(payloadJSON),
        QueueType: queueType, Status: models.OutboxStatusWait,
        MaxRetries: 5, ExecuteAt: executeAt,
    }
    // 关键：使用 Exec 直接执行 INSERT，绕过 GORM 回调系统
    // 在 Observer 回调中避免 Statement 干扰
    tx.Statement.SQL.Reset()
    tx.Statement.Vars = nil
    result := tx.Exec(
        `INSERT INTO event_outbox (...) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
        // ... 参数 ...
    )
    // Redis PUBLISH 异步通知 Relay 立即扫描（失败不影响主流程）
    go notifyRelay(outbox.EventId)
    return nil
}
```

**模式效果**：
- 业务操作和事件记录在同一个数据库事务中原子提交
- Relay 异步扫描投递，不阻塞业务流程
- 即使消息队列暂时不可用，事件也不会丢失（最终一致性）

---

## 三、关键技术决策

### 3.1 消息模型演进策略

**决策点**：原有 `Event` 结构体包含 Kafka 特有字段（Offset、Partition），如何演进到支持多队列的 `Message`？

| 选项 | 方案 | 优点 | 缺点 |
|------|------|------|------|
| A | 直接重构 Event → Message | 一步到位，无历史包袱 | 改动面大，所有 Handler 需同步修改 |
| B | 保留 Event，新增 Message，提供转换函数 | 改动面小 | 两套模型并存，认知负担重 |
| **C** | **新增 Message，`type Event = Message` 别名** | **向后兼容 + 统一模型** | **需要所有 Handler 逐步迁移** |

**最终选择**：C

```go
// internal/worker/message.go
type Message struct {
    Source    string            // 来源标识: "kafka" / "redis"
    Topic     string            // 统一术语 (Kafka topic / Redis stream key)
    Key       string            // 路由键
    Value     []byte            // 消息体
    Headers   map[string]string // 元数据
    EventType string            // 事件类型
    Delay     time.Duration     // 延迟时间（Redis 特有，Kafka 为 0）
    RawOffset interface{}       // Kafka: int64 offset, Redis: Stream ID string
}

// 向后兼容别名
type Event = Message
```

**选择理由**：当前 Handler 数量少（4 个），改动面可控。类型别名 `Event = Message` 保证现有代码零修改即可编译通过。

### 3.2 Redis 延迟队列方案

**决策点**：选择哪种 Redis 数据结构实现延迟队列？

| 选项 | 方案 | 优点 | 缺点 |
|------|------|------|------|
| A | Sorted Set（调度）+ Stream（消费） | 两段式，职责清晰 | 需要两套数据结构 |
| B | 纯 Stream + 定时 XADD | 单一数据结构 | 无法精确控制延迟时间 |
| C | 纯 Sorted Set + ZRANGEBYSCORE | 简单 | 无消费组 + ACK 语义 |

**最终选择**：A

```
延迟消息两段式架构：

Sorted Set (delay:schedule)          Redis Stream (delay-order-timeout)
┌─────────────────────────┐          ┌─────────────────────────┐
│ score: execute_at (ms)  │          │ 消息体 + headers        │
│ member: event_id        │          │ 消费组 + ACK 语义       │
│                         │  到期后   │                         │
│ ZRANGEBYSCORE ≤ now     │──XADD──▶│ XReadGroup BLOCK        │
│ ZREM (投递成功后清理)    │          │ XACK (处理成功后确认)    │
└─────────────────────────┘          └─────────────────────────┘
        ↑ 调度层                            ↑ 消费层
   DelayScheduler                    RedisStreamConsumer
   每秒轮询                           双缓冲架构
```

**选择理由**：
- Sorted Set 天然支持按时间排序和范围查询（`ZRANGEBYSCORE`）
- Stream 提供消费组 + ACK 语义，与 Kafka Consumer 架构对齐
- 两段式解耦：调度层负责"何时投递"，消费层负责"如何处理"

### 3.3 双路径投递（主路径 + 降级兜底）

**决策点**：延迟消息如何保证可靠投递？

**最终选择**：双路径投递

```
主路径（低延迟）：
WriteDelayOutbox → event_outbox + ZADD Sorted Set
    → DelayScheduler 每秒轮询 ZRANGEBYSCORE
    → 到期消息 XADD 到 Redis Stream
    → RedisStreamConsumer 消费

降级路径（高可靠）：
Relay 每 10s 扫描 execute_at <= NOW() 的 WAIT 记录
    → XADD 到 Redis Stream（兜底投递）

幂等保证：
两条路径都通过 UPDATE status=SENDING WHERE status=WAIT 原子抢占
先抢占成功的路径负责投递，另一条路径发现 status≠WAIT 则跳过
```

**选择理由**：Redis 可能不可用（网络抖动、重启等），DB Relay 作为兜底保证最终一致性。双路径通过原子抢占避免重复投递。

### 3.4 Redis Stream 消费模式

**决策点**：Redis Stream 消费者采用什么架构模式？

| 选项 | 方案 | 优点 | 缺点 |
|------|------|------|------|
| A | 复用双缓冲模式（Poller + Worker Pool） | 与 Kafka 架构一致 | 代码稍复杂 |
| B | XReadGroup BLOCK 直接消费 | 简单直接 | 慢处理阻塞拉取 |

**最终选择**：A

```go
// internal/worker/redis_consumer.go — 双缓冲架构
type RedisStreamConsumer struct {
    tasks    chan *redisConsumeTask  // 有界缓冲 channel（背压）
    workerWg sync.WaitGroup         // Worker Pool WaitGroup
    consumeWg sync.WaitGroup        // Poller WaitGroup
    // ...
}
```

**选择理由**：与 Kafka Consumer 架构保持一致，背压机制在两种队列中都有价值。慢处理不会阻塞 Redis Stream 的消息拉取。

---

## 四、代码实现详解

### 4.1 统一消息模型 Message

**文件**：`internal/worker/message.go`

**设计思路**：用 `Source` 字段区分消息来源，用 `RawOffset`（`interface{}` 类型）承载队列特有的 ACK 信息。

| 字段 | 类型 | 说明 | Kafka 场景 | Redis 场景 |
|------|------|------|-----------|-----------|
| `Source` | `string` | 来源标识 | `"kafka"` | `"redis"` |
| `Topic` | `string` | 统一术语 | Kafka topic | Redis stream key |
| `Key` | `string` | 路由键 | 分区键 | Stream 路由键 |
| `Value` | `[]byte` | 消息体 | JSON payload | JSON payload |
| `Headers` | `map[string]string` | 元数据 | RecordHeader | Stream field |
| `EventType` | `string` | 事件类型 | 从 Header 解析 | 从 Header 解析 |
| `Delay` | `time.Duration` | 延迟时间 | 0（不支持） | 实际延迟值 |
| `RawOffset` | `interface{}` | ACK 偏移量 | `int64` offset | Stream ID `string` |

### 4.2 ProcessManager 编排策略

**文件**：`internal/worker/process_manager.go`

**启动策略**：串行启动

```go
// internal/worker/process_manager.go — Start()
func (pm *ProcessManager) Start() error {
    pm.mu.Lock()
    defer pm.mu.Unlock()

    // 串行启动所有 Process（避免并发创建 ConsumerGroup 时的竞争）
    for _, p := range pm.processes {
        if err := p.Runner.Start(pm.ctx); err != nil {
            pm.stopStartedProcesses(p) // 启动失败：回滚已启动的 Process
            return fmt.Errorf("启动 Process[%s] 失败: %w", p.Name, err)
        }
        p.Status = ProcessStatusRunning
    }
    pm.started = true
    return nil
}
```

**关闭策略**：逆序关闭（后启动的先关闭，类似栈的 LIFO 原则）

```go
func (pm *ProcessManager) Stop() error {
    // 逆序关闭
    for i := len(pm.processes) - 1; i >= 0; i-- {
        p := pm.processes[i]
        if err := p.Runner.Stop(); err != nil {
            p.Status = ProcessStatusFailed
        } else {
            p.Status = ProcessStatusStopped
        }
    }
    pm.cancel()
    return nil
}
```

**线程安全**：`sync.Mutex` 保护 `processes` 列表，`AddProcess()` 和 `GetProcesses()` 都加锁。

### 4.3 DelayScheduler 调度机制

**文件**：`internal/worker/delay_scheduler.go`

**核心循环**：

```
Ticker 每秒触发
    │
    ▼
ZRANGEBYSCORE delay:schedule -inf {now}
    │
    ▼ (获取到期消息列表)
    │
    ├── 遍历每条 eventId ──▶ deliverDelayedMessage()
    │       │
    │       ├── 1. 查 DB: WHERE event_id=? AND status=WAIT
    │       ├── 2. 原子抢占: UPDATE status=SENDING WHERE status=WAIT
    │       ├── 3. XADD Stream: 写入 Redis Stream
    │       ├── 4. 更新 SENT: status=SENT
    │       └── 5. ZREM: 从 Sorted Set 移除
    │
    └── 失败处理:
            ├── XADD 失败 → 回滚 status=WAIT，等待下次调度
            └── DB 记录不存在 → ZREM 清理 Sorted Set 残留
```

**ScheduleDelay() 函数**：异步 ZADD，失败不影响主流程

```go
// internal/worker/delay_scheduler.go
func ScheduleDelay(client *redis.Client, eventId string, executeAt time.Time) error {
    if client == nil {
        return nil // Redis 不可用，依赖 DB Relay 兜底
    }
    score := float64(executeAt.UnixMilli())
    return client.ZAdd(delayScheduleKey, &redis.Z{
        Score: score, Member: eventId,
    }).Err()
}
```

### 4.4 RedisStreamConsumer 双缓冲实现

**文件**：`internal/worker/redis_consumer.go`

**Poller**：`XReadGroup BLOCK` 拉取 → 转换为 `Message` → 放入 `tasks` channel

```go
func (c *RedisStreamConsumer) redisConsumeLoop() {
    // XReadGroup: 从 Stream 读取新消息（">" 表示只读取新消息）
    streams, err := c.client.XReadGroup(&redis.XReadGroupArgs{
        Group:    c.config.ConsumerGroup,
        Consumer: c.config.ConsumerName,
        Streams:  []string{c.config.StreamKey, ">"},
        Count:    int64(c.config.BufferSize),
        Block:    c.config.BlockTimeout,
    }).Result()
    // ... 转换为 Message 并分发到 Worker Pool ...
}
```

**Worker Pool**：从 `tasks` channel 读取 → `Handler.Handle()` → `XACK` 确认

```go
func (c *RedisStreamConsumer) processRedisEvent(task *redisConsumeTask) {
    handler, ok := c.registry.GetHandler(task.msg.Topic)
    wrappedHandler := Chain(handler, c.middlewares...)
    ctx, cancel := context.WithTimeout(c.ctx, timeout)
    defer cancel()

    if err := wrappedHandler.Handle(ctx, task.msg); err != nil {
        // 处理失败，不 ACK → Redis Pending 列表保留该消息
    } else {
        // 处理成功，ACK 消息
        c.client.XAck(c.config.StreamKey, c.config.ConsumerGroup, task.streamID)
    }
}
```

**消费组创建**：`ensureConsumerGroup()` 处理 go-redis v7 无 `MkStream` 的问题

```go
func (c *RedisStreamConsumer) ensureConsumerGroup() error {
    err := c.client.XGroupCreate(c.config.StreamKey, c.config.ConsumerGroup, "0").Err()
    if err == nil { return nil }
    if err.Error() == "BUSYGROUP Consumer Group name already exists" { return nil }
    // Stream 可能不存在，先创建（XADD 占位消息再删除）
    dummyID, _ := c.client.XAdd(&redis.XAddArgs{
        Stream: c.config.StreamKey, Values: map[string]interface{}{"__init__": "1"},
    }).Result()
    c.client.XDel(c.config.StreamKey, dummyID)
    // 重新创建消费组
    return c.client.XGroupCreate(c.config.StreamKey, c.config.ConsumerGroup, "0").Err()
}
```

### 4.5 Relay 多队列投递

**文件**：`internal/worker/relay.go`

**scanAndDeliver()**：按 `queue_type` 分别扫描

```go
func (r *Relay) scanAndDeliver() {
    // 1. 扫描即时消息（Kafka）：queue_type=kafka 且 execute_at IS NULL
    r.scanAndDeliverByType(models.QueueTypeKafka, true)
    // 2. 扫描延迟消息（Redis）：queue_type=redis 且 execute_at <= NOW()
    r.scanAndDeliverByType(models.QueueTypeRedis, false)
}
```

**deliverToKafka()**：sarama.ProducerMessage 投递

```go
func (r *Relay) deliverToKafka(outbox *models.EventOutbox) error {
    producer := mq.GetKafkaSyncProducer(mq.DefaultKafkaSyncProducer)
    msg := &sarama.ProducerMessage{
        Topic: outbox.Topic,
        Key:   mq.KafkaMsgValueStrEncoder(outbox.PartitionKey),
        Value: mq.KafkaMsgValueEncoder([]byte(outbox.Payload)),
        Headers: []sarama.RecordHeader{
            {Key: []byte("event_id"), Value: []byte(outbox.EventId)},
            {Key: []byte("event_type"), Value: []byte(outbox.EventType)},
        },
    }
    _, _, err := producer.Send(msg)
    return err
}
```

**deliverToRedis()**：XADD 投递 + ZREM 清理 Sorted Set

```go
func (r *Relay) deliverToRedis(outbox *models.EventOutbox) error {
    // headers 序列化为 JSON 字符串（Redis Stream field 不支持嵌套 map）
    headersJSON, _ := json.Marshal(map[string]string{
        "event_id": outbox.EventId, "event_type": outbox.EventType,
    })
    result, err := global.RedisClient.XAdd(&redis.XAddArgs{
        Stream: outbox.Topic,
        Values: map[string]interface{}{
            "event_id": outbox.EventId, "value": outbox.Payload,
            "headers": string(headersJSON),
        },
    }).Result()
    // 投递成功后清理 Sorted Set 调度记录
    global.RedisClient.ZRem(delayScheduleKey, outbox.EventId)
    return err
}
```

---

## 五、架构图解

### 5.1 多队列拓扑图

```
┌─────────────────────────────────────────────────────────────────────────┐
│                          Engine (引擎层)                                 │
│                                                                         │
│  ┌─────────────────────────────────────────────────────────────────┐    │
│  │                    ProcessManager                                │    │
│  │  ┌─────────────────┐  ┌─────────────────┐  ┌────────────────┐  │    │
│  │  │ Process[Kafka]  │  │ Process[Kafka]  │  │Process[Redis]  │  │    │
│  │  │ order-consumer  │  │ product-consumer│  │delay-consumer  │  │    │
│  │  │ Runnable:Consumer│ │ Runnable:Consumer│ │Runnable:Redis..│  │    │
│  │  └────────┬────────┘  └────────┬────────┘  └───────┬────────┘  │    │
│  └───────────┼────────────────────┼───────────────────┼────────────┘    │
│              │                    │                   │                 │
│  ┌───────────▼────────────────────▼───────────────────▼────────────┐    │
│  │                    Registry (Handler 注册表)                      │    │
│  │  shop-order-events    → OrderHandler                            │    │
│  │  shop-product-events  → ProductHandler                          │    │
│  │  delay-order-timeout  → DelayOrderHandler                       │    │
│  └──────────────────────────────────────────────────────────────────┘    │
│                                                                         │
│  ┌──────────────────┐    ┌──────────────────────────────────────────┐   │
│  │     Relay         │    │          DelayScheduler                  │   │
│  │  通知驱动+Ticker  │    │  Sorted Set 轮询 → XADD Stream          │   │
│  │  扫描 outbox 表   │    │  每秒触发（主调度路径）                   │   │
│  │  按 queue_type    │    │                                          │   │
│  │  分发投递         │    │                                          │   │
│  └────────┬─────────┘    └─────────────────┬────────────────────────┘   │
└───────────┼─────────────────────────────────┼───────────────────────────┘
            │                                 │
    ┌───────▼────────┐               ┌────────▼────────┐
    │     Kafka       │               │    Redis         │
    │ ┌─────────────┐ │               │ ┌─────────────┐ │
    │ │ Topics:     │ │               │ │ Streams:    │ │
    │ │ order-events│ │               │ │ delay-order │ │
    │ │ product-evts│ │               │ │ delay-remind│ │
    │ └─────────────┘ │               │ │ Sorted Set: │ │
    │                 │               │ │ delay:sched │ │
    └─────────────────┘               │ └─────────────┘ │
                                      └─────────────────┘
```

### 5.2 消息流转时序图

#### Kafka 即时消息流转

```
业务代码          Observer         Outbox表          Relay           Kafka          Consumer         Handler
  │                │               │                │              │               │               │
  │── 创建订单 ──▶│               │                │              │               │               │
  │               │── BEGIN TX ──▶│                │              │               │               │
  │               │── INSERT 订单▶│                │              │               │               │
  │               │── INSERT outbox(WAIT)──▶│       │              │               │               │
  │               │── COMMIT TX ──▶│               │              │               │               │
  │               │── PUBLISH ────────────────────▶│              │               │               │
  │               │               │   (异步通知)    │              │               │               │
  │               │               │◀── 扫描 WAIT ──│              │               │               │
  │               │               │── 抢占 SENDING─▶│              │               │               │
  │               │               │               │── 发送 ──────▶│               │               │
  │               │               │               │◀── 成功 ──────│               │               │
  │               │               │── 更新 SENT ──▶│              │               │               │
  │               │               │               │              │── 推送 ───────▶│               │
  │               │               │               │              │               │── Handle() ──▶│
  │               │               │               │              │               │◀── 成功 ──────│
  │               │               │               │              │               │── MarkMessage  │
```

#### Redis 延迟消息流转

```
业务代码      Outbox表    SortedSet   DelayScheduler   RedisStream   RedisConsumer   Handler
  │            │           │              │               │              │            │
  │── 创建订单▶│           │              │               │              │            │
  │           │── BEGIN TX │              │               │              │            │
  │           │── INSERT   │              │               │              │            │
  │           │  outbox    │              │               │              │            │
  │           │  (WAIT,    │              │               │              │            │
  │           │  execute_at│              │               │              │            │
  │           │  =now+30m) │              │               │              │            │
  │           │── COMMIT   │              │               │              │            │
  │           │── ZADD ───▶│              │               │              │            │
  │           │  (score=   │              │               │              │            │
  │           │  execute_at)│              │               │              │            │
  │           │            │── 每秒轮询 ─▶│               │              │            │
  │           │            │  ZRANGEBYSCORE│               │              │            │
  │           │◀── 查 DB ──│              │               │              │            │
  │           │── 抢占 ────│              │               │              │            │
  │           │  SENDING   │              │               │              │            │
  │           │            │              │── XADD ──────▶│              │            │
  │           │── 更新 SENT│              │               │              │            │
  │           │            │              │── ZREM ──────▶│              │            │
  │           │            │              │               │── 推送 ─────▶│            │
  │           │            │              │               │              │── Handle()▶│
  │           │            │              │               │              │◀── XACK ──│
```

#### 降级路径（Relay 兜底）

```
Relay 每 10s 扫描:
  WHERE queue_type='redis'
    AND execute_at <= NOW()
    AND status = 'WAIT'

  → deliverToRedis(): XADD Stream + ZREM Sorted Set
  → RedisStreamConsumer 消费
```

### 5.3 组件依赖关系图

```
cmd/worker/main.go
  │
  ▼
Engine (编排所有组件)
  ├── ProcessManager
  │     ├── Process[Kafka] → Consumer (Runnable)
  │     ├── Process[Kafka] → Consumer (Runnable)
  │     └── Process[Redis] → RedisStreamConsumer (Runnable)
  │
  ├── Relay
  │     ├── 扫描 outbox → 按 queue_type 分发
  │     ├── deliverToKafka() → sarama.Producer
  │     └── deliverToRedis() → XADD Stream
  │
  └── DelayScheduler
        ├── 轮询 Sorted Set → ZRANGEBYSCORE
        └── XADD Stream → RedisStreamConsumer 消费

所有 Consumer 共享:
  ├── Registry (Handler 注册表)
  ├── Middleware Chain (Recovery → Logging → Timeout)
  └── Handler 接口 (*Message 队列无关)
```

---

## 六、与原有架构对比

### 6.1 改造前后对比表

| 维度 | 改造前 | 改造后 |
|------|--------|--------|
| **队列支持** | Kafka 专用 | Kafka + Redis Stream |
| **任务抽象** | 无（直接创建 ConsumerGroup） | Runnable 通用接口 |
| **生命周期管理** | Engine 直接管理 | ProcessManager 统一管理 |
| **消息模型** | Event（含 Kafka 特有字段） | Message（队列无关） |
| **延迟队列** | 不支持 | Sorted Set + Stream 两段式 |
| **故障降级** | 无 | DB Relay 兜底 |
| **扩展新队列** | 需重构核心代码 | 新增 Runnable 实现即可 |
| **Outbox 表** | 仅 Kafka 投递 | 按 queue_type 分发 |
| **消费架构** | 双缓冲（仅 Kafka） | 双缓冲（Kafka + Redis 统一） |
| **通知机制** | Redis Pub/Sub（仅即时消息） | Redis Pub/Sub + 双路径投递 |

### 6.2 性能对比

| 指标 | Kafka Consumer | Redis Stream Consumer |
|------|---------------|----------------------|
| **吞吐量** | 高（Partition 并行消费） | 中（单 Stream 串行拉取） |
| **延迟** | 毫秒级（Poller 持续拉取） | 毫秒级（XReadGroup BLOCK） |
| **消息持久化** | 磁盘持久化（Kafka Broker） | 内存 + AOF/RDB（Redis） |
| **消费确认** | Offset 手动提交 | XACK 确认 |
| **重试机制** | 不 ACK → 下次拉取重投 | 不 ACK → Pending 列表 |
| **背压控制** | tasks channel 缓冲 | tasks channel 缓冲 |
| **适用场景** | 高吞吐实时事件流 | 轻量级延迟任务 |

双缓冲架构在两者中均采用，背压机制一致：tasks channel 满时 Poller 阻塞，自然降速。
---

## 七、最佳实践与注意事项

### 7.1 新增 Runnable 任务的步骤

以新增 RocketMQ Consumer 为例：

**步骤 1：实现 Runnable 接口**

```go
// internal/worker/rocketmq_consumer.go
type RocketMQConsumer struct {
    // RocketMQ 特有字段
}
var _ Runnable = (*RocketMQConsumer)(nil) // 编译期检查

func (c *RocketMQConsumer) Name() string { return "rocketmq:xxx" }
func (c *RocketMQConsumer) Start(ctx context.Context) error {
    // 启动消费循环（双缓冲模式）
    return nil
}
func (c *RocketMQConsumer) Stop() error {
    // 优雅关闭
    return nil
}
```

**步骤 2：在 Engine 或 ProcessManager 中注册**

```go
// cmd/worker/main.go
rocketConsumer := worker.NewRocketMQConsumer(/* ... */)
process := worker.NewProcess("rocketmq:topic-name", "rocketmq", rocketConsumer)
engine.AddProcess(process)
```

**步骤 3：配置文件中添加任务配置**

```yaml
worker:
  handlers:
    - topic: "your-topic"
      concurrency: 4
      buffer-size: 1000
```

**步骤 4：实现 Handler 并注册到 Registry**

```go
// internal/workers/register.go
func RegisterHandlers(registry *worker.Registry) {
    registry.Register("your-topic", &YourHandler{})
}
```

### 7.2 常见陷阱

**陷阱 1：慢处理阻塞拉取**

```
❌ 错误：Poller 直接调用 Handler.Handle()
   → 慢处理（如第三方 API 超时 60s）阻塞拉取循环
   → Kafka: 触发 rebalance / Redis: 消费组超时

✅ 正确：使用双缓冲架构隔离
   → Poller 只负责拉取并写入 tasks channel
   → Worker Pool 独立处理，慢处理不影响拉取
```

**陷阱 2：Goroutine 泄漏**

```
❌ 错误：Stop() 只 cancel context，不等待 goroutine 退出
   → goroutine 泄漏，资源无法回收

✅ 正确：使用 sync.WaitGroup 等待所有 goroutine 退出
   → Consumer.Stop(): cancel → consumeWg.Wait → close(tasks) → workerWg.Wait
   → 确保每个 goroutine 都有明确的退出条件
```

**陷阱 3：并发竞争**

```
❌ 错误：先 SELECT 再 UPDATE（两步操作，存在竞态窗口）
   → 多个 Relay 实例可能同时抢占同一条记录

✅ 正确：原子抢占（UPDATE WHERE status=WAIT）
   → 数据库行锁保证只有一个实例能成功
   → RowsAffected == 0 表示被其他实例抢占，跳过
```

**陷阱 4：Redis Stream headers 序列化**

Redis Stream field value 不支持嵌套 map，headers 必须序列化为 JSON 字符串：`XADD Values: {"headers": string(headersJSON)}`，消费端反序列化解析。

**陷阱 5：go-redis v7 消费组创建**

go-redis v7 无 `XGroupCreateMkStream` 方法，需先 `XADD` 占位消息确保 Stream 存在，再 `XGroupCreate`。见 `RedisStreamConsumer.ensureConsumerGroup()` 实现。

### 7.3 监控指标建议

| 指标名称 | 类型 | 说明 |
|---------|------|------|
| `worker_message_handle_duration_seconds` | Histogram | Handler 处理耗时（P99 > 5s 告警） |
| `worker_message_error_total` | Counter | 消息处理错误数（速率 > 10/min 告警） |
| `worker_outbox_wait_count` | Gauge | WAIT 状态 outbox 记录数（> 1000 告警） |
| `worker_outbox_dead_count` | Gauge | DEAD 状态 outbox 记录数（> 100 告警） |
| `worker_redis_stream_length` | Gauge | Redis Stream 队列深度（> 10000 告警） |
| `worker_goroutine_count` | Gauge | Worker goroutine 数量（异常增长告警） |

---

## 八、总结

本架构的核心设计思想可以浓缩为一句话：

> **用 Runnable 接口抽象通用任务，用 ProcessManager 统一管理生命周期，用 Strategy 模式支持多队列实现，用 Transactional Outbox 保证事件一致性，用双缓冲架构隔离拉取与处理。**

整套架构通过以下经典设计模式的组合，构建了一个从任务注册到执行的完整闭环：

| 设计模式 | 应用位置 | 核心价值 |
|---------|---------|----------|
| **Strategy** | Runnable + Consumer 实现 | 多队列可替换，不改调度逻辑 |
| **Factory** | Engine.Start() | 创建与使用分离 |
| **Observer** | GORM Observer → Outbox | 业务零侵入事件捕获 |
| **State Machine** | event_outbox.status | 状态转换安全，防非法操作 |
| **Chain of Responsibility** | Middleware 链 | 横切关注点可插拔 |
| **Producer-Consumer** | 双缓冲架构 | 拉取与处理解耦，背压控制 |
| **Transactional Outbox** | Outbox + Relay | 分布式事务一致性 |

覆盖了三大生产环境核心诉求：
- **可靠性**：Transactional Outbox + 双路径投递 + 原子抢占 + 租约回收 + 死信管理
- **可扩展性**：Runnable 接口 + Strategy 模式 + Factory 模式 + 队列无关 Message
- **可观测性**：Name() 标识 + 结构化日志 + 监控指标 + 状态流转追踪