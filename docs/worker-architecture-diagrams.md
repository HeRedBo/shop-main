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

**说明**：四个入口编译为独立二进制，共享 `internal/` 和 `pkg/` 下的所有代码。Worker 进程采用选择性 Bootstrap（`WithKafkaConsumer()`），不加载 HTTP Server、Casbin、Observer 等不需要的组件，减少资源占用。

---

## 二、Worker 进程内部架构

### 2.1 启动流程

```
cmd/worker/main.go
│
├── 1. BootstrapWith(configPath, WithKafkaConsumer())
│       → 配置 → 日志 → Redis → MySQL → KafkaConsumerGroup
│
├── 2. engine := worker.NewEngine(CONFIG.Worker)
│       → 创建 Engine + Registry + Context
│
├── 3. workers.RegisterAll(engine)
│       → 按 topic 注册 ProductHandler / OrderHandler
│
├── 4. engine.Start()
│       → 创建 sarama ConsumerGroup
│       → 创建 Consumer（启动 Worker Pool + consumeLoop）
│       → 创建并启动 Relay（3 个 Ticker 循环）
│
├── 5. signal.NotifyContext → 阻塞等待 SIGINT/SIGTERM
│
├── 6. engine.Stop()
│       → 有序关闭 Relay → Consumer → ConsumerGroup
│
└── 7. bootstrap.Shutdown()
        → 关闭 Kafka → Redis → MySQL → 刷新日志
```

### 2.2 Engine 组件关系

```
┌─────────────────────────────────────────────────────────────┐
│                       Engine                                 │
│  (管理所有 Worker 组件的生命周期)                              │
│                                                             │
│  ┌─────────────────┐  ┌─────────────────┐  ┌─────────────┐ │
│  │     Relay       │  │    Consumer     │  │   Registry  │ │
│  │                 │  │                 │  │             │ │
│  │  3 个 Ticker:   │  │  双缓冲架构:    │  │  topic →    │ │
│  │  • scan-deliver │  │  • Poller       │  │  Handler    │ │
│  │  • reclaim-lease│  │  • tasks chan   │  │  映射表     │ │
│  │  • cleanup-sent │  │  • Worker Pool  │  │             │ │
│  └─────────────────┘  └─────────────────┘  └─────────────┘ │
│                                                             │
│  config: WorkerConfig                                       │
│  ctx/cancel: 全局上下文控制                                   │
│  mu + started: 防重入锁                                      │
└─────────────────────────────────────────────────────────────┘
```

**说明**：Engine 是 Worker 进程的核心，管理 Relay（Outbox 扫描投递）、Consumer（Kafka 消费）和 Registry（Handler 注册表）三个核心组件的创建、启动和关闭。

---

## 三、双缓冲消费者架构（核心）

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
              │   tasks channel     │  ← chan *Event, 缓冲 1000
              │   (背压机制)         │    满时阻塞 ConsumeClaim
              └──────────┬──────────┘    不会丢失消息
                         │
          ┌──────────────┼──────────────┐
          ▼              ▼              ▼
    ┌──────────┐  ┌──────────┐  ┌──────────┐
    │Worker-0  │  │Worker-1  │  │Worker-N  │  ← Worker Pool
    │          │  │          │  │          │    concurrency=10
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
    │  3. wrappedHandler.Handle(ctx,event) │  ← 执行处理
    └──────────────────────────────────────┘
                         │
                         ▼
    ┌──────────────────────────────────────┐
    │         Middleware Chain             │
    │                                      │
    │  RecoveryMiddleware                  │  ← panic 恢复 (TODO)
    │    → LoggingMiddleware               │  ← 日志记录 (TODO)
    │      → Handler.Handle()              │  ← 实际业务处理
    └──────────────────────────────────────┘
```

**说明**：双缓冲架构将消息拉取（ConsumeClaim/Poller）和消息处理（Worker Pool）分离为独立的 goroutine 池。慢处理不会阻塞 Kafka 拉取，避免 rebalance。`tasks` channel 设置有界缓冲（1000），满时自动背压，天然限流。每个 Worker goroutine 顶层有 `defer recover()` 防止 panic 导致 goroutine 泄漏。

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
                 │  Ticker(2s) ──▶ scanAndDeliver()          │
                 │       │                                   │
                 │       ▼                                   │
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
                 │  ConsumeClaim → tasks chan → Worker Pool  │
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

**说明**：完整的数据流从 HTTP 请求开始，经过 Controller → Service → GORM 事务。在事务中，Observer 回调自动写入 `event_outbox` 记录，保证业务数据与事件记录的原子性。Relay 定时扫描 outbox 表投递到 Kafka，Worker Consumer 消费消息后交给 Handler 处理，Handler 通过双层幂等保障（查询 + 唯一约束）确保不重复处理。

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
  RegisterAll(engine)
    → engine.GetRegistry()
    → registry.Register("shop-product-events", ProductHandler)
    → registry.Register("shop-order-events", OrderHandler)

分发流程 (consumer.go/processEvent):
  消息到达 → registry.GetHandler(event.Topic) → Handler.Handle(ctx, event)
```

**说明**：Registry 是一个简单的 `map[string]Handler` 映射表，以 Kafka topic 为 key。消息到达时，根据 `event.Topic` 查找对应的 Handler 并调用其 `Handle()` 方法。新增事件类型只需实现 Handler 接口并在 `RegisterAll()` 中注册即可。

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

**说明**：Observer 通过 GORM Plugin 机制注册回调，在 `gorm:create`/`gorm:update`/`gorm:delete` 之后自动触发。Observer 回调接收同一个 `tx` 事务对象，调用 `WriteOutbox()` 将事件记录写入 `event_outbox` 表，保证业务数据与事件记录的原子性。订单 Observer 根据 `status` 字段映射不同的事件类型。

---

## 八、配置结构图

### 8.1 config.yml 中的 worker 配置

```yaml
kafka:
  hosts: ["127.0.0.1:9092"]

worker:
  enabled: true
  group-id: "shop-worker"
  topics:
    - "shop-product-events"
    - "shop-order-events"
  concurrency: 10
  poll-interval: "2s"
  max-retries: 5
  retry-base-delay: "1s"
  relay-batch-size: 100
  relay-lease-timeout: "60s"
```

### 8.2 配置项层级与说明

```
WorkerConfig (conf/conf.go)
│
├── enabled: bool              → 是否启用 Worker 模块（总开关）
│
├── group-id: string           → Consumer Group ID（Kafka 消费组标识）
│
├── topics: []string           → 监听的 Topic 列表
│                               └── 对应 Registry 中的 Handler
│
├── concurrency: int           → Worker Pool 并发数（全局共享）
│                               └── 即从 tasks channel 读取的 goroutine 数量
│
├── poll-interval: string      → Relay 主循环扫描间隔
│                               └── scanAndDeliver 的 Ticker 周期
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

### 8.3 Relay 三个定时任务的间隔计算

```
parseRelayConfig() 解析后的实际值:

┌─────────────────┬──────────────────┬──────────────────────────────┐
│ 定时任务         │ 间隔             │ 计算逻辑                      │
├─────────────────┼──────────────────┼──────────────────────────────┤
│ scan-deliver    │ 2s               │ = poll-interval               │
│ reclaim-lease   │ max(2s×2, 10s)   │ = poll-interval × 2, 最小 10s │
│                 │ = 10s            │                              │
│ cleanup-sent    │ 1h (固定)         │ 硬编码 1 小时                  │
└─────────────────┴──────────────────┴──────────────────────────────┘
```

**说明**：配置通过 `mapstructure` 标签映射到 `WorkerConfig` 结构体。`parseRelayConfig()` 将字符串类型的时间配置解析为 `time.Duration`，并为每个参数设置合理的默认值。

---

## 九、文件依赖关系图

```
cmd/worker/main.go
│
├── conf/config.yml                              ← 配置定义
├── conf/conf.go (WorkerConfig)                  ← 配置结构体
│
├── internal/bootstrap/bootstrap.go              ← 基础组件初始化
│   └── WithKafkaConsumer()                      ← 选择性初始化 Kafka ConsumerGroup
│
├── internal/worker/engine.go                    ← Engine 生命周期管理
│   ├── internal/worker/consumer.go              ← 双缓冲消费者
│   │   ├── internal/worker/handler.go           ← Event 结构体 + Handler 接口
│   │   ├── internal/worker/registry.go          ← Handler 注册表 (topic→Handler)
│   │   └── internal/worker/middleware.go        ← 中间件链 (Recovery/Logging/Timeout)
│   │
│   ├── internal/worker/relay.go                 ← Outbox 扫描投递器
│   │   ├── internal/models/event_outbox.go      ← EventOutbox 模型 + 状态常量
│   │   ├── internal/models/event_dead_letter.go ← EventDeadLetter 模型
│   │   └── pkg/mq (KafkaSyncProducer)          ← Kafka 生产者（投递用）
│   │
│   └── internal/worker/outbox.go                ← WriteOutbox 工具函数
│       └── internal/models/event_outbox.go      ← EventOutbox 模型
│
├── internal/workers/register.go                 ← RegisterAll 注册入口
│   ├── internal/workers/product_handler.go      ← ProductHandler 实现
│   ├── internal/workers/order_handler.go        ← OrderHandler 实现
│   └── internal/models/processed_event.go       ← ProcessedEvent 幂等模型
│
└── pkg/global/global.go                         ← 全局变量 (Db, LOG, CONFIG, KafkaConsumerGroup)
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
engine.Stop()
        │
        ├──▶ 1. relay.Stop()
        │       ├── cancel() → relay ctx.Done()
        │       ├── scan-deliver ticker 退出
        │       ├── reclaim-lease ticker 退出
        │       └── cleanup-sent ticker 退出
        │
        ├──▶ 2. consumer.Stop()
        │       │
        │       ├── (a) cancel() → consumer ctx.Done()
        │       │       → consumeLoop 退出循环
        │       │       → ConsumeClaim 不再写入 tasks
        │       │
        │       ├── (b) consumeWg.Wait()
        │       │       → 等待 consumeLoop goroutine 完全退出
        │       │
        │       ├── (c) close(tasks)
        │       │       → 通知 Worker Pool 不再有new消息
        │       │
        │       └── (d) workerWg.Wait()
        │               → 每个 Worker 处理完 range tasks 中剩余消息
        │               → 所有 Worker goroutine 退出
        │
        ├──▶ 3. engine.cancel()
        │       → 取消 Engine 级别的 context
        │
        └──▶ 4. consumer.group.Close()
                → 关闭 sarama ConsumerGroup 底层连接
                │
                ▼
        bootstrap.Shutdown()
                │
                ├── KafkaConsumerGroup.Close()  (兜底关闭)
                ├── Kafka Producer.Close()
                ├── Redis.Close()
                ├── MySQL CloseMysqlClient()
                └── logging.SyncAll()  (刷新日志缓冲)
                        │
                        ▼
                  程序退出 ✓
```

**说明**：关闭流程严格保证有序：先停 Relay（停止产生新消息），再停 Consumer（停拉取 → 等循环退出 → 关 channel → 等 Worker 处理完剩余消息），最后关闭底层连接。`close(tasks)` 后 Worker 通过 `for event := range tasks` 自然退出，确保不丢失已拉取的消息。

---

## 十一、当前架构的局限与演进方向

### 11.1 当前：全局共享 Worker Pool

```
┌──────────────────────────────────────────────────────────────┐
│                                                              │
│  shop-product-events ──┐                                    │
│                        ├─→ tasks (1000) ─→ Worker Pool (10) │
│  shop-order-events ────┘                                    │
│                                                              │
│  特点:                                                      │
│  • 所有 topic 共享同一个 tasks channel                       │
│  • 所有 topic 共享同一个 Worker Pool (concurrency=10)        │
│  • Registry 按 topic 分发到不同 Handler                      │
│                                                              │
│  局限:                                                      │
│  1. 无法为不同 topic 设置不同并发数                           │
│     (商品事件可能需要 5 并发，订单事件可能需要 20 并发)        │
│  2. 慢处理可能影响其他 topic                                 │
│     (订单处理超时 → 占满 Worker → 商品消息积压)              │
│  3. 无法独立监控每个 topic 的消费延迟和处理速率               │
│  4. tasks channel 满时所有 topic 的 Poller 都被阻塞          │
└──────────────────────────────────────────────────────────────┘
```

### 11.2 未来演进：per-Handler 独立 Pool

```
┌──────────────────────────────────────────────────────────────┐
│                                                              │
│  shop-product-events → tasks A (500)  → Pool A (5)          │
│                                         └→ ProductHandler    │
│                                                              │
│  shop-order-events   → tasks B (2000) → Pool B (20)         │
│                                         └→ OrderHandler      │
│                                                              │
│  优势:                                                      │
│  1. 每个 topic 独立并发数，按需分配                           │
│  2. 资源隔离，慢处理不影响其他 topic                          │
│  3. 独立监控每个 topic 的消费延迟和处理速率                    │
│  4. 可以为不同 topic 设置不同的中间件链                       │
│  5. 可以为不同 topic 设置不同的背压阈值                       │
│                                                              │
│  改造方向:                                                   │
│  • Consumer 内部按 topic 拆分为多个独立的子 Consumer           │
│  • 每个子 Consumer 拥有自己的 tasks channel 和 Worker Pool    │
│  • Engine 管理多个子 Consumer 的生命周期                      │
└──────────────────────────────────────────────────────────────┘
```

### 11.3 其他可优化点

```
┌──────────────────────────────────────────────────────────────┐
│  1. 中间件实现                                                │
│     当前: RecoveryMiddleware / LoggingMiddleware 为 TODO 空壳 │
│     方向: 补充实际的 panic 恢复、日志记录、超时控制逻辑        │
│                                                              │
│  2. Offset 提交策略                                          │
│     当前: ConsumeClaim 中先 MarkMessage 再发送到 tasks        │
│     风险: Worker 处理失败时 offset 已提交，消息丢失            │
│     方向: 改为 Handler 处理成功后才 MarkMessage               │
│                                                              │
│  3. 监控指标                                                  │
│     当前: 仅日志输出                                          │
│     方向: 接入 Prometheus，暴露消费速率/延迟/错误率/队列深度   │
│                                                              │
│  4. 死信管理                                                  │
│     当前: 死信写入 event_dead_letter 表                       │
│     方向: 提供 CLI/Web UI 查看死信列表、手动重试               │
└──────────────────────────────────────────────────────────────┘
```

**说明**：当前架构采用全局共享 Worker Pool 的简单设计，适合项目初期快速验证。当业务增长、topic 增多、处理复杂度提升时，可演进为 per-Handler 独立 Pool 架构，实现资源隔离和精细化调优。
