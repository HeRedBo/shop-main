# Worker 多队列架构改造文档

> 基于 Phase 1-5 的代码改造，记录 Worker 模块从 Kafka 专用消费引擎向多队列（Kafka + Redis Stream）支持的 ProcessManager 架构演进的全过程。

---

## 目录

- [一、改造背景与目标](#一改造背景与目标)
- [二、架构总览](#二架构总览)
- [三、核心抽象设计](#三核心抽象设计)
- [四、Redis 延迟队列方案](#四redis-延迟队列方案)
- [五、配置模型](#五配置模型)
- [六、改造文件清单](#六改造文件清单)
- [七、DLQ 运维 CLI](#七dlq-运维-cli)
- [八、迁移阶段规划](#八迁移阶段规划)
- [九、风险评估与后续规划](#九风险评估与后续规划)

---

## 一、改造背景与目标

### 1.1 当前架构问题

Worker 模块原本是 **Kafka 专用消费引擎**，存在 6 个硬编码耦合点：

| # | 耦合点 | 具体表现 |
|---|--------|----------|
| 1 | Consumer 层 | 直接持有 `sarama.ConsumerGroup`，无法消费其他队列 |
| 2 | Event 模型 | 包含 `Offset`/`Partition` 等 Kafka 特有字段 |
| 3 | ACK 机制 | 与 `sarama.Session` 绑定，无法适配其他队列的确认语义 |
| 4 | Relay 层 | 硬编码调用 Kafka Producer |
| 5 | Outbox 层 | 使用 `Topic`/`PartitionKey` 等 Kafka 术语 |
| 6 | Engine 层 | 只创建 `sarama.ConsumerGroup`，无法管理异构消费者 |

这些耦合导致：**无法引入 Redis 延迟队列、RocketMQ 等其他消息中间件**，业务扩展受限。

### 1.2 改造目标

1. **支持多队列**：同时支持 Kafka（实时事件流）和 Redis Stream（延迟队列）
2. **引入 ProcessManager 模式**：统一管理异构消费者的生命周期
3. **保持向后兼容**：原有 Kafka 消费逻辑不受影响，新旧模式可共存
4. **统一消息模型**：屏蔽队列差异，Handler 无需关心消息来源

### 1.3 与 Hyperf Process 模型的对比

| 维度 | Hyperf (PHP) | Shop-Main (Go) |
|------|--------------|----------------|
| 进程模型 | `fork` 子进程，每个 Process 是独立 OS 进程 | goroutine，每个 Process 是轻量级协程 |
| 内存共享 | 子进程独立内存空间，需 IPC 通信 | 共享进程内存，通过 channel/mutex 协调 |
| 启动方式 | `ProcessManager::start()` 批量 fork | `ProcessManager.Start()` 串行启动 goroutine |
| 故障隔离 | 子进程崩溃不影响其他进程 | goroutine panic 需 recover，否则影响整个进程 |
| 资源开销 | 每个进程 MB 级内存 | 每个 goroutine KB 级栈空间 |
| 适用场景 | PHP-FPM 长驻进程 | 高并发消息消费 |

---

## 二、架构总览

### 2.1 改造后架构图

```
cmd/worker/main.go
  └── ProcessManager（统一管理所有 Process 生命周期）
       ├── Process 1: KafkaConsumer (shop-order-events)     ← NewKafkaProcesses() 工厂创建
       ├── Process 2: KafkaConsumer (shop-product-events)   ← NewKafkaProcesses() 工厂创建
       ├── Process 3: Relay (多队列投递)                    ← NewRelayProcess() 工厂创建
       │    ├── Kafka Relay: event_outbox (queue_type=kafka) → Kafka
       │    └── Redis Relay: event_outbox (queue_type=redis) → Redis Stream
       └── Process 4: DelayScheduler                        ← NewSchedulerProcess() 工厂创建
            └── Redis Sorted Set 毫秒级延迟调度

       Handler Registry (共享，通过 pm.GetRegistry() 获取)
            ├── OrderHandler       ← shop-order-events
            ├── ProductHandler     ← shop-product-events
            └── DelayOrderHandler  ← delay-order-timeout
```

> **旧模式（兼容路径）**：通过 `Engine` 间接管理，`main → Engine → ProcessManager + Relay + DelayScheduler`。当前 `cmd/worker/main.go` 已切换为直接使用 `ProcessManager` 模式。

### 2.2 数据流全景

```
┌─────────────────────────────────────────────────────────────────────┐
│                        业务层 (API/Cron)                            │
│                                                                     │
│   OrderService.Create()          DelayQueue.Push()                  │
│        │                              │                            │
│        ▼                              ▼                            │
│   WriteKafkaOutbox()            WriteDelayOutbox()                  │
│        │                              │                            │
│        ▼                              ▼                            │
│   ┌──────────────────────────────────────────┐                     │
│   │         event_outbox 表                   │                     │
│   │  queue_type=kafka  queue_type=redis       │                     │
│   │  execute_at=NULL   execute_at=未来时间     │                     │
│   └──────────────────────────────────────────┘                     │
│                              │                                      │
│                              ▼                                      │
│                     ┌──────────────┐                                │
│                     │    Relay     │                                │
│                     │ 扫描+投递    │                                │
│                     └──────┬───────┘                                │
│                    ┌───────┴───────┐                                │
│                    ▼               ▼                                │
│            Kafka Topic      Redis Stream                            │
│                    │               │                                │
│                    ▼               ▼                                │
│            ┌─────────────┐ ┌────────────────┐                      │
│            │   Kafka     │ │ Redis Stream   │                      │
│            │  Consumer   │ │   Consumer     │                      │
│            └──────┬──────┘ └───────┬────────┘                      │
│                   │                │                                │
│                   ▼                ▼                                │
│            ┌─────────────────────────────┐                         │
│            │     Handler Registry        │                         │
│            └─────────────────────────────┘                         │
└─────────────────────────────────────────────────────────────────────┘
```

### 2.3 ProcessManager 直接管理模式

```
ProcessManager
 ├── processes []*Process           ← 所有 Process 统一注册
 │    ├── Process { Name="kafka-order-events", TaskType="kafka", Runner=Consumer }
 │    ├── Process { Name="kafka-product-events", TaskType="kafka", Runner=Consumer }
 │    ├── Process { Name="relay", TaskType="relay", Runner=Relay }
 │    └── Process { Name="delay-scheduler", TaskType="scheduler", Runner=DelayScheduler }
 ├── registry *Registry             ← Handler 注册表（共享）
 ├── ctx/cancel                     ← 统一 context 管理
 └── started bool                   ← 启动状态标记

工厂函数创建 Process：
 ├── NewKafkaProcesses()  → []*Process（每个 topic 独立 Consumer）
 ├── NewRelayProcess()    → *Process（Relay 扫描投递）
 └── NewSchedulerProcess() → *Process（Redis 不可用时返回 nil）
```

> **旧模式（兼容路径）**：`Engine` 内部持有 `processManager`、`relay`、`delayScheduler`、`runners` 等字段，由 `engine.Start()` / `engine.Stop()` 统一管理。当前 `cmd/worker/main.go` 已绕过 Engine，直接使用 ProcessManager。

---

## 三、核心抽象设计

### 3.1 Message 统一消息模型

**文件**: `internal/worker/message.go`

```go
type Message struct {
    // 通用字段
    Source    string            // 来源标识: "kafka" / "redis"
    Topic     string            // 统一术语 (Kafka topic / Redis stream key)
    Key       string            // 路由键
    Value     []byte            // 消息体
    Headers   map[string]string // 元数据 (event_id, event_type, aggregate_id 等)
    EventType string            // 事件类型

    // 队列特有字段（按需使用）
    Delay     time.Duration     // 延迟时间（Redis 特有，Kafka 为 0）
    RawOffset interface{}       // Kafka: int64 offset, Redis: Stream ID string
}

// Event 保留为 Message 的类型别名，向后兼容
type Event = Message
```

**与原 Event 的对比**：

| 字段 | 原 Event | 新 Message | 说明 |
|------|---------|-----------|------|
| Source | 无 | ✅ | 标识消息来源队列 |
| Topic | Kafka Topic | 统一为 topic/stream key | 屏蔽队列术语差异 |
| Key | PartitionKey | Key | 通用路由键 |
| Value | []byte | []byte | 不变 |
| Headers | map[string]string | map[string]string | 不变 |
| Offset | int64 | RawOffset (interface{}) | Kafka=int64, Redis=string |
| Delay | 无 | ✅ | Redis 延迟队列专用 |

### 3.2 QueueConsumer 接口

**文件**: `internal/worker/queue_consumer.go`

```go
type QueueConsumer interface {
    Name() string                    // 消费者名称（日志标识）
    Start(ctx context.Context) error // 启动消费循环
    Stop() error                     // 优雅关闭
}
```

**实现类**：

| 实现 | 文件 | 说明 |
|------|------|------|
| `Consumer` | `consumer.go` | Kafka ConsumerGroup 消费 |
| `RedisStreamConsumer` | `redis_consumer.go` | Redis Stream XReadGroup 消费 |

两者均采用 **双缓冲架构**（Poller + Worker Pool），保证慢处理不阻塞拉取。

### 3.3 QueueProducer 接口

**文件**: `internal/worker/queue_producer.go`

```go
type QueueProducer interface {
    Produce(ctx context.Context, msg *Message) error
    ProduceBatch(ctx context.Context, msgs []*Message) error
    Close() error
}
```

> 当前 Relay 直接调用 Kafka SyncProducer 和 Redis XADD，QueueProducer 接口为后续扩展预留。

### 3.4 Process 结构体与生命周期

**文件**: `internal/worker/process.go`

```go
type ProcessStatus int  // Created → Running → Stopped / Failed

type Process struct {
    Name      string        // 进程名称（如 "kafka-order-consumer"）
    QueueType string        // 队列类型（"kafka" / "redis"）
    Consumer  QueueConsumer // 消费者实例
    Status    ProcessStatus // 运行状态
    Err       error         // 最后一次错误
}
```

**状态机**：

```
Created ──Start()──▶ Running ──Stop()──▶ Stopped
                       │
                       └──异常──▶ Failed
```

### 3.5 ProcessManager 编排策略

**文件**: `internal/worker/process_manager.go`

```go
type ProcessManager struct {
    processes []*Process
    registry  *Registry
    ctx       context.Context
    cancel    context.CancelFunc
    mu        sync.Mutex
    started   bool
}
```

**编排策略**：

| 操作 | 策略 | 说明 |
|------|------|------|
| **启动** | 串行启动 | 避免并发创建 ConsumerGroup 的竞争条件 |
| **关闭** | 逆序关闭 | 后启动的先关闭，保证依赖关系正确释放 |
| **失败回滚** | 回滚已启动 | 启动某个 Process 失败时，逆序关闭已启动的 Process |
| **线程安全** | sync.Mutex | 所有操作加锁，防止并发修改 processes 列表 |

**启动流程**：

```
ProcessManager.Start()
  ├── for each Process (串行):
  │     ├── Consumer.Start(ctx)
  │     │    ├── 成功 → Status = Running
  │     │    └── 失败 → stopStartedProcesses() → return error
  │     └── 下一个
  └── started = true
```

**关闭流程**：

```
ProcessManager.Stop()
  ├── for i = len-1 → 0 (逆序):
  │     ├── Consumer.Stop()
  │     │    ├── 成功 → Status = Stopped
  │     │    └── 失败 → Status = Failed, Err = err
  │     └── 下一个
  ├── cancel()  // 取消 context
  └── started = false
```

### 3.6 工厂函数与声明式编排

各 Process 通过工厂函数创建，将配置解析、资源初始化、条件降级等细节封装在工厂内部：

| 工厂函数 | 文件 | 返回值 | 关键逻辑 |
|---------|------|--------|---------|
| `NewKafkaProcesses()` | `kafka_process.go` | `[]*Process` | 创建 saramaConfig → 遍历 handlers 配置 → 每个 topic 独立 ConsumerGroup → ConsumerGroup 创建失败则跳过该 handler |
| `NewRelayProcess()` | `relay_process.go` | `*Process` | 调用 `ParseRelayConfig()` 解析配置 → `NewRelay()` 创建实例 |
| `NewSchedulerProcess()` | `scheduler_process.go` | `*Process` | 检查 `global.RedisClient` 是否可用 → 不可用返回 nil（降级为 DB Relay 兜底） |

**main.go 声明式编排示例**：

```go
// 1. 创建共享 Registry 和 ProcessManager
registry := worker.NewRegistry()
pm := worker.NewProcessManager(registry)

// 2. 注册 Handler
workers.RegisterAllToManager(pm)

// 3. 声明式添加 Process（工厂函数创建）
pm.AddProcesses(worker.NewKafkaProcesses(workerCfg, kafkaCfg, registry)...)
pm.AddProcess(worker.NewRelayProcess(workerCfg))
pm.AddProcess(worker.NewSchedulerProcess())

// 4. 统一启动 / 统一关闭
pm.Start()
defer pm.Stop()
```

> **旧模式（兼容路径）**：通过 `engine := worker.NewEngine(config)` + `workers.RegisterAll(engine)` + `engine.Start()` 启动，Engine 内部委托 ProcessManager 管理异构 Process。

## 四、Redis 延迟队列方案

### 4.1 技术选型

采用 **Redis Sorted Set（延迟调度）+ Redis Stream（消费处理）** 的两段式方案：

```
业务写入 event_outbox (execute_at=未来时间)
       │
       ▼
Relay 扫描 execute_at <= NOW() 的记录
       │
       ▼
XADD 写入 Redis Stream（立即消费）
       │
       ▼
RedisStreamConsumer 消费处理
```

> 当前实现中，延迟调度直接通过 DB 轮询（Relay 扫描 `execute_at`），未使用 Sorted Set 做精度调度。这是因为 Relay 的 Ticker 兜底间隔（10s）已满足业务精度需求。

### 4.2 为什么选 Redis Stream

| 方案 | 延迟精度 | 消费语义 | 可靠性 | 复杂度 | 适用场景 |
|------|---------|---------|--------|--------|---------|
| **Redis Stream** | 秒级（Ticker 轮询） | 消费组 + ACK + Pending | 高（XACK 保证 at-least-once） | 中 | ✅ 当前方案 |
| Redis Sorted Set | 毫秒级（ZREVRANGEBYSCORE） | 需自行实现竞争消费 | 中（需处理重复消费） | 高 | 高精度调度场景 |
| Redis LIST | 无延迟能力 | BLPOP 阻塞消费 | 低（消息取出即删） | 低 | 简单任务队列 |
| RocketMQ | 秒级（延迟级别） | 原生延迟消息 | 高（Broker 持久化） | 高 | 已有 RocketMQ 基础设施 |
| 数据库轮询 | 秒级（SQL 扫描） | 需自行实现锁竞争 | 高（DB 事务保证） | 低 | ✅ 当前 Relay 兜底方案 |

**选择 Redis Stream 的理由**：

1. **消费组语义**：XReadGroup 天然支持多消费者竞争消费，与 Kafka ConsumerGroup 同构
2. **ACK 机制**：XACK 保证 at-least-once 语义，失败消息保留在 Pending 列表
3. **项目已有 Redis**：使用 `go-redis/redis/v7`，无需引入新中间件
4. **双缓冲架构复用**：RedisStreamConsumer 与 Kafka Consumer 采用相同的双缓冲模式

### 4.3 延迟队列完整链路

```
┌──────────────┐     ┌──────────────┐     ┌──────────────┐     ┌──────────────┐
│  业务层       │     │  event_outbox │     │   Relay      │     │ Redis Stream │
│  (API/Cron)  │────▶│  (DB 表)      │────▶│  (扫描投递)   │────▶│ (XADD)       │
└──────────────┘     └──────────────┘     └──────────────┘     └──────┬───────┘
                                                                       │
       ┌──────────────┐     ┌──────────────┐                          │
       │   Handler    │◀────│   Registry   │◀─────────────────────────┘
       │ (业务处理)    │     │ (路由分发)    │     ┌──────────────────────┐
       └──────────────┘     └──────────────┘     │ RedisStreamConsumer  │
                                                  │ (XReadGroup 消费)    │
                                                  └──────────────────────┘
```

**详细步骤**：

1. **业务写入**：`DelayQueue.Push(tx, aggregateId, eventType, topic, delay, payload)`
   - 在同一事务中写入 `event_outbox`（`queue_type=redis`, `execute_at=NOW()+delay`）
2. **Relay 扫描**：`scanAndDeliverByType("redis", false)`
   - 查询 `status=WAIT AND queue_type=redis AND execute_at <= NOW()`
3. **抢占 + 投递**：`deliverOne()` → `deliverToRedis()`
   - 原子更新 `status=SENDING`，然后 `XADD` 写入 Redis Stream
4. **消费处理**：`RedisStreamConsumer.redisConsumeLoop()`
   - `XReadGroup` 拉取消息 → 分发到 Worker Pool → Handler 处理 → `XACK`

### 4.4 业务场景

| 场景 | eventType | topic | 延迟时间 | 说明 |
|------|-----------|-------|---------|------|
| 订单超时取消 | `order.timeout` | `delay-order-timeout` | 30 分钟 | 未支付订单自动取消 |
| 自动确认收货 | `order.auto_confirm` | `delay-order-timeout` | 7 天 | 发货后自动确认 |
| 定时提醒 | `order.reminder` | `delay-reminder` | 自定义 | 物流/评价提醒 |

**DelayOrderHandler 处理逻辑**（`internal/workers/delay_order_handler.go`）：

```
收到延迟消息
  ├── 幂等检查（processed_event 表）
  ├── 解析 DelayOrderPayload
  └── 根据 eventType 分发：
       ├── order.timeout → 检查 Paid=0 && Status=0 → 取消订单
       └── order.auto_confirm → 检查 Paid=1 && Status=1 → 确认收货
```

### 4.5 Redis 故障降级策略

当 Redis 不可用时，系统自动降级为 **DB 轮询兜底**：

1. **Relay 投递失败**：`deliverToRedis()` 返回错误 → `handleDeliveryFailure()` → 指数退避重试
2. **消费端故障**：`RedisStreamConsumer` 的 `XReadGroup` 失败 → 1 秒退避重试
3. **最终一致保证**：Relay 的 Ticker 兜底（10s 间隔）保证消息最终被投递

> Redis 故障不会导致消息丢失，所有延迟消息持久化在 `event_outbox` 表中。

---

## 五、配置模型

### 5.1 WorkerConfig 与 ProcessConfig

**原有配置**（`WorkerConfig`）：

```go
type WorkerConfig struct {
    Enabled        bool            // 是否启用
    GroupId        string          // Kafka 消费组 ID
    Topics         []string        // 订阅 topics（旧模式）
    Concurrency    int             // 全局并发数（旧模式）
    Handlers       []HandlerConfig // per-Handler 配置（推荐模式）
    // ... Relay 相关配置
    Processes      []ProcessConfig // 多队列模式（新增）
}
```

**新增配置**（`ProcessConfig`）：

```go
type ProcessConfig struct {
    Name     string                 // 进程名称（如 "redis-delay-consumer"）
    Type     string                 // 队列类型: "kafka" / "redis"
    Config   map[string]interface{} // 队列特有配置
    Handlers []HandlerConfig        // Handler 配置列表
}
```

### 5.2 config.yml 多队列配置示例

```yaml
worker:
  enabled: true
  group-id: "shop-worker"

  # ===== 方式一：per-Handler 独立配置（当前使用） =====
  handlers:
    - topic: "shop-order-events"
      concurrency: 2
      ordered: false
      buffer-size: 2000
    - topic: "shop-product-events"
      concurrency: 2
      ordered: false
      buffer-size: 1000

  # ===== 方式二：多队列模式（与 handlers/topics 互斥） =====
  # processes:
  #   # Kafka 实时事件流
  #   - name: kafka-order-consumer
  #     type: kafka
  #     config:
  #       group-id: shop-worker
  #     handlers:
  #       - topic: "shop-order-events"
  #         concurrency: 2
  #         buffer-size: 2000
  #       - topic: "shop-product-events"
  #         concurrency: 2
  #         buffer-size: 1000
  #
  #   # Redis Stream 延迟队列
  #   - name: redis-delay-consumer
  #     type: redis
  #     config:
  #       stream-key: delay-order-queue
  #       consumer-group: shop-delay-group
  #       consumer-name: shop-worker-1
  #     handlers:
  #       - topic: "delay-order-timeout"
  #         concurrency: 2
  #         buffer-size: 500

  # Relay 配置
  poll-interval: "2s"
  max-retries: 5
  retry-base-delay: "1s"
  relay-batch-size: 100
  relay-lease-timeout: "60s"
  handler-timeout: "30s"
  relay-cleanup-days: 7
```

**配置优先级**：`Processes 模式` > `Handlers per-Handler 模式` > `Topics 全局模式`

> 当 `processes` 配置存在时，`handlers` 和 `topics` 配置被忽略。

---

## 六、改造文件清单

### 6.1 新增文件（11 个）

| 文件路径 | 说明 | Phase |
|---------|------|-------|
| `internal/worker/message.go` | 统一消息模型 Message | Phase 1 |
| `internal/worker/queue_consumer.go` | QueueConsumer 抽象接口 | Phase 2 |
| `internal/worker/queue_producer.go` | QueueProducer 抽象接口 | Phase 2 |
| `internal/worker/redis_consumer.go` | Redis Stream 消费者实现 | Phase 2 |
| `internal/worker/process.go` | Process 结构体与状态机 | Phase 3 |
| `internal/worker/process_manager.go` | ProcessManager 编排层 | Phase 3 |
| `internal/worker/kafka_process.go` | Kafka Process 工厂函数（封装 ConsumerGroup 创建细节） | Phase 6 |
| `internal/worker/relay_process.go` | Relay Process 工厂函数（封装配置解析与实例化） | Phase 6 |
| `internal/worker/scheduler_process.go` | DelayScheduler Process 工厂函数（Redis 不可用时降级） | Phase 6 |
| `internal/worker/delay_queue.go` | DelayQueue 延迟队列写入接口 | Phase 5 |
| `cmd/cli/dlq.go` | DLQ 运维 CLI 命令 | Phase 5 |

### 6.2 修改文件（14+ 个）

| 文件路径 | 修改内容 | Phase |
|---------|---------|-------|
| `internal/worker/consumer.go` | Event→Message 重构，实现 QueueConsumer 接口 | Phase 1-2 |
| `internal/worker/handler.go` | Handler 接口参数改为 `*Message` | Phase 1 |
| `internal/worker/registry.go` | 无修改（Handler 接口变化自动适配） | Phase 1 |
| `internal/worker/middleware.go` | 无修改（Middleware 签名不变） | Phase 1 |
| `internal/worker/engine.go` | 集成 ProcessManager，支持多队列启动 | Phase 3 |
| `internal/worker/outbox.go` | 新增 `WriteOutbox` 多队列版本，`WriteDelayOutbox` | Phase 4 |
| `internal/worker/relay.go` | 按 `queue_type` 分发投递（Kafka/Redis） | Phase 4 |
| `internal/models/event_outbox.go` | 新增 `QueueType`、`ExecuteAt` 字段 | Phase 4 |
| `internal/models/event_dead_letter.go` | 新增 `QueueType` 字段 | Phase 5 |
| `conf/conf.go` | 新增 `ProcessConfig`、`WorkerConfig.Processes` | Phase 3 |
| `conf/config.yml` | 新增 `processes` 配置段（注释示例） | Phase 3 |
| `internal/workers/delay_order_handler.go` | 新增延迟订单处理器 | Phase 5 |
| `internal/workers/register.go` | 注册 DelayOrderHandler | Phase 5 |
| `cmd/worker/main.go` | 适配 ProcessManager 统一管理模式（旧模式通过 Engine 启动） | Phase 3/6 |

### 6.3 SQL 迁移脚本

**文件**: `sql/event_outbox_v2.sql`

```sql
-- Phase 4: event_outbox 表新增 queue_type 和 execute_at 字段
ALTER TABLE event_outbox 
    ADD COLUMN queue_type VARCHAR(16) NOT NULL DEFAULT 'kafka' 
        COMMENT '目标队列类型: kafka/redis' AFTER payload,
    ADD COLUMN execute_at DATETIME NULL DEFAULT NULL 
        COMMENT '延迟执行时间（Redis 延迟队列专用）' AFTER sending_at;

-- 为延迟消息查询添加索引
ALTER TABLE event_outbox
    ADD INDEX idx_queue_type_status_execute (queue_type, status, execute_at);

-- Phase 5: event_dead_letter 表新增 queue_type 字段
ALTER TABLE event_dead_letter
    ADD COLUMN queue_type VARCHAR(16) NOT NULL DEFAULT 'kafka' 
        COMMENT '来源队列类型: kafka/redis' AFTER payload;
```

---

## 七、DLQ 运维 CLI

### 7.1 命令总览

基于 Cobra 框架实现，入口文件：`cmd/cli/dlq.go`

| 命令 | 说明 | 示例 |
|------|------|------|
| `dlq list` | 查看死信列表 | `./shop-cli dlq list --topic shop-order-events --limit 20` |
| `dlq retry` | 重试单条/全部死信 | `./shop-cli dlq retry <event_id>` |
| `dlq retry --all` | 批量重试全部 PENDING 死信 | `./shop-cli dlq retry --all --topic shop-order-events` |
| `dlq discard` | 丢弃死信消息 | `./shop-cli dlq discard <event_id>` |
| `dlq clean` | 清理过期已解决记录 | `./shop-cli dlq clean --days 30` |
| `dlq stats` | 死信队列统计 | `./shop-cli dlq stats` |

### 7.2 使用示例

**查看死信列表**：

```bash
$ ./shop-cli dlq list --status PENDING --limit 10

EVENT_ID                              TOPIC                 AGGREGATE_ID                    RETRIES   STATUS      CREATED_AT
──────────────────────────────────    ────────────────────  ──────────────────────────────  ────────  ──────────  ──────────────────
a1b2c3d4-e5f6-7890-abcd-ef123456    shop-order-events     ORD-2024-001                    5         PENDING     2024-01-15 10:30

共 1 条记录
```

**重试单条死信**：

```bash
$ ./shop-cli dlq retry a1b2c3d4-e5f6-7890-abcd-ef123456
死信已重新投递: event_id=a1b2c3d4-e5f6-7890-abcd-ef123456, topic=shop-order-events
```

**批量重试全部 PENDING 死信**：

```bash
$ ./shop-cli dlq retry --all
批量重试完成: 成功 3/3
```

**死信统计**：

```bash
$ ./shop-cli dlq stats

TOPIC                           STATUS        COUNT
──────────────────────────────  ────────────  ──────
shop-order-events               PENDING       3
shop-order-events               RESOLVED      15
shop-product-events             PENDING       0
──────────────────────────────  ────────────  ──────
TOTAL                           PENDING       3
TOTAL                           RESOLVED      15
```

**丢弃死信**：

```bash
$ ./shop-cli dlq discard a1b2c3d4-e5f6-7890-abcd-ef123456
死信已丢弃: id=a1b2c3d4-e5f6-7890-abcd-ef123456
```

**清理过期记录**：

```bash
$ ./shop-cli dlq clean --days 7
清理完成: 删除 12 条过期死信记录（7 天前已解决）
```

### 7.3 重试流程

```
dlq retry <event_id>
  ├── 查询 event_dead_letter (status=PENDING)
  ├── 重新写入 event_outbox (queue_type=kafka, status=WAIT)
  ├── 更新死信状态 → RESOLVED
  └── Relay 自动扫描并投递到 Kafka
```

---

## 八、迁移阶段规划

### Phase 1: 统一消息模型

**目标**：将 `Event` 重命名为 `Message`，消除 Kafka 特有字段。

**改造点**：
- `Event` → `Message`（Source/Topic/Key/Value/Headers/RawOffset）
- `Event = Message` 类型别名保持向后兼容
- `Handler` 接口参数改为 `*Message`
- `Consumer` 中构造 `Message` 替代原 `Event`

### Phase 2: 队列抽象接口 + Redis 消费者

**目标**：抽象 QueueConsumer/QueueProducer 接口，实现 RedisStreamConsumer。

**改造点**：
- 新增 `QueueConsumer` 接口（Name/Start/Stop）
- 新增 `QueueProducer` 接口（Produce/ProduceBatch/Close）
- `Consumer`（Kafka）实现 `QueueConsumer`
- 新增 `RedisStreamConsumer`（双缓冲架构，XReadGroup 消费）
- 编译期接口检查：`var _ QueueConsumer = (*RedisStreamConsumer)(nil)`

### Phase 3: Process/ProcessManager 编排层

**目标**：引入 ProcessManager 统一管理异构消费者生命周期。

**改造点**：
- 新增 `Process` 结构体（Name/QueueType/Consumer/Status/Err）
- 新增 `ProcessManager`（串行启动/逆序关闭/失败回滚）
- `Engine` 集成 `ProcessManager`，新增 `AddProcess()` 方法
- 新增 `ProcessConfig` 配置模型
- `config.yml` 新增 `processes` 配置段

### Phase 4: Relay 多队列投递

**目标**：Relay 按 `queue_type` 分发投递到 Kafka 或 Redis Stream。

**改造点**：
- `event_outbox` 表新增 `queue_type`、`execute_at` 字段
- `WriteOutbox()` 支持 `queueType` 和 `executeAt` 参数
- 新增 `WriteKafkaOutbox()`（向后兼容便捷函数）
- 新增 `WriteDelayOutbox()`（Redis 延迟队列专用）
- `Relay.scanAndDeliver()` 按 `queue_type` 分别扫描
- `Relay.deliverOne()` 根据 `queue_type` 选择 Kafka/Redis 投递

### Phase 5: 延迟队列业务集成

**目标**：完成延迟队列端到端集成，提供运维工具。

**改造点**：
- 新增 `DelayQueue` 接口（Push/PushWithKey）
- 新增 `DelayOrderHandler`（订单超时取消/自动确认收货）
- 新增 DLQ CLI 命令（list/retry/discard/clean/stats）
- `event_dead_letter` 表新增 `queue_type` 字段
- `workers/register.go` 注册 `DelayOrderHandler`

---

## 九、风险评估与后续规划

### 9.1 已知风险

| 风险 | 影响 | 缓解措施 |
|------|------|---------|
| **Redis 单点故障** | 延迟消息无法投递到 Stream | Relay DB 轮询兜底 + Ticker 10s 重试 |
| **消息重复消费** | 同一延迟消息被多次处理 | Handler 层幂等检查（processed_event 表） |
| **Relay 扫描延迟** | 延迟消息精度受 Ticker 间隔影响 | 当前 10s 间隔满足业务需求，可调优 |
| **Redis Stream 内存增长** | 未 ACK 消息堆积导致内存压力 | 监控 Pending 列表长度，设置告警阈值 |
| **ProcessManager 单点** | Worker 进程崩溃影响所有 Process | 多实例部署 + 租约回收机制 |
| **DLQ 重试风暴** | 批量重试大量死信可能压垮下游 | 限制单次重试数量，指数退避 |

### 9.2 后续扩展方向

#### 1. RocketMQ 接入

```
ProcessManager
  ├── Process: KafkaConsumer (实时事件)
  ├── Process: RedisStreamConsumer (延迟队列)
  └── Process: RocketMQConsumer (事务消息)  ← 新增
```

- 实现 `RocketMQConsumer` 满足 `QueueConsumer` 接口
- 通过 `ProcessConfig.Type = "rocketmq"` 配置
- Handler Registry 共享，无需修改业务代码

#### 2. 延迟队列精度提升

- **当前**：DB 轮询（10s 精度）→ Redis Stream → Consumer
- **优化**：Redis Sorted Set 做一级调度（毫秒精度）→ 到期后 XADD → Stream
- **适用场景**：秒杀倒计时、限时优惠等高精度需求

#### 3. 死信队列自动重试

- 当前：CLI 手动重试
- 优化：自动退避重试（指数退避 + 抖动），超过 N 次后移入 DLQ
- 工具：`dlq retry --all --strategy exponential`

#### 4. 监控与告警

- Prometheus 指标：
  - `worker_messages_total{queue_type, status}` — 消息处理总量
  - `worker_message_duration_seconds{queue_type}` — 处理耗时分布
  - `worker_dlq_size{queue_type}` — 死信队列深度
  - `worker_relay_pending{queue_type}` — Relay 待投递数量
- Grafana 面板：实时展示各队列的消费速率、延迟、错误率

#### 5. 多实例部署

- 当前：单 Worker 实例
- 优化：多实例 + Redis 分布式锁（防止 Relay 重复扫描）
- 消费组天然支持多实例（Kafka ConsumerGroup / Redis XReadGroup）

---

## 附录：关键设计决策记录

### D1: 为什么 Event 改名为 Message？

`Event` 隐含"事件溯源"语义，但实际消息来源不仅是领域事件，还包括延迟任务、通知等。`Message` 更通用，能准确表达"统一消息模型"的设计意图。保留 `type Event = Message` 别名避免破坏性变更。

### D2: 为什么 ProcessManager 串行启动？

Kafka `sarama.NewConsumerGroup()` 内部会协调分区分配，并发创建可能导致竞争。串行启动虽然耗时略长，但保证了确定性。实际影响很小（每个 Consumer 启动 < 100ms）。

### D3: 为什么 Relay 不直接调用 QueueProducer？

当前 Relay 直接依赖 `sarama.SyncProducer` 和 `redis.Client`，是为了避免引入额外的抽象层。`QueueProducer` 接口已定义但尚未被 Relay 使用，后续可通过依赖注入方式接入。

### D4: 为什么延迟消息走 DB 轮询而非 Redis Sorted Set？

1. **事务一致性**：延迟消息与业务数据写入同一 DB 事务，保证原子性
2. **可靠性**：DB 持久化比 Redis 内存更可靠
3. **精度够用**：10s 轮询间隔满足订单超时（30 分钟+）等业务场景
4. **简化架构**：无需维护 Sorted Set 与 DB 的数据一致性

---

> **文档版本**: v1.0  
> **最后更新**: 2024-01  
> **维护者**: Shop-Main 团队
