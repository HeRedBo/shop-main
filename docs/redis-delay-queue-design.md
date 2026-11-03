# Redis 延迟队列专项设计文档

> 基于 Shop-Main 多队列架构改造，详细说明 Redis 延迟队列的设计原理、核心实现与运维指南。

---

## 目录

- [一、概述](#一概述)
- [二、技术选型](#二技术选型)
- [三、架构设计](#三架构设计)
- [四、核心 Redis 命令](#四核心-redis-命令)
- [五、数据模型](#五数据模型)
- [六、消息生命周期](#六消息生命周期)
- [七、故障降级策略](#七故障降级策略)
- [八、业务场景](#八业务场景)
- [九、使用指南](#九使用指南)
- [十、运维命令](#十运维命令)
- [十一、监控指标](#十一监控指标)
- [十二、性能优化](#十二性能优化)
- [十三、未来扩展](#十三未来扩展)

---

## 一、概述

### 1.1 Redis 延迟队列在项目中的定位

Shop-Main 项目采用 **Kafka + Redis 双队列架构**，两者分工明确：

| 维度 | Kafka 实时队列 | Redis 延迟队列 |
|------|---------------|---------------|
| **职责** | 即时事件流处理 | 延迟任务调度与执行 |
| **典型场景** | 商品同步、订单状态变更通知 | 订单超时取消、自动确认收货、定时提醒 |
| **时效性** | 毫秒级即时投递 | 秒级~分钟级延迟触发 |
| **可靠性** | 分区副本 + 手动 Offset 提交 | DB 持久化 + Redis 内存调度 |
| **消费模型** | ConsumerGroup 竞争消费 | Stream XReadGroup 竞争消费 |

Redis 延迟队列的核心定位是：**为业务提供可靠的延迟任务调度能力**。所有延迟消息先持久化在 `event_outbox` 数据库表中，再通过 Redis Sorted Set 做高精度调度，最终投递到 Redis Stream 由消费者处理。

### 1.2 设计原则

1. **DB 优先，Redis 加速**：消息持久化在数据库，Redis 仅作为调度加速层
2. **降级无损**：Redis 不可用时，完全依赖 DB Relay 轮询兜底，不丢失任何消息
3. **统一消息模型**：Handler 无需关心消息来自 Kafka 还是 Redis
4. **双缓冲架构**：拉取与处理分离，慢处理不阻塞消息拉取

---

## 二、技术选型

### 2.1 为什么选择 Redis Sorted Set + Stream

项目采用 **Sorted Set（延迟调度）+ Stream（消费处理）** 的两段式方案，而非单一数据结构：

- **Sorted Set**：利用 `score = 执行时间戳` 天然支持按时间范围查询，适合做延迟调度
- **Stream**：利用 `XReadGroup` 消费组语义，支持多消费者竞争、ACK 确认、Pending 列表

两者结合兼顾了 **调度精度** 和 **消费可靠性**。

### 2.2 能力对比表

| 能力维度 | Redis Stream | Redis Sorted Set | Redis LIST | Redis Pub/Sub |
|---------|-------------|-----------------|-----------|--------------|
| **延迟调度** | ❌ 无原生支持 | ✅ score 天然支持 | ❌ 无时间维度 | ❌ 无持久化 |
| **消费组** | ✅ XReadGroup | ❌ 需自行实现 | ❌ BLPOP 竞争 | ❌ 广播模式 |
| **ACK 确认** | ✅ XACK | ❌ 无确认机制 | ❌ 取出即删 | ❌ 无确认 |
| **Pending 列表** | ✅ XPENDING | ❌ 无 | ❌ 无 | ❌ 无 |
| **消息持久化** | ✅ 内存+RDB/AOF | ✅ 内存+RDB/AOF | ✅ 内存+RDB/AOF | ❌ 纯内存 |
| **有序性保证** | ✅ 按追加顺序 | ✅ 按 score 排序 | ✅ FIFO | ❌ 不保证 |
| **多消费者** | ✅ 消费组天然支持 | ⚠️ 需竞争抢占 | ⚠️ BLPOP 竞争 | ✅ 广播 |
| **回溯能力** | ✅ 按 ID 范围读取 | ❌ 仅按 score 范围 | ❌ 仅两端操作 | ❌ 无 |
| **适用场景** | 消费处理层 | 延迟调度层 | 简单任务队列 | 实时通知 |

### 2.3 排除方案说明

| 方案 | 排除原因 |
|------|---------|
| **纯 Sorted Set** | 缺少消费组和 ACK 语义，需自行实现竞争消费和重试逻辑，复杂度高 |
| **纯 Stream** | 不支持按时间范围查询，无法原生实现延迟调度（需额外定时器扫描） |
| **LIST** | 无时间维度，无法实现延迟；取出即删除，无 ACK 确认机制 |
| **Pub/Sub** | 广播模式，消息不持久化，无法保证 at-least-once 语义 |
| **RocketMQ 延迟消息** | 需引入额外中间件，当前项目已有 Redis 基础设施，无需增加运维复杂度 |

---

## 三、架构设计

### 3.1 整体架构图

```
┌──────────────────────────────────────────────────────────────────────────┐
│                          业务层 (API / Cron)                             │
│                                                                          │
│   OrderService.Create()              DelayQueue.Push()                   │
│        │                                   │                             │
│        ▼                                   ▼                             │
│   WriteKafkaOutbox()                 WriteDelayOutbox()                  │
│   queue_type=kafka                   queue_type=redis                    │
│   execute_at=NULL                    execute_at=NOW()+delay              │
│        │                                   │                             │
│        ▼                                   ▼                             │
│   ┌─────────────────────────────────────────────────┐                    │
│   │              event_outbox 表 (MySQL)              │                    │
│   │  ┌─────────────────┐  ┌───────────────────────┐  │                    │
│   │  │ queue_type=kafka │  │ queue_type=redis      │  │                    │
│   │  │ execute_at=NULL  │  │ execute_at=未来时间    │  │                    │
│   │  └─────────────────┘  └───────────────────────┘  │                    │
│   └─────────────────────────────────────────────────┘                    │
│                          │                                               │
│                          ▼                                               │
│                  ┌───────────────┐                                       │
│                  │     Relay     │                                       │
│                  │  扫描 + 投递  │                                       │
│                  └───────┬───────┘                                       │
│                   ┌──────┴──────┐                                        │
│                   ▼             ▼                                        │
│           Kafka Producer   Redis XADD                                    │
│                   │             │                                        │
│                   ▼             ▼                                        │
│           ┌─────────────┐ ┌──────────────────┐                          │
│           │   Kafka     │ │  Redis Stream    │                          │
│           │  Consumer   │ │  Consumer        │                          │
│           │ (双缓冲)    │ │  (双缓冲)        │                          │
│           └──────┬──────┘ └───────┬──────────┘                          │
│                  │                │                                      │
│                  ▼                ▼                                      │
│           ┌──────────────────────────────┐                              │
│           │      Handler Registry        │                              │
│           │  ┌──────────┐ ┌────────────┐ │                              │
│           │  │ Order    │ │ DelayOrder │ │                              │
│           │  │ Handler  │ │ Handler    │ │                              │
│           │  └──────────┘ └────────────┘ │                              │
│           └──────────────────────────────┘                              │
└──────────────────────────────────────────────────────────────────────────┘
```

### 3.2 延迟消息主路径（Redis Sorted Set 调度）

```
业务写入 event_outbox (queue_type=redis, execute_at=未来时间)
       │
       ├── 异步 ZADD delay:schedule (score=executeAt毫秒, member=event_id)
       │
       ▼
DelayScheduler 每秒轮询 ZRANGEBYSCORE
       │
       ├── 查询 event_outbox 获取完整消息
       ├── 原子抢占 UPDATE status=SENDING WHERE status=WAIT
       ├── XADD 到 Redis Stream (key=topic, 如 delay-order-timeout)
       ├── UPDATE status=SENT
       └── ZREM 从 Sorted Set 移除
              │
              ▼
RedisStreamConsumer 消费
       ├── XReadGroup 拉取消息
       ├── 分发到 Worker Pool (tasks channel)
       ├── Handler.Handle() 执行业务逻辑
       └── XACK 确认
```

### 3.3 降级兜底路径（DB Relay 轮询）

```
Relay scanLoop (Ticker=10s + Redis Pub/Sub 通知)
       │
       ▼
scanAndDeliverByType("redis", false)
       │  查询: status=WAIT AND queue_type=redis AND execute_at <= NOW()
       │
       ▼
deliverOne() → deliverToRedis()
       ├── 原子抢占 UPDATE status=SENDING
       ├── XADD 到 Redis Stream
       ├── UPDATE status=SENT
       └── ZREM 清理 Sorted Set 残留
```

### 3.4 组件说明

| 组件 | 文件 | 职责 |
|------|------|------|
| **DelayScheduler** | `delay_scheduler.go` | 基于 Sorted Set 的延迟调度器，每秒扫描到期消息并投递到 Stream |
| **RedisStreamConsumer** | `redis_consumer.go` | 基于 Stream 的消费者，双缓冲架构（Poller + Worker Pool） |
| **Relay** | `relay.go` | DB 轮询兜底投递器，通知驱动 + Ticker 兜底（10s） |
| **DelayQueue** | `delay_queue.go` | 延迟队列写入接口，封装 `WriteDelayOutbox` |
| **Engine** | `engine.go` | 管理所有组件生命周期，协调启动和关闭 |

### 3.5 双缓冲架构详解

RedisStreamConsumer 采用与 Kafka Consumer 相同的 **Poller + Worker Pool** 双缓冲设计：

```
Redis Stream
     │
     ▼
┌─────────────────┐
│  Poller (1个)    │  XReadGroup 拉取消息
│  redisConsume   │  写入 tasks channel (有界缓冲)
│  Loop()         │
└────────┬────────┘
         │ tasks channel (BufferSize)
         ▼
┌─────────────────────────────────┐
│  Worker Pool (N个 goroutine)     │
│  ┌──────────┐ ┌──────────┐     │
│  │ Worker-0 │ │ Worker-1 │ ... │  从 channel 读取 → Handler.Handle()
│  └──────────┘ └──────────┘     │  处理成功 → XACK
└─────────────────────────────────┘  处理失败 → 不 ACK → Pending 列表保留
```

**核心优势**：
- 慢处理不阻塞 Redis Stream 拉取
- 拉取并发和处理并发可独立调优
- tasks channel 满时自然背压，限制内存占用

---

## 四、核心 Redis 命令

### 4.1 延迟调度：Sorted Set 操作

#### ZADD — 注册延迟消息

```redis
ZADD delay:schedule <score> <member>
```

| 参数 | 说明 | 示例 |
|------|------|------|
| `delay:schedule` | Sorted Set key | 固定值 |
| `<score>` | 执行时间戳（毫秒） | `1704067200000`（2024-01-01 00:00:00） |
| `<member>` | event_id（UUID） | `a1b2c3d4-e5f6-7890-abcd-ef1234567890` |

**使用场景**：`WriteDelayOutbox` 写入 event_outbox 后，异步调用 `ScheduleDelay()` 将 event_id 注册到 Sorted Set。

**代码对应**：
```go
// internal/worker/delay_scheduler.go
client.ZAdd(delayScheduleKey, &redis.Z{
    Score:  float64(executeAt.UnixMilli()),
    Member: eventId,
})
```

#### ZRANGEBYSCORE — 扫描到期消息

```redis
ZRANGEBYSCORE delay:schedule -inf <now_millis> LIMIT 0 50
```

| 参数 | 说明 |
|------|------|
| `-inf` | score 下界（无限制） |
| `<now_millis>` | 当前时间戳（毫秒），获取所有 score <= now 的消息 |
| `LIMIT 0 50` | 每次最多取 50 条（batchSize 配置） |

**使用场景**：`DelayScheduler.scheduleDueMessages()` 每秒执行一次，获取所有到期的延迟消息。

**代码对应**：
```go
// internal/worker/delay_scheduler.go
client.ZRangeByScore(delayScheduleKey, &redis.ZRangeBy{
    Min:    "-inf",
    Max:    fmt.Sprintf("%d", now),
    Offset: 0,
    Count:  int64(ds.batchSize), // 50
})
```

#### ZREM — 移除已调度消息

```redis
ZREM delay:schedule <event_id>
```

**使用场景**：
1. DelayScheduler 投递成功后移除
2. DB Relay 兜底投递后清理残留
3. 消息已被其他实例处理（status != WAIT）时移除

**代码对应**：
```go
// internal/worker/delay_scheduler.go
ds.client.ZRem(delayScheduleKey, eventId)

// internal/worker/relay.go (deliverToRedis)
global.RedisClient.ZRem(delayScheduleKey, outbox.EventId)
```

### 4.2 消息投递：Stream 操作

#### XADD — 写入 Redis Stream

```redis
XADD <stream-key> * event_id <id> event_type <type> aggregate_id <aid> key <key> value <json> headers <json_str>
```

| 参数 | 说明 | 示例 |
|------|------|------|
| `<stream-key>` | Stream key（等于 topic） | `delay-order-timeout` |
| `*` | 自动生成 Stream Entry ID | — |
| `event_id` | 事件唯一 ID | UUID |
| `event_type` | 事件类型 | `order.timeout` |
| `aggregate_id` | 业务实体 ID | 订单号 |
| `key` | 路由键 | 订单号（保证有序） |
| `value` | 消息体 JSON | `{"order_id":"...", "user_id":...}` |
| `headers` | 元数据 JSON 字符串 | `{"event_id":"...", "event_type":"..."}` |

**注意**：Redis Stream 的 field-value 不支持嵌套 map，headers 需序列化为 JSON 字符串。

**代码对应**：
```go
// internal/worker/relay.go (deliverToRedis)
headersJSON, _ := json.Marshal(map[string]string{
    "event_id":     outbox.EventId,
    "event_type":   outbox.EventType,
    "aggregate_id": outbox.AggregateId,
})
global.RedisClient.XAdd(&redis.XAddArgs{
    Stream: outbox.Topic,
    Values: map[string]interface{}{
        "event_id":     outbox.EventId,
        "event_type":   outbox.EventType,
        "aggregate_id": outbox.AggregateId,
        "key":          outbox.PartitionKey,
        "value":        outbox.Payload,
        "headers":      string(headersJSON),
    },
})
```

### 4.3 消费处理：Stream 消费者操作

#### XREADGROUP — 消费组读取

```redis
XREADGROUP GROUP <group> <consumer> COUNT <n> BLOCK <ms> STREAMS <stream-key> >
```

| 参数 | 说明 | 项目值 |
|------|------|--------|
| `<group>` | 消费组名称 | `shop-delay-group` |
| `<consumer>` | 消费者名称 | `shop-worker-1` |
| `COUNT <n>` | 每次最多读取条数 | BufferSize 配置（默认 1000） |
| `BLOCK <ms>` | 阻塞等待毫秒数 | 5000（5 秒） |
| `>` | 仅读取新消息 | — |

**代码对应**：
```go
// internal/worker/redis_consumer.go (redisConsumeLoop)
client.XReadGroup(&redis.XReadGroupArgs{
    Group:    config.ConsumerGroup,
    Consumer: config.ConsumerName,
    Streams:  []string{config.StreamKey, ">"},
    Count:    int64(config.BufferSize),
    Block:    config.BlockTimeout, // 5s
})
```

#### XACK — 确认消息

```redis
XACK <stream-key> <group> <entry-id>
```

**使用场景**：Handler 处理成功后调用，消息从 Pending 列表移除。

```go
// internal/worker/redis_consumer.go (processRedisEvent)
client.XAck(config.StreamKey, config.ConsumerGroup, task.streamID)
```

#### XCLAIM — 接管 Pending 消息

```redis
XCLAIM <stream-key> <group> <consumer> <min-idle-time> <entry-id> [...]
```

**使用场景**：消费者长时间未 ACK 的消息，可被其他消费者接管重新处理。

> 当前版本未实现自动 XCLAIM，Pending 消息依赖消费者重启后重新分配。

#### XTRIM — 裁剪 Stream

```redis
XTRIM <stream-key> MAXLEN ~ <count>
```

**使用场景**：限制 Stream 长度，防止已 ACK 消息无限增长占用内存。

> 建议定期执行，如 `XTRIM delay-order-timeout MAXLEN ~ 10000`。

---

## 五、数据模型

### 5.1 event_outbox 表结构

```sql
CREATE TABLE event_outbox (
    id            BIGINT AUTO_INCREMENT PRIMARY KEY,
    event_id      VARCHAR(36)  NOT NULL,           -- UUID，消费端幂等键
    aggregate_id  VARCHAR(64)  NOT NULL,           -- 业务实体 ID（如订单号）
    event_type    VARCHAR(64)  NOT NULL,           -- 事件类型（如 order.timeout）
    topic         VARCHAR(128) NOT NULL,           -- Kafka topic / Redis stream key
    partition_key VARCHAR(64)  NOT NULL DEFAULT '', -- Kafka 分区键 / Redis 路由键
    payload       TEXT         NOT NULL,           -- 事件数据（JSON 格式）
    queue_type    VARCHAR(16)  NOT NULL DEFAULT 'kafka', -- 目标队列: kafka/redis
    status        VARCHAR(16)  NOT NULL DEFAULT 'WAIT',  -- WAIT/SENDING/SENT/DEAD
    retry_count   INT          NOT NULL DEFAULT 0,       -- 已重试次数
    max_retries   INT          NOT NULL DEFAULT 5,       -- 最大重试次数
    error_msg     TEXT,                                  -- 最近一次错误信息
    sending_at    DATETIME     NULL,                     -- 开始投递时间（租约）
    execute_at    DATETIME     NULL,                     -- 延迟执行时间（Redis 专用）
    created_at    DATETIME     NOT NULL,                 -- 创建时间
    updated_at    DATETIME     NOT NULL,                 -- 更新时间

    UNIQUE INDEX idx_event_id (event_id),
    INDEX idx_aggregate_id (aggregate_id),
    INDEX idx_event_type (event_type),
    INDEX idx_queue_type_status_execute (queue_type, status, execute_at),
    INDEX idx_sending_at (sending_at)
);
```

**重点字段说明**：

| 字段 | Kafka 即时消息 | Redis 延迟消息 |
|------|--------------|---------------|
| `queue_type` | `"kafka"` | `"redis"` |
| `execute_at` | `NULL` | `NOW() + delay`（如 30 分钟后） |
| `topic` | `"shop-order-events"` | `"delay-order-timeout"` |
| `status` | WAIT → SENDING → SENT | WAIT → SENDING → SENT |

### 5.2 Redis Sorted Set 数据结构

```
Key:    delay:schedule
Type:   Sorted Set

Score:  执行时间戳（毫秒）
Member: event_id (UUID)
```

**示例**：

```
delay:schedule = {
    "a1b2c3d4-..." : 1704067200000,  // 2024-01-01 00:00:00 执行
    "e5f6a7b8-..." : 1704069000000,  // 2024-01-01 00:30:00 执行
    "c9d0e1f2-..." : 1704153600000,  // 2024-01-02 00:00:00 执行
}
```

**特点**：
- score 使用毫秒时间戳，精度满足业务需求
- member 使用 event_id（UUID），保证唯一性
- ZRANGEBYSCORE 按时间范围查询，天然支持"获取所有到期消息"

### 5.3 Redis Stream 数据结构

```
Key:    delay-order-timeout (等于 topic 值)
Type:   Stream

Entry:  {
    event_id:     "a1b2c3d4-...",
    event_type:   "order.timeout",
    aggregate_id: "ORD-2024-001",
    key:          "ORD-2024-001",
    value:        "{\"order_id\":\"ORD-2024-001\",\"user_id\":123,\"order_type\":\"normal\",\"action\":\"timeout_cancel\"}",
    headers:      "{\"event_id\":\"a1b2c3d4-...\",\"event_type\":\"order.timeout\",\"aggregate_id\":\"ORD-2024-001\"}"
}
```

**Stream Entry ID**：由 Redis 自动生成，格式为 `<timestamp>-<seq>`（如 `1704067200000-0`）。

**消费组**：

```
Group: shop-delay-group
  ├── Consumer: shop-worker-1
  ├── Consumer: shop-worker-2  (多实例部署时)
  └── Pending: [未 ACK 的 Entry ID 列表]
```

---

## 六、消息生命周期

### 6.1 完整流程图

```
┌─────────┐     ┌──────────────┐     ┌───────────────┐     ┌──────────────┐     ┌─────────┐
│ 业务写入  │────▶│ event_outbox │────▶│ DelayScheduler│────▶│ Redis Stream │────▶│ Handler │
│ (事务内)  │     │ status=WAIT  │     │ (Sorted Set)  │     │  (XADD)      │     │ (消费)   │
└─────────┘     └──────────────┘     └───────────────┘     └──────────────┘     └────┬────┘
                                                                                      │
                                                                              ┌───────┴───────┐
                                                                              │ 成功 → XACK   │
                                                                              │ 失败 → Pending │
                                                                              └───────────────┘
```

### 6.2 各阶段状态转换

```
                    写入（事务内）
                        │
                        ▼
                ┌───────────────┐
                │ status = WAIT │
                │ execute_at =  │
                │ NOW() + delay │
                └───────┬───────┘
                        │
            ┌───────────┼───────────┐
            │                       │
    DelayScheduler 调度      DB Relay 兜底
    (Sorted Set 路径)       (execute_at <= NOW())
            │                       │
            └───────────┬───────────┘
                        │
                        ▼
                ┌───────────────┐
                │ 原子抢占       │
                │ UPDATE WHERE   │
                │ status=WAIT    │
                │ → status=      │
                │   SENDING      │
                └───────┬───────┘
                        │
                        ▼
                ┌───────────────┐
                │ XADD 写入      │
                │ Redis Stream   │
                └───────┬───────┘
                        │
              ┌─────────┼─────────┐
              │ 成功               │ 失败
              ▼                   ▼
    ┌───────────────┐   ┌─────────────────┐
    │ status=SENT   │   │ 指数退避重试      │
    │ ZREM 清理     │   │ status→WAIT      │
    └───────────────┘   │ retry_count++    │
                        └────────┬────────┘
                                 │
                        retry_count >= max_retries?
                        ├── 否 → 继续重试
                        └── 是 → 移入 event_dead_letter (status=DEAD)
```

### 6.3 幂等保证

**双层幂等机制**：

1. **写入层 — 原子抢占**：
   ```sql
   UPDATE event_outbox
   SET status='SENDING', sending_at=NOW()
   WHERE id=? AND status='WAIT'
   ```
   `WHERE status='WAIT'` 保证同一消息不会被多个 Relay/DelayScheduler 实例重复投递。

2. **消费层 — processed_event 表**：
   ```go
   // Handler 处理前检查
   var count int64
   db.Model(&ProcessedEvent{}).Where("event_id = ?", eventId).Count(&count)
   if count > 0 { return nil } // 已处理，跳过

   // 处理后写入（唯一约束防并发）
   db.Create(&ProcessedEvent{EventId: eventId, ...})
   ```

---

## 七、故障降级策略

### 7.1 降级矩阵

| 故障场景 | 影响范围 | 降级策略 | 数据丢失风险 |
|---------|---------|---------|-------------|
| **Redis 完全不可用** | Sorted Set 调度 + Stream 消费均失效 | `ScheduleDelay()` 检测 `client==nil` 返回 nil，完全依赖 DB Relay 轮询 | ❌ 无 |
| **Redis 短暂抖动** | 部分 ZADD/XADD 失败 | Relay 指数退避重试 + Ticker 10s 兜底 | ❌ 无 |
| **DelayScheduler 异常** | Sorted Set 调度停止 | DB Relay 扫描 `execute_at <= NOW()` 兜底 | ❌ 无 |
| **RedisStreamConsumer 异常** | Stream 消息无法消费 | Pending 列表保留消息，消费者重启后继续 | ❌ 无 |
| **DB 不可用** | 所有路径失效 | 系统整体不可用，等待 DB 恢复 | ❌ 无（消息在 Redis 中暂存） |

### 7.2 切换机制

```go
// internal/worker/delay_scheduler.go
func ScheduleDelay(client *redis.Client, eventId string, executeAt time.Time) error {
    if client == nil {
        return nil // Redis 不可用，依赖 DB Relay 兜底
    }
    // ... ZADD 操作
}
```

```go
// internal/worker/outbox.go (WriteDelayOutbox)
if global.RedisClient != nil {
    go func() {
        if err := ScheduleDelay(global.RedisClient, eventId, executeAt); err != nil {
            global.LOG.Warnf("Sorted Set 注册失败，将依赖 DB Relay 兜底")
        }
    }()
}
// RedisClient == nil 时，完全跳过 Sorted Set 注册
```

### 7.3 降级恢复流程

```
Redis 恢复
    │
    ├── DelayScheduler 自动恢复 ZRANGEBYSCORE 扫描
    │   └── Sorted Set 中积压的到期消息立即被调度
    │
    ├── RedisStreamConsumer 自动重连
    │   └── XReadGroup 继续消费 Pending 列表中的消息
    │
    └── Relay 继续正常轮询
        └── 新的延迟消息走 Sorted Set 主路径
```

---

## 八、业务场景

### 8.1 订单超时取消（30 分钟）

| 属性 | 值 |
|------|-----|
| **eventType** | `order.timeout` |
| **topic** | `delay-order-timeout` |
| **delay** | `30 * time.Minute` |
| **触发条件** | 订单创建后未支付 |
| **处理逻辑** | 检查 `Paid=0 && Status=0` → 更新为已取消 |

**消息内容**：
```json
{
    "order_id": "ORD-2024-001",
    "user_id": 12345,
    "order_type": "normal",
    "action": "timeout_cancel"
}
```

**写入代码**：
```go
payload := worker.OrderEventPayload{
    OrderId:   order.OrderId,
    UserId:    order.UserId,
    OrderType: order.OrderType,
    Action:    "timeout_cancel",
}
delayQueue := worker.NewDelayQueue()
delayQueue.Push(tx, order.OrderId, "order.timeout",
    worker.TopicDelayOrderTimeout, 30*time.Minute, payload)
```

### 8.2 自动确认收货（7 天）

| 属性 | 值 |
|------|-----|
| **eventType** | `order.auto_confirm` |
| **topic** | `delay-order-timeout` |
| **delay** | `7 * 24 * time.Hour` |
| **触发条件** | 订单发货后 |
| **处理逻辑** | 检查 `Paid=1 && Status=1` → 更新为已完成 |

**消息内容**：
```json
{
    "order_id": "ORD-2024-001",
    "user_id": 12345,
    "order_type": "normal",
    "action": "auto_confirm"
}
```

### 8.3 定时提醒（可配置）

| 属性 | 值 |
|------|-----|
| **eventType** | `order.reminder` |
| **topic** | `delay-reminder` |
| **delay** | 业务自定义（如发货后 3 天提醒评价） |
| **触发条件** | 业务事件驱动 |
| **处理逻辑** | 发送站内信/短信/微信模板消息 |

### 8.4 延迟重试（指数退避）

| 属性 | 值 |
|------|-----|
| **eventType** | 继承原事件类型 |
| **topic** | 继承原 topic |
| **delay** | `retryBaseDelay * 2^retryCount`（最大 30 分钟） |
| **触发条件** | Handler 处理失败且未达最大重试次数 |
| **处理逻辑** |  Relay 自动重试回退状态为 WAIT，重新走调度流程 |

**退避序列**：1s → 2s → 4s → 8s → 16s → 30min（上限）

### 8.5 OrderEventPayload 统一结构

```go
// internal/worker/payload.go
type OrderEventPayload struct {
    OrderId   string `json:"order_id"`   // 订单号
    UserId    int64  `json:"user_id"`    // 用户 ID
    OrderType string `json:"order_type"` // "normal" / "seckill" / "bargain" / "combination"
    Action    string `json:"action"`     // "timeout_cancel" / "auto_confirm" / "created" / "paid"
}
```

### 8.6 DelayOrderHandler 处理流程

```
收到 Redis Stream 消息
    │
    ├── 1. 提取 Headers 元数据 (event_id, event_type, aggregate_id)
    │
    ├── 2. 幂等检查 (processed_event 表)
    │      └── 已处理 → 跳过，返回 nil
    │
    ├── 3. 解析 OrderEventPayload (JSON Unmarshal)
    │
    ├── 4. 根据 eventType 分发：
    │      ├── order.timeout
    │      │    └── 查询订单 → Paid=0 && Status=0?
    │      │         ├── 是 → UPDATE status=已取消
    │      │         └── 否 → 跳过（已支付或已发货）
    │      │
    │      └── order.auto_confirm
    │           └── 查询订单 → Paid=1 && Status=1(待收货)?
    │                ├── 是 → UPDATE status=已完成
    │                └── 否 → 跳过
    │
    └── 5. 写入 processed_event 幂等记录
           └── 唯一约束冲突 → 忽略（并发写入场景）
```

---

## 九、使用指南

### 9.1 写入延迟消息

**基础用法**：

```go
import (
    "time"
    "shop/internal/worker"
)

// 在业务事务中写入
err := tx.Transaction(func(tx *gorm.DB) error {
    // 1. 业务操作（如创建订单）
    order := &models.StoreOrder{...}
    if err := tx.Create(order).Error; err != nil {
        return err
    }

    // 2. 写入延迟消息（同一事务，原子性保证）
    delayQueue := worker.NewDelayQueue()
    payload := worker.OrderEventPayload{
        OrderId:   order.OrderId,
        UserId:    order.UserId,
        OrderType: order.OrderType,
        Action:    "timeout_cancel",
    }
    return delayQueue.Push(
        tx,                          // 当前事务
        order.OrderId,               // aggregateId（业务实体 ID）
        "order.timeout",             // eventType（事件类型）
        worker.TopicDelayOrderTimeout, // topic（Redis Stream key）
        30*time.Minute,              // delay（延迟时间）
        payload,                     // payload（消息体）
    )
})
```

**指定路由键**（保证同一订单的消息有序）：

```go
delayQueue.PushWithKey(tx, order.OrderId, "order.timeout",
    worker.TopicDelayOrderTimeout, order.OrderId, 30*time.Minute, payload)
```

### 9.2 创建延迟 Handler

**Step 1：定义 Handler**

```go
// internal/workers/delay_order_handler.go
package workers

import (
    "context"
    "encoding/json"
    "shop/internal/worker"
    "shop/pkg/global"
)

type DelayOrderHandler struct{}

func NewDelayOrderHandler() *DelayOrderHandler {
    return &DelayOrderHandler{}
}

func (h *DelayOrderHandler) Handle(ctx context.Context, msg *worker.Message) error {
    // 1. 提取元数据
    eventId := msg.Headers["event_id"]
    eventType := msg.EventType

    // 2. 幂等检查
    // ...

    // 3. 解析 payload
    var payload worker.OrderEventPayload
    if err := json.Unmarshal(msg.Value, &payload); err != nil {
        return err
    }

    // 4. 业务逻辑
    switch eventType {
    case "order.timeout":
        return h.handleOrderTimeout(ctx, &payload)
    case "order.auto_confirm":
        return h.handleAutoConfirm(ctx, &payload)
    }

    return nil
}
```

**Step 2：注册 Handler**

```go
// internal/workers/register.go
func RegisterAll(engine *worker.Engine) {
    registry := engine.GetRegistry()

    // 注册延迟订单处理器
    delayOrderHandler := NewDelayOrderHandler()
    registry.Register(worker.TopicDelayOrderTimeout, delayOrderHandler)

    global.LOG.Info("[Worker] 事件处理器注册完成")
}
```

### 9.3 Topic 常量定义

```go
// internal/worker/outbox.go
const (
    // Kafka Topics
    TopicProductEvents = "shop-product-events"
    TopicOrderEvents   = "shop-order-events"

    // Redis Stream Keys
    TopicDelayOrderTimeout = "delay-order-timeout"
    TopicDelayReminder     = "delay-reminder"
)
```

### 9.4 配置示例

**config.yml（per-Handler 模式，当前使用）**：

```yaml
worker:
  enabled: true
  group-id: "shop-worker"

  handlers:
    - topic: "shop-order-events"
      concurrency: 2
      ordered: false
      buffer-size: 2000
    - topic: "shop-product-events"
      concurrency: 2
      ordered: false
      buffer-size: 1000

  # Relay 配置
  poll-interval: "2s"
  max-retries: 5
  retry-base-delay: "1s"
  relay-batch-size: 100
  relay-lease-timeout: "60s"
  handler-timeout: "30s"
  relay-cleanup-days: 7
```

**config.yml（多队列 Process 模式，扩展用）**：

```yaml
worker:
  enabled: true
  group-id: "shop-worker"

  processes:
    # Kafka 实时事件流
    - name: kafka-order-consumer
      type: kafka
      config:
        group-id: shop-worker
      handlers:
        - topic: "shop-order-events"
          concurrency: 2
          buffer-size: 2000
        - topic: "shop-product-events"
          concurrency: 2
          buffer-size: 1000

    # Redis Stream 延迟队列
    - name: redis-delay-consumer
      type: redis
      config:
        stream-key: delay-order-timeout
        consumer-group: shop-delay-group
        consumer-name: shop-worker-1
        concurrency: 2
        buffer-size: 500
        block-timeout: "5s"
        handler-timeout: "30s"
        max-retries: 5
      handlers:
        - topic: "delay-order-timeout"
          concurrency:2
          buffer-size: 500
```

---

## 十、运维命令

### 10.1 DLQ CLI 命令

基于 Cobra 框架实现，入口文件：`cmd/cli/dlq.go`

| 命令 | 说明 | 示例 |
|------|------|------|
| `dlq list` | 查看死信列表 | `./shop-cli dlq list --topic delay-order-timeout --limit 20` |
| `dlq retry <id>` | 重试单条死信 | `./shop-cli dlq retry <event_id>` |
| `dlq retry --all` | 批量重试全部 PENDING 死信 | `./shop-cli dlq retry --all --topic delay-order-timeout` |
| `dlq discard <id>` | 丢弃死信消息 | `./shop-cli dlq discard <event_id>` |
| `dlq clean` | 清理过期已解决记录 | `./shop-cli dlq clean --days 30` |
| `dlq stats` | 死信队列统计 | `./shop-cli dlq stats` |

**使用示例**：

```bash
# 查看延迟队列相关死信
$ ./shop-cli dlq list --topic delay-order-timeout --status PENDING

EVENT_ID                              TOPIC                     AGGREGATE_ID       RETRIES   STATUS      CREATED_AT
──────────────────────────────────    ──────────────────────    ────────────────   ────────  ──────────  ──────────────────
a1b2c3d4-e5f6-7890-abcd-ef123456    delay-order-timeout       ORD-2024-001       5         PENDING     2024-01-15 10:30

共 1 条记录

# 重试单条
$ ./shop-cli dlq retry a1b2c3d4-e5f6-7890-abcd-ef123456
死信已重新投递: event_id=a1b2c3d4-e5f6-7890-abcd-ef123456, topic=delay-order-timeout

# 统计
$ ./shop-cli dlq stats

TOPIC                           STATUS        COUNT
──────────────────────────────  ────────────  ──────
delay-order-timeout             PENDING       1
delay-order-timeout             RESOLVED      8
──────────────────────────────  ────────────  ──────
TOTAL                           PENDING       1
TOTAL                           RESOLVED      8
```

### 10.2 Redis 验证命令

#### 查看延迟调度队列

```redis
-- 查看所有待调度的延迟消息（按执行时间排序）
ZRANGE delay:schedule 0 -1 WITHSCORES

-- 查看已到期但未调度的消息（score <= 当前时间戳毫秒）
ZRANGEBYSCORE delay:schedule -inf 1704067200000 WITHSCORES

-- 查看延迟队列总数
ZCARD delay:schedule
```

#### 查看 Redis Stream

```redis
-- 查看 Stream 消息总数
XLEN delay-order-timeout

-- 查看 Stream 中最近 10 条消息
XRANGE delay-order-timeout - + COUNT 10

-- 查看 Stream 详细信息
XINFO STREAM delay-order-timeout
```

#### 查看消费组

```redis
-- 查看 Stream 的所有消费组
XINFO GROUPS delay-order-timeout

-- 输出示例：
-- name: shop-delay-group
-- consumers: 1
-- pending: 0        ← 未 ACK 的消息数
-- last-delivered-id: 1704067200000-0
```

#### 查看 Pending 列表

```redis
-- 查看消费组的 Pending 消息
XPENDING delay-order-timeout shop-delay-group

-- 查看详细的 Pending 消息列表
XPENDING delay-order-timeout shop-delay-group - + 10

-- 查看特定消费者的 Pending
XPENDING delay-order-timeout shop-delay-group - + 10 shop-worker-1
```

#### 手动接管 Pending 消息

```redis
-- 接管空闲超过 60000ms 的 Pending 消息
XCLAIM delay-order-timeout shop-delay-group shop-worker-2 60000 1704067200000-0

-- 强制接管（无论空闲时间）
XCLAIM delay-order-timeout shop-delay-group shop-worker-2 0 1704067200000-0 FORCE
```

#### 清理 Stream

```redis
-- 裁剪 Stream 长度（保留最近 10000 条）
XTRIM delay-order-timeout MAXLEN ~ 10000

-- 按 ID 删除特定消息
XDEL delay-order-timeout 1704067200000-0
```

---

## 十一、监控指标

### 11.1 核心监控指标

| 指标名称 | 数据来源 | 计算方式 | 说明 |
|---------|---------|---------|------|
| **延迟消息积压量** | Redis | `ZCARD delay:schedule` | Sorted Set 中待调度的消息总数 |
| **到期未调度量** | Redis | `ZRANGEBYSCORE delay:schedule -inf <now> LIMIT 0 1` 是否有结果 | 已超过执行时间但仍在 Sorted Set 中的消息 |
| **Stream 消费延迟** | Redis | `XPENDING` 返回的 pending 数量 | 已投递但未 ACK 的消息数 |
| **Stream 总长度** | Redis | `XLEN <stream-key>` | Stream 中的消息总数（含已 ACK） |
| **DB Relay 兜底频率** | MySQL | `SELECT COUNT(*) FROM event_outbox WHERE queue_type='redis' AND status='WAIT' AND execute_at <= NOW()` | 到期但未被 Sorted Set 调度、依赖 DB 轮询的消息数 |
| **死信队列深度** | MySQL | `SELECT COUNT(*) FROM event_dead_letter WHERE status='PENDING'` | 待处理的死信消息数 |
| **投递失败率** | 日志/指标 | 投递失败次数 / 总投递次数 | Relay 投递到 Redis Stream 的失败比例 |
| **消费处理耗时** | 日志/指标 | Handler.Handle() 执行时间 P99 | 单条消息的处理延迟 |

### 11.2 建议告警阈值

| 指标 | 告警阈值 | 告警级别 | 处理建议 |
|------|---------|---------|---------|
| 延迟消息积压量 | > 10,000 | P2 Warning | 检查 DelayScheduler 是否正常运行 |
| 到期未调度量 | > 0 持续 30s | P1 Critical | DelayScheduler 可能异常，检查 Redis 连接 |
| Stream 消费延迟 | > 1,000 | P2 Warning | 消费者可能处理不过来，考虑增加 Worker 并发 |
| 死信队列深度 | > 100 | P1 Critical | 存在大量处理失败的消息，需人工介入 |
| DB Relay 兜底频率 | > 0 持续 5min | P2 Warning | Redis Sorted Set 调度可能失效 |
| 投递失败率 | > 5% | P2 Warning | 检查 Redis 服务状态和网络连接 |
| 消费处理耗时 P99 | > 10s | P2 Warning | Handler 处理逻辑需优化或增加超时控制 |

### 11.3 Prometheus 指标建议

```
# 延迟队列指标
worker_delay_schedule_size              — Sorted Set 当前大小
worker_delay_schedule_due_total         — 到期调度次数（counter）
worker_delay_stream_pending_count       — Stream Pending 消息数
worker_delay_handler_duration_seconds   — Handler 处理耗时（histogram）
worker_delay_relay_fallback_total       — DB Relay 兜底投递次数（counter）

# 死信队列指标
worker_dlq_size{topic}                  — 按 topic 的死信数量
worker_dlq_retry_total                  — 死信重试次数
worker_dlq_discard_total                — 死信丢弃次数

# Relay 指标
worker_relay_pending{queue_type}        — 按队列类型的待投递数量
worker_relay_delivery_errors_total      — 投递失败次数
```

---

## 十二、性能优化

### 12.1 批量调度

DelayScheduler 每次最多处理 **50 条**到期消息，避免单次 ZRANGEBYSCORE 返回过多数据导致阻塞：

```go
// internal/worker/delay_scheduler.go
ds.client.ZRangeByScore(delayScheduleKey, &redis.ZRangeBy{
    Min:    "-inf",
    Max:    fmt.Sprintf("%d", now),
    Offset: 0,
    Count:  int64(ds.batchSize), // 50
})
```

**调优建议**：
- 低延迟场景（< 1s）：batchSize 可调大到 100
- 高吞吐场景：保持 50，依赖 1s 轮询间隔自然消化

### 12.2 异步注册

ZADD 操作在 goroutine 中异步执行，不阻塞业务事务：

```go
// internal/worker/outbox.go (WriteDelayOutbox)
if global.RedisClient != nil {
    go func() {
        if err := ScheduleDelay(global.RedisClient, eventId, executeAt); err != nil {
            global.LOG.Warnf("Sorted Set 注册失败，将依赖 DB Relay 兜底")
        }
    }()
}
```

**优势**：
- 业务事务提交不受 Redis 网络延迟影响
- ZADD 失败仅影响调度精度，不影响消息可靠性（DB Relay 兜底）

### 12.3 连接池配置

Redis 客户端连接池建议配置：

```go
// 推荐配置（go-redis/redis v7）
client := redis.NewClient(&redis.Options{
    Addr:         "localhost:6379",
    Password:     "",
    DB:           0,
    PoolSize:     20,        // 连接池大小（根据并发调整）
    MinIdleConns: 5,         // 最小空闲连接
    MaxRetries:   3,         // 命令重试次数
    DialTimeout:  5 * time.Second,
    ReadTimeout:  3 * time.Second,
    WriteTimeout: 3 * time.Second,
    IdleTimeout:  5 * time.Minute,
})
```

**注意事项**：
- Pub/Sub 需独立连接（`cache.Redis` 封装层不支持 Pub/Sub）
- `go-redis/v7` 的 `Subscribe` API 签名：`Subscribe(channels ...string)`，不传 context 参数

### 12.4 轮询间隔优化

| 组件 | 默认间隔 | 优化建议 |
|------|---------|---------|
| DelayScheduler | 1s | 高精度场景保持 1s；低精度可调到 5s 减少 Redis 压力 |
| Relay Ticker | 10s | 作为兜底，10s 足够；不宜过短（增加 DB 压力） |
| 租约回收 | 30s | 保持 30s，平衡及时回收和 DB 查询频率 |
| SENT 清理 | 1h | 保持 1h，分批删除（每次 1000 条）避免锁表 |

### 12.5 Stream 内存控制

Redis Stream 会持续增长，需定期清理已 ACK 的消息：

```redis
-- 建议每小时执行（可通过 Cron 任务自动化）
XTRIM delay-order-timeout MAXLEN ~ 10000
```

---

## 十三、未来扩展

### 13.1 支持更多延迟场景

| 场景 | 实现方式 | 优先级 |
|------|---------|--------|
| **周期任务** | 新增 `DelayScheduler` 支持 cron 表达式，到期后重新注册下一次 | P2 |
| **定时任务迁移** | 将 `internal/cron/` 中的 robfig/cron 任务逐步迁移到延迟队列 | P3 |
| **延迟消息优先级** | Sorted Set score 结合优先级权重（如 `score = executeAt * 10 + priority`） | P3 |

### 13.2 Redis Cluster 支持

当前使用单节点 Redis，扩展至 Cluster 需注意：

| 问题 | 解决方案 |
|------|---------|
| **Sorted Set 跨 slot** | 使用 Hash Tag（如 `{delay}:schedule`）保证相关 key 在同一 slot |
| **Stream 跨 slot** | 同上，`{delay}:order-timeout` |
| **事务/MULTI** | Redis Cluster 不支持跨 slot 事务，需改用 Lua 脚本保证原子性 |
| **Pub/Sub** | Cluster 模式下 Pub/Sub 消息广播到所有节点，需客户端过滤 |

### 13.3 延迟消息优先级

在 Sorted Set 中引入优先级维度：

```
score = executeAt * 1000 + (maxPriority - priority)
```

- 同时间到期的消息，优先级高的先被调度
- ZRANGEBYSCORE 天然按 score 升序返回，无需额外排序逻辑

### 13.4 自动 XCLAIM 接管

当前 Pending 消息依赖消费者重启后重新分配。后续可实现自动 XCLAIM 接管：

```go
// 定期扫描 Pending 列表，接管空闲超过阈值的消息
func (c *RedisStreamConsumer) reclaimPending() {
    pending := c.client.XPendingExt(&redis.XPendingExtArgs{
        Stream: c.config.StreamKey,
        Group:  c.config.ConsumerGroup,
        Start:  "-",
        End:    "+",
        Count:  100,
        Idle:   60 * time.Second, // 空闲超过 60s 的消息
    })
    for _, p := range pending {
        c.client.XClaim(...) // 接管并重新处理
    }
}
```

### 13.5 死信自动重试

当前死信需手动 CLI 重试。后续可实现自动退避重试：

```
死信产生 → 等待 1min → 自动重试 → 失败 → 等待 5min → 自动重试
→ 失败 → 等待 30min → 自动重试 → 失败 → 标记为 DISCARDED + 告警
```

---

> **文档版本**: v1.0
> **最后更新**: 2024-01
> **维护者**: Shop-Main 团队
> **关联文档**: [Worker 多队列架构文档](worker-multi-queue-architecture.md)
