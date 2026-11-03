# Worker 模块详细架构设计

## 一、模块定位与总体架构

### 1.1 模块定位

`cmd/worker` 是一个独立的常驻进程入口，负责：

1. **Outbox Relay**：扫描 `event_outbox` 表中 `status=WAIT` 的记录，按 `queue_type` 分发投递到 Kafka 或 Redis Stream
2. **Kafka Consumer**：消费 Kafka Topic 中的事件消息
3. **Redis Stream Consumer**：消费 Redis Stream 中的延迟队列消息
4. **DelayScheduler**：基于 Redis Sorted Set 的毫秒级延迟调度器
5. **Worker Pool**：并发处理事件，路由到对应的 Handler 执行业务逻辑

多队列架构下的完整事件驱动链路：

```
即时消息（Kafka）：
  MySQL (event_outbox, queue_type=kafka) → Relay → Kafka → KafkaConsumer → Worker Pool → Handler

延迟消息（Redis）：
  MySQL (event_outbox, queue_type=redis) → Relay/DelayScheduler → Redis Stream → RedisConsumer → Worker Pool → Handler
```

### 1.2 总体架构图

**当前模式（ProcessManager 统一管理模式）**：

```
┌─────────────────────────────────────────────────────────────────────────┐
│                          cmd/worker/main.go                             │
│                                                                         │
│  1. bootstrap.BootstrapWith(WithKafka())                               │
│  2. registry := worker.NewRegistry()                                    │
│  3. pm := worker.NewProcessManager(registry)                            │
│  4. workers.RegisterAllToManager(pm)  // 注册业务 Handler               │
│  5. pm.AddProcesses(                                                    │
│       worker.NewKafkaProcesses(...)...,  // Kafka Consumer Process      │
│     )                                                                   │
│  6. pm.AddProcess(worker.NewRelayProcess(...))  // Relay Process        │
│  7. pm.AddProcess(worker.NewSchedulerProcess()) // Scheduler Process    │
│  8. pm.Start()                  // 串行启动所有 Process                 │
│  9. <-ctx.Done()                // 等待 SIGTERM/SIGINT                 │
│ 10. pm.Stop()                   // 逆序关闭所有 Process                 │
│ 11. bootstrap.Shutdown()                                                │
└─────────────────────────────────────────────────────────────────────────┘
```

> **旧模式（兼容路径）**：`engine := worker.NewEngine(cfg)` → `engine.Start()` → `engine.Stop()`。Engine 内部仍使用 runners[] + consumerGroups[] 管理组件，但当前 `cmd/worker/main.go` 已切换为 ProcessManager 统一管理模式。

### 1.3 ProcessManager 核心编排（当前模式）

ProcessManager 是 Worker 进程的核心编排层，统一管理所有 Process（Kafka Consumer、Relay、DelayScheduler）的生命周期。每个组件通过工厂函数创建并包装为 `Process`，由 ProcessManager 串行启动、逆序关闭：

```
┌─────────────────────────────────────────────────────────────────────┐
│                     ProcessManager                                   │
│                  (统一管理所有 Process 生命周期)                       │
│                                                                     │
│  ┌─────────────────────────────────────────────────────────────┐   │
│  │  processes []*Process                                        │   │
│  │                                                              │   │
│  │  ┌──────────────────┐ ┌──────────────────┐ ┌─────────────┐ │   │
│  │  │ Process: kafka-  │ │ Process: relay   │ │ Process:    │ │   │
│  │  │ order-events     │ │                  │ │ delay-      │ │   │
│  │  │ (Consumer)       │ │ (Relay)          │ │ scheduler   │ │   │
│  │  │ Runner: Consumer │ │ Runner: Relay    │ │ (DelaySched)│ │   │
│  │  └──────────────────┘ └──────────────────┘ └─────────────┘ │   │
│  │                                                              │   │
│  │  ┌──────────────────┐                                        │   │
│  │  │ Process: kafka-  │  ← 每个 Handler 对应独立 Process       │   │
│  │  │ product-events   │                                        │   │
│  │  │ (Consumer)       │                                        │   │
│  │  └──────────────────┘                                        │   │
│  └─────────────────────────────────────────────────────────────┘   │
│                                                                     │
│  ┌─────────────────────────────────────────────────────────────┐   │
│  │  Registry (共享)                                             │   │
│  │  topic → Handler 映射表                                      │   │
│  │  所有 Kafka Consumer / Redis Consumer 共享同一 Registry      │   │
│  └─────────────────────────────────────────────────────────────┘   │
│                                                                     │
│  编排策略：                                                         │
│  • 启动：串行 Start()，避免 ConsumerGroup 并发创建竞争              │
│  • 关闭：逆序 Stop()，后启动的先关闭                                 │
│  • 回滚：启动失败时逆序关闭已启动的 Process                          │
│                                                                     │
│  共享资源：MySQL DB / Redis / Kafka Producer / Logger / Config      │
└─────────────────────────────────────────────────────────────────────┘
```

> **旧模式（兼容路径）—— Engine 核心引擎**：Engine 曾是 Worker 模块的核心，直接管理 `runners[]`（Kafka Consumer）、`consumerGroups[]`、`relay`、`delayScheduler` 和内部 `processManager`。当前 `cmd/worker/main.go` 已切换为直接使用 ProcessManager 模式，Engine 仍保留在 `internal/worker/engine.go` 中作为兼容路径。

### 1.4 工厂函数（Process 创建）

ProcessManager 模式下，所有组件通过工厂函数创建为 `Process`，封装内部创建细节：

| 工厂函数 | 文件 | 返回 | 说明 |
|---------|------|------|------|
| `NewKafkaProcesses()` | `kafka_process.go` | `[]*Process` | 根据 `config.Handlers` 为每个 topic 创建独立 Consumer Process；无 Handler 配置时回退兼容模式 |
| `NewRelayProcess()` | `relay_process.go` | `*Process` | 创建 Relay Process，封装 `ParseRelayConfig()` + `NewRelay()` |
| `NewSchedulerProcess()` | `scheduler_process.go` | `*Process` | 创建 DelayScheduler Process，Redis 不可用时返回 nil（降级为 DB Relay 兜底） |

**工厂函数调用链**：

```
cmd/worker/main.go
│
├── registry := worker.NewRegistry()
├── pm := worker.NewProcessManager(registry)
│
├── workers.RegisterAllToManager(pm)          // Handler 注册到 pm.registry
│
├── pm.AddProcesses(
│     worker.NewKafkaProcesses(workerCfg, kafkaCfg, registry)...,
│   )
│     └── 每个 Handler → sarama.NewConsumerGroup() → NewConsumerWithConfig()
│         → NewProcess("kafka-{topic}", "kafka", consumer)
│
├── pm.AddProcess(worker.NewRelayProcess(workerCfg))
│     └── ParseRelayConfig() → NewRelay() → NewProcess("relay", "relay", relay)
│
├── pm.AddProcess(worker.NewSchedulerProcess())
│     └── Redis 可用? → NewDelayScheduler() → NewProcess("delay-scheduler", "scheduler", ds)
│                      → nil（跳过）
│
└── pm.Start()   // 串行启动所有 Process
```

**设计要点**：
- 工厂函数内部封装所有配置解析、默认值兜底、资源创建细节，`main.go` 只需声明式调用
- `AddProcess(nil)` 安全跳过（如 `NewSchedulerProcess` Redis 不可用时返回 nil）
- `NewKafkaProcesses` 返回切片，支持 per-Handler 创建多个独立 Consumer Process

---

## 二、Outbox Relay 详细设计

### 2.1 职责

Outbox Relay 是一个定时轮询组件，负责：

1. 扫描 `event_outbox` 表中 `status=WAIT` 的记录
2. 通过行锁抢占，将记录状态更新为 `SENDING`
3. 将事件 Payload 发送到 Kafka 对应的 Topic
4. 成功后更新状态为 `SENT`，失败则根据重试策略回退为 `WAIT` 或标记为 `DEAD`
5. 回收超时的 `SENDING` 记录（回收器）

### 2.2 核心流程

```
┌─────────────────────────────────────────────────────────────────────┐
│                     Outbox Relay 主循环                              │
│                                                                     │
│  ┌──────────┐    ┌──────────┐    ┌──────────┐    ┌──────────────┐  │
│  │ 定时唤醒  │───▶│ 批量扫描  │───▶│ 行锁抢占  │───▶│ 投递 Kafka   │  │
│  │ (ticker) │    │ WAIT 记录│    │ WAIT→    │    │              │  │
│  │          │    │ LIMIT N  │    │ SENDING  │    │              │  │
│  └──────────┘    └──────────┘    └──────────┘    └──────┬───────┘  │
│                                                         │          │
│                                          ┌──────────────┼───────┐  │
│                                          │              │       │  │
│                                     成功  │         失败 │       │  │
│                                          ▼              ▼       │  │
│                                    ┌──────────┐  ┌──────────┐   │  │
│                                    │ status=  │  │ 重试判断  │   │  │
│                                    │ SENT     │  │          │   │  │
│                                    └──────────┘  └────┬─────┘   │  │
│                                                       │         │  │
│                                              ┌────────┼────────┐│  │
│                                              │        │        ││  │
│                                         可重试   不可重试  超限  ││  │
│                                              │        │        ││  │
│                                              ▼        ▼        ▼│  │
│                                         ┌──────┐ ┌──────┐ ┌──────┐
│                                         │WAIT  │ │DEAD  │ │DEAD  │
│                                         │(退避)│ │      │ │(死信)│
│                                         └──────┘ └──────┘ └──────┘
└─────────────────────────────────────────────────────────────────────┘
```

### 2.3 扫描与抢占机制

#### 批量扫描

```sql
SELECT * FROM event_outbox
WHERE status = 'WAIT'
  AND created_at < NOW() - INTERVAL 2 SECOND   -- 避免抢占刚写入的记录
ORDER BY created_at ASC
LIMIT 100
```

#### 行锁抢占

使用 `SELECT ... FOR UPDATE SKIP LOCKED` 实现多实例安全抢占：

```sql
UPDATE event_outbox
SET status = 'SENDING',
    sending_at = NOW(),
    retry_count = retry_count + 1
WHERE event_id IN (
    SELECT event_id FROM event_outbox
    WHERE status = 'WAIT'
      AND created_at < NOW() - INTERVAL 2 SECOND
    ORDER BY created_at ASC
    LIMIT 100
    FOR UPDATE SKIP LOCKED
)
```

> `SKIP LOCKED` 确保多个 Relay 实例不会阻塞等待同一行锁，各自跳过已被锁定的记录，实现无锁竞争的多实例并行。

### 2.4 Kafka 投递逻辑

```
┌─────────────────────────────────────────────────────────────────────┐
│                     单条记录投递流程                                  │
│                                                                     │
│  1. 解析 Payload → 确定 Topic（根据 event_type 路由）               │
│  2. 构造 Kafka Message：                                            │
│     - Key: aggregate_id（保证同一业务实体的消息有序）                │
│     - Value: JSON 编码的事件数据                                     │
│     - Headers: event_id, event_type, created_at                     │
│  3. 调用 Producer.Send(msg)                                         │
│  4. 根据结果更新 outbox 状态                                         │
└─────────────────────────────────────────────────────────────────────┘
```

**Topic 路由规则**：

| event_type 前缀 | Kafka Topic | 说明 |
|-----------------|-------------|------|
| `order.*` | `shop-order-events` | 订单相关事件 |
| `product.*` | `shop-product-events` | 商品相关事件 |
| `user.*` | `shop-user-events` | 用户相关事件 |
| 其他 | `shop-default-events` | 默认 Topic |

### 2.5 回收器（Recovery Scanner）

回收器是 Relay 的辅助组件，负责回收超时的 `SENDING` 记录：

```
┌─────────────────────────────────────────────────────────────────────┐
│                     回收器主循环                                     │
│                                                                     │
│  定时执行（每 30 秒）：                                              │
│                                                                     │
│  UPDATE event_outbox                                                │
│  SET status = 'WAIT',                                               │
│      sending_at = NULL                                              │
│  WHERE status = 'SENDING'                                           │
│    AND sending_at < NOW() - INTERVAL 60 SECOND                      │
│                                                                     │
│  说明：sending_at 超过 60 秒仍未完成，认为 Relay 进程已崩溃，         │
│  重置为 WAIT 让其他实例重新投递                                      │
└─────────────────────────────────────────────────────────────────────┘
```

### 2.6 Relay 配置

```yaml
worker:
  relay:
    enabled: true               # 是否启用 Relay
    poll_interval: 2s           # 轮询间隔
    batch_size: 100             # 每次扫描最大条数
    lease_timeout: 60s          # SENDING 租约超时（回收器判断依据）
    recovery_interval: 30s      # 回收器执行间隔
    max_retry: 5                # 最大重试次数（超过则标记 DEAD）
    retry_base_delay: 1s        # 重试基础延迟
    retry_max_delay: 30m        # 重试最大延迟（指数退避上限）
```

---

## 三、Kafka Consumer 详细设计

### 3.1 职责

Kafka Consumer 负责：

1. 从 Kafka Topic 拉取事件消息
2. 解码消息为统一的 Event 结构
3. 将消息分发到 Worker Pool 的 Channel 中
4. 管理 Consumer Group 的 Offset 提交

### 3.2 Sarama Consumer Group 架构

```
┌─────────────────────────────────────────────────────────────────────┐
│                  Sarama Consumer Group                               │
│                                                                     │
│  Consumer Group: "shop-worker"                                      │
│                                                                     │
│  ┌─────────────────────────────────────────────────────────────┐   │
│  │               Sarama ConsumerGroup 实例                      │   │
│  │                                                             │   │
│  │  Topic: shop-order-events     Partition 0 ──┐               │   │
│  │  Topic: shop-order-events     Partition 1 ──┤               │   │
│  │  Topic: shop-product-events   Partition 0 ──┼── Session ──▶ │   │
│  │  Topic: shop-product-events   Partition 1 ──┤               │   │
│  │  Topic: shop-user-events      Partition 0 ──┘               │   │
│  │                                                             │   │
│  │  多实例部署时，Sarama 自动 Rebalance 分配 Partition          │   │
│  └─────────────────────────────────────────────────────────────┘   │
│                                                                     │
│  每个 Partition 对应一个 ConsumeClaim goroutine                     │
│  ConsumeClaim → 解码 → 分发到 Worker Pool Channel                 │
└─────────────────────────────────────────────────────────────────────┘
```

### 3.3 消息拉取与分发流程

```
┌─────────────────────────────────────────────────────────────────────┐
│                  ConsumeClaim 处理流程                               │
│                                                                     │
│  for msg := range claim.Messages() {                                │
│                                                                     │
│      ┌──────────────────────────────────────────────────────────┐   │
│      │  1. 解码消息                                              │   │
│      │     JSON → Event 结构体                                   │   │
│      │     校验必填字段（event_id, event_type, aggregate_id）     │   │
│      └──────────────────────────┬───────────────────────────────┘   │
│                                 │                                   │
│      ┌──────────────────────────▼───────────────────────────────┐   │
│      │  2. 构造 WorkerTask                                      │   │
│      │     WorkerTask {                                         │   │
│      │       Event:    event,                                   │   │
│      │       KafkaMsg: msg,      // 保留引用用于 commit offset  │   │
│      │       Ctx:      ctx,                                     │   │
│      │     }                                                    │   │
│      └──────────────────────────┬───────────────────────────────┘   │
│                                 │                                   │
│      ┌──────────────────────────▼───────────────────────────────┐   │
│      │  3. 分发到 Worker Pool                                    │   │
│      │                                                         │   │
│      │     select {                                            │   │
│      │     case taskChan <- task:    // 非阻塞写入              │   │
│      │     case <-ctx.Done():        // 优雅关闭                │   │
│      │         return                                          │   │
│      │     }                                                   │   │
│      │                                                         │   │
│      │     注意：如果 taskChan 已满，此处阻塞                   │   │
│      │     背压传导到 Kafka 拉取，自动降速                      │   │
│      └─────────────────────────────────────────────────────────┘   │
│  }                                                                  │
└─────────────────────────────────────────────────────────────────────┘
```

### 3.4 Offset 管理策略

采用**手动提交 + 处理成功后提交**的策略，保证 at-least-once 语义：

```
┌─────────────────────────────────────────────────────────────────────┐
│                     Offset 提交流程                                  │
│                                                                     │
│  消息拉取 ──▶ 分发到 Worker Pool ──▶ Handler 处理完成               │
│                                        │                            │
│                               ┌────────▼────────┐                   │
│                               │  处理成功？      │                   │
│                               └────────┬────────┘                   │
│                                    │         │                       │
│                                   是          否                     │
│                                    │         │                       │
│                           ┌────────▼──┐  ┌───▼──────────┐           │
│                           │ 标记消息  │  │ 进入重试/死信 │           │
│                           │ 已处理    │  │ 路径          │           │
│                           └────────┬──┘  └───┬──────────┘           │
│                                    │         │                       │
│                                    └────┬────┘                       │
│                                         │                            │
│                                ┌────────▼────────┐                   │
│                                │ Session.MarkMsg │                   │
│                                │ (msg)           │                   │
│                                └────────┬────────┘                   │
│                                         │                            │
│                                ┌────────▼────────┐                   │
│                                │ 异步批量 Commit  │                   │
│                                │ (每 1s 或每 N条) │                   │
│                                └─────────────────┘                   │
│                                                                     │
│  关键：只有 Handler 处理完成（或进入死信）后才 MarkMessage            │
│  如果 Worker 崩溃，未 Mark 的消息会在重启后被重新投递                │
└─────────────────────────────────────────────────────────────────────┘
```

### 3.5 Consumer 配置

```yaml
worker:
  consumer:
    enabled: true
    group_id: "shop-worker"
    topics:
      - "shop-order-events"
      - "shop-product-events"
      - "shop-user-events"
      - "shop-default-events"
    initial_offset: "oldest"        # 从最早消息开始消费（首次启动）
    offset_commit_interval: 1s      # offset 批量提交间隔
    task_channel_buffer: 1000       # taskChan 缓冲大小（背压控制）
```

---

## 四、Worker Pool 详细设计

### 4.1 职责

Worker Pool 是事件处理的核心执行层，负责：

1. 从 Task Channel 中接收待处理的事件
2. 通过 Middleware 链对事件进行横切处理（日志、指标、超时控制等）
3. 根据 `event_type` 路由到对应的 Handler
4. 控制并发数量，防止资源耗尽

### 4.2 Pool 架构图

```
┌─────────────────────────────────────────────────────────────────────┐
│                       Worker Pool                                   │
│                                                                     │
│  ┌─────────────────────────────────────────────────────────────┐   │
│  │                  Task Channel (有界缓冲)                     │   │
│  │                  buffer size: 1000                          │   │
│  │                  ← Consumer 写入    Worker 读取 →            │   │
│  └─────────────────────────────────────────────────────────────┘   │
│                             │                                       │
│                    ┌────────▼────────┐                              │
│                    │   Semaphore     │                              │
│                    │   (信号量)       │                              │
│                    │   max = N       │                              │
│                    └────────┬────────┘                              │
│                             │                                       │
│         ┌───────────────────┼───────────────────┐                   │
│         │                   │                   │                   │
│  ┌──────▼──────┐    ┌──────▼──────┐    ┌──────▼──────┐            │
│  │  Worker 1   │    │  Worker 2   │    │  Worker N   │            │
│  │  goroutine  │    │  goroutine  │    │  goroutine  │            │
│  │             │    │             │    │             │            │
│  │ ┌─────────┐ │    │ ┌─────────┐ │    │ ┌─────────┐ │            │
│  │ │Middleware│ │    │ │Middleware│ │    │ │Middleware│ │            │
│  │ │ Chain   │ │    │ │ Chain   │ │    │ │ Chain   │ │            │
│  │ └────┬────┘ │    │ └────┬────┘ │    │ └────┬────┘ │            │
│  │      │      │    │      │      │    │      │      │            │
│  │ ┌────▼────┐ │    │ ┌────▼────┐ │    │ ┌────▼────┐ │            │
│  │ │ Handler │ │    │ │ Handler │ │    │ │ Handler │ │            │
│  │ └─────────┘ │    │ └─────────┘ │    │ └─────────┘ │            │
│  └─────────────┘    └─────────────┘    └─────────────┘            │
│                                                                     │
│  Worker 数量由配置控制，默认 = min(runtime.NumCPU(), 10)            │
└─────────────────────────────────────────────────────────────────────┘
```

### 4.3 Worker Goroutine 处理流程

每个 Worker Goroutine 的执行逻辑：

```
func workerLoop(taskChan <-chan WorkerTask, sem chan struct{}) {
    defer func() {
        if r := recover(); r != nil {
            logger.Error("Worker panic recovered", zap.Any("panic", r))
            // 重新进入循环，不退出
        }
    }()

    for task := range taskChan {
        sem <- struct{}{}           // 获取信号量
        processTask(task)           // 执行处理（含 Middleware 链）
        <-sem                       // 释放信号量
    }
}
```

> **注意**：信号量在 `processTask` 外获取和释放，确保 panic recover 时也能正确释放。更安全的做法是使用 `defer` 释放。

### 4.4 并发控制

```
┌─────────────────────────────────────────────────────────────────────┐
│                     并发控制机制                                     │
│                                                                     │
│  方案：有界 Channel + 信号量双重控制                                 │
│                                                                     │
│  第一层：Task Channel 缓冲（背压）                                  │
│  ┌─────────────────────────────────────────────────────────────┐   │
│  │  buffer = 1000                                              │   │
│  │  当 Channel 满时，Consumer 的分发操作阻塞                    │   │
│  │  → 背压传导到 Kafka 拉取 → 自动降速                         │   │
│  └─────────────────────────────────────────────────────────────┘   │
│                                                                     │
│  第二层：Worker 数量限制（并发上限）                                │
│  ┌─────────────────────────────────────────────────────────────┐   │
│  │  workers = 10（可配置）                                      │   │
│  │  最多 10 个 goroutine 同时处理消息                           │   │
│  │  防止 goroutine 爆炸导致 DB 连接耗尽                         │   │
│  └─────────────────────────────────────────────────────────────┘   │
│                                                                     │
│  第三层：单消息超时控制                                             │
│  ┌─────────────────────────────────────────────────────────────┐   │
│  │  timeout = 30s（可配置）                                     │   │
│  │  每条消息通过 context.WithTimeout 控制处理时间               │   │
│  │  超时自动取消，标记为可重试                                   │   │
│  └─────────────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────────────┘
```

### 4.5 Pool 配置

```yaml
worker:
  pool:
    workers: 10                    # Worker goroutine 数量
    task_channel_buffer: 1000      # Task Channel 缓冲大小
    message_timeout: 30s           # 单条消息处理超时
    panic_recovery: true           # 是否启用 panic recover
```

---

## 五、Event Handler 与 Middleware 详细设计

### 5.1 Handler 接口定义

```
┌─────────────────────────────────────────────────────────────────────┐
│                     Handler 接口                                     │
│                                                                     │
│  type EventHandler interface {                                      │
│      // EventType 返回该 Handler 处理的事件类型                     │
│      EventType() string                                             │
│                                                                     │
│      // Handle 处理事件，返回 error 表示处理失败（触发重试）         │
│      Handle(ctx context.Context, event *Event) error                │
│  }                                                                  │
│                                                                     │
│  Event 结构体：                                                     │
│  type Event struct {                                                │
│      EventID     string          // 唯一事件 ID（幂等键）           │
│      EventType   string          // 事件类型（如 order.created）    │
│      AggregateID string          // 业务实体 ID（如订单号）         │
│      Payload     json.RawMessage // 事件数据                        │
│      Metadata    map[string]string // 元数据（trace_id 等）        │
│      CreatedAt   time.Time       // 事件创建时间                   │
│  }                                                                  │
└─────────────────────────────────────────────────────────────────────┘
```

### 5.2 Handler Registry（策略注册表）

Handler Registry 实现策略模式，按 `event_type` 路由到对应的 Handler：

```
┌─────────────────────────────────────────────────────────────────────┐
│                     Handler Registry                                 │
│                                                                     │
│  ┌─────────────────────────────────────────────────────────────┐   │
│  │                  注册表 (map[string]EventHandler)            │   │
│  │                                                             │   │
│  │  "order.created"    ──────▶ OrderCreatedHandler             │   │
│  │  "order.paid"       ──────▶ OrderPaidHandler                │   │
│  │  "order.shipped"    ──────▶ OrderShippedHandler              │   │
│  │  "product.updated"  ──────▶ ProductUpdatedHandler            │   │
│  │  "user.registered"  ──────▶ UserRegisteredHandler            │   │
│  │  ...                                                        │   │
│  └─────────────────────────────────────────────────────────────┘   │
│                                                                     │
│  注册方式（两种）：                                                 │
│                                                                     │
│  方式一：init() 自动注册                                            │
│  func init() {                                                      │
│      workers.Register(&OrderCreatedHandler{})                       │
│  }                                                                  │
│                                                                     │
│  方式二：集中注册（ProcessManager 模式，当前使用）                    │
│  func RegisterAllToManager(pm *worker.ProcessManager) {             │
│      registry := pm.GetRegistry()                                   │
│      registry.Register("order.created", &OrderCreatedHandler{})     │
│      registry.Register("order.paid", &OrderPaidHandler{})           │
│      registry.Register("product.updated", &ProductUpdatedHandler{}) │
│  }                                                                  │
│                                                                     │
│  方式三：集中注册（旧模式，兼容路径）                                 │
│  func RegisterAll(engine *worker.Engine) {                          │
│      registry := engine.GetRegistry()                               │
│      registry.Register("order.created", &OrderCreatedHandler{})     │
│      registry.Register("order.paid", &OrderPaidHandler{})           │
│      registry.Register("product.updated", &ProductUpdatedHandler{}) │
│  }                                                                  │
│                                                                     │
│  路由逻辑：                                                         │
│  handler, ok := registry[event.EventType]                           │
│  if !ok {                                                           │
│      // 未注册的 event_type → 记日志 + 直接 Ack（不重试）          │
│      logger.Warn("unregistered event_type", event.EventType)        │
│      return nil                                                     │
│  }                                                                  │
│  return handler.Handle(ctx, event)                                  │
└─────────────────────────────────────────────────────────────────────┘
```

### 5.3 Middleware 链（责任链模式）

Middleware 链在 Handler 执行前后插入横切逻辑，每个 Middleware 职责单一：

```
┌─────────────────────────────────────────────────────────────────────┐
│                     Middleware Chain                                  │
│                                                                     │
│  消息进入 ──▶ ┌──────────────────────────────────────────────┐      │
│               │  1. Recovery Middleware                      │      │
│               │     defer recover() → 捕获 panic → 记日志    │      │
│               │     → 标记为可重试错误                        │      │
│               └──────────────────┬───────────────────────────┘      │
│                                  │                                   │
│               ┌──────────────────▼───────────────────────────┐      │
│               │  2. Logging Middleware                       │      │
│               │     记录：event_id, event_type,              │      │
│               │           aggregate_id, 开始时间              │      │
│               │     → 使用 worker 日志模块                    │      │
│               └──────────────────┬───────────────────────────┘      │
│                                  │                                   │
│               ┌──────────────────▼───────────────────────────┐      │
│               │  3. Metrics Middleware                       │      │
│               │     记录：消费开始时间（用于计算延迟）         │      │
│               │     → Prometheus Counter/Histogram           │      │
│               └──────────────────┬───────────────────────────┘      │
│                                  │                                   │
│               ┌──────────────────▼───────────────────────────┐      │
│               │  4. Timeout Middleware                       │      │
│               │     ctx = context.WithTimeout(ctx, 30s)      │      │
│               │     → 控制单条消息最大处理时间                │      │
│               └──────────────────┬───────────────────────────┘      │
│                                  │                                   │
│               ┌──────────────────▼───────────────────────────┐      │
│               │  5. Business Handler                         │      │
│               │     幂等检查 → 业务逻辑 → 提交               │      │
│               └──────────────────┬───────────────────────────┘      │
│                                  │                                   │
│               ┌──────────────────▼───────────────────────────┐      │
│               │  后处理（Middleware 逆序执行）                │      │
│               │     Metrics: 记录处理耗时                     │      │
│               │     Logging: 记录处理结果（成功/失败）        │      │
│               └──────────────────────────────────────────────┘      │
│                                                                     │
│  Middleware 类型：                                                   │
│  type Middleware func(HandlerFunc) HandlerFunc                      │
│  type HandlerFunc func(ctx context.Context, event *Event) error     │
└─────────────────────────────────────────────────────────────────────┘
```

### 5.4 Middleware 注册配置

```yaml
worker:
  middleware:
    # 按顺序执行，不可调整顺序
    - recovery       # 必须第一个（捕获后续所有 panic）
    - logging        # 日志记录
    - metrics        # 指标采集
    - timeout        # 超时控制（必须最后一个，包裹 Handler）
```

---

## 六、幂等性设计详细方案

### 6.1 为什么需要双层幂等

Kafka 提供 at-least-once 语义，消息可能被重复投递。幂等保障需要覆盖两个层面：

```
┌─────────────────────────────────────────────────────────────────────┐
│                     双层幂等设计                                     │
│                                                                     │
│  第一层：状态机校验（业务级幂等）                                    │
│  ┌─────────────────────────────────────────────────────────────┐   │
│  │  场景：订单已支付，重复收到 order.paid 事件                  │   │
│  │  处理：检查订单当前状态，已是"已支付"则直接跳过              │   │
│  │  优点：无需额外存储，利用业务数据本身判断                    │   │
│  │  局限：仅适用于有明确状态流转的场景                          │   │
│  └─────────────────────────────────────────────────────────────┘   │
│                                                                     │
│  第二层：Inbox 表（存储级幂等）                                     │
│  ┌─────────────────────────────────────────────────────────────┐   │
│  │  场景：同一事件被重复投递，但状态机无法判断（如通知类事件）  │   │
│  │  处理：查询 processed_events 表，event_id 已存在则跳过       │   │
│  │  优点：覆盖所有场景，通用性强                                │   │
│  │  局限：需要额外存储和查询                                    │   │
│  └─────────────────────────────────────────────────────────────┘   │
│                                                                     │
│  执行顺序：第一层 → 第二层 → 业务逻辑                              │
│  两层都通过才执行业务逻辑，任一层拦截则直接 Ack                     │
└─────────────────────────────────────────────────────────────────────┘
```

### 6.2 第一层：状态机校验

```
┌─────────────────────────────────────────────────────────────────────┐
│                     状态机校验流程                                    │
│                                                                     │
│  以订单支付事件为例：                                                │
│                                                                     │
│  收到 order.paid 事件                                               │
│       │                                                             │
│       ▼                                                             │
│  查询订单当前状态                                                    │
│       │                                                             │
│       ▼                                                             │
│  ┌─────────────────────────────────────────────────────────────┐   │
│  │  订单状态        是否处理    说明                            │   │
│  │  ─────────       ────────    ────                            │   │
│  │  待支付(0)       ✅ 处理     正常流程，执行支付回调          │   │
│  │  已支付(1)       ⏭ 跳过     重复消息，幂等跳过              │   │
│  │  已发货(2)       ⏭ 跳过     已处理过，幂等跳过              │   │
│  │  已取消(3)       ❌ 告警     异常状态，记日志+告警           │   │
│  └─────────────────────────────────────────────────────────────┘   │
│                                                                     │
│  实现要点：                                                         │
│  - 状态检查与业务操作必须在同一事务中（防止并发竞态）              │
│  - 使用 SELECT ... FOR UPDATE 锁定订单行                           │
│  - 状态转换规则集中管理，避免散落在各 Handler 中                   │
└─────────────────────────────────────────────────────────────────────┘
```

### 6.3 第二层：Inbox 表

```
┌─────────────────────────────────────────────────────────────────────┐
│                     Inbox 表设计                                     │
│                                                                     │
│  表名：processed_events                                             │
│                                                                     │
│  ┌─────────────────────────────────────────────────────────────┐   │
│  │  字段              类型            说明                      │   │
│  │  ─────────         ─────           ────                      │   │
│  │  event_id          VARCHAR(36)     事件唯一 ID（主键）       │   │
│  │  event_type        VARCHAR(64)     事件类型                  │   │
│  │  aggregate_id      VARCHAR(64)     业务实体 ID               │   │
│  │  processed_at      DATETIME        处理时间                  │   │
│  │  handler_name      VARCHAR(64)     处理的 Handler 名称       │   │
│  └─────────────────────────────────────────────────────────────┘   │
│                                                                     │
│  核心约束：                                                         │
│  - event_id 为 PRIMARY KEY（唯一约束）                              │
│  - INSERT 失败（Duplicate Key）= 重复消息，直接跳过                │
│                                                                     │
│  处理流程：                                                         │
│  ┌─────────────────────────────────────────────────────────────┐   │
│  │  BEGIN TRANSACTION                                          │   │
│  │                                                             │   │
│  │  INSERT INTO processed_events (event_id, event_type, ...)   │   │
│  │  → 成功：继续执行业务逻辑                                    │   │
│  │  → Duplicate Key：跳过（已处理过）                           │   │
│  │                                                             │   │
│  │  执行业务逻辑...                                             │   │
│  │                                                             │   │
│  │  COMMIT                                                     │   │
│  │                                                             │   │
│  │  关键：INSERT 幂等记录与业务操作在同一事务                   │   │
│  │  要么都成功，要么都回滚                                      │   │
│  └─────────────────────────────────────────────────────────────┘   │
│                                                                     │
│  数据清理：                                                         │
│  - 定期清理 processed_at 超过 30 天的记录                          │
│  - 由 cmd/cron 定时任务执行                                         │
└─────────────────────────────────────────────────────────────────────┘
```

### 6.4 幂等处理完整流程图

```
┌─────────────────────────────────────────────────────────────────────┐
│                     幂等处理完整流程                                  │
│                                                                     │
│  收到事件 (event_id, event_type, aggregate_id)                      │
│       │                                                             │
│       ▼                                                             │
│  ┌──────────────────────────┐                                       │
│  │ 第一层：状态机校验        │                                       │
│  │ 查询业务实体当前状态      │                                       │
│  └────────────┬─────────────┘                                       │
│               │                                                     │
│        ┌──────┼──────┐                                              │
│        │      │      │                                              │
│    可处理  已处理  异常状态                                           │
│        │      │      │                                              │
│        │      ▼      ▼                                              │
│        │   跳过+Ack  告警+Ack                                        │
│        │                                                             │
│        ▼                                                             │
│  ┌──────────────────────────┐                                       │
│  │ 第二层：Inbox 表检查      │                                       │
│  │ INSERT processed_events   │                                       │
│  └────────────┬─────────────┘                                       │
│               │                                                     │
│        ┌──────┴──────┐                                              │
│        │             │                                              │
│    插入成功      Duplicate Key                                       │
│        │             │                                              │
│        │             ▼                                              │
│        │        跳过+Ack                                            │
│        │                                                             │
│        ▼                                                             │
│  ┌──────────────────────────┐                                       │
│  │ 执行业务逻辑              │                                       │
│  │ (与 INSERT 在同一事务)    │                                       │
│  └────────────┬─────────────┘                                       │
│               │                                                     │
│        ┌──────┴──────┐                                              │
│        │             │                                              │
│    成功           失败                                               │
│        │             │                                              │
│        ▼             ▼                                              │
│   COMMIT        ROLLBACK                                            │
│   +Ack          (Inbox 记录也回滚)                                   │
│                 +重试                                                │
└─────────────────────────────────────────────────────────────────────┘
```

---

## 七、重试与死信机制详细设计

### 7.1 重试策略

```
┌─────────────────────────────────────────────────────────────────────┐
│                     指数退避重试策略                                  │
│                                                                     │
│  公式：delay = min(base_delay × 2^(retry_count-1), max_delay)       │
│                                                                     │
│  示例（base=1s, max=30m）：                                         │
│  ┌─────────────────────────────────────────────────────────────┐   │
│  │  重试次数    延迟时间        累计等待                        │   │
│  │  ────────    ────────        ────────                        │   │
│  │  1           1s              1s                              │   │
│  │  2           2s              3s                              │   │
│  │  3           4s              7s                              │   │
│  │  4           8s              15s                             │   │
│  │  5           16s             31s                             │   │
│  │  6           32s             ~1m                             │   │
│  │  7           64s             ~2m                             │   │
│  │  8           128s            ~4m                             │   │
│  │  9           256s            ~8m                             │   │
│  │  10          512s            ~17m                            │   │
│  │  11          1024s           ~34m → 超过 max_delay           │   │
│  │  12+         1800s (30m)     每次 +30m                       │   │
│  └─────────────────────────────────────────────────────────────┘   │
│                                                                     │
│  实现方式：                                                         │
│  - Outbox 记录回退为 WAIT 时，设置下次可扫描时间                    │
│  - WHERE status='WAIT' AND next_retry_at <= NOW()                  │
│  - next_retry_at = NOW() + calculated_delay                        │
│                                                                     │
│  抖动（Jitter）：                                                   │
│  - 在延迟基础上增加 ±20% 随机抖动                                  │
│  - 防止大量记录同时重试造成 DB/Kafka 压力                          │
└─────────────────────────────────────────────────────────────────────┘
```

### 7.2 错误分类

```
┌─────────────────────────────────────────────────────────────────────┐
│                     错误分类与处理策略                                │
│                                                                     │
│  ┌─────────────────────────────────────────────────────────────┐   │
│  │  错误类型            示例                    处理策略        │   │
│  │  ────────            ────                    ────────        │   │
│  │  可重试错误          DB 连接超时             指数退避重试    │   │
│  │                      Kafka 发送失败                          │   │
│  │                      Redis 超时                              │   │
│  │                      网络抖动                                │   │
│  │                                                             │   │
│  │  不可重试错误        参数校验失败            直接标记 DEAD   │   │
│  │                      数据格式错误            记日志+告警     │   │
│  │                      业务规则冲突（非状态）                  │   │
│  │                                                             │   │
│  │  不确定错误          第三方 API 超时         查询确认状态    │   │
│  │                      （支付回调）           再决定重试/跳过 │   │
│  │                                                             │   │
│  │  超过最大重试        retry_count >= max      标记 DEAD       │   │
│  │                                            写入死信表       │   │
│  │                                            触发告警         │   │
│  └─────────────────────────────────────────────────────────────┘   │
│                                                                     │
│  错误判断接口：                                                     │
│  type RetryableError interface {                                    │
│      error                                                          │
│      IsRetryable() bool    // 是否可重试                            │
│  }                                                                  │
│                                                                     │
│  Handler 返回的 error 如果实现了该接口，框架自动判断                │
│  否则默认视为可重试错误                                             │
└─────────────────────────────────────────────────────────────────────┘
```

### 7.3 死信表设计

```
┌─────────────────────────────────────────────────────────────────────┐
│                     死信表设计                                        │
│                                                                     │
│  表名：event_dead_letter                                            │
│                                                                     │
│  ┌─────────────────────────────────────────────────────────────┐   │
│  │  字段              类型            说明                      │   │
│  │  ─────────         ─────           ────                      │   │
│  │  id                BIGINT          自增主键                  │   │
│  │  event_id          VARCHAR(36)     原始事件 ID               │   │
│  │  event_type        VARCHAR(64)     事件类型                  │   │
│  │  aggregate_id      VARCHAR(64)     业务实体 ID               │   │
│  │  payload           JSON            原始事件数据              │   │
│  │  error_msg         TEXT            最后一次错误信息          │   │
│  │  retry_count       INT             已重试次数                │   │
│  │  original_status   VARCHAR(16)     原始 outbox 状态          │   │
│  │  created_at        DATETIME        进入死信时间              │   │
│  │  resolved_at       DATETIME        人工处理时间（可空）      │   │
│  │  resolved_by       VARCHAR(64)     处理人（可空）            │   │
│  │  resolve_note      TEXT            处理备注（可空）          │   │
│  └─────────────────────────────────────────────────────────────┘   │
│                                                                     │
│  死信处理流程：                                                     │
│  ┌─────────────────────────────────────────────────────────────┐   │
│  │  1. 监控告警：死信数量 > 100 → 触发告警                     │   │
│  │  2. 人工排查：通过 CLI 或管理后台查看死信详情                │   │
│  │  3. 修复问题：修复导致死信的业务 bug 或数据问题              │   │
│  │  4. 手动重试：通过 CLI 将死信记录重新写入 outbox 表          │   │
│  │     或直接调用 Handler 处理                                  │   │
│  │  5. 标记已处理：更新 resolved_at 和 resolved_by              │   │
│  └─────────────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────────────┘
```

### 7.4 重试配置

```yaml
worker:
  retry:
    max_retry: 10                # 最大重试次数
    base_delay: 1s               # 基础延迟
    max_delay: 30m               # 最大延迟（指数退避上限）
    jitter_factor: 0.2           # 抖动因子（±20%）
    dead_letter_enabled: true    # 是否启用死信表
```

---

## 八、优雅关闭详细设计

### 8.1 关闭流程

```
┌─────────────────────────────────────────────────────────────────────┐
│                     优雅关闭流程                                      │
│                                                                     │
│  SIGTERM / SIGINT 信号                                              │
│       │                                                             │
│       ▼                                                             │
│  ┌─────────────────────────────────────────────────────────────┐   │
│  │  Phase 1: 停止拉取（Stop Pulling）            [立即执行]     │   │
│  │                                                             │   │
│  │  pm.Stop() → 逆序关闭所有 Process:                          │   │
│  │  - DelayScheduler.Stop(): 停止 Redis Sorted Set 调度        │   │
│  │  - Relay.Stop(): 停止定时扫描，不再读取新的 WAIT 记录       │   │
│  │  - Consumer.Stop(): 停止从 Kafka 拉取（内部自行关闭         │   │
│  │    ConsumerGroup 连接）                                     │   │
│  │  - 效果：不再有新的消息进入 Worker Pool                     │   │
│  └──────────────────────────┬──────────────────────────────────┘   │
│                             │                                       │
│                             ▼                                       │
│  ┌─────────────────────────────────────────────────────────────┐   │
│  │  Phase 2: 等待处理完成（Drain）               [最多 30s]     │   │
│  │                                                             │   │
│  │  - 等待 Worker Pool 中所有正在处理的消息完成                │   │
│  │  - 使用 sync.WaitGroup 等待所有 Worker goroutine 退出       │   │
│  │  - 设置超时上限（shutdown_timeout: 30s）                    │   │
│  │  - 超时后强制进入下一阶段（记日志+告警）                    │   │
│  └──────────────────────────┬──────────────────────────────────┘   │
│                             │                                       │
│                             ▼                                       │
│  ┌─────────────────────────────────────────────────────────────┐   │
│  │  Phase 3: 提交 Offset（Commit Offsets）       [立即执行]     │   │
│  │                                                             │   │
│  │  - 将所有已 MarkMessage 的 offset 提交到 Kafka              │   │
│  │  - 确保下次启动不会重复消费已处理的消息                     │   │
│  └──────────────────────────┬──────────────────────────────────┘   │
│                             │                                       │
│                             ▼                                       │
│  ┌─────────────────────────────────────────────────────────────┐   │
│  │  Phase 4: 关闭连接（Close Connections）       [立即执行]     │   │
│  │                                                             │   │
│  │  - 关闭 Kafka Producer（Relay 使用）                        │   │
│  │  - 关闭 Kafka Consumer                                      │   │
│  │  - 关闭 Redis 连接                                          │   │
│  │  - 关闭 MySQL 连接（通过 bootstrap.Shutdown）               │   │
│  │  - 关闭 Logger（flush 缓冲日志）                            │   │
│  └─────────────────────────────────────────────────────────────┘   │
│                                                                     │
│  总超时控制：shutdown_timeout = 30s                                 │
│  超过总超时 → 强制退出（os.Exit(1)）+ 告警                         │
└─────────────────────────────────────────────────────────────────────┘
```

### 8.2 关闭时序图

```
main goroutine          Relay goroutine       Consumer goroutine     Worker goroutines
     │                       │                      │                      │
     │ ◄── SIGTERM ───       │                      │                      │
     │                       │                      │                      │
     │── cancel(ctx) ──────▶ │                      │                      │
     │                       │── 停止扫描 ──▶       │                      │
     │                       │── 完成当前投递 ──▶   │                      │
     │                       │── exit ──▶           │                      │
     │                       │                      │                      │
     │── consumer.Close() ─────────────────────────▶│                      │
     │                                              │── 停止拉取 ──▶      │
     │                                              │── 退出循环 ──▶      │
     │                                              │                      │
     │── wg.Wait() ─────────────────────────────────────────────────────▶│
     │                                              │                      │── 完成当前处理
     │                                              │                      │── exit
     │                                              │                      │
     │◄── wg.Done() (all) ───────────────────────────────────────────────│
     │                       │                      │                      │
     │── commit offsets ───────────────────────────▶│                      │
     │                       │                      │                      │
     │── close connections (Kafka, Redis, MySQL, Logger) ──▶              │
     │                       │                      │                      │
     ▼ exit(0)               │                      │                      │
```

### 8.3 关闭配置

```yaml
worker:
  shutdown:
    timeout: 30s                 # 总关闭超时
    drain_timeout: 25s           # 等待处理完成的超时（需 < timeout）
    commit_before_close: true    # 关闭前是否强制提交 offset
```

---

## 九、可观测性设计

### 9.1 监控指标

```
┌─────────────────────────────────────────────────────────────────────┐
│                     Prometheus 指标设计                               │
│                                                                     │
│  ┌─────────────────────────────────────────────────────────────┐   │
│  │  指标名称                        类型        说明            │   │
│  │  ────────                        ────        ────            │   │
│  │  worker_messages_total           Counter     消费消息总数     │   │
│  │    labels: event_type, status                               │   │
│  │    (status: success/retry/dead)                              │   │
│  │                                                             │   │
│  │  worker_message_duration_seconds Histogram   处理延迟分布   │   │
│  │    labels: event_type                                        │   │
│  │    buckets: [0.01, 0.05, 0.1, 0.5, 1, 5, 10, 30]           │   │
│  │                                                             │   │
│  │  worker_queue_depth              Gauge       队列深度        │   │
│  │    (taskChan 中待处理消息数)                                  │   │
│  │                                                             │   │
│  │  worker_active_workers           Gauge       活跃 Worker 数  │   │
│  │    (正在处理消息 goroutine 数)                                │   │
│  │                                                             │   │
│  │  worker_outbox_pending           Gauge       Outbox 积压量   │   │
│  │    (status=WAIT 的 outbox 记录数)                             │   │
│  │                                                             │   │
│  │  worker_dead_letter_total        Counter     死信总数        │   │
│  │    labels: event_type                                        │   │
│  │                                                             │   │
│  │  worker_relay_batch_size         Histogram   Relay 批次大小  │   │
│  │    buckets: [10, 25, 50, 100, 200, 500]                      │   │
│  │                                                             │   │
│  │  worker_kafka_lag                Gauge       Kafka 消费 lag  │   │
│  │    labels: topic, partition                                  │   │
│  └─────────────────────────────────────────────────────────────┘   │
│                                                                     │
│  指标暴露：HTTP :9090/metrics（独立端口，不与业务 API 混用）       │
└─────────────────────────────────────────────────────────────────────┘
```

### 9.2 告警规则

```
┌─────────────────────────────────────────────────────────────────────┐
│                     告警规则设计                                      │
│                                                                     │
│  ┌─────────────────────────────────────────────────────────────┐   │
│  │  告警名称              条件                      级别        │   │
│  │  ────────              ────                      ────        │   │
│  │  消费延迟过高          kafka_lag > 10000         Warning     │   │
│  │  消费停滞              lag > 0 持续 5min         Critical    │   │
│  │  死信积压              dead_letter_total > 100   Warning     │   │
│  │  Outbox 积压           outbox_pending > 5000     Warning     │   │
│  │  Worker 异常重启       进程重启 > 3次/小时       Critical    │   │
│  │  处理延迟异常          P99 > 10s 持续 5min       Warning     │   │
│  │  Goroutine 泄漏        active_workers 持续下降   Warning     │   │
│  │  Relay 投递失败        relay 连续失败 > 10次     Critical    │   │
│  └─────────────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────────────┘
```

### 9.3 日志规范

```
┌─────────────────────────────────────────────────────────────────────┐
│                     Worker 日志规范                                   │
│                                                                     │
│  日志模块：worker（独立于 http/order/product 等模块）               │
│  日志文件：runtime/logs/worker/worker.log                           │
│                                                                     │
│  必须包含的字段：                                                   │
│  ┌─────────────────────────────────────────────────────────────┐   │
│  │  字段              说明              示例                    │   │
│  │  ──────            ────              ────                    │   │
│  │  event_id          事件唯一 ID       "evt_abc123"            │   │
│  │  event_type        事件类型          "order.created"         │   │
│  │  aggregate_id      业务实体 ID       "order_456"             │   │
│  │  trace_id          链路追踪 ID       "trace_xyz789"          │   │
│  │  worker_id         Worker goroutine 编号  "worker-3"         │   │
│  │  duration_ms       处理耗时          152                     │   │
│  │  retry_count       当前重试次数      2                       │   │
│  └─────────────────────────────────────────────────────────────┘   │
│                                                                     │
│  关键日志点：                                                       │
│  - Relay: 每次扫描结果（扫描数量、投递成功/失败数量）              │
│  - Consumer: 消息拉取异常、分发阻塞                                │
│  - Worker: 每条消息处理开始/结束（含耗时）、panic 恢复             │
│  - Handler: 幂等检查结果、业务操作结果                             │
│  - Shutdown: 每个阶段的开始/完成/超时                              │
└─────────────────────────────────────────────────────────────────────┘
```

### 9.4 链路追踪

```
┌─────────────────────────────────────────────────────────────────────┐
│                     链路追踪设计                                      │
│                                                                     │
│  trace_id 传递路径：                                                │
│                                                                     │
│  Observer (写 outbox)                                               │
│    → outbox.metadata["trace_id"]                                    │
│                                                                     │
│  Relay (投递 Kafka)                                                 │
│    → Kafka Message Header["trace_id"]                               │
│                                                                     │
│  Consumer (拉取消息)                                                │
│    → Event.Metadata["trace_id"]                                     │
│                                                                     │
│  Worker / Handler (处理消息)                                        │
│    → ctx = trace.ContextWithRemoteSpanParent(ctx, trace_id)         │
│    → 创建新的 Span，关联到原始 trace                                │
│                                                                     │
│  效果：                                                             │
│  一个 HTTP 请求产生的事件，从生产到消费的完整链路                    │
│  可以在 Jaeger/Tempo 中查看完整的 Span 树                           │
└─────────────────────────────────────────────────────────────────────┘
```

---

## 十、配置设计

### 10.1 完整配置结构

```yaml
# config.yml 中新增 worker 配置段

worker:
  # ── Relay 配置 ──
  relay:
    enabled: true                    # 是否启用 Relay
    poll_interval: 2s                # 轮询间隔
    batch_size: 100                  # 每次扫描最大条数
    lease_timeout: 60s               # SENDING 租约超时
    recovery_interval: 30s           # 回收器执行间隔
    max_retry: 10                    # 最大重试次数
    retry_base_delay: 1s             # 重试基础延迟
    retry_max_delay: 30m             # 重试最大延迟
    jitter_factor: 0.2               # 抖动因子

  # ── Consumer 配置 ──
  consumer:
    enabled: true                    # 是否启用 Consumer
    group_id: "shop-worker"          # Consumer Group ID
    topics:                          # 消费的 Topic 列表
      - "shop-order-events"
      - "shop-product-events"
      - "shop-user-events"
      - "shop-default-events"
    initial_offset: "oldest"         # 首次启动的初始 offset
    offset_commit_interval: 1s       # offset 提交间隔
    task_channel_buffer: 1000        # Task Channel 缓冲大小

  # ── Worker Pool 配置 ──
  pool:
    workers: 10                      # Worker goroutine 数量
    message_timeout: 30s             # 单条消息处理超时
    panic_recovery: true             # 是否启用 panic recover

  # ── 死信配置 ──
  dead_letter:
    enabled: true                    # 是否启用死信表
    alert_threshold: 100             # 死信告警阈值

  # ── 优雅关闭配置 ──
  shutdown:
    timeout: 30s                     # 总关闭超时
    drain_timeout: 25s               # 等待处理超时

  # ── 可观测性配置 ──
  observability:
    metrics_enabled: true            # 是否暴露 Prometheus 指标
    metrics_port: 9090               # 指标暴露端口
    tracing_enabled: false           # 是否启用链路追踪
```

### 10.2 配置加载

```
┌─────────────────────────────────────────────────────────────────────┐
│                     配置加载流程                                      │
│                                                                     │
│  config.yml                                                         │
│       │                                                             │
│       ▼                                                             │
│  Viper 解析                                                         │
│       │                                                             │
│       ▼                                                             │
│  conf.Config.Worker → WorkerConfig 结构体                           │
│                                                                     │
│  ┌─────────────────────────────────────────────────────────────┐   │
│  │  type WorkerConfig struct {                                  │   │
│  │      Relay       RelayConfig       `mapstructure:"relay"`    │   │
│  │      Consumer    ConsumerConfig    `mapstructure:"consumer"` │   │
│  │      Pool        PoolConfig        `mapstructure:"pool"`     │   │
│  │      DeadLetter  DeadLetterConfig  `mapstructure:"dead_letter"` │
│  │      Shutdown    ShutdownConfig    `mapstructure:"shutdown"` │   │
│  │      Observability ObsConfig       `mapstructure:"observability"` │
│  │  }                                                          │   │
│  └─────────────────────────────────────────────────────────────┘   │
│                                                                     │
│  配置校验（启动时检查）：                                           │
│  - relay.enabled 和 consumer.enabled 至少一个为 true               │
│  - pool.workers > 0 且 <= 100                                      │
│  - shutdown.drain_timeout < shutdown.timeout                       │
│  - consumer.group_id 不为空                                        │
│  - consumer.topics 不为空                                          │
└─────────────────────────────────────────────────────────────────────┘
```

---

## 十一、错误处理设计

### 11.1 错误处理全景

```
┌─────────────────────────────────────────────────────────────────────┐
│                     错误处理全景                                      │
│                                                                     │
│  ┌─────────────────────────────────────────────────────────────┐   │
│  │  层级                错误类型              处理方式          │   │
│  │  ────                ────────              ────────          │   │
│  │                                                             │   │
│  │  Relay 层                                                   │   │
│  │  ├─ DB 连接失败       等待+重试（指数退避）                  │   │
│  │  ├─ Kafka 发送失败   根据错误类型判断可重试性               │   │
│  │  ├─ 序列化失败        标记 DEAD（数据损坏不可恢复）          │   │
│  │  └─ 行锁超时          跳过本批，下轮重试                     │   │
│  │                                                             │   │
│  │  Consumer 层                                                │   │
│  │  ├─ Kafka 连接断开   Sarama 自动重连 + Rebalance            │   │
│  │  ├─ 消息解码失败      记日志+Ack（跳过毒消息）              │   │
│  │  ├─ Channel 满        阻塞拉取（背压）                      │   │
│  │  └─ Rebalance        提交当前 offset，等待新分配            │   │
│  │                                                             │   │
│  │  Worker 层                                                  │   │
│  │  ├─ Panic            recover 捕获，记日志，继续消费          │   │
│  │  ├─ Context 超时     标记为可重试错误                        │   │
│  │  ├─ Handler 返回错误  根据 RetryableError 接口判断          │   │
│  │  └─ DB 操作失败      事务回滚，根据错误类型决定重试/死信    │   │
│  │                                                             │   │
│  │  基础设施层                                                 │   │
│  │  ├─ MySQL 断连       GORM 自动重连 + 连接池管理             │   │
│  │  ├─ Redis 断连       自动重连 + 降级（跳过 Redis 幂等检查） │   │
│  │  └─ 磁盘满           日志 rotate + 告警                     │   │
│  └─────────────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────────────┘
```

### 11.2 毒消息处理

```
┌─────────────────────────────────────────────────────────────────────┐
│                     毒消息（Poison Message）处理                      │
│                                                                     │
│  毒消息：无法正常解码或处理的消息（如 JSON 格式损坏）               │
│                                                                     │
│  处理策略：                                                         │
│  ┌─────────────────────────────────────────────────────────────┐   │
│  │  1. 解码失败 → 记录原始消息到 dead_letter 表                │   │
│  │  2. 标记 error_msg = "decode failed: ..."                   │   │
│  │  3. Ack 该消息（提交 offset，不再重复投递）                  │   │
│  │  4. 触发告警（通知开发人员排查）                             │   │
│  │                                                             │   │
│  │  关键：必须 Ack，否则毒消息会无限循环消费                    │   │
│  └─────────────────────────────────────────────────────────────┘   │
│                                                                     │
│  与业务死信的区别：                                                 │
│  - 业务死信：消息解码正常，但 Handler 处理多次失败                 │
│  - 毒消息：消息本身有问题，无法进入 Handler                        │
└─────────────────────────────────────────────────────────────────────┘
```

---

## 十二、组件交互时序图

### 12.1 正常处理流程

```
┌──────────┐  ┌──────────┐  ┌──────────┐  ┌──────────┐  ┌──────────┐  ┌──────────┐
│  MySQL   │  │  Relay   │  │  Kafka   │  │ Consumer │  │  Worker  │  │ Handler  │
│ (outbox) │  │          │  │          │  │          │  │  Pool    │  │          │
└────┬─────┘  └────┬─────┘  └────┬─────┘  └────┬─────┘  └────┬─────┘  └────┬─────┘
     │             │             │             │             │             │
     │◄── 扫描 WAIT ──│             │             │             │             │
     │─── 返回记录 ──▶│             │             │             │             │
     │◄── UPDATE SENDING│            │             │             │             │
     │             │             │             │             │             │
     │             │── Produce ──▶│             │             │             │
     │             │             │             │             │             │
     │◄── UPDATE SENT ─│            │             │             │             │
     │             │             │             │             │             │
     │             │             │── Message ─▶│             │             │
     │             │             │             │             │             │
     │             │             │             │── task ────▶│             │
     │             │             │             │             │             │
     │             │             │             │             │── Handle ──▶│
     │             │             │             │             │             │
     │             │             │             │             │             │── 幂等检查
     │             │             │             │             │             │── 业务逻辑
     │             │             │             │             │             │── COMMIT
     │             │             │             │             │             │
     │             │             │             │             │◄── success ──│
     │             │             │             │             │             │
     │             │             │             │             │── MarkMsg ─▶│
     │             │             │             │             │  (Ack)      │
     │             │             │             │             │             │
```

### 12.2 重试流程

```
┌──────────┐  ┌──────────┐  ┌──────────┐  ┌──────────┐
│  Worker  │  │  Relay   │  │  MySQL   │  │  Kafka   │
│  Pool    │  │          │  │ (outbox) │  │          │
└────┬─────┘  └────┬─────┘  └────┬─────┘  └────┬─────┘
     │             │             │             │
     │── Handle 失败│             │             │
     │             │             │             │
     │── 返回 error ─────────────────────────▶│
     │   (RetryableError)        │             │
     │             │             │             │
     │             │             │◄── 不 MarkMsg ──│
     │             │             │    (不提交 offset)│
     │             │             │             │
     │             │             │◄── Relay 重新扫描│
     │             │             │    (退避后)  │
     │             │             │             │
     │             │── UPDATE WAIT ──▶│        │
     │             │   next_retry_at = NOW()+delay│
     │             │             │             │
     │             │             │◄── 等待退避时间 ─│
     │             │             │             │
     │             │── 重新扫描 ─▶│             │
     │             │             │── 返回记录 ──▶│
     │             │             │             │
     │             │── Produce ───────────────▶│
     │             │             │             │
     │             │             │             │── 重新消费 ──▶ Worker
```

---

## 十三、目录结构设计

```
internal/
├── worker/                          # Worker 框架核心代码
│   ├── process_manager.go           # ProcessManager 编排层（当前模式核心）
│   ├── process.go                   # Process 结构体与状态机
│   ├── runnable.go                  # Runnable 通用任务接口（Start/Stop/Name）
│   ├── engine.go                    # Engine 核心引擎（旧模式，兼容路径）
│   ├── kafka_process.go             # Kafka Process 工厂（per-Handler 独立 Consumer）
│   ├── relay_process.go             # Relay Process 工厂（Outbox 扫描投递）
│   ├── scheduler_process.go         # DelayScheduler Process 工厂（Redis 延迟调度）
│   ├── consumer.go                  # Kafka Consumer（双缓冲 + 有序消费 + 超时控制）
│   ├── redis_consumer.go            # Redis Stream Consumer（双缓冲架构）
│   ├── handler.go                   # EventHandler 接口 + Event/Message 结构体
│   ├── registry.go                  # Handler Registry（注册表）
│   ├── middleware.go                # Middleware 类型定义 + 内置 Middleware
│   ├── message.go                   # 统一消息模型 Message（Event 类型别名）
│   ├── payload.go                   # 业务 Payload 结构体（OrderEventPayload 等）
│   ├── relay.go                     # Relay 主循环（通知驱动 + Ticker 兜底）
│   ├── outbox.go                    # WriteOutbox 工具函数 + Redis 通知
│   ├── delay_scheduler.go           # DelayScheduler（Redis Sorted Set 延迟调度）
│   ├── delay_queue.go               # DelayQueue 延迟队列写入接口
│   ├── queue_producer.go            # QueueProducer 抽象接口
│   ├── config.go                    # WorkerConfig 配置结构体
│   ├── retry/                       # 重试策略
│   │   ├── strategy.go              # RetryStrategy 接口
│   │   └── exponential.go           # 指数退避实现
│   ├── deadletter/                  # 死信处理
│   │   └── deadletter.go            # 死信表读写
│   └── metrics/                     # 监控指标
│       └── metrics.go               # Prometheus 指标定义 + 注册
│
├── workers/                         # 业务 Handler 实现与注册
│   ├── register.go                  # RegisterAll + RegisterAllToManager
│   ├── order_handler.go             # OrderHandler 实现
│   ├── product_handler.go           # ProductHandler 实现
│   ├── delay_order_handler.go       # DelayOrderHandler 实现
│   └── demo_handler.go              # DemoHandler（验证模式）
│
├── models/
│   ├── event_outbox.go              # event_outbox 表模型
│   ├── event_dead_letter.go         # event_dead_letter 表模型
│   └── processed_events.go          # processed_events 表模型（Inbox）
│
cmd/
└── worker/
    └── main.go                      # Worker 入口（ProcessManager 模式）
```

---

## 十四、关键设计决策总结

| 决策点 | 选择 | 备选方案 | 选择理由 |
|--------|------|---------|---------|
| Relay 抢占机制 | `SELECT FOR UPDATE SKIP LOCKED` | Redis 分布式锁 | DB 行锁更可靠，无额外依赖，与 outbox 同库 |
| Offset 提交策略 | 手动提交（处理后 Mark） | 自动提交 | 自动提交可能导致消息丢失 |
| 幂等方案 | 状态机 + Inbox 双层 | Redis NX | 双层更健壮，Inbox 与业务同事务保证原子性 |
| Worker 并发模型 | 固定 goroutine 池 + Channel | goroutine-per-message | 可控并发数，防止 DB 连接耗尽 |
| 背压机制 | 有界 Channel 阻塞 Consumer | 丢弃消息 | 不丢消息，自然降速 |
| 重试延迟 | 指数退避 + Jitter | 固定间隔 | 退避避免雪崩，Jitter 避免同步重试 |
| 毒消息处理 | 记死信 + Ack 跳过 | 重试 N 次后丢弃 | 毒消息无法通过重试恢复，必须人工介入 |
| 优雅关闭 | 四阶段有序关闭 | 强制退出 | 不丢消息，不丢数据 |
| 日志模块 | 独立 worker 模块 | 复用 app 模块 | 独立文件便于排查，不影响业务日志 |
| Topic 路由 | event_type 前缀匹配 | 单 Topic | 分 Topic 支持独立扩缩容和监控 |

---

## 十五、多队列架构改造（Phase 1-5）

> 本章节记录 Worker 模块从 Kafka 专用消费引擎向多队列（Kafka + Redis Stream）ProcessManager 架构的演进。

### 15.1 统一消息模型 Message

**文件**：`internal/worker/message.go`

原有的 `Event` 结构体包含 `Offset`/`Partition` 等 Kafka 特有字段，无法适配 Redis Stream。Phase 1 将其重构为 `Message`，屏蔽队列差异：

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

**设计决策**：选择直接重构 Event → Message（选项 A），而非引入中间层。`type Event = Message` 类型别名保证所有现有 Handler 代码无需修改。

### 15.2 QueueConsumer / QueueProducer 接口

**文件**：`internal/worker/queue_consumer.go`、`internal/worker/queue_producer.go`

为屏蔽不同队列的消费差异，引入抽象接口：

```go
// QueueConsumer 队列消费者抽象接口
type QueueConsumer interface {
    Name() string                    // 消费者名称（日志标识）
    Start(ctx context.Context) error // 启动消费循环
    Stop() error                     // 优雅关闭
}

// QueueProducer 队列生产者抽象接口
type QueueProducer interface {
    Produce(ctx context.Context, msg *Message) error
    ProduceBatch(ctx context.Context, msgs []*Message) error
    Close() error
}
```

**实现类**：

| 实现 | 文件 | 说明 |
|------|------|------|
| `Consumer` | `consumer.go` | Kafka ConsumerGroup 消费，实现 QueueConsumer |
| `RedisStreamConsumer` | `redis_consumer.go` | Redis Stream XReadGroup 消费，实现 QueueConsumer |

编译期接口检查：`var _ QueueConsumer = (*RedisStreamConsumer)(nil)`

> QueueProducer 接口当前已定义但 Relay 仍直接调用 Kafka SyncProducer 和 Redis XADD，后续可通过依赖注入方式接入。

### 15.3 RedisStreamConsumer 双缓冲架构

**文件**：`internal/worker/redis_consumer.go`

RedisStreamConsumer 采用与 Kafka Consumer 一致的 **双缓冲架构**（Poller + Worker Pool），保证慢处理不阻塞拉取：

```
Redis Stream
     │
     ▼
┌────────────────────────────────┐
│  redisConsumeLoop (Poller)     │  ← 独立 goroutine
│  XReadGroup → 构造 Message     │
│  → tasks <- task               │  ← 发送到有界 channel
└──────────────┬─────────────────┘
               │
    ┌──────────▼──────────┐
    │   tasks channel     │  ← chan *redisConsumeTask, 缓冲可配置
    │   (背压机制)         │    满时阻塞 Poller
    └──────────┬──────────┘
               │
    ┌──────────┼──────────┐
    ▼          ▼          ▼
 Worker-0   Worker-1   Worker-N   ← Worker Pool
    │          │          │
    ▼          ▼          ▼
 processRedisEvent()              ← 中间件链 + 超时控制
    │
    ├── 成功 → XACK (从 Pending 移除)
    └── 失败 → 不 ACK (保留在 Pending，后续可 XCLAIM 重分配)
```

**关键配置**（`RedisConsumerConfig`）：

| 字段 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| StreamKey | string | — | Redis Stream key |
| ConsumerGroup | string | — | 消费组名称 |
| ConsumerName | string | — | 消费者名称 |
| Concurrency | int | 1 | Worker Pool 并发数 |
| BufferSize | int | 1000 | tasks channel 缓冲大小 |
| BlockTimeout | duration | 5s | XReadGroup BLOCK 超时 |
| HandlerTimeout | duration | 30s | 单条消息处理超时 |
| MaxRetries | int | 5 | Pending 消息最大重试次数 |

**XReadGroup 消费组语义**：
- `>` 表示只读取新消息（未分配给任何消费者）
- 消费组内多消费者竞争分配，与 Kafka ConsumerGroup 同构
- XACK 保证 at-least-once 语义，失败消息保留在 Pending 列表

### 15.4 Process 结构体与生命周期

**文件**：`internal/worker/process.go`

每个 Process 封装一个独立的 QueueConsumer 实例，拥有明确的状态机：

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

### 15.5 ProcessManager 编排层

**文件**：`internal/worker/process_manager.go`

ProcessManager 管理多个异构 Process（Kafka/Redis 等）的生命周期，类似 Hyperf 的 ProcessManager，但使用 goroutine 而非 fork 进程：

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
| **启动** | 串行启动 | 避免并发创建 ConsumerGroup 的竞争条件（Kafka sarama 限制） |
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

**ProcessManager 统一管理模式（当前）**：

```
cmd/worker/main.go
 └── ProcessManager
      ├── processes []*Process
      │    ├── Process{kafka-order-events, kafka, Consumer}
      │    ├── Process{kafka-product-events, kafka, Consumer}
      │    ├── Process{relay, relay, Relay}
      │    └── Process{delay-scheduler, scheduler, DelayScheduler}
      └── registry *Registry（共享）
```

> **旧模式（兼容路径）—— Engine 集成**：
> ```
> Engine
>  ├── consumers []QueueConsumer     ← Kafka Consumer 列表（直接管理）
>  ├── consumerGroups []sarama.ConsumerGroup  ← Kafka 特有资源
>  ├── processManager *ProcessManager ← 异构 Process 管理（Redis 等）
>  │    └── processes []*Process
>  ├── relay *Relay                   ← 多队列 Outbox 投递器
>  ├── delayScheduler *DelayScheduler ← Redis Sorted Set 延迟调度器
>  └── registry *Registry             ← Handler 注册表（共享）
> ```

### 15.6 DelayScheduler 延迟调度器

**文件**：`internal/worker/delay_scheduler.go`

DelayScheduler 基于 Redis Sorted Set 实现毫秒级延迟调度，是延迟消息的**主调度路径**，DB Relay 轮询作为降级兜底：

**调度流程**：

```
WriteDelayOutbox() 写入 event_outbox (queue_type=redis, execute_at=未来时间)
       │
       ├── go ScheduleDelay() → ZADD delay:schedule {score=executeAt_ms, member=event_id}
       │
       ▼
DelayScheduler.scheduleLoop() (Ticker 1s)
       │
       ▼
ZRANGEBYSCORE delay:schedule -inf {now_ms} LIMIT 0 50
       │
       ├── 查询 event_outbox 获取完整消息
       ├── 原子抢占: UPDATE status=SENDING WHERE id=? AND status=WAIT
       ├── XADD {topic} → 写入 Redis Stream（立即可消费）
       ├── UPDATE status=SENT
       └── ZREM delay:schedule {event_id}
```

**降级机制**：
- Redis 不可用时，`ScheduleDelay()` 返回 nil，完全依赖 DB Relay 兜底
- Relay 的 `scanAndDeliverByType("redis", false)` 每 10s 扫描 `execute_at <= NOW()` 的记录
- DelayScheduler 与 Relay 通过原子抢占（`UPDATE WHERE status=WAIT`）保证消息不被重复投递

**关键常量**：
- `delayScheduleKey = "delay:schedule"` — Redis Sorted Set key
- `pollInterval = 1s` — 轮询间隔
- `batchSize = 50` — 每次最多调度的消息数

### 15.7 多队列 Outbox 写入

**文件**：`internal/worker/outbox.go`

改造后的 Outbox 写入支持多队列分发：

| 函数 | 用途 | queue_type | execute_at |
|------|------|-----------|------------|
| `WriteOutbox(tx, ..., queueType, executeAt)` | 通用写入（支持多队列） | 参数指定 | 参数指定 |
| `WriteKafkaOutbox(tx, ...)` | Kafka 即时消息（向后兼容） | `kafka` | `NULL` |
| `WriteDelayOutbox(tx, ..., delay)` | Redis 延迟消息 | `redis` | `NOW()+delay` |

`WriteDelayOutbox` 额外将 `event_id` 注册到 Redis Sorted Set（`ScheduleDelay`），实现毫秒级调度。若 Redis 不可用则完全依赖 DB Relay 兜底。

### 15.8 多队列 Relay 投递

**文件**：`internal/worker/relay.go`

Relay 的 `scanAndDeliver()` 按 `queue_type` 分别扫描：

```
scanAndDeliver()
  ├── scanAndDeliverByType("kafka", true)   ← execute_at IS NULL 的即时消息
  │     └── deliverOne() → deliverToKafka() → Kafka Producer.Send()
  │
  └── scanAndDeliverByType("redis", false)  ← execute_at <= NOW() 的延迟消息
        └── deliverOne() → deliverToRedis() → Redis XADD
```

投递成功后清理 Sorted Set 中的调度记录（`ZREM delay:schedule {event_id}`），防止 DelayScheduler 与 Relay 重复投递。

### 15.9 延迟队列业务集成

**DelayQueue 接口**（`internal/worker/delay_queue.go`）：

```go
dq := worker.NewDelayQueue()

// 写入延迟消息（与业务同事务）
err := dq.Push(tx, aggregateId, eventType, topic, delay, payload)

// 指定路由键（保证同一业务实体消息有序）
err := dq.PushWithKey(tx, aggregateId, eventType, topic, partitionKey, delay, payload)
```

**DelayOrderHandler**（`internal/workers/delay_order_handler.go`）：

处理 Redis Stream 中的延迟订单消息：

| eventType | 业务逻辑 | 前置条件 |
|-----------|---------|----------|
| `order.timeout` | 超时取消订单 | Paid=0 且 Status=0（未支付未发货） |
| `order.auto_confirm` | 自动确认收货 | Paid=1 且 Status=1（已支付待收货） |

**OrderEventPayload**（`internal/worker/payload.go`）：

```go
type OrderEventPayload struct {
    OrderId   string `json:"order_id"`
    UserId    int64  `json:"user_id"`
    OrderType string `json:"order_type"` // "normal" / "seckill" / "bargain" / "combination"
    Action    string `json:"action"`     // "timeout_cancel" / "auto_confirm" / "created" / "paid"
}
```

### 15.10 多队列配置模型

**ProcessConfig**（`conf/conf.go`）：

```go
type ProcessConfig struct {
    Name     string                 // 进程名称（如 "redis-delay-consumer"）
    Type     string                 // 队列类型: "kafka" / "redis"
    Config   map[string]interface{} // 队列特有配置
    Handlers []HandlerConfig        // Handler 配置列表
}
```

**配置优先级**：`Processes 模式` > `Handlers per-Handler 模式` > `Topics 全局模式`

当 `processes` 配置存在时，`handlers` 和 `topics` 配置被忽略。

### 15.11 DLQ 运维 CLI

**文件**：`cmd/cli/dlq.go`

| 命令 | 说明 | 示例 |
|------|------|------|
| `dlq list` | 查看死信列表 | `./shop-cli dlq list --topic shop-order-events --limit 20` |
| `dlq retry` | 重试单条死信 | `./shop-cli dlq retry <event_id>` |
| `dlq retry --all` | 批量重试全部 PENDING 死信 | `./shop-cli dlq retry --all --topic shop-order-events` |
| `dlq discard` | 丢弃死信消息 | `./shop-cli dlq discard <event_id>` |
| `dlq clean` | 清理过期已解决记录 | `./shop-cli dlq clean --days 30` |
| `dlq stats` | 死信队列统计 | `./shop-cli dlq stats` |

重试流程：`dlq retry <event_id>` → 查询 `event_dead_letter` → 重新写入 `event_outbox` (queue_type=kafka, status=WAIT) → 更新死信状态为 RESOLVED → Relay 自动扫描并投递。

### 15.12 多队列架构新增文件清单

| 文件路径 | 说明 | Phase |
|---------|------|-------|
| `internal/worker/message.go` | 统一消息模型 Message | Phase 1 |
| `internal/worker/queue_consumer.go` | QueueConsumer 抽象接口 | Phase 2 |
| `internal/worker/queue_producer.go` | QueueProducer 抽象接口 | Phase 2 |
| `internal/worker/redis_consumer.go` | Redis Stream 消费者实现 | Phase 2 |
| `internal/worker/process.go` | Process 结构体与状态机 | Phase 3 |
| `internal/worker/process_manager.go` | ProcessManager 编排层 | Phase 3 |
| `internal/worker/kafka_process.go` | Kafka Process 工厂函数 | Phase 6 |
| `internal/worker/relay_process.go` | Relay Process 工厂函数 | Phase 6 |
| `internal/worker/scheduler_process.go` | DelayScheduler Process 工厂函数 | Phase 6 |
| `internal/worker/runnable.go` | Runnable 通用任务接口 | Phase 6 |
| `internal/worker/delay_queue.go` | DelayQueue 延迟队列写入接口 | Phase 5 |
| `internal/worker/delay_scheduler.go` | DelayScheduler 延迟调度器 | Phase 5 |
| `internal/worker/payload.go` | OrderEventPayload 统一消息体 | Phase 5 |
| `cmd/cli/dlq.go` | DLQ 运维 CLI 命令 | Phase 5 |

### 15.13 多队列关键设计决策

| 决策点 | 选择 | 备选方案 | 选择理由 |
|--------|------|---------|----------|
| 消息模型 | 直接重构 Event → Message | 引入中间层 | 类型别名保证向后兼容，零迁移成本 |
| Redis 消费模式 | 复用双缓冲架构 | 单缓冲直消费 | 与 Kafka Consumer 一致，慢处理不阻塞拉取 |
| 延迟精度 | DB 轮询 + Sorted Set 调度 | 纯 DB 轮询 | Sorted Set 毫秒级精度，DB 兜底保证可靠性 |
| ProcessManager 启动 | 串行启动 | 并发启动 | Kafka sarama ConsumerGroup 不支持并发创建 |
| Relay 多队列 | 按 queue_type 分别扫描 | 统一扫描 | 即时消息和延迟消息的查询条件不同 |
| QueueProducer | 预留接口，Relay 直接调用 | 立即接入 | 避免过度抽象，后续通过 DI 接入 |
