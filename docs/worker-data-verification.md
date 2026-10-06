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
