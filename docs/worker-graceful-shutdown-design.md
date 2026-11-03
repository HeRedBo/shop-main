# Worker 模块优雅关闭架构设计文档

> 本文档系统性描述 Worker 模块优雅关闭的架构设计，涵盖关闭流程全貌、各组件职责、关键设计原则和死锁防范策略。
> 基于 `internal/worker/` 源码和 `cmd/worker/main.go` 入口实现编写。

---

## 一、概述

### 1.1 整体目标

Worker 模块作为常驻进程，负责 Kafka 消息消费、Redis Stream 消费、Outbox 投递、延迟调度等核心异步任务。优雅关闭需要满足三个目标：

| 目标 | 说明 |
|------|------|
| **不丢消息** | channel 中尚未处理的剩余消息必须在退出前全部消费完毕 |
| **不死锁** | 关闭顺序必须严格保证，任何 goroutine 都不能无限阻塞 |
| **快速退出** | 收到信号后应在合理时间内完成所有资源释放并退出进程 |

### 1.2 架构模式：ProcessManager 统一管理

Worker 模块采用 **ProcessManager 统一管理模式**，所有长期运行的任务（Kafka Consumer、Relay、DelayScheduler）都实现 `Runnable` 接口，由 `ProcessManager` 统一管理生命周期。

```
main() → ProcessManager.Start() → 串行启动所有 Process
main() → ProcessManager.Stop()  → 逆序关闭所有 Process
```

**核心设计原则**：

- **逆序关闭**：与启动顺序相反，先启动的后关闭，保证依赖关系正确解除
- **组件自管理**：每个组件的 `Stop()` 方法内部自行完成所有资源释放（包括底层连接关闭）
- **Context 向下传递**：`ProcessManager.ctx` 通过 `Start(ctx)` 传递给各组件，各组件内部 `context.WithCancel(ctx)` 派生子 context

### 1.3 设计原则

- **先断源头，再清尾端**：cancel 通知生产者停止 → 等传送带停稳 → 关闭 channel → Worker 处理完剩余消息退出
- **错误不中断**：单个 Process 关闭失败只记录日志，不影响其他 Process 的关闭
- **防御性 nil 检查**：每个组件关闭前检查是否已初始化

---

## 二、关闭流程全景图

### 2.1 完整关闭链路

```
Ctrl+C (SIGINT/SIGTERM)
    │
    ▼
┌──────────────────────────────────────────────────────────────────┐
│  cmd/worker/main.go                                              │
│  signal.NotifyContext → ctx.Done() → pm.Stop()                   │
└──────────────────────────┬───────────────────────────────────────┘
                           │
                           ▼
┌──────────────────────────────────────────────────────────────────┐
│  ProcessManager.Stop()  ─── 调度层，逆序关闭所有 Process          │
│                                                                  │
│  注册顺序（启动顺序）：                                           │
│    ① Kafka Consumer Process (order-events)                       │
│    ② Kafka Consumer Process (product-events)                     │
│    ③ Relay Process                                               │
│    ④ DelayScheduler Process                                      │
│                                                                  │
│  逆序关闭：                                                       │
│    ④ DelayScheduler.Stop() ← 停止延迟调度                        │
│       └─ cancel()          ← scheduleLoop 退出                    │
│                                                                  │
│    ③ Relay.Stop()          ← 停止 Outbox 扫描投递                 │
│       └─ cancel()          ← subscribeNotify/scanLoop/定时任务退出 │
│                                                                  │
│    ② Consumer.Stop()       ← 停止 Kafka Consumer (product)       │
│       ├─ cancel()          ← 断源头：ConsumeClaim 停止写入        │
│       ├─ consumeWg.Wait()  ← 等传送带停稳                         │
│       ├─ close(tasks)      ← 封入口：通知 Worker 处理完剩余消息   │
│       ├─ workerWg.Wait()   ← 等分拣员走人                         │
│       └─ group.Close()     ← 断开 Kafka 连接（Consumer 自管理）   │
│                                                                  │
│    ① Consumer.Stop()       ← 停止 Kafka Consumer (order)         │
│       └─ （同上四步 + group.Close）                               │
│                                                                  │
│  ⑤ pm.cancel()            ← 取消 ProcessManager 级 context       │
└──────────────────────────────────────────────────────────────────┘
                           │
                           ▼
┌──────────────────────────────────────────────────────────────────┐
│  bootstrap.Shutdown()  ─── 清理基础资源（MySQL/Redis/Kafka等）   │
└──────────────────────────────────────────────────────────────────┘
                           │
                           ▼
                    进程退出（exit 0）
```

### 2.2 组件依赖关系图

```
                    ┌────────────────────┐
                    │ ProcessManager.ctx │
                    │  (全局控制)          │
                    └─────────┬──────────┘
                              │
              Start(ctx) 向下传递
                              │
         ┌────────────────────┼────────────────────┐
         │                    │                    │
    ┌────▼────┐         ┌────▼────┐         ┌─────▼──────┐
    │ Consumer │         │  Relay  │         │   Delay    │
    │ (Kafka)  │         │         │         │ Scheduler  │
    └────┬────┘         └────┬────┘         └─────┬──────┘
         │                    │                   │
    ┌────▼────┐          独立 goroutine       独立 goroutine
    │Consumer │           (scanLoop,          (scheduleLoop)
    │ Group   │            subscribeNotify,
    │(sarama) │            等)
    └─────────┘

    注：各组件在 Start(ctx) 中通过 context.WithCancel(ctx)
        派生子 context，Stop() 时调用自己的 cancel()

    ┌──────────────────────────────────────────────────────┐
    │          bootstrap.Shutdown()                         │
    │    (MySQL / Redis / Kafka Producer 等)                │
    └──────────────────────────────────────────────────────┘
```

> **关键设计决策**：各组件在 `Start(ctx)` 中通过 `context.WithCancel(ctx)` 从 `ProcessManager.ctx` 派生子 context，而非各自使用 `context.Background()`。这确保了 `ProcessManager` 的 context 取消信号可以传播到所有组件，作为防御性的全局关闭兜底。同时每个组件的 `Stop()` 方法内部会先调用自己的 `cancel()`，实现有序独立关闭。

---

## 三、信号处理与 ProcessManager 调度

### 3.1 信号监听入口

`cmd/worker/main.go` 使用 `signal.NotifyContext` 监听系统信号：

```go
// cmd/worker/main.go
func main() {
    // ... 初始化、注册 Handler、添加 Process ...

    // 7. 启动 ProcessManager（统一管理所有 Process）
    if err := pm.Start(); err != nil {
        global.LOG.Fatalf("[worker] ProcessManager 启动失败: %v", err)
    }
    global.LOG.Info("[worker] Worker 服务已启动（ProcessManager 模式）")

    // 8. 阻塞等待系统信号
    ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
    defer stop()
    <-ctx.Done()

    // 9. 优雅关闭（逆序关闭所有 Process）
    global.LOG.Info("[worker] 收到退出信号，开始关闭...")
    pm.Stop()

    fmt.Println("[worker] Worker 服务已安全退出")
}
```

**设计要点**：

- `signal.NotifyContext` 同时监听 `SIGINT`（Ctrl+C）和 `SIGTERM`（K8s/Docker 停止信号）
- `<-ctx.Done()` 阻塞主 goroutine，直到收到信号
- 关闭顺序：`pm.Stop()` → `bootstrap.Shutdown()`（通过 defer），从业务到基础设施层层退出
- `main.go` 直接调用 `pm.Stop()`，不再经过 Engine 层调度

### 3.2 ProcessManager.Stop() 调度顺序

`ProcessManager.Stop()` 是整个关闭流程的核心调度器，负责逆序关闭所有 Process：

```go
// internal/worker/process_manager.go 第 92-121 行
func (pm *ProcessManager) Stop() error {
    pm.mu.Lock()
    defer pm.mu.Unlock()

    if !pm.started {
        return nil
    }

    global.LOG.Info("[process-manager] 开始关闭所有 Process...")

    // 逆序关闭
    for i := len(pm.processes) - 1; i >= 0; i-- {
        p := pm.processes[i]
        global.LOG.Infof("[process-manager] 关闭 Process: %s", p)
        if err := p.Runner.Stop(); err != nil {
            global.LOG.Errorf("[process-manager] Process[%s] 关闭失败: %v", p.Name, err)
            p.Status = ProcessStatusFailed
            p.Err = err
        } else {
            p.Status = ProcessStatusStopped
        }
    }

    // 取消 context
    pm.cancel()
    pm.started = false

    global.LOG.Info("[process-manager] 所有 Process 已关闭")
    return nil
}
```

**调度顺序总结**：

| 步骤 | 动作 | 目的 |
|------|------|------|
| 1 | 逆序遍历 `pm.processes`，逐个调用 `Runner.Stop()` | 按注册逆序关闭所有 Process（DelayScheduler → Relay → Kafka Consumer） |
| 2 | `pm.cancel()` | 取消 ProcessManager 级 context（防御性兜底） |
| 3 | `pm.started = false` | 标记已关闭，防止重复调用 |

> **与旧架构的关键区别**：旧架构中 `Engine.Stop()` 需要手动编排 Relay → DelayScheduler → ProcessManager → Runner → cancel → ConsumerGroup 的六步关闭顺序。新架构中 `ProcessManager.Stop()` 只需逆序遍历所有 Process，每个 Process 的 `Stop()` 方法内部自行完成所有资源释放（包括 ConsumerGroup 连接关闭）。关闭顺序由注册顺序决定，无需额外的协调逻辑。

---

## 四、各组件关闭详解

### 4.1 Relay 模块

**职责**：定时扫描 `event_outbox` 表中 `status=WAIT` 的记录，投递到 Kafka 或 Redis。

**内部 goroutine 结构**：

```
Relay.Start(ctx)
    │  └─ r.ctx, r.cancel = context.WithCancel(ctx)
    │
    ├── go subscribeNotify()   ← Redis Pub/Sub 监听（通知驱动）
    ├── go scanLoop()          ← 主扫描循环（Ticker 10s 兜底）
    ├── go loop("reclaim-lease", 30s, ...)   ← 租约回收
    └── go loop("cleanup-sent", 1h, ...)     ← 定期清理
```

**关闭流程**：

```go
// internal/worker/relay.go 第 84-90 行
func (r *Relay) Stop() error {
    if r.cancel != nil {
        r.cancel()  // 取消 context，所有 goroutine 通过 ctx.Done() 退出
    }
    global.LOG.Info("[relay] Relay 已停止")
    return nil
}
```

**各 goroutine 退出机制**：

- `subscribeNotify()`：`select` 监听 `r.ctx.Done()` 和 Redis channel，收到取消信号后 `return`，`defer pubsub.Close()` 关闭 Redis 订阅连接
- `scanLoop()`：`select` 监听 `r.ctx.Done()`、`r.notify`、`ticker.C`，收到取消信号后 `return`，`defer ticker.Stop()` 释放定时器
- `loop(name, ...)`：通用定时循环，同样监听 `r.ctx.Done()`，收到取消信号后 `return`
- `cleanupSent()`：分批删除逻辑中，每轮循环都检查 `r.ctx.Done()`，收到取消信号后中断删除

**为什么 Relay 在 Kafka Consumer 之后关闭**：在 ProcessManager 逆序关闭中，Relay 注册在 Kafka Consumer 之后，因此会先关闭 Kafka Consumer 再关闭 Relay。这确保了在 Relay 停止投递之前，Consumer 已经停止消费，不会有消息在 Consumer 关闭期间被投递到 Kafka 而无法被消费。

### 4.2 DelayScheduler（延迟调度器）

**职责**：基于 Redis Sorted Set 的延迟消息调度，定时扫描到期消息并投递到 Redis Stream。

**关闭流程**：

```go
// internal/worker/delay_scheduler.go 第 57-63 行
func (ds *DelayScheduler) Stop() error {
    if ds.cancel != nil {
        ds.cancel()  // 取消 context
    }
    global.LOG.Info("[delay-scheduler] 延迟调度器已停止")
    return nil
}
```

**内部 goroutine 退出**：

`scheduleLoop()` 通过 `select` 监听 `ds.ctx.Done()`，收到取消信号后 `return`，`defer ticker.Stop()` 释放定时器。

```go
func (ds *DelayScheduler) scheduleLoop() {
    ticker := time.NewTicker(ds.pollInterval)
    defer ticker.Stop()

    for {
        select {
        case <-ds.ctx.Done():
            return  // ← 收到取消信号，退出循环
        case <-ticker.C:
            ds.scheduleDueMessages()
        }
    }
}
```

**与 Relay 的关系**：DelayScheduler 是 Redis 延迟消息的主调度路径，Relay 是 DB 轮询兜底路径。两者在关闭时互不依赖，但都必须在 Consumer 关闭之前停止，避免在 Consumer 退出期间仍有新消息被投递。

### 4.3 Consumer（双缓冲架构关闭流程）

**职责**：Kafka 消息消费，采用双缓冲架构（Poller + Worker Pool 分离）。

这是整个关闭流程中最复杂也最关键的部分。

#### 4.3.1 内部 goroutine 结构

```
Consumer.Start(ctx)
    │  └─ c.ctx, c.cancel = context.WithCancel(ctx)
    │
    ├── Worker Pool（N 个 goroutine）
    │   ├── go workerLoop(0)         ← 无序模式：从 tasks channel 读取处理
    │   ├── go workerLoop(1)
    │   └── ...
    │   （或有序模式：go orderedWorkerLoop(i, ch)）
    │
    └── go consumeLoop()             ← Poller：ConsumerGroup 消费循环
        └── group.Consume(ctx, topics, handler)
            └── ConsumeClaim()       ← 从 Kafka 拉取消息，写入 tasks channel
```

#### 4.3.2 快递分拣中心类比

理解 Consumer 关闭流程的最佳方式是快递分拣中心类比：

| 组件 | 类比 |
|------|------|
| `ConsumeClaim` | **传送带**：从 Kafka 拉取消息，放到桌上 |
| `tasks` channel | **桌子**：消息的缓冲区，传送带往桌上放，分拣员从桌上拿 |
| `workerLoop` | **分拣员**：从桌上拿快递，逐个处理 |
| `consumeWg` | **传送带停稳确认**：确保传送带上不再有新的快递 |
| `workerWg` | **分拣员离场确认**：确保所有分拣员处理完手上的快递离开 |

#### 4.3.3 五步关闭流程

```go
// internal/worker/consumer.go 第 191-219 行
func (c *Consumer) Stop() error {
    global.LOG.Infof("[worker] [%s] Consumer 开始关闭...", c.config.Name())

    // 步骤 1：取消 context，停止拉取新消息
    c.cancel()

    // 步骤 2：等待 consumeLoop 退出，确保不再有新的消息写入 tasks channel
    c.consumeWg.Wait()

    // 步骤 3：关闭 channel，通知 Worker 处理完剩余消息后退出
    if c.ordered {
        for _, ch := range c.orderedChs {
            close(ch)
        }
    } else {
        close(c.tasks)
    }

    // 步骤 4：等待所有 Worker 完成
    c.workerWg.Wait()

    // 步骤 5：关闭 ConsumerGroup 连接（Consumer 自管理，无需 Engine 层单独关闭）
    if err := c.group.Close(); err != nil {
        global.LOG.Errorf("[worker] [%s] ConsumerGroup 关闭失败: %v", c.config.Name(), err)
    }

    global.LOG.Infof("[worker] [%s] Consumer 已关闭", c.config.Name())
    return nil
}
```

**逐步详解**：

**步骤 1 — `c.cancel()`：断源头**

取消 Consumer 内部的 context。这个取消信号会传播到：
- `consumeLoop()` 中的 `c.ctx.Done()` → 退出消费循环
- `ConsumeClaim()` 中的 `c.ctx.Done()` → 停止从 Kafka 拉取新消息
- `processEvent()` 中的超时 context → 基于 `c.ctx` 的超时立即失效

此时 `ConsumeClaim` 可能正在向 `tasks` channel 写入消息。`cancel()` 后，`ConsumeClaim` 的 `select` 会优先命中 `c.ctx.Done()` 分支并退出，不再写入新消息。

**步骤 2 — `c.consumeWg.Wait()`：等传送带停稳**

等待 `consumeLoop()` goroutine 完全退出。这确保了：
- `ConsumeClaim` 已经不再向 `tasks` channel 写入任何消息
- 所有正在进行的 Kafka session 已经结束

**为什么必须等**：如果跳过这一步直接 `close(tasks)`，`ConsumeClaim` 可能还在执行 `tasks <- &consumeTask{...}`，向一个已关闭的 channel 写入会触发 `panic: send on closed channel`。

**步骤 3 — `close(tasks)`：封入口**

关闭 `tasks` channel。此时所有 `workerLoop` 中的 `for task := range c.tasks` 循环会在处理完 channel 中剩余消息后自然退出。

**不丢消息的关键**：`close` 不会丢弃 channel 中已有的数据。Go 的 channel 语义保证：关闭后，`range` 循环会继续消费完所有已缓冲的元素，然后才退出。

**步骤 4 — `c.workerWg.Wait()`：等分拣员走人**

等待所有 Worker goroutine 处理完剩余消息并退出。每个 Worker 的退出流程：

```go
func (c *Consumer) workerLoop(id int) {
    defer c.workerWg.Done()  // ← 退出时通知 WaitGroup

    for task := range c.tasks {  // ← channel 关闭后，处理完剩余元素自动退出
        c.processEvent(task)
    }
    // 所有剩余消息处理完毕，goroutine 退出
}
```

**步骤 5 — `c.group.Close()`：断开 Kafka 连接**

Consumer 自行关闭 ConsumerGroup 连接。这是新架构的关键改进——不再需要 Engine 层单独管理 ConsumerGroup 的生命周期。

```go
// internal/worker/consumer.go 第 212-215 行
// 5. 关闭 ConsumerGroup 连接（Consumer 自管理，无需 Engine 层单独关闭）
if err := c.group.Close(); err != nil {
    global.LOG.Errorf("[worker] [%s] ConsumerGroup 关闭失败: %v", c.config.Name(), err)
}
```

**为什么 ConsumerGroup 在最后关闭**：

ConsumerGroup 是 Consumer 的底层依赖。在步骤 1-4 完成之前关闭 ConsumerGroup 会导致：
- `group.Consume()` 会立即返回错误
- `ConsumeClaim` 的 `claim.Messages()` channel 会被关闭
- 可能导致 Consumer 内部的 goroutine 出现意外行为

正确顺序是：先完成四步关闭流程（cancel → consumeWg → close(tasks) → workerWg），确保所有消息处理完毕后，再断开底层 Kafka 连接。

#### 4.3.4 有序模式 vs 无序模式

关闭逻辑对两种模式的处理差异：

| 模式 | channel 结构 | 关闭方式 |
|------|-------------|---------|
| 无序模式 | 单个 `tasks` channel，所有 Worker 共享 | `close(c.tasks)` |
| 有序模式 | 每个 Worker 拥有独立的 `orderedChs[i]` | 遍历 `close(ch)` 关闭每个 channel |

有序模式的 Worker 通过 `for task := range ch` 消费专属 channel，关闭逻辑与无序模式完全对称。

### 4.4 RedisConsumer（Redis Stream 消费者）

**职责**：基于 Redis Stream 的消费者，采用与 Kafka Consumer 相同的双缓冲架构。

**关闭流程**：

```go
// internal/worker/redis_consumer.go
func (c *RedisStreamConsumer) Stop() error {
    global.LOG.Infof("[redis-consumer] [%s] Consumer 开始关闭...", c.Name())

    c.cancel()           // 1. 断源头：停止 XReadGroup 拉取
    c.consumeWg.Wait()   // 2. 等传送带停稳：Poller 退出
    close(c.tasks)        // 3. 封入口：通知 Worker 处理完剩余
    c.workerWg.Wait()     // 4. 等分拣员走人：Worker 退出

    global.LOG.Infof("[redis-consumer] [%s] Consumer 已关闭", c.Name())
    return nil
}
```

**与 Kafka Consumer 关闭流程的对称性**：

RedisConsumer 和 Kafka Consumer 采用完全相同的四步关闭模式，区别仅在于：
- Kafka Consumer 的 Poller 通过 `group.Consume()` → `ConsumeClaim()` 拉取消息
- RedisConsumer 的 Poller 通过 `XReadGroup` 拉取消息
- 两者的 Worker Pool 都通过 `for task := range tasks` 消费，关闭 channel 后自然退出

**Context 管理**：

RedisStreamConsumer 在 `Start(ctx)` 中通过 `context.WithCancel(ctx)` 从 ProcessManager.ctx 派生子 context，与 Kafka Consumer 的 context 管理方式完全一致。

---

## 五、关键设计原则

### 5.1 层层关闭，从外到内

```
业务入口层     Relay / DelayScheduler    ← 最后关闭（逆序，最先启动的最后关闭）
    │
消费层         Consumer / RedisConsumer  ← 最先关闭（逆序，最后启动的最先关闭）
    │
连接层         ConsumerGroup (sarama)    ← Consumer.Stop() 内部自行关闭
    │
基础设施层     bootstrap.Shutdown()      ← 最终关闭：MySQL/Redis/Kafka Producer
```

> **注意**：在 ProcessManager 模式下，关闭顺序由注册顺序决定（逆序关闭）。注册时通常先添加 Kafka Consumer，再添加 Relay，最后添加 DelayScheduler。因此关闭时先关 DelayScheduler，再关 Relay，最后关 Kafka Consumer。这确保了消息生产者在消费者之前停止。

**为什么必须从外到内**：

- Relay 是消息的"入口"，负责将 Outbox 记录投递到 Kafka。如果先关闭 Consumer 再关闭 Relay，Relay 可能在 Consumer 关闭期间仍在投递新消息到 Kafka，这些消息永远不会被消费
- ConsumerGroup 是 Consumer 的底层依赖。Consumer.Stop() 内部在完成消息处理后才调用 `group.Close()`，确保关闭流程不会混乱

### 5.2 先断源头，再清尾端

这是 Consumer 五步关闭流程的核心哲学：

```
1. cancel()           ← 断源头：通知 ConsumeClaim 停止拉取
2. consumeWg.Wait()   ← 等传送带停稳
3. close(tasks)       ← 封入口：标记 channel 不再接收
4. workerWg.Wait()    ← 等分拣员处理完剩余
5. group.Close()      ← 断连接：关闭底层 Kafka 连接
```

**源头**：`ConsumeClaim` 从 Kafka 拉取消息并写入 `tasks` channel。
**尾端**：`workerLoop` 从 `tasks` channel 读取消息并处理。

先断源头（cancel），确保没有新消息写入；再清尾端（close + wait），确保现有消息被处理完；最后断连接（group.Close），确保底层资源释放。

### 5.3 不丢消息

Go channel 的关闭语义保证了不丢消息：

```go
// close(c.tasks) 之后：
// - 不能再向 channel 写入数据（否则 panic）
// - 但仍然可以从 channel 读取已缓冲的数据
// - range 循环会消费完所有剩余元素后才退出

for task := range c.tasks {  // 处理完所有剩余 task 后才退出循环
    c.processEvent(task)
}
```

这意味着：
1. `cancel()` 后，`ConsumeClaim` 可能已经将一些消息写入了 `tasks` channel
2. `consumeWg.Wait()` 确保 `ConsumeClaim` 完全退出，不再有新的写入
3. `close(tasks)` 标记 channel 关闭，但已缓冲的消息仍在
4. `workerWg.Wait()` 等待 Worker 处理完所有剩余消息

**at-least-once 语义保证**：Worker 处理成功后才调用 `session.MarkMessage()` 标记 offset。如果 Worker 在处理某条消息时进程被强制杀死（如 SIGKILL），该消息的 offset 未被标记，Kafka 会在下次消费时重新投递。

### 5.4 顺序的重要性

错误的关闭顺序会导致 panic 或死锁：

| 错误顺序 | 后果 | 原因 |
|---------|------|------|
| 先 `close(tasks)` 再 `cancel()` | **panic: send on closed channel** | `ConsumeClaim` 还在向已关闭的 channel 写入 |
| 先 `workerWg.Wait()` 再 `close(tasks)` | **死锁** | Worker 在 `range tasks` 上阻塞，channel 未关闭永远不会退出 |
| 先关闭 `ConsumerGroup` 再完成四步关闭 | **未定义行为** | Consumer 内部的 `group.Consume()` 可能正在执行 |
| 先关闭 `Consumer` 再关闭 `Relay` | **消息丢失风险** | Relay 可能在 Consumer 关闭后仍在投递新消息 |

---

## 六、死锁防范

### 6.1 Bug 案例：先 close(channel) 再 cancel()

**现象**：Consumer 关闭时 panic: send on closed channel。

**根因**：

```go
// ❌ 错误顺序
func (c *Consumer) Stop() error {
    close(c.tasks)       // 先关闭 channel
    c.cancel()           // 再取消 context
    c.consumeWg.Wait()   // 等待 consumeLoop 退出
    c.workerWg.Wait()
    return nil
}
```

`close(c.tasks)` 执行后，`ConsumeClaim` 可能正在执行：

```go
// ConsumeClaim 中
select {
case c.tasks <- &consumeTask{...}:  // ← panic! 向已关闭的 channel 写入
case <-c.ctx.Done():
    return nil
}
```

由于 `cancel()` 尚未调用，`c.ctx.Done()` 还没有触发，`ConsumeClaim` 继续尝试写入 `tasks`，触发 panic。

**修复**：严格遵循 cancel → consumeWg.Wait → close → workerWg.Wait → group.Close 的顺序。

### 6.2 Bug 案例：先等 Worker 退出再关闭 channel

**现象**：Consumer 关闭时死锁，进程永远无法退出。

**根因**：

```go
// ❌ 错误顺序
func (c *Consumer) Stop() error {
    c.cancel()
    c.consumeWg.Wait()
    c.workerWg.Wait()   // 先等 Worker 退出
    close(c.tasks)       // 再关闭 channel
    return nil
}
```

`workerWg.Wait()` 等待 Worker goroutine 退出，但 Worker 在 `for task := range c.tasks` 上阻塞——channel 未关闭，`range` 永远不会退出。形成死锁：

- `workerWg.Wait()` 等待 Worker 退出
- Worker 等待 `tasks` channel 关闭
- `close(tasks)` 等待 `workerWg.Wait()` 返回

**修复**：必须在 `workerWg.Wait()` 之前 `close(tasks)`。

### 6.3 Context 生命周期管理

**Context 传递链路**：

```
ProcessManager.ctx (context.WithCancel(context.Background()))
    │
    │  Start(ctx) 传递
    │
    ├── Consumer.ctx (context.WithCancel(pm.ctx))  ← 派生子 context
    │       └── Consumer.cancel → 取消 Consumer.ctx
    │
    ├── Relay.ctx (context.WithCancel(pm.ctx))     ← 派生子 context
    │       └── Relay.cancel → 取消 Relay.ctx
    │
    └── DelayScheduler.ctx (context.WithCancel(pm.ctx)) ← 派生子 context
            └── DelayScheduler.cancel → 取消 DelayScheduler.ctx
```

**各组件 Context 从 ProcessManager.ctx 派生**：

每个组件在 `Start(ctx)` 中通过 `context.WithCancel(ctx)` 派生子 context，而非使用独立的 `context.Background()`。

**为什么使用派生 context 而非独立 context**：

- 派生 context 允许 `ProcessManager.cancel()` 作为全局兜底信号，确保所有组件都能收到取消通知
- 各组件的 `Stop()` 方法内部先调用自己的 `cancel()`，实现有序独立关闭
- 即使某个组件的 `Stop()` 未被调用（如 panic），`ProcessManager.cancel()` 仍能确保其 goroutine 最终退出

**pm.cancel() 的时机**：

`pm.cancel()` 在所有 Process 关闭完成后才调用（`ProcessManager.Stop()` 最后一步），此时它更多是一种防御性编程——确保没有任何遗漏的 goroutine 仍在引用 ProcessManager 级 context。

### 6.4 关闭顺序依赖分析

```
ProcessManager.Stop()
    │
    ├─→ 逆序遍历 pm.processes
    │       │
    │       ├─→ DelayScheduler.Stop()   ← cancel()，scheduleLoop 退出
    │       ├─→ Relay.Stop()            ← cancel()，所有 goroutine 退出
    │       └─→ Consumer.Stop()         ← 五步关闭：cancel → consumeWg → close → workerWg → group.Close
    │
    └─→ pm.cancel()                     ← 防御性兜底，取消 ProcessManager.ctx
```

**Consumer.Stop() 内部顺序**：

```
Consumer.Stop()
    │
    ├─→ c.cancel()              ← 断源头：ConsumeClaim 停止写入
    ├─→ c.consumeWg.Wait()      ← 等传送带停稳：consumeLoop 退出
    ├─→ close(tasks)            ← 封入口：通知 Worker 处理完剩余
    ├─→ c.workerWg.Wait()       ← 等分拣员走人：Worker 处理完退出
    └─→ c.group.Close()         ← 断连接：关闭 Kafka 连接
```

> **关键改进**：在旧 Engine 架构中，`ConsumerGroup.Close()` 由 `Engine.Stop()` 单独管理，需要精确协调关闭顺序以避免死锁。在新 ProcessManager 架构中，`ConsumerGroup.Close()` 由 `Consumer.Stop()` 内部管理，天然保证了正确的关闭顺序——先完成消息处理，再断开连接。

---

## 七、超时保护

### 7.1 当前状态

当前代码中各组件的 `Stop()` 方法没有设置超时保护。如果某个 goroutine 因为网络阻塞、数据库慢查询等原因无法退出，`Stop()` 会无限等待。

### 7.2 建议的超时保护方案

#### 7.2.1 Consumer 级别超时

```go
// 建议：为 Consumer.Stop() 添加超时保护
func (c *Consumer) Stop() error {
    c.cancel()

    // 为 consumeWg.Wait() 设置超时
    done := make(chan struct{})
    go func() {
        c.consumeWg.Wait()
        close(done)
    }()

    select {
    case <-done:
        // 正常退出
    case <-time.After(30 * time.Second):
        global.LOG.Errorf("[worker] [%s] consumeWg.Wait() 超时（30s），强制继续", c.config.Name())
    }

    // close(tasks) + workerWg.Wait() 同理...
    return nil
}
```

#### 7.2.2 ProcessManager 级别超时

```go
// 建议：为整个 ProcessManager.Stop() 设置最大等待时间
func (pm *ProcessManager) StopWithTimeout(timeout time.Duration) error {
    done := make(chan error, 1)
    go func() {
        done <- pm.Stop()
    }()

    select {
    case err := <-done:
        return err
    case <-time.After(timeout):
        global.LOG.Errorf("[worker] ProcessManager.Stop() 超时（%v），强制退出", timeout)
        return fmt.Errorf("process manager stop timeout after %v", timeout)
    }
}
```

#### 7.2.3 整体 Shutdown 超时

在 `cmd/worker/main.go` 中建议设置全局超时：

```go
// 建议：main.go 中添加全局超时保护
func main() {
    // ... 启动逻辑 ...

    <-ctx.Done()
    global.LOG.Info("[worker] 收到退出信号，开始关闭...")

    // 设置最大关闭时间（如 60s），超时后强制退出
    shutdownDone := make(chan struct{})
    go func() {
        pm.Stop()
        close(shutdownDone)
    }()

    select {
    case <-shutdownDone:
        fmt.Println("[worker] Worker 服务已安全退出")
    case <-time.After(60 * time.Second):
        global.LOG.Error("[worker] 关闭超时（60s），强制退出")
        os.Exit(1)
    }
}
```

### 7.3 各组件建议超时值

| 组件 | 建议超时 | 说明 |
|------|---------|------|
| `consumeWg.Wait()` | 30s | Kafka rebalance 可能需要较长时间 |
| `workerWg.Wait()` | 60s | 取决于 channel 中剩余消息数量和处理速度 |
| `Relay.Stop()` | 10s | 主要是等待当前正在执行的扫描完成 |
| `group.Close()` | 15s | 需要与 Broker 通信，提交 offset |
| 整体 `pm.Stop()` | 120s | 覆盖所有组件关闭的总时间 |

---

## 八、日志规范

### 8.1 关闭流程日志标准

关闭流程中的日志应遵循统一格式，便于排查问题：

```
[组件标识] [动作] [状态/结果]
```

**标准日志输出**：

```
[worker] 收到退出信号，开始关闭...
[process-manager] 开始关闭所有 Process...
[process-manager] 关闭 Process: Process{name=delay-scheduler, type=scheduler, status=RUNNING}
[delay-scheduler] 延迟调度器已停止
[process-manager] 关闭 Process: Process{name=relay, type=relay, status=RUNNING}
[relay] Relay 已停止
[process-manager] 关闭 Process: Process{name=kafka-shop-order-events, type=kafka, status=RUNNING}
[worker] [shop-order-events] Consumer 开始关闭...
[worker] [shop-order-events] Consumer session 结束
[worker] [shop-order-events] consumeLoop 收到取消信号，退出消费循环
[worker] [shop-order-events] Worker-0 已退出
[worker] [shop-order-events] Consumer 已关闭
[process-manager] 所有 Process 已关闭
[worker] Worker 服务已安全退出
```

### 8.2 日志级别使用规范

| 级别 | 使用场景 | 示例 |
|------|---------|------|
| `Info` | 正常关闭流程的关键步骤 | "Consumer 开始关闭..."、"Relay 已停止" |
| `Warn` | 非关键异常但不影响关闭 | "Process[x] 关闭失败: ..." |
| `Error` | 关闭过程中的错误 | "ConsumerGroup 关闭失败: ..." |
| `Debug` | 详细的内部状态（生产环境通常关闭） | "扫描到 N 条 WAIT 记录" |

### 8.3 关键日志点

以下是必须输出日志的关键步骤：

```go
// 1. ProcessManager 开始关闭
global.LOG.Info("[process-manager] 开始关闭所有 Process...")

// 2. 每个 Process 关闭
global.LOG.Infof("[process-manager] 关闭 Process: %s", p)

// 3. 每个组件关闭开始和完成
global.LOG.Infof("[worker] [%s] Consumer 开始关闭...", c.config.Name())
global.LOG.Infof("[worker] [%s] Consumer 已关闭", c.config.Name())

// 4. consumeLoop 退出
global.LOG.Infof("[worker] [%s] consumeLoop 收到取消信号，退出消费循环", name)

// 5. Worker 退出
global.LOG.Infof("[worker] [%s] Worker-%d 已退出", c.config.Name(), id)

// 6. 关闭失败（Error 级别）
global.LOG.Errorf("[process-manager] Process[%s] 关闭失败: %v", p.Name, err)

// 7. ProcessManager 关闭完成
global.LOG.Info("[process-manager] 所有 Process 已关闭")
```

### 8.4 排查指南

当关闭流程出现异常时，可通过日志定位问题：

| 现象 | 可能原因 | 排查方法 |
|------|---------|---------|
| 进程无法退出 | Worker 死锁或阻塞 | 检查是否有 "Worker-N 已退出" 日志 |
| panic: send on closed channel | 关闭顺序错误 | 检查 `cancel()` 是否在 `close(tasks)` 之前 |
| 消息丢失 | Worker 未处理完剩余消息 | 检查 `workerWg.Wait()` 是否在 `close(tasks)` 之后 |
| ConsumerGroup 关闭失败 | Kafka Broker 不可达 | 检查网络连通性和 Broker 状态 |
| Relay 未停止 | Redis 订阅阻塞 | 检查 `subscribeNotify` 的退出日志 |

---

## 九、总结

### 9.1 关闭流程一句话总结

> 收到信号后，ProcessManager 按**注册逆序**逐个关闭所有 Process（DelayScheduler → Relay → Consumer），每个 Consumer 内部按 **cancel → consumeWg.Wait → close(tasks) → workerWg.Wait → group.Close** 的五步流程确保不丢消息、不死锁。最后 `pm.cancel()` 取消全局 context 作为防御性兜底。

### 9.2 核心设计模式速查

| 模式 | 应用位置 | 说明 |
|------|---------|------|
| 五步关闭法 | Consumer / RedisConsumer | cancel → 等生产者 → 关 channel → 等消费者 → 关连接 |
| 逆序关闭 | ProcessManager | 后启动的先关闭，保证依赖关系 |
| Context 派生 | 各组件 Start(ctx) | `context.WithCancel(ctx)` 从 ProcessManager.ctx 派生 |
| 组件自管理 | Consumer.Stop() | ConsumerGroup 由 Consumer 自行关闭，无需外部协调 |
| 快递分拣类比 | Consumer | 传送带(ConsumeClaim) → 桌子(tasks) → 分拣员(Worker) |
| 防御性 nil 检查 | 各组件 Stop() | 每个组件关闭前检查 cancel 是否为 nil |

### 9.3 关键文件索引

| 文件 | 职责 |
|------|------|
| `cmd/worker/main.go` | 信号监听入口，调用 `pm.Stop()` |
| `internal/worker/process_manager.go` | ProcessManager 调度层，逆序关闭所有 Process |
| `internal/worker/process.go` | Process 封装，统一 Runnable 接口 |
| `internal/worker/runnable.go` | Runnable 接口定义（Name/Start/Stop） |
| `internal/worker/consumer.go` | Kafka Consumer 双缓冲关闭流程（含 group.Close） |
| `internal/worker/redis_consumer.go` | Redis Stream Consumer 关闭流程 |
| `internal/worker/relay.go` | Relay 模块关闭（Runnable 接口） |
| `internal/worker/delay_scheduler.go` | 延迟调度器关闭（Runnable 接口） |
| `internal/worker/kafka_process.go` | Kafka Consumer Process 工厂 |
| `internal/worker/relay_process.go` | Relay Process 工厂 |
| `internal/worker/scheduler_process.go` | DelayScheduler Process 工厂 |
