# Worker 模块数据验证指南

## 一、概述

本文档用于指导如何验证 Worker 模块的数据流转是否正常，重点验证 Observer 事件捕获与 Outbox 写入环节。

**适用场景：**
- 生产环境的问题排查与定位
- 新 Handler 开发后的联调测试

**数据流转链路：**

```
业务操作 → Observer 捕获事件 → 写入 event_outbox
```

---

## 二、端到端验证步骤

### 步骤 1：触发业务操作

通过 HTTP API 或数据库直接操作触发一个业务事件。例如：

- **创建商品**（触发 `product.created` 事件）
- **创建订单**（触发 `order.created` 事件）

也可以通过 API 工具（Postman/curl）调用对应的业务接口来触发。

### 步骤 2：检查 event_outbox 表

```sql
-- 查看最近的 outbox 记录
SELECT id, event_id, event_type, aggregate_id, topic, status, retry_count, created_at
FROM event_outbox
ORDER BY created_at DESC
LIMIT 10;
```

**验证点：**
- 应该有新增的 outbox 记录，`status=WAIT`
- `event_type` 应该是触发的事件类型（如 `product.created`、`order.created`）
- `aggregate_id` 应该是业务实体 ID（如商品 ID、订单号）
- `topic` 应该是对应的事件 Topic（如 `shop-product-events`、`shop-order-events`）
- `payload` 应为 JSON 格式的业务数据

---

## 三、异常场景排查

### 场景 1：触发业务操作后 outbox 表无新记录

**可能原因：**
- Observer 未注册（检查 `internal/observers/register.go` 中是否注册了对应的 Observer）
- 业务操作未触发 GORM 的 AfterCreate/AfterUpdate 回调（检查操作是否通过 GORM 执行）
- 数据库连接失败

**排查命令：**

```sql
-- 检查 outbox 表是否有任何记录
SELECT COUNT(*) FROM event_outbox;

-- 检查最近的记录时间
SELECT MAX(created_at) AS latest_record FROM event_outbox;
```

### 场景 2：outbox 记录字段异常

**排查命令：**

```sql
-- 检查最新记录的详细信息
SELECT event_id, event_type, aggregate_id, topic, payload, status, created_at
FROM event_outbox
ORDER BY created_at DESC
LIMIT 5;
```

**常见问题：**
- `event_type` 为空或不正确：检查 Observer 中事件类型的设置逻辑
- `aggregate_id` 为空：检查 Observer 中是否正确提取了业务实体 ID
- `payload` 格式异常：检查 Observer 中 JSON 序列化逻辑
- `topic` 为空：检查 Observer 中 Topic 的设置逻辑

### 场景 3：重复写入（同一业务操作产生多条 outbox 记录）

```sql
-- 检查短时间内同一业务实体的重复记录
SELECT aggregate_id, event_type, COUNT(*) AS cnt, MIN(created_at), MAX(created_at)
FROM event_outbox
WHERE created_at > DATE_SUB(NOW(), INTERVAL 1 HOUR)
GROUP BY aggregate_id, event_type
HAVING cnt > 1
ORDER BY cnt DESC;
```

**可能原因：**
- Observer 被重复注册
- 业务操作触发了多次 GORM 回调（如先 Create 再 Save）
- 事务重试导致重复写入

---

## 四、监控 SQL 速查

### Outbox 记录统计

```sql
SELECT status, COUNT(*) AS cnt
FROM event_outbox
GROUP BY status;
```

### 最近 1 小时的事件统计

```sql
SELECT event_type, status, COUNT(*) AS cnt
FROM event_outbox
WHERE created_at > DATE_SUB(NOW(), INTERVAL 1 HOUR)
GROUP BY event_type, status;
```

### 各事件类型写入量

```sql
SELECT event_type, COUNT(*) AS cnt
FROM event_outbox
WHERE created_at > DATE_SUB(NOW(), INTERVAL 1 HOUR)
GROUP BY event_type
ORDER BY cnt DESC;
```

### 检查写入异常（长时间无新记录）

```sql
-- 如果最新记录超过 10 分钟，可能有异常
SELECT TIMESTAMPDIFF(MINUTE, MAX(created_at), NOW()) AS minutes_since_last
FROM event_outbox;
```

---

## 五、清理维护

### 清理过期 outbox 记录

```sql
-- 清理 7 天前的记录（分批删除，避免锁表）
DELETE FROM event_outbox
WHERE created_at < DATE_SUB(NOW(), INTERVAL 7 DAY)
LIMIT 1000;
```

### 清理已解决的死信

```sql
DELETE FROM event_dead_letter
WHERE status = 'RESOLVED'
  AND created_at < DATE_SUB(NOW(), INTERVAL 30 DAY);
```

### 清理过期的幂等记录

```sql
-- 清理 30 天前的幂等记录（按需保留）
DELETE FROM processed_events
WHERE created_at < DATE_SUB(NOW(), INTERVAL 30 DAY)
LIMIT 1000;
```

---

## 六、多队列数据验证

多队列架构下，`event_outbox` 新增 `queue_type` 和 `execute_at` 字段，支持 Kafka（即时消息）和 Redis Stream（延迟消息）两种队列类型。

### 6.1 按队列类型统计 outbox

```sql
-- 各队列类型的记录数与状态分布
SELECT queue_type, status, COUNT(*) AS cnt
FROM event_outbox
GROUP BY queue_type, status;
```

### 6.2 Kafka 即时消息查询

```sql
-- 查看最近的即时消息（queue_type=kafka 或 NULL）
SELECT id, event_id, event_type, topic, status, queue_type, retry_count, created_at
FROM event_outbox
WHERE (queue_type = 'kafka' OR queue_type IS NULL)
ORDER BY created_at DESC
LIMIT 10;
```

### 6.3 Redis 延迟消息查询

```sql
-- 查看待执行的延迟消息
SELECT id, event_id, event_type, topic, status, queue_type,
       execute_at, TIMESTAMPDIFF(SECOND, NOW(), execute_at) AS seconds_until_exec,
       retry_count, created_at
FROM event_outbox
WHERE queue_type = 'redis'
  AND status = 'WAIT'
ORDER BY execute_at ASC
LIMIT 20;
```

### 6.4 延迟消息积压检查

```sql
-- 检查 execute_at 已过期但仍未投递的延迟消息（积压）
SELECT COUNT(*) AS overdue_count,
       MIN(execute_at) AS oldest_overdue,
       MAX(execute_at) AS newest_overdue
FROM event_outbox
WHERE queue_type = 'redis'
  AND status = 'WAIT'
  AND execute_at <= NOW();

-- 按超时时长分段统计
SELECT
  CASE
    WHEN TIMESTAMPDIFF(MINUTE, execute_at, NOW()) < 1 THEN '< 1min'
    WHEN TIMESTAMPDIFF(MINUTE, execute_at, NOW()) < 5 THEN '1-5min'
    WHEN TIMESTAMPDIFF(MINUTE, execute_at, NOW()) < 30 THEN '5-30min'
    ELSE '> 30min'
  END AS delay_bucket,
  COUNT(*) AS cnt
FROM event_outbox
WHERE queue_type = 'redis'
  AND status = 'WAIT'
  AND execute_at <= NOW()
GROUP BY delay_bucket
ORDER BY delay_bucket;
```

### 6.5 死信多队列查询

```sql
-- 按队列类型统计死信
SELECT queue_type, status, COUNT(*) AS cnt
FROM event_dead_letter
GROUP BY queue_type, status;

-- 查看 Redis 延迟消息的死信记录
SELECT id, event_id, event_type, topic, queue_type, error_message, retry_count, created_at
FROM event_dead_letter
WHERE queue_type = 'redis'
ORDER BY created_at DESC
LIMIT 10;
```

---

## 七、Redis 队列验证

### 7.1 Redis Stream 验证

```bash
# 检查 Stream 是否存在及长度
redis-cli XLEN shop:delay-events

# 查看 Stream 最新 N 条消息
redis-cli XRANGE shop:delay-events - + COUNT 10

# 反向查看（最新的在前）
redis-cli XREVRANGE shop:delay-events + - COUNT 10

# 查看消费者组信息
redis-cli XINFO GROUPS shop:delay-events

# 查看消费者详情（pending 消息数、空闲时间等）
redis-cli XINFO CONSUMERS shop:delay-events shop-delay-workers

# 查看 Pending 列表（已投递但未 ACK 的消息）
redis-cli XPENDING shop:delay-events shop-delay-workers - + 10

# 查看某条消息的详细内容
redis-cli XRANGE shop:delay-events <message_id> <message_id>
```

### 7.2 Sorted Set 延迟调度验证

DelayScheduler 使用 Redis Sorted Set 存储待调度的延迟消息，score 为执行时间戳（毫秒）。

```bash
# 查看 Sorted Set 中待调度的消息总数
redis-cli ZCARD shop:delay:schedule

# 查看已到期待调度的消息（score <= 当前时间戳毫秒）
redis-cli ZRANGEBYSCORE shop:delay:schedule -inf <current_timestamp_ms> WITHSCORES LIMIT 0 20

# 查看最近将要调度的消息（按 score 排序取前 10）
redis-cli ZRANGE shop:delay:schedule 0 9 WITHSCORES

# 查看某条消息的调度时间
redis-cli ZSCORE shop:delay:schedule <event_id>

# 检查是否有消息长期滞留（score 远小于当前时间，说明调度可能异常）
redis-cli ZRANGEBYSCORE shop:delay:schedule -inf <(current_timestamp_ms - 300000) WITHSCORES LIMIT 0 20
```

### 7.3 故障降级验证

当 Redis Stream 不可用时，Relay 会降级为仅 Kafka 模式。验证方式：

```bash
# 1. 模拟 Redis 不可用（临时关闭 Redis 或修改配置）
# 2. 触发一个延迟业务操作
# 3. 检查 event_outbox 中记录状态

# 延迟消息应仍写入 event_outbox（queue_type=redis），但不会被 DelayScheduler 调度
# Relay 的 Ticker 兜底轮询会扫描 execute_at <= NOW() 的记录
# 如果 Redis 不可用，deliverToRedis 会失败，记录保持 WAIT 状态并增加 retry_count

# 4. 恢复 Redis 后，检查积压消息是否被逐步消费
redis-cli XLEN shop:delay-events
```

```sql
-- 检查降级期间的重试情况
SELECT id, event_id, status, retry_count, last_error, updated_at
FROM event_outbox
WHERE queue_type = 'redis'
  AND retry_count > 0
ORDER BY updated_at DESC
LIMIT 10;
```

### 7.4 降级恢复验证

Redis 恢复后，验证消息自动恢复投递：

```bash
# 1. 恢复 Redis 连接
# 2. 观察 Relay 日志，确认开始重新投递
# 3. 检查 Stream 长度增长
redis-cli XLEN shop:delay-events

# 4. 检查 Sorted Set 中积压消息被清理
redis-cli ZCARD shop:delay:schedule
```

```sql
-- 确认恢复后 outbox 记录状态变为 SENT
SELECT status, COUNT(*) AS cnt
FROM event_outbox
WHERE queue_type = 'redis'
  AND updated_at > DATE_SUB(NOW(), INTERVAL 10 MINUTE)
GROUP BY status;
```

---

## 八、多队列端到端验证

### 8.1 延迟消息完整验证流程

以订单超时取消为例，端到端验证延迟消息的完整生命周期：

**Step 1：触发业务操作，创建延迟消息**

```bash
# 通过 API 创建一个订单（会自动生成 order.timeout 延迟消息）
curl -X POST http://localhost:8080/api/orders -d '{...}'
```

**Step 2：验证 event_outbox 写入**

```sql
-- 确认 outbox 记录已写入，queue_type=redis，execute_at 为超时时间
SELECT id, event_id, event_type, topic, queue_type, status, execute_at, created_at
FROM event_outbox
WHERE event_type = 'order.timeout'
ORDER BY created_at DESC
LIMIT 5;
```

**Step 3：验证 Sorted Set 注册**

```bash
# 确认 event_id 已注册到 DelayScheduler 的 Sorted Set
redis-cli ZSCORE shop:delay:schedule <event_id>
# 返回的 score 应为 execute_at 的毫秒时间戳
```

**Step 4：等待调度触发（或手动加速）**

```bash
# 如果 execute_at 已到，DelayScheduler 会：
# ZRANGEBYSCORE → 原子抢占 → XADD Stream → ZREM
# 检查 Stream 是否有新消息
redis-cli XRANGE shop:delay-events - + COUNT 5
```

**Step 5：验证消费者处理**

```bash
# 检查消费者组的 pending 列表（消息是否被 ACK）
redis-cli XPENDING shop:delay-events shop-delay-workers - + 10

# 如果 pending 为空，说明消息已被成功消费并 ACK
```

**Step 6：验证业务结果**

```sql
-- 订单状态应变为已取消
SELECT order_id, status, paid, updated_at
FROM orders
WHERE order_id = '<order_id>';

-- 幂等记录应已写入
SELECT event_id, event_type, created_at
FROM processed_events
WHERE event_id = '<event_id>';

-- outbox 记录状态应为 SENT
SELECT status, updated_at
FROM event_outbox
WHERE event_id = '<event_id>';
```

### 8.2 多队列 Relay 验证

验证 Relay 能够正确按 `queue_type` 分发到不同队列：

```sql
-- 同时查看 Kafka 和 Redis 两种队列的投递情况
SELECT queue_type, status, COUNT(*) AS cnt, MAX(updated_at) AS last_update
FROM event_outbox
WHERE created_at > DATE_SUB(NOW(), INTERVAL 1 HOUR)
GROUP BY queue_type, status;
```

```bash
# 验证 Kafka 消息（使用 Kafka 命令行工具）
kafka-console-consumer.sh --bootstrap-server localhost:9092 --topic shop-order-events --max-messages 5 --from-beginning

# 验证 Redis Stream 消息
redis-cli XRANGE shop:delay-events - + COUNT 5
```

### 8.3 跨队列一致性检查

```sql
-- 检查是否有消息被错误地投递到非预期队列
-- 延迟类事件（order.timeout, order.auto_confirm）应 queue_type=redis
-- 即时类事件（order.created, order.paid）应 queue_type=kafka 或 NULL
SELECT event_type, queue_type, COUNT(*) AS cnt
FROM event_outbox
WHERE created_at > DATE_SUB(NOW(), INTERVAL 24 HOUR)
GROUP BY event_type, queue_type
ORDER BY event_type, queue_type;
```

### 8.4 DLQ CLI 多队列验证

```bash
# 查看所有队列的死信统计
./build/shop-cli dlq stats

# 按队列类型查看死信
./build/shop-cli dlq list --queue-type redis
./build/shop-cli dlq list --queue-type kafka

# 重试指定队列类型的死信
./build/shop-cli dlq retry --queue-type redis --limit 10

# 清理已解决的死信（两种队列类型都清理）
./build/shop-cli dlq clean --status RESOLVED --days 30
```
