# hibiken/asynq 集成技术评估报告

> 评估在当前 Worker 多队列架构下，引入 hibiken/asynq 的可行性、工作量、风险和收益，与现有自研 Redis 队列实现进行全面对比。

---

## 目录

- [一、hibiken/asynq 技术调研](#一hibikenasynq-技术调研)
- [二、当前自研 Redis 队列分析](#二当前自研-redis-队列分析)
- [三、核心对比：asynq vs 自研方案](#三核心对比asynq-vs-自研方案)
- [四、集成方案设计](#四集成方案设计)
- [五、工作量评估](#五工作量评估)
- [六、风险与收益分析](#六风险与收益分析)
- [七、推荐方案与实施路线图](#七推荐方案与实施路线图)

---

## 一、hibiken/asynq 技术调研

### 1.1 架构分析

**核心架构：Client / Server / Scheduler 三层分离**

```
┌─────────────────┐     ┌──────────────────┐     ┌──────────────────┐
│   asynq.Client   │     │  asynq.Server    │     │ asynq.Scheduler  │
│   (任务生产者)    │     │  (任务消费者)     │     │ (周期任务调度)    │
│                  │     │                  │     │                  │
│  Enqueue(task)   │     │  Start(mux)      │     │  Register(cron)  │
│  Enqueue(task,   │     │  Concurrency: 10 │     │  Enqueue(task)   │
│    ProcessIn(5m))│     │  Queues: {...}   │     │                  │
└────────┬─────────┘     └────────┬─────────┘     └────────┬─────────┘
         │                        │                        │
         └────────────────────────┼────────────────────────┘
                                  │
                          ┌───────▼───────┐
                          │    Redis      │
                          │  (底层存储)    │
                          └───────────────┘
```

**底层存储机制**：

| 维度 | asynq 实现 | 说明 |
|------|-----------|------|
| **任务队列** | Redis List + Sorted Set | 普通队列用 List，延迟/重试队列用 Sorted Set（score=执行时间戳） |
| **消费模式** | BLPOP（阻塞弹出） | 从 List 右端阻塞弹出任务，非 XREADGROUP |
| **任务数据** | Redis Hash | 每个任务存储为 Hash（payload、重试次数、超时等元数据） |
| **去重机制** | Redis String (SET NX) | 通过 unique key + TTL 实现任务去重 |
| **聚合任务** | Redis List (group key) | 按 group key 聚合，满足条件后批量投递 |

**任务生命周期**：

```
Client.Enqueue()
    │
    ├── 立即执行 → LPUSH 到 queue List
    ├── 延迟执行 → ZADD 到 queue:delayed Sorted Set (score=执行时间戳)
    └── 周期任务 → Scheduler 计算下次执行时间 → ZADD 到 delayed
           │
           ▼
    Forwarder (内部 goroutine，每秒)
    ZRANGEBYSCORE delayed → 到期任务 → LPUSH 到 queue List
           │
           ▼
    Server.Worker (BLPOP 消费)
    取出任务 → 启动 goroutine → Handler.ProcessTask()
           │
    ┌──────┴──────┐
    │ 成功         │ 失败
    ▼              ▼
    删除任务     retry_count < max?
                 ├── 是 → ZADD 到 queue:retry (指数退避)
                 └── 否 → 移入 queue:dead (死信)
```

### 1.2 功能清单

| 功能 | asynq 支持 | 实现方式 | 成熟度 |
|------|-----------|---------|--------|
| **延迟任务** | ✅ 原生支持 | `ProcessIn(duration)` / `ProcessAt(time)` → Sorted Set | 生产就绪 |
| **周期任务** | ✅ 内置 Scheduler | `scheduler.Register("@daily", task)` cron 表达式 | 生产就绪 |
| **任务重试** | ✅ 自动重试 | `MaxRetry(n)` + 指数退避，支持 `SkipRetry` 跳过 | 生产就绪 |
| **任务去重** | ✅ Unique 选项 | `Unique(ttl)` 基于 Redis SET NX | 生产就绪 |
| **优先级队列** | ✅ 加权 + 严格 | `Queues: map[string]int{"critical":6, "default":3, "low":1}` | 生产就绪 |
| **聚合任务** | ✅ Group 选项 | `Group("group_key")` 批量聚合执行 | 生产就绪 |
| **中间件** | ✅ Middleware 链 | `asynq.MiddlewareFunc` 与项目现有模式同构 | 生产就绪 |
| **监控 Web UI** | ✅ asynqmon | 独立 Web 服务，队列/任务可视化 | 生产就绪 |
| **CLI 工具** | ✅ asynq CLI | `asynq dash` / `queue ls` / `task ls` | 生产就绪 |
| **Prometheus** | ✅ 内置集成 | Handler 中间件暴露 metrics | 生产就绪 |
| **Redis Sentinel** | ✅ 原生支持 | `RedisSentinelOpt` 配置 | 生产就绪 |
| **Redis Cluster** | ⚠️ 部分支持 | 官方文档注明部分 Lua 脚本不兼容 | 有限制 |
| **超时控制** | ✅ per-task | `Timeout(duration)` / `Deadline(time)` | 生产就绪 |
| **队列暂停** | ✅ 运行时 | `srv.QueueInspector().PauseQueue(name)` | 生产就绪 |

### 1.3 集成要求

| 要求 | 具体条件 | 与项目兼容性 |
|------|---------|-------------|
| **Redis 版本** | >= 4.0 | ✅ 项目已有 Redis，版本满足 |
| **Go 版本** | 最近两个 Go 版本 | ✅ 项目 Go 1.25.0，满足 |
| **依赖包** | `github.com/hibiken/asynq` (单包) | ✅ 无传递依赖冲突 |
| **Redis 客户端** | 自行管理连接（`RedisClientOpt`） | ✅ 与项目 `go-redis/v7` 互不干扰 |
| **配置复杂度** | 低（Addr/Password/DB + Concurrency/Queues） | ✅ 配置项简洁 |
| **部署方式** | 嵌入现有进程 或 独立进程均可 | ✅ 两种方式都支持 |

### 1.4 性能指标（基于社区基准与官方数据）

| 指标 | 数值 | 说明 |
|------|------|------|
| **单机入队吞吐** | ~15,000 ops/s | 单 Redis 实例，Client.Enqueue() |
| **单机消费吞吐** | ~10,000 tasks/s | Concurrency=10，简单 Handler |
| **入队延迟** | < 1ms (P99) | Redis LPUSH 延迟极低 |
| **消费延迟** | < 5ms (P99) | BLPOP 阻塞等待 + 处理 |
| **内存占用** | ~50-200MB | 取决于积压任务量，Server 本身轻量 |
| **CPU 消耗** | < 5% (空闲) | BLPOP 阻塞等待，无轮询开销 |

> 注：以上数据基于单 Redis 实例、标准硬件环境。asynq 的性能瓶颈在 Redis 单线程写入，通常不是应用瓶颈。

---

## 二、当前自研 Redis 队列分析

### 2.1 架构特点

项目采用 **Redis Sorted Set（延迟调度）+ Redis Stream（消费处理）+ DB Relay（兜底）** 的三段式架构：

```
┌──────────────────────────────────────────────────────────────┐
│                    自研 Redis 延迟队列架构                     │
│                                                              │
│  业务写入 event_outbox (DB 事务内)                            │
│       │                                                      │
│       ├── 异步 ZADD delay:schedule (Sorted Set 调度层)        │
│       │                                                      │
│       ▼                                                      │
│  DelayScheduler (1s 轮询 Sorted Set)                         │
│       │ 到期消息 → 原子抢占 → XADD Redis Stream               │
│       ▼                                                      │
│  RedisStreamConsumer (XREADGROUP 消费)                        │
│       │ 双缓冲：Poller → tasks channel → Worker Pool          │
│       ▼                                                      │
│  Handler.Handle() (统一 Message 模型)                         │
│                                                              │
│  ┌────────────────────────────────────────────────────┐      │
│  │  DB Relay 兜底（10s Ticker + Redis Pub/Sub 通知）   │      │
│  │  扫描 event_outbox WHERE status=WAIT               │      │
│  │  AND execute_at <= NOW() → XADD Stream             │      │
│  └────────────────────────────────────────────────────┘      │
│                                                              │
│  租约回收 (30s)：SENDING 超时 → 重置为 WAIT                   │
│  定期清理 (1h)：SENT 超 7 天 → 批量删除                       │
│  死信处理：retry_count >= max_retries → event_dead_letter     │
└──────────────────────────────────────────────────────────────┘
```

**核心组件**：

| 组件 | 文件 | 职责 | 行数 |
|------|------|------|------|
| `RedisStreamConsumer` | `redis_consumer.go` | XReadGroup 消费，双缓冲架构 | 306 行 |
| `DelayScheduler` | `delay_scheduler.go` | Sorted Set 延迟调度，1s 轮询 | 190 行 |
| `Relay` | `relay.go` | DB 兜底投递，通知驱动+Ticker | 467 行 |
| `DelayQueue` | `delay_queue.go` | 延迟队列写入接口 | 34 行 |
| `Outbox` | `outbox.go` | 事务发件箱写入（WriteOutbox/WriteDelayOutbox） | 144 行 |
| `ProcessManager` | `process_manager.go` | 异构 Process 生命周期管理 | 132 行 |

### 2.2 功能现状

**已实现的功能**：

| 功能 | 状态 | 实现方式 |
|------|------|---------|
| 延迟任务 | ✅ 已实现 | Sorted Set 调度 + Stream 消费 |
| DB 持久化 | ✅ 已实现 | event_outbox 表事务写入 |
| DB Relay 兜底 | ✅ 已实现 | Ticker 10s + Pub/Sub 通知 |
| 租约回收 | ✅ 已实现 | SENDING 超时自动重置为 WAIT |
| 指数退避重试 | ✅ 已实现 | Relay 层 retry_count + backoff |
| 死信队列 | ✅ 已实现 | event_dead_letter 表 + CLI 运维 |
| 幂等消费 | ✅ 已实现 | processed_event 表唯一约束 |
| 双缓冲架构 | ✅ 已实现 | Poller + Worker Pool 分离 |
| 统一消息模型 | ✅ 已实现 | Message 屏蔽 Kafka/Redis 差异 |
| 有序消费 | ✅ 已实现 | key hash 路由到固定 Worker |
| 优雅关闭 | ✅ 已实现 | context cancel + WaitGroup |
| 多队列共存 | ✅ 已实现 | ProcessManager 编排 Kafka + Redis |

**未实现的功能**：

| 功能 | 状态 | 说明 |
|------|------|------|
| 周期任务（Cron） | ❌ 未实现 | 当前由 `robfig/cron` 独立模块处理 |
| 任务去重 | ⚠️ 部分实现 | 依赖 processed_event 表，非队列层原生 |
| 优先级队列 | ❌ 未实现 | 所有延迟消息平等调度 |
| 聚合任务 | ❌ 未实现 | 无批量聚合执行能力 |
| Web UI 监控 | ❌ 未实现 | 仅日志 + 计划中的 Prometheus |
| CLI 管理 | ⚠️ 部分实现 | 仅 DLQ 运维 CLI，无队列管理 CLI |
| 队列暂停/恢复 | ❌ 未实现 | 无法运行时暂停特定队列 |
| 自动 XCLAIM 接管 | ❌ 未实现 | Pending 消息依赖消费者重启 |
| Stream 自动裁剪 | ❌ 未实现 | 需手动 XTRIM |

### 2.3 集成现状

**与 Worker 框架的集成深度**：

| 集成点 | 方式 | 耦合度 |
|--------|------|--------|
| `Runnable` 接口 | `RedisStreamConsumer` 实现 `Start/Stop/Name` | 高（统一生命周期管理） |
| `Handler` 接口 | 共享 `Registry`，按 topic 路由 | 高（统一处理器抽象） |
| `Middleware` 链 | `RecoveryMiddleware` + `LoggingMiddleware` | 高（统一中间件） |
| `ProcessManager` | 作为异构 Process 注册和管理 | 高（统一编排） |
| `Engine` | 与 Kafka Consumer 共存于同一 Engine | 高（统一引擎） |
| `Relay/Outbox` | DB 持久化 + Redis 调度 + Relay 兜底 | 极高（核心数据链路） |
| `Message` 模型 | 统一消息屏蔽队列差异 | 高（统一数据模型） |

---

## 三、核心对比：asynq vs 自研方案

### 3.1 功能矩阵对比

| 功能维度 | asynq | 自研方案 | 差距分析 |
|---------|-------|---------|---------|
| **延迟任务** | ✅ 原生 `ProcessIn/ProcessAt` | ✅ Sorted Set + Stream | 功能等价，asynq 更简洁 |
| **周期任务** | ✅ 内置 Scheduler (cron) | ❌ 需独立 cron 模块 | asynq 有优势 |
| **任务重试** | ✅ 自动指数退避 | ✅ Relay 层指数退避 | 功能等价 |
| **任务去重** | ✅ `Unique(ttl)` 原生 | ⚠️ 应用层 processed_event | asynq 更优雅 |
| **优先级队列** | ✅ 加权 + 严格优先级 | ❌ 无 | asynq 独有 |
| **聚合任务** | ✅ Group 批量聚合 | ❌ 无 | asynq 独有 |
| **DB 持久化兜底** | ❌ 无（纯 Redis） | ✅ event_outbox + Relay | **自研独有优势** |
| **事务发件箱** | ❌ 无 | ✅ WriteOutbox 同事务写入 | **自研独有优势** |
| **双缓冲消费** | ❌ 无（BLPOP 直消费） | ✅ Poller + Worker Pool | **自研独有优势** |
| **统一消息模型** | ❌ 自有 Task 模型 | ✅ Message 屏蔽队列差异 | **自研独有优势** |
| **有序消费** | ❌ 无原生保证 | ✅ key hash 路由 | **自研独有优势** |
| **Web UI** | ✅ asynqmon | ❌ 无 | asynq 独有 |
| **CLI 工具** | ✅ asynq CLI | ⚠️ 仅 DLQ CLI | asynq 更完善 |
| **Prometheus** | ✅ 内置中间件 | ❌ 计划中 | asynq 独有 |
| **Redis 故障降级** | ❌ Redis 挂则完全不可用 | ✅ DB Relay 兜底 | **自研独有优势** |
| **租约回收** | ❌ 无（asynq 内部 recoverer） | ✅ 显式 SENDING 超时回收 | 自研更可控 |
| **死信管理** | ✅ 内置 dead queue | ✅ event_dead_letter 表 + CLI | 各有优势 |
| **中间件** | ✅ 内置 Middleware | ✅ 自建 Middleware 链 | 功能等价 |

### 3.2 架构契合度分析

| 维度 | asynq | 自研方案 | 评价 |
|------|-------|---------|------|
| **与 Outbox 模式集成** | ❌ 无法集成：asynq 任务是纯 Redis，无 DB 事务写入能力 | ✅ 深度集成：WriteOutbox 在同一 DB 事务中写入 | **自研显著优势** |
| **与 DB Relay 兜底共存** | ❌ 互斥：asynq 无 DB 持久化层，无法做 Relay 兜底 | ✅ 原生支持：Redis 故障时 Relay 自动接管 | **自研显著优势** |
| **与 Runnable 接口兼容** | ⚠️ 需适配：asynq.Server 有自己的生命周期，需包装 | ✅ 原生实现：RedisStreamConsumer 直接实现 Runnable | 自研更自然 |
| **与 Handler/Registry 兼容** | ⚠️ 需适配：asynq 用 ServeMux + HandlerFunc，签名不同 | ✅ 完全兼容：共享 Registry + Handler 接口 | 自研零成本 |
| **与 Message 模型兼容** | ❌ 不兼容：asynq 用自有 Task (type + payload) | ✅ 统一模型：Kafka/Redis 共享 Message | **自研显著优势** |
| **与 ProcessManager 兼容** | ⚠️ 需适配：asynq Server 需包装为 Process | ✅ 原生集成：作为 Process 注册 | 自研更自然 |
| **与 Middleware 兼容** | ✅ 兼容：asynq 有自建中间件链，概念一致 | ✅ 自建中间件链 | 功能等价 |
| **与监控系统兼容** | ✅ 内置 Prometheus + Web UI | ⚠️ 需自行开发 | asynq 有优势 |

### 3.3 关键差异总结

**自研方案的核心壁垒（asynq 无法替代）**：

1. **Transactional Outbox 深度集成**：`WriteOutbox` / `WriteDelayOutbox` 在同一 DB 事务中写入 `event_outbox`，保证业务数据与事件记录原子性。asynq 的 `Client.Enqueue()` 是直接写 Redis，无法参与 DB 事务。

2. **DB Relay 降级兜底**：Redis 不可用时，Relay 通过 DB 轮询（10s Ticker + Pub/Sub 通知）自动接管投递，保证消息零丢失。asynq 完全依赖 Redis，Redis 挂则系统不可用。

3. **统一消息模型**：`Message` 结构屏蔽 Kafka/Redis 差异，Handler 无需关心消息来源。asynq 有独立的 `Task` 模型，引入后需维护两套消息抽象。

4. **双缓冲消费架构**：`RedisStreamConsumer` 的 Poller + Worker Pool 分离设计，保证慢处理不阻塞消息拉取。asynq 的 BLPOP 消费模式无此隔离。

---

## 四、集成方案设计

### 4.1 方案 A：独立集成（asynq 作为补充组件）

```
Worker 进程
├─ Kafka Consumer（订单/商品实时事件）     ← 保持不变
├─ 自研 Redis Consumer（延迟任务）         ← 保持不变
├─ Relay + DelayScheduler（Outbox 投递）   ← 保持不变
└─ asynq Server（低优先级异步任务）         ← 新增
     └─ 独立 Redis 连接（RedisClientOpt）
```

**技术细节**：

| 维度 | 方案 |
|------|------|
| **部署方式** | 同进程嵌入：在 `Engine.Start()` 中额外启动 `asynq.Server` |
| **共存方式** | asynq 处理与 Outbox 无关的纯异步任务（如邮件发送、图片处理） |
| **配置管理** | `conf.go` 新增 `AsynqConfig`（Addr/Concurrency/Queues） |
| **监控集成** | asynq 自带 Prometheus metrics，独立暴露 `/metrics` 端口 |
| **日志集成** | asynq 内部 logger 适配项目 `global.LOG`（zap） |
| **Handler 注册** | asynq 使用自有 `ServeMux`，与项目 `Registry` 并存 |

**适用场景**：
- 低一致性异步任务（邮件、短信、图片处理、报表生成）
- 不需要 DB 事务写入的任务
- 需要优先级调度的任务
- 需要周期调度的任务（替代部分 robfig/cron 场景）

### 4.2 方案 B：替换集成（asynq 替换自研 Redis 队列）

```
Worker 进程
├─ Kafka Consumer（订单/商品实时事件）     ← 保持不变
├─ asynq Server（所有 Redis 任务）         ← 替换自研 Redis Consumer
│   ├─ 延迟任务（订单超时、自动确认）
│   └─ 其他异步任务
└─ Relay（仅 Kafka Outbox 投递）           ← 简化，不再投递 Redis
```

**技术细节**：

| 维度 | 方案 |
|------|------|
| **迁移策略** | 分步迁移：先迁移非核心延迟任务，再迁移订单超时等核心任务 |
| **Outbox 改造** | `WriteDelayOutbox` 改为调用 `asynq.Client.Enqueue(task, ProcessIn(delay))` |
| **Relay 简化** | 移除 Redis Relay 分支（`scanAndDeliverByType("redis", false)`），仅保留 Kafka |
| **DelayScheduler** | 完全移除，延迟调度由 asynq 内置 Sorted Set 处理 |
| **RedisStreamConsumer** | 完全移除，消费由 asynq Server 内部 BLPOP 处理 |
| **DB Relay 兜底** | **丧失**：asynq 无 DB 持久化，Redis 故障无降级路径 |
| **事务发件箱** | **丧失**：asynq Enqueue 无法参与 DB 事务 |
| **幂等保证** | 需从 `processed_event` 迁移到 asynq 的 `Unique(ttl)` 机制 |
| **消息模型** | 需适配层：asynq `Task` ↔ 项目 `Message` 转换 |

**关键丧失能力评估**：

| 丧失能力 | 影响等级 | 缓解方案 |
|---------|---------|---------|
| DB Relay 兜底 | **高** | 需额外开发 asynq 任务的 DB 持久化层（工作量大） |
| 事务发件箱 | **高** | 需改为"先写 DB 再 Enqueue"两阶段（丧失原子性） |
| 统一消息模型 | **中** | 需编写适配层转换 Task ↔ Message |
| 双缓冲隔离 | **中** | asynq 无此机制，慢处理可能影响消费吞吐 |
| 有序消费 | **中** | asynq 无原生有序消费保证 |

---

## 五、工作量评估

### 5.1 方案 A 工作量（独立集成）

| 任务项 | 工作内容 | 预估人天 |
|--------|---------|---------|
| 引入 asynq 依赖 | `go get`，确认无依赖冲突 | 0.5 |
| 编写 asynq Client 封装 | 统一入队接口，适配项目 Redis 配置 | 1 |
| 编写 asynq Server 启动逻辑 | 包装为 `Runnable`，集成到 `Engine` | 1.5 |
| 配置管理 | `AsynqConfig` 结构体 + `config.yml` 配置段 | 0.5 |
| 日志适配 | asynq logger → zap `global.LOG` | 0.5 |
| 监控集成 | Prometheus metrics endpoint | 1 |
| 示例任务开发 | 1-2 个低优先级异步任务迁移到 asynq | 2 |
| 测试验证 | 单元测试 + 集成测试 + 压力测试 | 2 |
| 文档编写 | 使用指南 + 架构说明 | 1 |
| **总计** | | **10 人天** |

### 5.2 方案 B 工作量（替换集成）

| 任务项 | 工作内容 | 预估人天 |
|--------|---------|---------|
| 方案 A 全部工作 | 基础集成 | 10 |
| 迁移延迟任务 Handler | `DelayOrderHandler` 适配 asynq Task 模型 | 2 |
| 改造 WriteDelayOutbox | DB 事务写入 → asynq Enqueue（两阶段） | 3 |
| 重新设计 DB 兜底方案 | 为 asynq 任务开发 DB 持久化层（或放弃兜底） | 5-8 |
| 移除 DelayScheduler | 清理 Sorted Set 调度代码 + 测试 | 1 |
| 移除 RedisStreamConsumer | 清理 Stream 消费代码 + 测试 | 1 |
| 简化 Relay | 移除 Redis 投递分支 + 测试 | 1 |
| 消息模型适配层 | asynq Task ↔ 项目 Message 转换 | 2 |
| 幂等机制迁移 | processed_event → asynq Unique 或保留 | 1 |
| 回归测试 | 全链路端到端测试 + 故障演练 | 3 |
| 文档更新 | 架构变更 + 迁移指南 | 1 |
| **总计** | | **30-33 人天** |

### 5.3 维护成本对比

| 维度 | 方案 A（共存） | 方案 B（替换） | 保持现状 |
|------|--------------|--------------|---------|
| **代码量** | +300 行（封装层） | +300 行封装 -800 行自研 = 净减 500 行 | 0 |
| **系统组件** | 多一套 asynq | 替换 Redis 消费链路 | 无变化 |
| **学习成本** | 团队需学 asynq API | 同左 + 需理解迁移细节 | 无 |
| **运维复杂度** | 中（两套 Redis 队列） | 中（一套 asynq，但丧失 DB 兜底） | 低（一套自研） |
| **功能扩展** | 低（asynq 社区持续开发） | 低（同左） | 高（需自行开发缺失功能） |
| **故障排查** | 中（需排查两套系统） | 中（asynq 黑盒排查） | 低（完全掌控） |

---

## 六、风险与收益分析

### 6.1 方案 A 风险评估

| 风险 | 等级 | 说明 | 缓解措施 |
|------|------|------|---------|
| 系统复杂度增加 | **中** | 同时维护 asynq 和自研 Redis 队列 | 明确分工：asynq 仅处理非 Outbox 任务 |
| 运维成本增加 | **低** | 多一套组件需监控 | asynq 自带 Web UI + Prometheus，降低运维成本 |
| Redis 连接翻倍 | **低** | asynq 自建连接 + 项目已有连接 | asynq 连接池可配置，影响可控 |
| 两套消息模型共存 | **中** | asynq Task 与项目 Message 并存 | 明确边界：asynq 任务不经过 Outbox/Relay |

### 6.2 方案 A 收益评估

| 收益 | 价值 | 说明 |
|------|------|------|
| 快速获得丰富功能 | **高** | 优先级队列、周期任务、任务去重、聚合任务等开箱即用 |
| 减少自研工作量 | **高** | 非 Outbox 场景的异步任务无需自研 |
| 成熟的监控工具 | **高** | Web UI + CLI + Prometheus，省去自研监控 |
| 社区支持 | **中** | 13k+ stars，活跃维护，问题可查 |
| 零迁移风险 | **高** | 不影响现有 Outbox/Relay/Redis Consumer 链路 |

### 6.3 方案 B 风险评估

| 风险 | 等级 | 说明 | 缓解措施 |
|------|------|------|---------|
| **丧失 DB Relay 兜底** | **高** | Redis 故障时延迟任务不可投递，消息可能丢失 | 需额外开发 DB 持久化层（成本高） |
| **破坏事务原子性** | **高** | WriteDelayOutbox 从单事务变为两阶段，存在中间态 | 需引入补偿机制 |
| 迁移风险 | **高** | 改动核心链路，回归测试工作量大 | 分步迁移 + 灰度发布 |
| 架构一致性破坏 | **中** | Outbox 模式被绕过，部分任务走 asynq 部分走 DB | 需明确架构规范 |
| 丧失有序消费 | **中** | asynq 无 key hash 路由保证 | 评估业务是否依赖有序消费 |

### 6.4 方案 B 收益评估

| 收益 | 价值 | 说明 |
|------|------|------|
| 统一 Redis 任务引擎 | **中** | 所有 Redis 任务统一走 asynq |
| 获得完整功能集 | **高** | 优先级、去重、聚合、周期任务等 |
| 减少自研代码量 | **中** | 净减约 500 行代码 |
| 成熟监控工具 | **中** | Web UI + Prometheus |

### 6.5 保持现状风险评估

| 维度 | 评价 |
|------|------|
| **风险** | 低（系统稳定运行，深度集成，完全可控） |
| **成本** | 中（缺失功能需持续投入开发：优先级、Web UI、周期任务等） |
| **收益** | 架构统一、DB 兜底可靠、事务原子性保证、有序消费 |

---

## 七、推荐方案与实施路线图

### 7.1 推荐方案：方案 A（独立集成，渐进增强）

**推荐理由**：

| 理由 | 权重 | 说明 |
|------|------|------|
| **保护核心资产** | ★★★★★ | 自研方案的 Outbox 事务集成 + DB Relay 兜底 + 统一消息模型是核心竞争力，asynq 无法替代 |
| **零迁移风险** | ★★★★★ | 不影响现有 Kafka/Redis/Outbox/Relay 任何链路 |
| **投入产出比最优** | ★★★★☆ | 10 人天获得 asynq 全部功能（优先级、周期、去重、监控），而替换方案需 30+ 人天且丧失核心能力 |
| **架构互补** | ★★★★☆ | asynq 处理"轻一致性"异步任务（邮件/图片/报表），自研处理"强一致性"事件流（订单/库存），分工明确 |
| **可逆性** | ★★★★★ | 方案 A 完全可逆，不满意可随时回退；方案 B 不可逆 |

**不推荐方案 B 的核心原因**：

1. **丧失 DB Relay 兜底是不可接受的**：当前自研方案的最大优势是 Redis 故障时 DB Relay 自动接管，保证消息零丢失。asynq 完全依赖 Redis，Redis 挂则所有延迟任务不可用。
2. **破坏 Transactional Outbox 原子性**：`WriteDelayOutbox` 在同一 DB 事务中写入 event_outbox，保证业务数据与事件记录原子性。替换为 asynq 后，需改为"先写 DB 再 Enqueue"两阶段，存在中间态不一致风险。
3. **迁移成本高、收益低**：30+ 人天迁移，获得的仅是 asynq 的功能集，但丧失了深度集成的核心能力。

### 7.2 实施路线图

#### 第一阶段：基础集成（1-2 周）

| 步骤 | 任务 | 产出 |
|------|------|------|
| 1 | `go get github.com/hibiken/asynq` | 依赖引入 |
| 2 | 编写 `internal/worker/asynq_client.go` | 统一 asynq Client 封装 |
| 3 | 编写 `internal/worker/asynq_server.go` | 包装为 Runnable，集成到 Engine |
| 4 | `conf.go` 新增 `AsynqConfig` | 配置管理 |
| 5 | 日志适配（asynq → zap） | 统一日志 |
| 6 | 编译验证 + 基础单元测试 | 质量保障 |

#### 第二阶段：首批任务迁移（1-2 周）

| 步骤 | 任务 | 产出 |
|------|------|------|
| 7 | 选择 1-2 个低优先级异步任务（如邮件通知、图片处理） | 迁移候选 |
| 8 | 编写 asynq Handler + Client 入队调用 | 业务代码 |
| 9 | 集成测试 + 灰度验证 | 功能验证 |
| 10 | Prometheus metrics endpoint 接入 | 监控集成 |

#### 第三阶段：运维工具部署（1 周）

| 步骤 | 任务 | 产出 |
|------|------|------|
| 11 | 部署 asynqmon Web UI | 可视化监控 |
| 12 | 安装 asynq CLI 运维工具 | 运维能力 |
| 13 | 编写使用文档 + 架构说明 | 团队知识 |
| 14 | 评估是否将部分 cron 任务迁移到 asynq Scheduler | 进一步优化 |

### 7.3 关键决策点

| 决策 | 选项 | 建议 | 理由 |
|------|------|------|------|
| **asynq 部署方式** | 同进程 vs 独立进程 | 同进程（方案 A） | 减少运维复杂度，asynq Server 本身轻量 |
| **asynq Redis 连接** | 共享 vs 独立 | 独立连接（RedisClientOpt） | 与 go-redis/v7 互不干扰，asynq 自行管理最优 |
| **哪些任务走 asynq** | 全部 vs 部分 | 仅非 Outbox 异步任务 | 保护 Outbox/Relay 核心链路 |
| **是否替换自研 Redis Consumer** | 是 vs 否 | 否 | 自研方案在 Outbox 场景有不可替代的优势 |
| **周期任务是否迁移** | 全部 vs 部分 | 仅新增周期任务用 asynq Scheduler | 现有 robfig/cron 运行稳定，无需迁移 |

### 7.4 分工边界（方案 A 落地后）

```
┌─────────────────────────────────────────────────────────────┐
│                    Worker 进程最终架构                        │
│                                                             │
│  ┌─── 强一致性事件流（Transactional Outbox）───┐            │
│  │                                              │            │
│  │  Kafka Consumer ← Outbox → Relay → Kafka    │            │
│  │  Redis Consumer ← Outbox → Relay → Stream   │            │
│  │  DelayScheduler (Sorted Set 加速调度)         │            │
│  │                                              │            │
│  │  适用：订单、库存、支付等需要 DB 事务保证的场景 │            │
│  └──────────────────────────────────────────────┘            │
│                                                             │
│  ┌─── 轻一致性异步任务（asynq）─────────────────┐            │
│  │                                              │            │
│  │  asynq Client → Redis Queue → asynq Server  │            │
│  │  asynq Scheduler (周期任务)                    │            │
│  │  asynqmon Web UI + Prometheus                │            │
│  │                                              │            │
│  │  适用：邮件、短信、图片处理、报表、通知等       │            │
│  └──────────────────────────────────────────────┘            │
│                                                             │
│  ┌─── 定时任务（robfig/cron）───────────────────┐            │
│  │  适用：订单统计、数据清理等固定周期任务         │            │
│  └──────────────────────────────────────────────┘            │
└─────────────────────────────────────────────────────────────┘
```

---

## 附录：参考信息

### A. asynq 项目信息

| 项目 | 值 |
|------|-----|
| GitHub | https://github.com/hibiken/asynq |
| Stars | 13,000+ |
| License | MIT |
| 最新版本 | v0.x（尚未 v1.0，API 可能变化） |
| Go 版本要求 | 最近两个 Go 版本 |
| Redis 版本要求 | >= 4.0 |
| Redis Cluster | 部分 Lua 脚本不兼容 |
| Redis Sentinel | 支持 |

### B. 项目当前环境

| 项目 | 值 |
|------|-----|
| Go 版本 | 1.25.0 |
| Redis 客户端 | go-redis/redis/v7 (v7.4.1) |
| Kafka 客户端 | IBM/sarama (v1.45.1) |
| 日志框架 | go.uber.org/zap |
| ORM | gorm.io/gorm |
| CLI 框架 | spf13/cobra |
| Worker 架构 | Engine + ProcessManager + Runnable |

### C. 相关文件清单

| 文件 | 说明 |
|------|------|
| `internal/worker/redis_consumer.go` | 自研 Redis Stream 消费者（306 行） |
| `internal/worker/delay_scheduler.go` | 自研延迟调度器（190 行） |
| `internal/worker/relay.go` | DB Relay 兜底投递器（467 行） |
| `internal/worker/outbox.go` | 事务发件箱写入（144 行） |
| `internal/worker/delay_queue.go` | 延迟队列写入接口（34 行） |
| `internal/worker/process_manager.go` | 异构 Process 管理（132 行） |
| `internal/worker/engine.go` | Worker 引擎（322 行） |
| `internal/worker/message.go` | 统一消息模型（24 行） |
| `internal/worker/handler.go` | Handler 接口（17 行） |
| `internal/worker/middleware.go` | 中间件链（39 行） |
| `internal/worker/runnable.go` | Runnable 抽象（20 行） |
| `internal/workers/delay_order_handler.go` | 延迟订单处理器（174 行） |
| `docs/redis-delay-queue-design.md` | Redis 延迟队列设计文档（1262 行） |
| `docs/worker-multi-queue-architecture.md` | 多队列架构文档（752 行） |

---

> **报告版本**: v1.0
> **编制日期**: 2026-11
> **编制人**: Shop-Main 技术团队
> **关联文档**: [Redis 延迟队列设计](redis-delay-queue-design.md) | [Worker 多队列架构](worker-multi-queue-architecture.md) | [Worker 模块详细设计](worker-module-detailed-design.md)
