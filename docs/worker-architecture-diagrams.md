# Worker 模块架构可视化文档

> 本文档通过 ASCII 图表直观展示 shop-main 项目中常驻任务模块（Worker）的架构设计。
> 所有图表基于实际源代码绘制，确保与代码实现一致。

---

## 一、多入口架构总览

### 1.1 项目入口结构

```
shop-main/
├── main.go              → shop-server     (HTTP 服务，端口 8000)
├── cmd/cli/             → shop-cli        (命令行工具：商品同步、订单报表等)
├── cmd/cron/            → shop-cron       (定时任务：订单统计、报表生成等)
└── cmd/worker/          → shop-worker     (常驻消费进程：Kafka 消息处理)  ← 本文档重点
```

### 1.2 四进程共享架构

```
                        ┌─────────────────────────┐
                        │      conf/config.yml     │
                        │  (共享配置文件)           │
                        └────────────┬────────────┘
                                     │
              ┌──────────────────────┼──────────────────────┐
              │                      │                      │
    ┌─────────▼─────────┐ ┌─────────▼─────────┐ ┌─────────▼─────────┐
    │   shop-server     │ │   shop-cron       │ │   shop-worker     │
    │   (HTTP 服务)     │ │   (定时任务)       │ │   (常驻消费)      │
    │                   │ │                   │ │                   │
    │  Bootstrap(全量)  │ │  Bootstrap(全量)  │ │  Bootstrap(选择性) │
    │  + Observer       │ │  + Observer       │ │  + KafkaConsumer  │
    │  + Casbin/JWT     │ │  + Casbin/JWT     │ │  (无 HTTP/Casbin) │
    └─────────┬─────────┘ └─────────┬─────────┘ └─────────┬─────────┘
              │                      │                      │
              └──────────────────────┼──────────────────────┘
                                     │
              ┌──────────────────────┼──────────────────────┐
              │          共享 internal/ 和 pkg/              │
              │                      │                      │
              │  ┌──────────┐  ┌─────┴─────┐  ┌─────────┐ │
              │  │ models/  │  │ service/  │  │ worker/ │ │
              │  │ observer/│  │ bootstrap/│  │ pkg/    │ │
              │  └──────────┘  └───────────┘  └─────────┘ │
              └─────────────────────────────────────────────┘
                                     │
              ┌──────────────────────┼──────────────────────┐
              │            各自独立的外部连接                 │
              │                      │                      │
              │  ┌──────┐  ┌──────┐  │  ┌──────┐           │
              │  │MySQL │  │Redis │  │  │Kafka │           │
              │  └──────┘  └──────┘  │  └──────┘           │
              └─────────────────────────────────────────────┘
```

**说明**：四个入口编译为独立二进制，共享 `internal/` 和 `pkg/` 下的所有代码。Worker 进程采用选择性 Bootstrap（`WithKafka()`），加载 Kafka Sync Producer（Relay 投递 outbox 需要），不加载 HTTP Server、Casbin、Observer 等不需要的组件，减少资源占用。

---

## 二、Worker 进程内部架构

### 2.1 启动流程（ProcessManager 统一管理模式）

```
cmd/worker/main.go
│
├── 1. BootstrapWith(configPath, WithKafka())
│       → 配置 → 日志 → Redis → MySQL → Kafka Sync Producer
│
├── 2. registry := worker.NewRegistry()
│       pm := worker.NewProcessManager(registry)
│       → 创建 ProcessManager + 共享 Registry + Context
│
├── 3. workers.RegisterAllToManager(pm)
│       → 按 topic 注册 ProductHandler / OrderHandler / DelayOrderHandler
│       → 注册到 pm.GetRegistry()
│
├── 4. 声明式添加 Process（工厂函数创建）
│       │
│       ├── pm.AddProcesses(worker.NewKafkaProcesses(workerCfg, kafkaCfg, registry)...)
│       │     → 创建 sarama ConsumerGroup（per-Handler 独立 ConsumerGroup）
│       │     → 遍历 config.Handlers，为每个 handler 创建独立 Consumer
│       │     → 包装为 Process{kafka-{topic}, "kafka", consumer}
│       │
│       ├── pm.AddProcess(worker.NewRelayProcess(workerCfg))
│       │     → ParseRelayConfig() → NewRelay()
│       │     → 包装为 Process{"relay", "relay", relay}
│       │
│       └── pm.AddProcess(worker.NewSchedulerProcess())
│             → Redis 可用? → NewDelayScheduler() → Process{"delay-scheduler", "scheduler", ds}
│                            → nil（跳过，降级为 DB Relay 兜底）
│
├── 5. pm.Start()
│       → 串行启动所有 Process（每个 Process.Runner.Start(ctx)）
│       → Kafka Consumer: 启动双缓冲（Poller + Worker Pool）
│       → Relay: 启动通知驱动 + Ticker 兜底 + 租约回收 + 定期清理
│       → DelayScheduler: 启动 Redis Sorted Set 毫秒级调度循环
│
├── 6. signal.NotifyContext → 阻塞等待 SIGINT/SIGTERM
│
├── 7. pm.Stop()
│       → 逆序关闭所有 Process（每个 Process.Runner.Stop()）
│       → DelayScheduler.Stop() → Relay.Stop() → Kafka Consumer.Stop()
│       → Consumer.Stop() 内部自行关闭 ConsumerGroup 连接
│
└── 8. bootstrap.Shutdown()
        → 关闭 Kafka Producer → Redis → MySQL → 刷新日志
```

> **旧模式（兼容路径）**：`engine := worker.NewEngine(CONFIG.Worker)` → `workers.RegisterAll(engine)` → `engine.Start()` → `engine.Stop()`。Engine 内部通过 runners[] + consumerGroups[] 管理组件，当前 `cmd/worker/main.go` 已切换为 ProcessManager 模式。

### 2.2 ProcessManager 组件关系（当前模式）

```
┌─────────────────────────────────────────────────────────────────────┐
│                     ProcessManager                                   │
│              (统一管理所有 Process 的生命周期)                         │
│                                                                     │
│  processes []*Process                                                │
│                                                                     │
│  ┌───────────────────────────────────────────────────────────────┐  │
│  │  Process: kafka-order-events (type=kafka)                     │  │
│  │  Runner: Consumer                                              │  │
│  │  ┌─────────────────────────────────────────────────────┐      │  │
│  │  │  topic: order-events, concurrency: 20, buffer: 2000 │      │  │
│  │  │  双缓冲: Poller + Worker Pool → OrderHandler        │      │  │
│  │  │  Consumer.Stop() 内部自行关闭 ConsumerGroup 连接     │      │  │
│  │  └─────────────────────────────────────────────────────┘      │  │
│  └───────────────────────────────────────────────────────────────┘  │
│                                                                     │
│  ┌───────────────────────────────────────────────────────────────┐  │
│  │  Process: kafka-product-events (type=kafka)                   │  │
│  │  Runner: Consumer                                              │  │
│  │  ┌─────────────────────────────────────────────────────┐      │  │
│  │  │  topic: product-events, concurrency: 10, buffer: 1000│     │  │
│  │  │  双缓冲: Poller + Worker Pool → ProductHandler      │      │  │
│  │  └─────────────────────────────────────────────────────┘      │  │
│  └───────────────────────────────────────────────────────────────┘  │
│                                                                     │
│  ┌───────────────────────────────────────────────────────────────┐  │
│  │  Process: relay (type=relay)                                  │  │
│  │  Runner: Relay                                                 │  │
│  │  ┌─────────────────────────────────────────────────────┐      │  │
│  │  │  通知驱动 + Ticker 兜底 + 租约回收 + 定期清理        │      │  │
│  │  │  按 queue_type 分发: Kafka → deliverToKafka()        │      │  │
│  │  │                       Redis → deliverToRedis()       │      │  │
│  │  └─────────────────────────────────────────────────────┘      │  │
│  └───────────────────────────────────────────────────────────────┘  │
│                                                                     │
│  ┌───────────────────────────────────────────────────────────────┐  │
│  │  Process: delay-scheduler (type=scheduler)                    │  │
│  │  Runner: DelayScheduler                                        │  │
│  │  ┌─────────────────────────────────────────────────────┐      │  │
│  │  │  Redis Sorted Set 毫秒级延迟调度（主路径）            │      │  │
│  │  │  scheduleLoop: Ticker(1s) → ZRANGEBYSCORE → XADD    │      │  │
│  │  │  Redis 不可用时返回 nil，降级为 DB Relay 兜底         │      │  │
│  │  └─────────────────────────────────────────────────────┘      │  │
│  └───────────────────────────────────────────────────────────────┘  │
│                                                                     │
│  ┌───────────────────────────────────────────────────────────────┐  │
│  │  Registry (共享)                                               │  │
│  │  topic → Handler 映射表 (所有 Consumer 共享)                   │  │
│  └───────────────────────────────────────────────────────────────┘  │
│                                                                     │
│  编排策略：                                                          │
│  • 串行启动（避免 ConsumerGroup 并发创建竞争）                       │
│  • 逆序关闭（后启动的先关闭）                                        │
│  • 启动失败回滚已启动的 Process                                      │
│  ctx/cancel: 全局上下文控制                                          │
│  mu + started: 防重入锁                                              │
└─────────────────────────────────────────────────────────────────────┘
```

**说明**：ProcessManager 是 Worker 进程的核心编排层，直接管理所有 Process（Kafka Consumer、Relay、DelayScheduler）的生命周期。每个组件通过工厂函数创建并包装为 `Process`，统一实现 `Runnable` 接口（`Start(ctx)` / `Stop()` / `Name()`）。所有 Process 共享同一个 `Registry`（Handler 注册表）。

> **旧模式（兼容路径）—— Engine 组件关系**：Engine 曾是 Worker 进程的核心，内部直接管理 `runners[]`（Kafka Consumer）、`consumerGroups[]`、`relay`、`delayScheduler`，以及内嵌的 `processManager`（异构 Process）。当前 `cmd/worker/main.go` 已切换为直接使用 ProcessManager 模式，Engine 保留在 `internal/worker/engine.go` 中作为兼容路径。

---

## 三、双缓冲消费者架构（核心）

### 3.1 per-Handler 独立 Consumer 架构（新模式）

```
                    Kafka Cluster
                    ┌─────────┐ ┌─────────┐
                    │order-   │ │product- │
                    │events   │ │events   │
                    └────┬────┘ └────┬────┘
                         │           │
              ┌──────────▼──┐  ┌────▼──────────┐
              │ Consumer A  │  │ Consumer B    │
              │ (订单)       │  │ (商品)         │
              │ concurrency │  │ concurrency   │
              │ = 20        │  │ = 10          │
              │ buffer=2000 │  │ buffer=1000   │
              └──────┬──────┘  └──────┬────────┘
                     │                │
              ┌──────▼──────┐  ┌─────▼─────────┐
              │ tasks A     │  │ tasks B       │
              │ (buffer     │  │ (buffer       │
              │  2000)      │  │  1000)        │
              └──────┬──────┘  └──────┬────────┘
                     │                │
         ┌───────────┼─────┐    ┌─────┼──────────┐
         ▼           ▼     ▼    ▼     ▼          ▼
      [W-0] ... [W-19]         [W-0] ... [W-9]
      (20 goroutine)           (10 goroutine)
```

**说明**：每个 topic 对应一个独立的 Consumer 实例，拥有自己的 tasks channel 和 Worker Pool。慢处理不会影响其他 topic 的消费。所有 Consumer 共享同一个 sarama ConsumerGroup，通过 RoundRobin 分区策略分配 partition。

### 3.2 单个 Consumer 内部双缓冲细节（无序模式 ordered=false）

```
                    Kafka Cluster
                         │
                    ┌────▼────┐
                    │Consumer │
                    │ Group   │  ← sarama.ConsumerGroup
                    │(Round   │    RoundRobin 分区策略
                    │ Robin)  │    手动提交 offset
                    └────┬────┘
                         │
              ┌──────────▼──────────┐
              │    consumeLoop      │  ← 独立 goroutine
              │  (自动重连/rebalance)│    session 结束后重新加入
              └──────────┬──────────┘
                         │
          ┌──────────────▼──────────────┐
          │       ConsumeClaim          │  ← sarama 回调 (per partition)
          │                             │
          │  for msg := range messages  │
          │    → 构造 Event             │
          │    → 提取 Headers           │
          │    → tasks <- event         │  ← 发送到有界 channel
          │    → session.MarkMessage()  │    (背压：满则阻塞)
          └──────────────┬──────────────┘
                         │
              ┌──────────▼──────────┐
              │   tasks channel     │  ← chan *Event, 缓冲可配置
              │   (背压机制)         │    默认 1000，满时阻塞
              └──────────┬──────────┘    ConsumeClaim
                         │
          ┌──────────────┼──────────────┐
          ▼              ▼              ▼
    ┌──────────┐  ┌──────────┐  ┌──────────┐
    │Worker-0  │  │Worker-1  │  │Worker-N  │  ← Worker Pool
    │          │  │          │  │          │    concurrency 可配置
    │defer     │  │defer     │  │defer     │    每个 Worker 有
    │recover() │  │recover() │  │recover() │    panic 恢复
    │          │  │          │  │          │
    │for event │  │for event │  │for event │
    │:= range  │  │:= range  │  │:= range  │
    │  tasks   │  │  tasks   │  │  tasks   │
    └────┬─────┘  └────┬─────┘  └────┬─────┘
         │              │              │
         ▼              ▼              ▼
    ┌──────────────────────────────────────┐
    │         processEvent                 │
    │                                      │
    │  1. registry.GetHandler(event.Topic) │  ← 按 topic 查找
    │  2. Chain(handler, middlewares...)   │  ← 中间件包装
    │  3. context.WithTimeout(timeout)     │  ← 超时控制
    │  4. wrappedHandler.Handle(ctx,event) │  ← 执行处理
    └──────────────────────────────────────┘
                         │
                         ▼
    ┌──────────────────────────────────────┐
    │         Middleware Chain             │
    │                                      │
    │  RecoveryMiddleware                  │  ← panic 恢复
    │    → LoggingMiddleware               │  ← 日志记录
    │      → Handler.Handle()              │  ← 实际业务处理
    └──────────────────────────────────────┘
```

**说明**：双缓冲架构将消息拉取（ConsumeClaim/Poller）和消息处理（Worker Pool）分离为独立的 goroutine 池。慢处理不会阻塞 Kafka 拉取，避免 rebalance。`tasks` channel 设置有界缓冲（默认 1000，可通过配置调整），满时自动背压，天然限流。每个 Worker goroutine 顶层有 `defer recover()` 防止 panic 导致 goroutine 泄漏。`processEvent` 通过 `context.WithTimeout` 为每条消息设置处理超时（默认 30s，可通过 `handler-timeout` 配置），防止慢处理占满 Worker 槽位。

### 3.3 有序消费模式（Key-Based Routing, ordered=true）

```
                    Kafka Cluster
                         │
                    ┌────▼────┐
                    │Consumer │
                    │ Group   │
                    └────┬────┘
                         │
              ┌──────────▼──────────┐
              │    consumeLoop      │
              └──────────┬──────────┘
                         │
          ┌──────────────▼──────────────┐
          │       ConsumeClaim          │
          │                             │
          │  构造 Event                 │
          │  key = event.Key            │
          │  idx = FNV-1a(key) % N      │  ← 哈希路由
          │  orderedChs[idx] <- event   │
          └──────────────┬──────────────┘
                         │
          ┌──────────────┼──────────────┐
          │              │              │
    ┌─────▼─────┐  ┌────▼──────┐  ┌───▼───────┐
    │orderedCh  │  │orderedCh  │  │orderedCh  │
    │  [0]      │  │  [1]      │  │  [N-1]    │
    │(buffer    │  │(buffer    │  │(buffer    │
    │ 1000)     │  │ 1000)     │  │ 1000)     │
    └─────┬─────┘  └────┬──────┘  └───┬───────┘
          │              │              │
          ▼              ▼              ▼
    ┌──────────┐  ┌──────────┐  ┌──────────┐
    │Ordered   │  │Ordered   │  │Ordered   │
    │Worker-0  │  │Worker-1  │  │Worker-N  │
    │          │  │          │  │          │
    │for event │  │for event │  │for event │
    │:= range  │  │:= range  │  │:= range  │
    │orderedCh │  │orderedCh │  │orderedCh │
    │  [0]     │  │  [1]     │  │  [N-1]   │
    └──────────┘  └──────────┘  └──────────┘

    同 key → FNV-1a hash → 同一 orderedCh → 同一 Worker
    保证同 key 消息严格有序处理
```

**说明**：当 `ordered=true` 时，Consumer 不再使用共享的 `tasks` channel，而是为每个 Worker 创建独立的 `orderedChs[i]` channel。消息到达时，通过 `FNV-1a` 哈希算法对 `event.Key` 计算路由目标：`idx = FNV-1a(key) % concurrency`，保证相同 key 的消息始终路由到同一个 Worker goroutine，实现严格有序消费。每个 orderedCh 同样有独立的缓冲大小（与 `buffer-size` 配置一致）。

### 3.4 无序模式 vs 有序模式对比

```
┌────────────────────────────────────────────────────────────────┐
│                                                                │
│  无序模式 (ordered=false, 默认):                                │
│  ┌─────────┐     ┌────────────┐     ┌─────────────────────┐   │
│  │Consume  │────▶│ tasks (共享)│────▶│ Worker Pool (N个)   │   │
│  │Claim    │     │ buffer=1000│     │ 竞争消费，无序保证   │   │
│  └─────────┘     └────────────┘     └─────────────────────┘   │
│                                                                │
│  特点: 高吞吐，所有 Worker 竞争消费，消息处理顺序不确定          │
│                                                                │
│  有序模式 (ordered=true):                                      │
│  ┌─────────┐     ┌────────────────────────────────────────┐   │
│  │Consume  │     │  orderedCh[0] ──▶ OrderedWorker-0      │   │
│  │Claim    │────▶│  orderedCh[1] ──▶ OrderedWorker-1      │   │
│  │ FNV-1a  │     │  orderedCh[N] ──▶ OrderedWorker-N      │   │
│  │ hash路由│     │  (每个 Worker 有独立 channel)            │   │
│  └─────────┘     └────────────────────────────────────────┘   │
│                                                                │
│  特点: 同 key 严格有序，并发度受 key 分布影响                   │
│                                                                │
└────────────────────────────────────────────────────────────────┘
```

---

## 四、数据流转全链路图

```
HTTP 请求
   │
   ▼
┌──────────────┐
│ Controller   │  ← 接收 HTTP 请求
└──────┬───────┘
       │
       ▼
┌──────────────┐
│   Service    │  ← 执行业务逻辑
└──────┬───────┘
       │
       ▼
┌──────────────────────────────────────────────────────┐
│              GORM 事务（同一个 DB 事务）                │
│                                                      │
│  ┌──────────────────┐    ┌────────────────────────┐  │
│  │  业务 DB 操作     │    │  Observer 回调触发      │  │
│  │  INSERT/UPDATE   │    │  AfterCreate/Update    │  │
│  │  store_product   │    │  AfterDelete           │  │
│  │  store_order     │    │                        │  │
│  └──────────────────┘    └───────────┬────────────┘  │
│                                      │               │
│                                      ▼               │
│                          ┌───────────────────────┐   │
│                          │  worker.WriteOutbox() │   │
│                          │                       │   │
│                          │  INSERT event_outbox  │   │
│                          │  status = 'WAIT'      │   │
│                          │  (与业务操作同事务)     │   │
│                          └───────────────────────┘   │
│                                      │               │
│                              事务提交 ✓               │
└──────────────────────────────────────┼───────────────┘
                                       │
                                 event_outbox
                                 status=WAIT
                                       │
                 ┌─────────────────────▼─────────────────────┐
                 │              Relay 模块                     │
                 │                                           │
                 │  ┌─ 通知驱动 ──────────────────────────┐  │
                 │  │ Redis SUBSCRIBE("outbox:new")       │  │
                 │  │   → 收到通知 → 立即 scanAndDeliver() │  │
                 │  └─────────────────────────────────────┘  │
                 │                                           │
                 │  ┌─ Ticker 兜底 (10s) ─────────────────┐  │
                 │  │ Ticker(10s) ──▶ scanAndDeliver()     │  │
                 │  └─────────────────────────────────────┘  │
                 │                                           │
                 │  ┌─ 租约回收 (30s) ────────────────────┐  │
                 │  │ reclaimLease()                       │  │
                 │  └─────────────────────────────────────┘  │
                 │                                           │
                 │  ┌─ 定期清理 (1h) ─────────────────────┐  │
                 │  │ cleanupSent()                        │  │
                 │  └─────────────────────────────────────┘  │
                 │                                           │
                 │  投递流程:                                │
                 │  原子抢占: UPDATE status='SENDING'         │
                 │           WHERE id=? AND status='WAIT'    │
                 │       │                                   │
                 │       ▼                                   │
                 │  Kafka Producer.Send()                    │
                 │  (topic + partition_key + headers)        │
                 │       │                                   │
                 │       ├──▶ 成功 → status='SENT'           │
                 │       │                                   │
                 │       └──▶ 失败 → retry_count < max?      │
                 │                  │是          │否          │
                 │                  ▼            ▼           │
                 │            回到 WAIT     event_dead_letter │
                 │           (retry+1)     (DEAD 状态)       │
                 └─────────────────────┬─────────────────────┘
                                       │
                                 Kafka Topic
                                 (shop-product-events
                                  或 shop-order-events)
                                       │
                 ┌─────────────────────▼─────────────────────┐
                 │           Worker Consumer                  │
                 │                                           │
                 │  每个 topic 独立 Consumer                  │
                 │  ConsumeClaim → tasks chan → Worker Pool  │
                 │  (订单: 20并发, 商品: 10并发)              │
                 └─────────────────────┬─────────────────────┘
                                       │
                                       ▼
                 ┌──────────────────────────────────────────┐
                 │              Handler                      │
                 │                                          │
                 │  1. 从 Headers 提取 event_id/type/       │
                 │     aggregate_id                         │
                 │  2. 幂等检查 (processed_events 表)        │
                 │     SELECT COUNT WHERE event_id=?        │
                 │  3. 解析 payload (JSON)                   │
                 │  4. 根据 event_type 执行业务逻辑           │
                 │  5. 写入幂等记录                           │
                 │     INSERT processed_events               │
                 │     (唯一约束防重复)                       │
                 └──────────────────────────────────────────┘
```

**说明**：完整的数据流从 HTTP 请求开始，经过 Controller → Service → GORM 事务。在事务中，Observer 回调自动写入 `event_outbox` 记录，保证业务数据与事件记录的原子性。事务提交后，`WriteOutbox` 通过 goroutine 异步执行 `Redis PUBLISH("outbox:new")` 通知 Relay。Relay 采用**通知驱动 + Ticker 兜底**双触发机制：Redis SUBSCRIBE 收到通知后立即扫描投递，Ticker 每 10 秒兜底扫描保证最终一致性。Worker Consumer 消费消息后交给 Handler 处理，Handler 通过双层幂等保障（查询 + 唯一约束）确保不重复处理。

---

## 五、Outbox 状态机流转图

### 5.1 主状态流转

```
    ┌──────────────┐
    │    WAIT      │  ◀── 初始状态（WriteOutbox 创建）
    └──────┬───────┘     ◀── 投递失败后重试（retry_count+1）
           │               ◀── 租约回收（sending_at 超时）
           │
     Relay 原子抢占
     UPDATE SET status='SENDING',
                sending_at=NOW()
     WHERE id=? AND status='WAIT'
           │
           ▼
    ┌──────────────┐
    │   SENDING    │  ← 正在投递到 Kafka
    └──────┬───────┘
           │
     ┌─────┴─────┐
     │           │
  Kafka 成功   Kafka 失败
     │           │
     ▼           ▼
┌────────┐  retry_count+1 >= max_retries ?
│  SENT  │     │否              │是
└────────┘     ▼                ▼
          ┌──────────┐   ┌──────────────┐
          │  回到     │   │    DEAD      │
          │  WAIT    │   │              │
          │(retry+1) │   │ 写入死信表    │
          └──────────┘   │event_dead_   │
                         │letter        │
                         └──────────────┘
```

### 5.2 租约回收机制

```
    SENDING 状态
        │
        │ sending_at < NOW() - leaseTimeout
        │ (默认 60s 未响应)
        │
        ▼
    ┌───────────────────────────────────────┐
    │  reclaimLease()                       │
    │                                       │
    │  UPDATE event_outbox                  │
    │  SET status='WAIT', sending_at=NULL   │
    │  WHERE status='SENDING'               │
    │    AND sending_at < threshold          │
    │                                       │
    │  → 多实例场景下，其他 Relay 可重新抢占  │
    └───────────────────────────────────────┘
```

### 5.3 定期清理

```
    ┌───────────────────────────────────────┐
    │  cleanupSent()  (每小时执行一次)       │
    │                                       │
    │  DELETE FROM event_outbox             │
    │  WHERE status='SENT'                  │
    │    AND created_at < NOW() - N天       │
    │  LIMIT 1000                           │
    │  (分批删除，避免锁表)                  │
    │                                       │
    │  默认清理 7 天前的 SENT 记录            │
    └───────────────────────────────────────┘
```

**说明**：Outbox 记录经历 WAIT → SENDING → SENT/DEAD 的状态流转。原子抢占保证多实例安全；租约回收防止进程崩溃导致的 SENDING 记录永久卡死；定期清理避免表膨胀。投递失败采用指数退避重试（1s → 2s → 4s → ... → 最大 30min），超过最大重试次数移入死信表。

---

## 六、Handler 注册与分发机制图

```
┌───────────────────────────────────────────────────────────────┐
│                        Registry                               │
│                                                               │
│  handlers: map[string]Handler                                 │
│                                                               │
│  ┌─────────────────────────┐  ┌────────────────────────────┐ │
│  │ "shop-product-events"   │  │ "shop-order-events"        │ │
│  │         │               │  │         │                  │ │
│  │         ▼               │  │         ▼                  │ │
│  │  ProductHandler         │  │  OrderHandler              │ │
│  │                         │  │                            │ │
│  │  处理事件:              │  │  处理事件:                  │ │
│  │  • product.created      │  │  • order.created           │ │
│  │  • product.updated      │  │  • order.paid              │ │
│  │  • product.deleted      │  │  • order.shipped           │ │
│  │                         │  │  • order.completed         │ │
│  │  当前逻辑:              │  │  • order.canceled          │ │
│  │  幂等检查 + 日志记录    │  │  • order.updated           │ │
│  │  (后续可扩展为更新 ES、 │  │                            │ │
│  │   刷新缓存等)           │  │  当前逻辑:                  │ │
│  │                         │  │  幂等检查 + 日志记录        │ │
│  └─────────────────────────┘  └────────────────────────────┘ │
└───────────────────────────────────────────────────────────────┘

注册流程 (workers/register.go):

  当前模式（ProcessManager）:
    RegisterAllToManager(pm)
      → pm.GetRegistry()
      → registry.Register("shop-product-events", ProductHandler)
      → registry.Register("shop-order-events", OrderHandler)
      → registry.Register("shop-delay-order-timeout", DelayOrderHandler)

  旧模式（兼容路径）:
    RegisterAll(engine)
      → engine.GetRegistry()
      → registry.Register("shop-product-events", ProductHandler)
      → registry.Register("shop-order-events", OrderHandler)

分发流程 (consumer.go/processEvent):
  消息到达 → registry.GetHandler(event.Topic) → Handler.Handle(ctx, event)
```

**说明**：Registry 是一个简单的 `map[string]Handler` 映射表，以 Kafka topic 为 key。消息到达时，根据 `event.Topic` 查找对应的 Handler 并调用其 `Handle()` 方法。新增事件类型只需实现 Handler 接口并在 `RegisterAllToManager()` 中注册即可。

---

## 七、Observer 回调触发图

```
┌──────────────────────────────────────────────────────────────────┐
│                    GORM 生命周期 (observer/plugin.go)             │
│                                                                  │
│  Create 操作:                                                    │
│    gorm:create → After(gorm:create) → ProductObserver.AfterCreate│
│                                      → OrderObserver.AfterCreate │
│                                                                  │
│  Update 操作:                                                    │
│    Before(gorm:update) → compute_dirty (计算变更字段)            │
│                        → BeforeUpdate observer                   │
│    gorm:update → After(gorm:update) → ProductObserver.AfterUpdate│
│                                      → OrderObserver.AfterUpdate │
│                                                                  │
│  Delete 操作:                                                    │
│    gorm:delete → After(gorm:delete) → ProductObserver.AfterDelete│
│                                      → OrderObserver.AfterDelete │
│                                                                  │
└────────────────────────────────────┬─────────────────────────────┘
                                     │
                              tx (同一个事务)
                                     │
                                     ▼
                   ┌──────────────────────────────────┐
                   │  worker.WriteOutbox(tx, ...)      │
                   │                                   │
                   │  参数:                            │
                   │  • tx:            当前 GORM 事务   │
                   │  • aggregateId:  业务实体 ID       │
                   │  • eventType:    事件类型字符串     │
                   │  • topic:        Kafka topic       │
                   │  • partitionKey: 分区键(保证有序)   │
                   │  • payload:      事件数据(JSON)     │
                   │                                   │
                   │  生成 UUID event_id               │
                   │  INSERT event_outbox              │
                   │  status = 'WAIT'                  │
                   │                                   │
                   │  go notifyRelay(event_id)         │  ← 异步 Redis PUBLISH
                   │  → PUBLISH("outbox:new", event_id)│    通知 Relay 立即扫描
                   └──────────────────────────────────┘
                                     │
                              与业务操作同事务提交
```

### Observer 注册清单

```
observers/register.go → RegisterAll(global.Db)
│
├── ProductObserver  → 观察表: store_product
│   ├── AfterCreate  → product.created  → topic: shop-product-events
│   ├── AfterUpdate  → product.updated  → topic: shop-product-events
│   └── AfterDelete  → product.deleted  → topic: shop-product-events
│
├── OrderObserver    → 观察表: store_order
│   ├── AfterCreate  → order.created   → topic: shop-order-events
│   └── AfterUpdate  → 根据 status 映射事件类型:
│       ├── status=0  → order.created
│       ├── status=1  → order.paid
│       ├── status=2  → order.shipped
│       ├── status=3  → order.completed
│       ├── status=-1 → order.canceled
│       └── 其他      → order.updated
│                       以上均 → topic: shop-order-events
│
├── UserObserver     → 观察表: user
└── SysUserObserver  → 观察表: sys_user
```

**说明**：Observer 通过 GORM Plugin 机制注册回调，在 `gorm:create`/`gorm:update`/`gorm:delete` 之后自动触发。Observer 回调接收同一个 `tx` 事务对象，调用 `WriteOutbox()` 将事件记录写入 `event_outbox` 表，保证业务数据与事件记录的原子性。事务提交后，`WriteOutbox` 通过 goroutine 异步执行 `Redis PUBLISH("outbox:new")` 通知 Relay 立即扫描投递。订单 Observer 根据 `status` 字段映射不同的事件类型。

---

## 八、配置结构图

### 8.1 config.yml 中的 worker 配置

```yaml
worker:
  enabled: true
  group-id: "shop-worker"

  # ===== 方式一：per-Handler 独立配置（推荐） =====
  handlers:
    - topic: "shop-order-events"
      concurrency: 20
      ordered: false           # 是否有序消费（同 key 同 worker）
      buffer-size: 2000
    - topic: "shop-product-events"
      concurrency: 10
      ordered: false
      buffer-size: 1000

  # ===== 方式二：全局配置（向后兼容，handlers 为空时生效） =====
  topics:
    - "shop-product-events"
    - "shop-order-events"
  concurrency: 10

  # 全局默认值（handlers 中未指定的字段使用此默认值）
  default-concurrency: 10
  default-buffer-size: 1000

  poll-interval: "2s"
  max-retries: 5
  retry-base-delay: "1s"
  relay-batch-size: 100
  relay-lease-timeout: "60s"
  handler-timeout: "30s"      # 单条消息处理超时时间
```

### 8.2 配置项层级与说明

```
WorkerConfig (conf/conf.go)
│
├── enabled: bool              → 是否启用 Worker 模块（总开关）
│
├── group-id: string           → Consumer Group ID（Kafka 消费组标识）
│
├── handlers: []HandlerConfig  → per-Handler 独立配置（推荐方式）
│   ├── [0] topic: string      → 监听的 Topic
│   │   [0] concurrency: int   → 该 Handler 的 Worker Pool 并发数
│   │   [0] ordered: bool      → 是否有序消费（同 key 同 worker，已实现）
│   │   │                         true: FNV-1a hash 路由，每个 Worker 独立 channel
│   │   │                         false: 共享 tasks channel，竞争消费
│   │   [0] buffer-size: int   → tasks/orderedCh channel 缓冲大小
│   └── [1] ...                → 更多 Handler 配置
│
├── default-concurrency: int   → 全局默认并发数（handlers 中未指定时使用）
│
├── default-buffer-size: int   → 全局默认缓冲大小（handlers 中未指定时使用）
│
├── topics: []string           → 旧配置：监听的 Topic 列表（向后兼容）
│                               └── handlers 为空时生效
│
├── concurrency: int           → 旧配置：Worker Pool 并发数（向后兼容）
│                               └── handlers 为空时生效
│
├── handler-timeout: string    → 单条消息处理超时时间
│                               └── 默认 30s，processEvent 中 context.WithTimeout
│
├── poll-interval: string      → Relay 主循环扫描间隔（已用于 Ticker 兜底计算）
│
├── max-retries: int           → 最大重试次数
│                               └── 超过后移入 event_dead_letter
│
├── retry-base-delay: string   → 重试基础延迟（指数退避）
│                               └── 实际延迟 = baseDelay × 2^retryCount
│                               └── 上限 30 分钟
│
├── relay-batch-size: int      → Relay 每次扫描最大记录数
│                               └── SELECT ... LIMIT batch_size
│
├── relay-lease-timeout: string → SENDING 状态租约超时
│                               └── 超时后 reclaimLease 重置为 WAIT
│
└── relay-cleanup-days: int    → 清理多少天前的 SENT 记录
                                └── cleanupSent 每小时执行，分批删除
```

### 8.3 配置降级优先级链

```
NewKafkaProcesses() 工厂函数中的配置降级逻辑（buildConsumerConfig）：

并发数 (Concurrency):
  Handler 配置值 → default-concurrency → 旧 concurrency → 常量默认值(1)

缓冲大小 (BufferSize):
  Handler 配置值 → default-buffer-size → 常量默认值(1000)

示例（当前 config.yml）：
  shop-order-events:   concurrency=20, buffer-size=2000  (显式配置)
  shop-product-events: concurrency=10, buffer-size=1000  (显式配置)
  若某 handler 未配置 concurrency，则使用 default-concurrency=10
  若 default-concurrency 也未配置，则回退到旧 concurrency=10
  若旧 concurrency 也未配置，则使用常量默认值 1
```

### 8.4 Relay 四个定时任务的间隔配置

```
Relay.Start() 中的实际配置:

┌─────────────────┬──────────────────┬──────────────────────────────────────┐
│ 定时任务         │ 间隔             │ 触发方式                              │
├─────────────────┼──────────────────┼──────────────────────────────────────┤
│ scan-deliver    │ 10s (Ticker 兜底) │ Redis 通知立即触发 + Ticker 10s 兜底 │
│ reclaim-lease   │ 30s (固定)        │ time.NewTicker(30s)                  │
│ cleanup-sent    │ 1h (固定)         │ time.NewTicker(1h)                   │
│ subscribe-notify│ 持续监听          │ Redis SUBSCRIBE 长连接                │
└─────────────────┴──────────────────┴──────────────────────────────────────┘
```

**说明**：配置通过 `mapstructure` 标签映射到 `WorkerConfig` 结构体。Relay 采用通知驱动 + Ticker 兜底模式，scanLoop 监听 Redis 通知 channel 和 10s Ticker，任一触发即执行 `scanAndDeliver()`。租约回收和定期清理使用硬编码间隔（30s 和 1h）。`handler-timeout` 控制 `processEvent` 中的 `context.WithTimeout` 超时时间。

---

## 九、文件依赖关系图

```
cmd/worker/main.go
│
├── conf/config.yml                              ← 配置定义
├── conf/conf.go (WorkerConfig + HandlerConfig)  ← 配置结构体
│   ├── WorkerConfig.handlers: []HandlerConfig   ← per-Handler 独立配置（新增）
│   ├── WorkerConfig.default-concurrency          ← 全局默认并发数（新增）
│   └── WorkerConfig.default-buffer-size          ← 全局默认缓冲大小（新增）
│
├── internal/bootstrap/bootstrap.go              ← 基础组件初始化
│   └── WithKafka()                              ← 选择性初始化 Kafka Sync Producer
│
├── internal/worker/process_manager.go           ← ProcessManager 编排层（当前模式核心）
│   ├── ProcessManager.Start()                   ← 串行启动所有 Process
│   ├── ProcessManager.Stop()                    ← 逆序关闭所有 Process
│   └── ProcessManager.GetRegistry()             ← 共享 Registry
│
├── internal/worker/kafka_process.go             ← Kafka Process 工厂（新增）
│   ├── NewKafkaProcesses()                      ← per-Handler 创建独立 Consumer Process
│   ├── buildConsumerConfig()                    ← 配置降级链（Handler级→Default级→旧全局级→常量）
│   └── internal/worker/consumer.go              ← 双缓冲消费者 + 有序消费 + 超时控制
│       ├── ConsumerConfig                       ← 单 Consumer 配置（含 Ordered/HandlerTimeout）
│       ├── NewConsumerWithConfig()              ← 新版 API
│       ├── routeKey()                           ← FNV-1a 哈希路由（有序模式）
│       ├── processEvent()                       ← 消息处理 + context.WithTimeout 超时控制
│       ├── internal/worker/handler.go           ← Event 结构体 + Handler 接口
│       ├── internal/worker/registry.go          ← Handler 注册表 (topic→Handler)
│       └── internal/worker/middleware.go        ← 中间件链 (Recovery/Logging/Timeout)
│
├── internal/worker/relay_process.go             ← Relay Process 工厂（新增）
│   ├── NewRelayProcess()                        ← ParseRelayConfig() → NewRelay() → Process
│   └── internal/worker/relay.go                 ← Outbox 扫描投递器 + Redis Pub/Sub 通知
│       ├── subscribeNotify()                    ← Redis SUBSCRIBE 监听
│       ├── scanLoop()                           ← 通知 + Ticker 双触发扫描循环
│       ├── internal/models/event_outbox.go      ← EventOutbox 模型 + 状态常量
│       ├── internal/models/event_dead_letter.go ← EventDeadLetter 模型
│       └── pkg/mq (KafkaSyncProducer)          ← Kafka 生产者（投递用）
│
├── internal/worker/scheduler_process.go         ← DelayScheduler Process 工厂（新增）
│   ├── NewSchedulerProcess()                    ← Redis 可用? → NewDelayScheduler() → Process
│   └── internal/worker/delay_scheduler.go       ← Redis Sorted Set 延迟调度器
│
├── internal/worker/process.go                   ← Process 结构体 + 状态机
├── internal/worker/runnable.go                  ← Runnable 通用任务接口
├── internal/worker/outbox.go                    ← WriteOutbox 工具函数 + Redis 通知
│   └── notifyRelay()                            ← PUBLISH("outbox:new") 异步通知 Relay
│
├── internal/worker/engine.go                    ← Engine（旧模式，兼容路径）
│
├── internal/workers/register.go                 ← RegisterAllToManager 注册入口
│   ├── internal/workers/product_handler.go      ← ProductHandler 实现
│   ├── internal/workers/order_handler.go        ← OrderHandler 实现
│   ├── internal/workers/delay_order_handler.go  ← DelayOrderHandler 实现
│   └── internal/models/processed_event.go       ← ProcessedEvent 幂等模型
│
└── pkg/global/global.go                         ← 全局变量 (Db, LOG, CONFIG, RedisClient)
```

### Observer 侧依赖（事件产生端）

```
internal/observers/register.go                   ← Observer 注册入口
│
├── internal/observers/product_observer.go
│   └── internal/worker/outbox.go (WriteOutbox)  ← 写入 Outbox
│
├── internal/observers/order_observer.go
│   └── internal/worker/outbox.go (WriteOutbox)  ← 写入 Outbox
│
├── internal/observer/plugin.go                  ← GORM Plugin 桥接
│   └── internal/observer/registry.go            ← Observer 注册表
│
└── 通过 db.Use(observer.NewPlugin(registry)) 挂载到 GORM
```

**说明**：Worker 模块涉及的核心文件分为框架层（`internal/worker/`）和业务层（`internal/workers/`）。框架层提供通用的消费、注册、中间件、Relay 等能力；业务层实现具体的事件处理逻辑。Observer 侧通过 `WriteOutbox()` 函数与 Worker 框架层耦合，保证事件记录与业务操作同事务。

---

## 十、优雅关闭流程图

```
收到 SIGINT / SIGTERM
        │
        ▼
signal.NotifyContext 触发 ctx.Done()
        │
        ▼
pm.Stop()  ← ProcessManager 逆序关闭所有 Process
        │
        ├──▶ 1. DelayScheduler.Stop()  (最后添加的 Process，最先关闭)
        │       ├── cancel() → scheduleLoop 退出
        │       └── Ticker 停止
        │
        ├──▶ 2. Relay.Stop()
        │       ├── cancel() → relay ctx.Done()
        │       ├── subscribeNotify 退出（Redis SUBSCRIBE 连接关闭）
        │       ├── scanLoop 退出（Ticker + notify channel）
        │       ├── reclaim-lease ticker 退出
        │       └── cleanup-sent ticker 退出
        │
        ├──▶ 3. Consumer B.Stop()  (商品)
        │       ├── (a) cancel() → consumeLoop 退出
        │       ├── (b) consumeWg.Wait() → 循环 goroutine 退出
        │       ├── (c) 关闭 channel（无序: tasks / 有序: orderedChs）
        │       ├── (d) workerWg.Wait() → 处理完剩余消息
        │       └── (e) ConsumerGroup.Close() → 关闭 sarama 连接（Consumer 自管理）
        │
        ├──▶ 4. Consumer A.Stop()  (订单)
        │       ├── (a) cancel() → consumeLoop 退出
        │       ├── (b) consumeWg.Wait()
        │       ├── (c) 关闭 channel（无序: tasks / 有序: orderedChs）
        │       ├── (d) workerWg.Wait()
        │       └── (e) ConsumerGroup.Close() → 关闭 sarama 连接（Consumer 自管理）
        │
        └──▶ 5. pm.cancel()
                → 取消 ProcessManager 级别的 context
                │
                ▼
        bootstrap.Shutdown()
                │
                ├── Kafka Producer.Close()
                ├── Redis.Close()
                ├── MySQL CloseMysqlClient()
                └── logging.SyncAll()  (刷新日志缓冲)
                        │
                        ▼
                  程序退出 ✓
```

**说明**：ProcessManager 模式下，关闭流程由 `pm.Stop()` 统一驱动，按 Process 注册的**逆序**逐个调用 `Runner.Stop()`。每个 Consumer 的 `Stop()` 内部自行关闭 ConsumerGroup 连接（无需 Engine 层单独管理）。关闭顺序：DelayScheduler → Relay → Kafka Consumer（每个 Consumer 内部：cancel → consumeWg.Wait → close channel → workerWg.Wait → ConsumerGroup.Close）。每个 Consumer 的关闭流程根据模式不同关闭不同的 channel：无序模式关闭 `tasks`，有序模式关闭所有 `orderedChs[0..N]`。

> **旧模式（兼容路径）**：`engine.Stop()` 的关闭顺序为 Relay → DelayScheduler → ProcessManager → cancel → Runners → ConsumerGroup.Close。

---

## 十一、架构演进与可优化点

### 11.1 架构演进：从全局共享到 per-Handler 独立

```
┌──────────────────────────────────────────────────────────────┐
│                                                              │
│  ✅ 已完成：per-Handler 独立 Consumer 架构                      │
│                                                              │
│  旧模式（全局共享，已淘汰）：                                  │
│  shop-order-events ──┐                                    │
│                      ├─→ tasks (1000) → Worker Pool (10)    │
│  shop-product-events ─┘                                    │
│                                                              │
│  新模式（per-Handler 独立，当前实现）：                        │
│  shop-order-events   → tasks A (2000) → Pool A (20)         │
│                                         └→ OrderHandler      │
│                                                              │
│  shop-product-events → tasks B (1000) → Pool B (10)         │
│                                         └→ ProductHandler    │
│                                                              │
│  已解决的局限：                                              │
│  ✅ 每个 topic 独立并发数，按需分配                           │
│  ✅ 资源隔离，慢处理不影响其他 topic                          │
│  ✅ 独立监控每个 topic 的消费延迟和处理速率                    │
│  ✅ 可以为不同 topic 设置不同的背压阈值                       │
│                                                              │
│  向后兼容保障：                                              │
│  • 保留旧版 topics + concurrency 配置，handlers 为空时自动回退  │
│  • 保留旧版 NewConsumer() API，内部委托给 NewConsumerWithConfig()│
│  • 配置降级链：Handler级 → Default级 → 旧全局级 → 常量默认值    │
└──────────────────────────────────────────────────────────────┘
```

### 11.2 已完成的优化项

```
┌──────────────────────────────────────────────────────────────┐
│  ✅ 已完成：per-Handler 独立 Consumer 架构                      │
│  ✅ 已完成：有序消费 (Key-Based Routing, FNV-1a hash)           │
│  ✅ 已完成：消息处理超时控制 (context.WithTimeout)              │
│  ✅ 已完成：Relay 通知驱动优化 (Redis Pub/Sub + Ticker 兜底)  │
│  ✅ 已完成：Middleware 链 (Recovery + Logging)                  │
│  ✅ 已完成：统一消息模型 Message (Event→Message 重构)           │
│  ✅ 已完成：QueueConsumer/QueueProducer 抽象接口               │
│  ✅ 已完成：RedisStreamConsumer 双缓冲架构                     │
│  ✅ 已完成：Process/ProcessManager 异构进程编排                │
│  ✅ 已完成：DelayScheduler 延迟调度器 (Redis Sorted Set)       │
│  ✅ 已完成：多队列 Relay 投递 (Kafka + Redis)                  │
│  ✅ 已完成：DelayQueue 延迟队列写入接口                        │
│  ✅ 已完成：DelayOrderHandler 延迟订单处理器                   │
│  ✅ 已完成：DLQ 运维 CLI (list/retry/discard/clean/stats)      │
└──────────────────────────────────────────────────────────────┘
```

### 11.3 未来演进方向

```
┌──────────────────────────────────────────────────────────────┐
│  1. Offset 提交策略优化                                        │
│     当前: ConsumeClaim 中先 MarkMessage 再发送到 tasks        │
│     风险: Worker 处理失败时 offset 已提交，消息丢失            │
│     方向: 改为 Handler 处理成功后才 MarkMessage               │
│                                                              │
│  2. 监控指标                                                  │
│     当前: 仅日志输出                                          │
│     方向: 接入 Prometheus，暴露消费速率/延迟/错误率/队列深度   │
│                                                              │
│  3. 死信管理                                                  │
│     当前: 死信写入 event_dead_letter 表                       │
│     方向: 提供 CLI/Web UI 查看死信列表、手动重试               │
│                                                              │
│  4. 多 Handler 扩展                                          │
│     当前: 单 Handler 模型（每个 Topic 对应一个 Handler）       │
│     方向: Registry 中 handlers map 改为 []Handler，            │
│           支持一个 Topic 对应多个 Handler 并行处理              │
│                                                              │
│  5. 优先级队列支持                                            │
│     当前: 所有事件平等处理                                    │
│     方向: 核心事件（支付/订单）优先于低优先级事件（商品更新） │
└──────────────────────────────────────────────────────────────┘
```

**说明**：per-Handler 独立 Consumer 架构、有序消费、超时控制、Relay 通知驱动优化、多队列架构（ProcessManager + Redis Stream + DelayScheduler）均已完成。架构已具备完善的消息处理保障机制，未来可根据业务需求在 Offset 提交策略、监控指标、死信管理等方面继续演进。

---

## 十二、多队列架构拓扑图

### 12.1 多队列整体拓扑

```
┌─────────────────────────────────────────────────────────────────────────┐
│                        多队列 Worker 架构拓扑                            │
│                                                                         │
│  ┌──────────────────────────────────────────────────────────────────┐   │
│  │                    业务层 (HTTP Server / Cron)                    │   │
│  │                                                                  │   │
│  │  OrderService.Create()         DelayQueue.Push()                │   │
│  │       │                              │                          │   │
│  │       ▼                              ▼                          │   │
│  │  WriteKafkaOutbox()            WriteDelayOutbox()               │   │
│  │       │                              │                          │   │
│  │       ▼                              ▼                          │   │
│  │  ┌─────────────────────────────────────────────┐               │   │
│  │  │         event_outbox 表                      │               │   │
│  │  │  queue_type=kafka  queue_type=redis          │               │   │
│  │  │  execute_at=NULL   execute_at=未来时间        │               │   │
│  │  └─────────────────────────────────────────────┘               │   │
│  └──────────────────────────┬───────────────────────────────────────┘   │
│                             │                                          │
│                             ▼                                          │
│  ┌──────────────────────────────────────────────────────────────────┐   │
│  │                    Worker 进程 (cmd/worker)                       │   │
│  │                                                                  │   │
│  │  ┌─────────────────┐    ┌─────────────────────┐                 │   │
│  │  │   Relay 模块     │    │  DelayScheduler     │                 │   │
│  │  │  (通知驱动+      │    │  (Redis Sorted Set)  │                 │   │
│  │  │   Ticker 兜底)   │    │  1s 轮询精度         │                 │   │
│  │  └────────┬────────┘    └──────────┬──────────┘                 │   │
│  │           │                        │                             │   │
│  │     按 queue_type 分发        ZRANGEBYSCORE                     │   │
│  │      ┌──────┴──────┐          → XADD                            │   │
│  │      ▼             ▼             │                              │   │
│  │  ┌────────┐  ┌──────────┐        │                              │   │
│  │  │ Kafka  │  │  Redis   │◀───────┘                              │   │
│  │  │Producer│  │  XADD    │                                       │   │
│  │  └───┬────┘  └────┬─────┘                                       │   │
│  │      │            │                                              │   │
│  └──────┼────────────┼──────────────────────────────────────────────┘   │
│         │            │                                                  │
│         ▼            ▼                                                  │
│  ┌────────────┐ ┌────────────────┐                                     │
│  │   Kafka    │ │  Redis Stream  │                                     │
│  │  Cluster   │ │  (延迟队列)    │                                     │
│  └──────┬─────┘ └───────┬────────┘                                     │
│         │               │                                               │
│         ▼               ▼                                               │
│  ┌──────────────┐ ┌──────────────────┐                                 │
│  │   Kafka      │ │ RedisStream      │                                 │
│  │   Consumer   │ │ Consumer         │                                 │
│  │ (双缓冲)     │ │ (双缓冲)         │                                 │
│  └──────┬───────┘ └───────┬──────────┘                                 │
│         │                 │                                              │
│         └────────┬────────┘                                              │
│                  ▼                                                       │
│         ┌────────────────┐                                               │
│         │ Handler        │                                               │
│         │ Registry       │                                               │
│         │ (共享)         │                                               │
│         └────────┬───────┘                                               │
│                  │                                                       │
│      ┌───────────┼───────────┐                                          │
│      ▼           ▼           ▼                                          │
│ OrderHandler  ProductH.   DelayOrderH.                                  │
│                                                                         │
│  ┌──────────────────────────────────────────────────────────────────┐   │
│  │              ProcessManager                                      │   │
│  │  管理 Kafka Consumer (直接) + Redis Consumer (通过 Process)      │   │
│  │  串行启动 / 逆序关闭 / 失败回滚                                   │   │
│  └──────────────────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────────────────┘
```

### 12.2 延迟消息完整流转图

```
┌──────────────────────────────────────────────────────────────────────┐
│                     延迟消息全链路流转                                 │
│                                                                      │
│  1. 业务写入                                                         │
│     OrderService.CreateOrder()                                       │
│       └── DelayQueue.Push(tx, orderId, "order.timeout",             │
│              "delay-order-timeout", 30*time.Minute, payload)         │
│             │                                                        │
│             ▼                                                        │
│       WriteDelayOutbox(tx, ...)                                      │
│             │                                                        │
│             ├── INSERT event_outbox                                  │
│             │   (queue_type=redis, execute_at=NOW()+30min)           │
│             │                                                        │
│             └── go ScheduleDelay(redis, eventId, executeAt)          │
│                 └── ZADD delay:schedule {score=executeAt_ms}         │
│                                                                      │
│  2. 主调度路径（DelayScheduler, 毫秒级精度）                          │
│     scheduleLoop (Ticker 1s)                                         │
│       └── ZRANGEBYSCORE delay:schedule -inf {now_ms} LIMIT 0 50     │
│           └── 对每个到期的 event_id:                                  │
│               ├── 查询 event_outbox (status=WAIT)                    │
│               ├── 原子抢占: UPDATE status=SENDING                    │
│               ├── XADD {topic} → Redis Stream                       │
│               ├── UPDATE status=SENT                                 │
│               └── ZREM delay:schedule {event_id}                    │
│                                                                      │
│  3. 兜底路径（Relay DB 轮询, 10s 精度）                               │
│     scanAndDeliverByType("redis", false)                             │
│       └── WHERE status=WAIT AND queue_type=redis                     │
│             AND execute_at <= NOW()                                  │
│           └── deliverOne() → deliverToRedis()                        │
│               ├── 原子抢占: UPDATE status=SENDING                    │
│               ├── XADD {topic} → Redis Stream                       │
│               ├── UPDATE status=SENT                                 │
│               └── ZREM delay:schedule {event_id} (清理残留)          │
│                                                                      │
│  4. 消费路径                                                         │
│     RedisStreamConsumer.redisConsumeLoop()                           │
│       └── XReadGroup → 构造 Message → tasks channel                 │
│           └── Worker Pool → processRedisEvent()                      │
│               └── Handler.Handle(ctx, msg) → XACK                   │
│                                                                      │
│  5. 降级保障                                                         │
│     Redis 不可用 → ScheduleDelay 返回 nil                            │
│                  → 完全依赖 DB Relay 兜底（10s 精度）                 │
│     消息不丢失 → 所有延迟消息持久化在 event_outbox 表中               │
└──────────────────────────────────────────────────────────────────────┘
```

### 12.3 ProcessManager 组件关系图

```
┌──────────────────────────────────────────────────────────────────────┐
│                    ProcessManager 统一管理模式（当前）                  │
│                                                                      │
│  cmd/worker/main.go                                                  │
│   └── ProcessManager                                                 │
│        ├── processes []*Process                                      │
│        │    │                                                        │
│        │    ├── Process{kafka-order-events, kafka, Consumer}         │
│        │    │    └── Consumer.Stop() 内部自行关闭 ConsumerGroup       │
│        │    │                                                        │
│        │    ├── Process{kafka-product-events, kafka, Consumer}       │
│        │    │    └── Consumer.Stop() 内部自行关闭 ConsumerGroup       │
│        │    │                                                        │
│        │    ├── Process{relay, relay, Relay}                         │
│        │    │                                                        │
│        │    └── Process{delay-scheduler, scheduler, DelayScheduler}  │
│        │                                                              │
│        └── registry *Registry (共享)                                  │
│             ├── "shop-order-events" → OrderHandler                   │
│             ├── "shop-product-events" → ProductHandler               │
│             └── "shop-delay-order-timeout" → DelayOrderHandler       │
│                                                                      │
│  Runnable 接口（所有 Process.Runner 实现）                            │
│   ├── Consumer (Kafka)        ← consumer.go                         │
│   ├── Relay                   ← relay.go                            │
│   ├── DelayScheduler          ← delay_scheduler.go                  │
│   └── RedisStreamConsumer     ← redis_consumer.go                   │
│                                                                      │
│  工厂函数（创建 Process）:                                            │
│   ├── NewKafkaProcesses()     ← kafka_process.go                    │
│   ├── NewRelayProcess()       ← relay_process.go                    │
│   └── NewSchedulerProcess()   ← scheduler_process.go                │
│                                                                      │
│  启动顺序（pm.Start() 串行）:                                         │
│   1. Kafka Consumer.Start(ctx)                                       │
│   2. Relay.Start(ctx)                                                │
│   3. DelayScheduler.Start(ctx) (Redis 可用时)                        │
│                                                                      │
│  关闭顺序（pm.Stop() 逆序）:                                          │
│   1. DelayScheduler.Stop()                                           │
│   2. Relay.Stop()                                                    │
│   3. Consumer.Stop() (每个内部自行关闭 ConsumerGroup)                 │
│   4. pm.cancel()                                                     │
└──────────────────────────────────────────────────────────────────────┘

┌──────────────────────────────────────────────────────────────────────┐
│                    Engine 组件关系（旧模式，兼容路径）                  │
│                                                                      │
│  Engine                                                              │
│   ├── consumers []QueueConsumer   ← Kafka Consumer 直接管理          │
│   │    ├── Consumer(order-events)  实现 QueueConsumer 接口            │
│   │    └── Consumer(product-events)                                  │
│   │                                                                  │
│   ├── processManager *ProcessManager  ← 异构 Process 管理            │
│   │    └── processes []*Process                                      │
│   │         └── Process{redis-delay, redis, RedisStreamConsumer}     │
│   │                                                                  │
│   ├── delayScheduler *DelayScheduler  ← Redis Sorted Set 调度        │
│   ├── relay *Relay                    ← 多队列 Outbox 投递           │
│   └── registry *Registry              ← Handler 注册表（共享）       │
│                                                                      │
│  启动顺序:                                                           │
│   1. Kafka Consumer.Start() (遍历 runners)                           │
│   2. Relay.Start()                                                   │
│   3. DelayScheduler.Start() (Redis 可用时)                           │
│   4. processManager.Start() (串行启动注册的 Process)                 │
│                                                                      │
│  关闭顺序（逆序）:                                                    │
│   1. Relay.Stop()                                                    │
│   2. DelayScheduler.Stop()                                           │
│   3. processManager.Stop()                                           │
│   4. engine.cancel()                                                 │
│   5. Consumer.Stop() (遍历 runners)                                  │
│   6. ConsumerGroup.Close() (Engine 层管理)                           │
└──────────────────────────────────────────────────────────────────────┘
```

### 12.4 多队列 Outbox 状态机流转图

```
┌──────────────────────────────────────────────────────────────────────┐
│                    多队列 Outbox 状态机                                │
│                                                                      │
│  ┌──────────────┐                                                    │
│  │    WAIT      │  ◀── 初始状态                                      │
│  │              │  ◀── 投递失败后重试（retry_count+1）                │
│  │ queue_type:  │  ◀── 租约回收（sending_at 超时）                    │
│  │ kafka/redis  │                                                    │
│  │ execute_at:  │                                                    │
│  │ NULL/未来时间│                                                    │
│  └──────┬───────┘                                                    │
│         │                                                            │
│   Relay/DelayScheduler 原子抢占                                      │
│   UPDATE SET status='SENDING', sending_at=NOW()                      │
│   WHERE id=? AND status='WAIT'                                       │
│         │                                                            │
│         ▼                                                            │
│  ┌──────────────┐                                                    │
│  │   SENDING    │                                                    │
│  └──────┬───────┘                                                    │
│         │                                                            │
│   ┌─────┴──────┐                                                    │
│   │            │                                                    │
│  成功         失败                                                   │
│   │            │                                                    │
│   ▼            ▼                                                    │
│ ┌────────┐  retry_count+1 >= max_retries ?                          │
│ │  SENT  │     │否              │是                                  │
│ └────────┘     ▼                ▼                                   │
│           ┌──────────┐   ┌──────────────┐                           │
│           │  回到     │   │    DEAD      │                           │
│           │  WAIT    │   │              │                           │
│           │(retry+1) │   │ 写入死信表    │                           │
│           └──────────┘   │event_dead_   │                           │
│                          │letter        │                           │
│                          │queue_type:   │                           │
│                          │kafka/redis   │                           │
│                          └──────────────┘                           │
│                                                                      │
│  投递路径分支:                                                       │
│  queue_type=kafka  → deliverToKafka() → Kafka Producer.Send()       │
│  queue_type=redis  → deliverToRedis() → Redis XADD                  │
│                                                                      │
│  抢占协调:                                                           │
│  DelayScheduler 与 Relay 通过原子抢占保证消息不被重复投递            │
│  RowsAffected == 0 表示已被另一方抢占，自动跳过                     │
└──────────────────────────────────────────────────────────────────────┘
```
