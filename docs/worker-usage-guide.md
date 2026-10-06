# Worker 模块使用指南

## 一、概述

Worker 模块是 Shop-Main 项目中的**常驻进程 Kafka 队列消费服务**，作为 `cmd/` 多入口架构中的第四个独立入口，与 HTTP Server、Cron、CLI 并列。

| 入口 | 二进制文件 | 职责 |
|------|-----------|------|
| `cmd/server`（main.go） | `shop-server` | HTTP API 服务 |
| `cmd/cron` | `shop-cron` | 定时任务调度 |
| `cmd/cli` | `shop-cli` | 命令行工具 |
| **`cmd/worker`** | **`shop-worker`** | **Kafka 队列消费 + Outbox 事件投递** |

Worker 模块的核心能力：

- **Transactional Outbox 事件投递**：业务操作与事件记录写入同一事务，保证原子性；Relay 异步扫描并投递到 Kafka
- **Kafka 消费**：双缓冲架构（Poller + Worker Pool 分离），慢处理不阻塞拉取，避免 rebalance
- **幂等处理**：双层幂等保障（状态机校验 + processed_events 幂等表），实现 at-least-once 语义下的恰好一次效果

---

## 二、架构设计回顾

### 核心数据流

```
业务操作 → Observer 写 event_outbox（同事务）
         → Relay 扫描投递 Kafka（带 Headers）
         → Consumer 拉取 → Worker Pool 分发
         → Handler 幂等检查 → 业务处理
```

### 关键设计模式

| 设计模式 | 应用位置 | 说明 |
|---------|---------|------|
| **Producer-Consumer** | `consumer.go` | Poller 拉取消息写入 tasks channel，Worker Pool 从 channel 读取处理，实现解耦和背压 |
| **Outbox** | `outbox.go` + `relay.go` | 业务事务中写入 event_outbox，Relay 异步投递 Kafka，保证事务一致性 |
| **Strategy** | `handler.go` + `registry.go` | 不同 topic 对应不同 Handler 策略，通过注册表动态分发 |
| **Chain of Responsibility** | `middleware.go` | Recovery → Logging → Timeout 中间件链，逐层包装 Handler |

---

## 三、目录结构说明

```
cmd/worker/main.go              — Worker 入口程序（选择性 Bootstrap + Engine 启动）
internal/worker/
  ├── engine.go                 — 引擎（生命周期管理：创建 Consumer/Relay，协调启动和关闭）
  ├── consumer.go               — Kafka Consumer（双缓冲架构：Poller + Worker Pool）
  ├── handler.go                — Handler 接口定义 + Event 结构体 + HandlerFunc 适配
  ├── registry.go               — Handler 注册表（按 topic 注册，线程安全）
  ├── middleware.go             — Middleware 链（Recovery/Logging/Timeout 中间件）
  ├── relay.go                  — Outbox Relay（扫描投递 + 租约回收 + 定期清理）
  └── outbox.go                 — Outbox 写入辅助函数 + Topic 常量定义
internal/workers/
  ├── register.go               — 统一注册所有 Handler 到 Engine
  ├── demo_handler.go           — Demo Handler（验证 Kafka 消费用）
  ├── product_handler.go        — 商品事件处理器（幂等检查 + 业务逻辑）
  └── order_handler.go          — 订单事件处理器（幂等检查 + 业务逻辑）
internal/models/
  ├── event_outbox.go           — Outbox 模型（状态常量：WAIT/SENDING/SENT/DEAD）
  ├── event_dead_letter.go      — 死信表模型（状态常量：PENDING/RESOLVED）
  └── processed_event.go        — 幂等消费记录表模型
sql/event_outbox.sql            — 建表 DDL（event_outbox + event_dead_letter + processed_events）
```

---

## 四、配置说明

Worker 模块读取 `conf/config.yml` 中的 `kafka` 和 `worker` 两个配置段：

```yaml
kafka:
  hosts: ["127.0.0.1:9092"]     # Kafka 集群地址（必填）

worker:
  enabled: true                  # 是否启用 Worker（false 则跳过启动）
  group-id: "shop-worker"        # Consumer Group ID（必填）
  topics:                        # 监听的 Topic 列表（必填，至少一个）
    - "shop-product-events"
    - "shop-order-events"
  concurrency: 10                # Worker Pool 并发 goroutine 数（默认 1）
  poll-interval: "2s"            # Outbox Relay 扫描间隔（Go duration 格式）
  max-retries: 5                 # 投递失败最大重试次数（默认 5）
  retry-base-delay: "1s"         # 重试基础延迟，指数退避（默认 1s）
  relay-batch-size: 100          # Relay 每次扫描最大记录数（默认 100）
  relay-lease-timeout: "60s"     # SENDING 状态租约超时，超时后回收（默认 60s）
  relay-cleanup-days: 7          # 清理多少天前的 SENT 记录（默认 7 天）
```

### 配置项详解

| 配置项 | 类型 | 默认值 | 说明 |
|--------|------|--------|------|
| `enabled` | bool | — | 设为 `false` 时 Engine 不启动，进程直接跳过 |
| `group-id` | string | — | Kafka Consumer Group ID，同组消费者共享分区分配 |
| `topics` | []string | — | 订阅的 Kafka Topic 列表，必须与 Relay 投递的 Topic 一致 |
| `concurrency` | int | 1 | Worker Pool 中处理消息的 goroutine 数量，建议设为 CPU 核数的 2-4 倍 |
| `poll-interval` | string | 5s | Relay 扫描 WAIT 记录的间隔，格式为 Go duration（如 `2s`、`500ms`） |
| `max-retries` | int | 5 | 投递失败后的最大重试次数，超过后移入死信表 |
| `retry-base-delay` | string | 1s | 指数退避基础延迟：`baseDelay × 2^retryCount`，最大 30 分钟 |
| `relay-batch-size` | int | 100 | 每次 Relay 扫描最多处理的记录数，防止一次加载过多 |
| `relay-lease-timeout` | string | 60s | SENDING 状态的租约超时，超时后自动回收为 WAIT 重新投递 |
| `relay-cleanup-days` | int | 7 | 已投递成功（SENT）记录的保留天数，超过后自动清理 |

---

## 五、使用方式

### 编译

```bash
# 仅编译 Worker 服务
make worker

# 编译所有服务（server + cli + cron + worker）
make all
```

编译产物位于 `build/shop-worker`。

### 运行

```bash
# 方式一：编译并运行（推荐）
make run-worker

# 方式二：直接运行编译好的二进制
./build/shop-worker                                    # 使用默认配置路径
./build/shop-worker --config /path/to/config.yml       # 指定配置文件路径
```

### 建表

运行前必须先创建所需的数据库表：

```bash
mysql -u root -p your_database < sql/event_outbox.sql
```

该 DDL 会创建 3 张表：

| 表名 | 用途 |
|------|------|
| `event_outbox` | 事务发件箱，存储待投递的事件记录 |
| `event_dead_letter` | 死信表，存储超过最大重试次数的失败事件 |
| `processed_events` | 幂等消费记录表，防止重复消费 |

---

## 六、事件处理器注册

### 新增 Handler 步骤

**第一步**：在 `internal/workers/` 目录下创建新的 Handler 文件

```go
// internal/workers/payment_handler.go
package workers

import (
    "context"
    "shop/internal/worker"
    "shop/pkg/global"
)

type PaymentHandler struct{}

func NewPaymentHandler() *PaymentHandler {
    return &PaymentHandler{}
}

func (h *PaymentHandler) Handle(ctx context.Context, event *worker.Event) error {
    eventId := event.Headers["event_id"]
    eventType := event.EventType
    aggregateId := event.Headers["aggregate_id"]

    global.LOG.Infof("[PaymentHandler] 收到事件: event_id=%s type=%s aggregate_id=%s",
        eventId, eventType, aggregateId)

    // 1. 幂等检查（查 processed_events 表）
    // 2. 解析 payload
    // 3. 根据 eventType 执行业务逻辑
    // 4. 写入幂等记录

    return nil
}
```

**第二步**：在 `internal/workers/register.go` 中注册到对应 topic

```go
func RegisterAll(engine *worker.Engine) {
    registry := engine.GetRegistry()

    productHandler := NewProductHandler()
    orderHandler := NewOrderHandler()
    paymentHandler := NewPaymentHandler()  // 新增

    registry.Register(worker.TopicProductEvents, productHandler)
    registry.Register(worker.TopicOrderEvents, orderHandler)
    registry.Register("shop-payment-events", paymentHandler)  // 新增

    // ...
}
```

**第三步**：确保 `conf/config.yml` 的 `worker.topics` 中包含新 topic

```yaml
worker:
  topics:
    - "shop-product-events"
    - "shop-order-events"
    - "shop-payment-events"    # 新增
```

**第四步**：重新编译运行

```bash
make run-worker
```

### Handler 接口说明

所有 Handler 必须实现 `worker.Handler` 接口：

```go
type Handler interface {
    Handle(ctx context.Context, event *Event) error
}
```

其中 `Event` 结构体包含以下字段：

| 字段 | 类型 | 说明 |
|------|------|------|
| `Topic` | string | 消息所属 Topic |
| `Key` | string | 消息 Key |
| `Value` | []byte | 消息体（JSON 原文） |
| `Headers` | map[string]string | 消息头（含 event_id、event_type、aggregate_id） |
| `EventType` | string | 从 Headers 中提取的事件类型 |
| `Offset` | int64 | Kafka Offset |
| `Partition` | int32 | Kafka Partition |

---

## 七、事件类型清单

当前项目支持的所有事件类型：

### 商品事件（Topic: `shop-product-events`）

| 事件类型 | 触发场景 | 说明 |
|---------|---------|------|
| `product.created` | 商品创建 | Observer 在商品创建时写入 Outbox |
| `product.updated` | 商品更新 | Observer 在商品更新时写入 Outbox |
| `product.deleted` | 商品删除 | Observer 在商品删除时写入 Outbox |

### 订单事件（Topic: `shop-order-events`）

| 事件类型 | 触发场景 | 说明 |
|---------|---------|------|
| `order.created` | 订单创建 | 用户下单时触发 |
| `order.paid` | 订单支付 | 支付成功回调时触发 |
| `order.shipped` | 订单发货 | 商家确认发货时触发 |
| `order.completed` | 订单完成 | 用户确认收货或自动完成时触发 |
| `order.canceled` | 订单取消 | 用户取消或超时未支付时触发 |
| `order.updated` | 订单更新 | 订单信息变更时触发 |

---

## 八、注意事项

### 1. Outbox 表自动清理

Relay 内置三个定时任务自动管理 Outbox 表：

- **扫描投递**（间隔 = `poll-interval`）：将 WAIT 记录投递到 Kafka
- **租约回收**（间隔 = `poll-interval × 2`，最小 10s）：将超时的 SENDING 记录重置为 WAIT
- **定期清理**（间隔 = 1 小时）：分批删除超过 `relay-cleanup-days` 天的 SENT 记录（每批 1000 条，避免锁表）

无需手动干预，但建议监控 `event_outbox` 表数据量，异常积压时排查 Relay 日志。

### 2. 消费端幂等保障

Worker 采用**双层幂等**机制：

- **第一层**：Handler 在处理前查询 `processed_events` 表，已处理则跳过
- **第二层**：`processed_events.event_id` 设有唯一约束（UNIQUE KEY），并发写入时通过 MySQL 1062 错误（Duplicate entry）自动识别重复，不会重复处理

这保证了 Kafka at-least-once 语义下的恰好一次消费效果。

### 3. 优雅关闭流程

Worker 收到 SIGINT/SIGTERM 信号后按以下顺序有序关闭：

```
1. 停止 Relay（停止扫描新 Outbox 记录）
2. 停止 Consumer：
   a. cancel context → 停止拉取新消息
   b. 等待 consumeLoop 退出 → 确保无新消息写入 tasks channel
   c. close(tasks) → 通知 Worker 处理完剩余消息
   d. 等待所有 Worker goroutine 完成
3. 取消 Engine context
4. 关闭 ConsumerGroup 底层连接
5. Bootstrap.Shutdown() 清理基础资源（DB/Redis/Kafka）
```

整个过程保证不丢失消息、不重复处理。

### 4. CPU 空转防范

Relay 的所有定时循环均使用 `time.NewTicker` + `defer ticker.Stop()`，通过 `select` 监听 `ticker.C` 和 `ctx.Done()`，**不使用** `time.After`（会泄漏定时器）和忙等待循环，避免 CPU 空转。

### 5. Goroutine 泄漏防范

- 每个 Worker goroutine 顶层均有 `defer recover()`，防止单条消息处理 panic 导致 goroutine 永久退出
- Relay 的每个定时任务也有独立的 `defer recover()` 保护
- 所有 goroutine 均有明确退出条件（context 取消 或 channel 关闭）
- 建议生产环境监控 `runtime.NumGoroutine()`，设置异常增长告警

### 6. 多实例部署

Relay 支持多实例并行运行：

- 通过 `UPDATE ... WHERE status='WAIT'` 原子抢占实现分布式锁效果
- `RowsAffected == 0` 表示已被其他实例抢占，自动跳过
- 租约回收机制保证异常实例的 SENDING 记录能被回收重投
