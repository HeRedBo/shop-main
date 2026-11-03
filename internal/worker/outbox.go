package worker

import (
	"encoding/json"
	"fmt"
	"shop/internal/models"
	"shop/pkg/global"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Kafka Topic 常量
const (
	TopicProductEvents = "shop-product-events"
	TopicOrderEvents   = "shop-order-events"
)

// Redis Stream / Topic 常量
const (
	TopicDelayOrderTimeout = "delay-order-timeout"
	TopicDelayReminder     = "delay-reminder"
)

// WriteOutbox 写入事务发件箱（支持多队列）
// 在业务事务中调用，保证业务数据与事件记录原子性
// 参数：
//   - tx: 当前 GORM 事务（与业务操作同一个事务）
//   - aggregateId: 业务实体 ID（如订单号、商品 ID）
//   - eventType: 事件类型（如 "order.created"、"product.updated"）
//   - topic: Kafka topic / Redis stream key
//   - partitionKey: Kafka 分区键 / Redis 路由键（保证同一业务实体消息有序）
//   - payload: 事件数据（会被 JSON 序列化）
//   - queueType: 目标队列类型 ("kafka" / "redis")
//   - executeAt: 延迟执行时间（仅 redis 有效，kafka 传 nil）
func WriteOutbox(tx *gorm.DB, aggregateId, eventType, topic, partitionKey string, payload interface{}, queueType string, executeAt *time.Time) error {
	return writeOutboxWithId(tx, uuid.New().String(), aggregateId, eventType, topic, partitionKey, payload, queueType, executeAt)
}

// writeOutboxWithId 内部核心函数：使用预生成的 eventId 写入 event_outbox
// WriteOutbox / WriteDelayOutbox 均通过此函数完成实际写入
func writeOutboxWithId(tx *gorm.DB, eventId string, aggregateId, eventType, topic, partitionKey string, payload interface{}, queueType string, executeAt *time.Time) error {
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("序列化 payload 失败: %w", err)
	}

	// 默认 queueType 为 kafka
	if queueType == "" {
		queueType = models.QueueTypeKafka
	}

	now := time.Now()
	outbox := models.EventOutbox{
		EventId:      eventId,
		AggregateId:  aggregateId,
		EventType:    eventType,
		Topic:        topic,
		PartitionKey: partitionKey,
		Payload:      string(payloadJSON),
		QueueType:    queueType,
		Status:       models.OutboxStatusWait,
		MaxRetries:   5,
		ExecuteAt:    executeAt,
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	// 关键：在 GORM Callback 链（Observer 观察者回调）中调用 tx 时，
	// tx 的内部 Statement 仍绑定原始操作的上下文（Model/Dest/Schema 等）。
	// 即使使用 Session({NewDB: true}) 创建新 Statement，GORM 的 Create 回调链
	// 仍可能通过 SaveAfterAssociations 等内部机制干扰原始 Statement，导致 UPDATE 重复执行。
	//
	// 解决方案：使用 Exec 直接执行原始 SQL INSERT，完全绕过 GORM 回调系统。
	// 底层 ConnPool 仍是同一个事务连接（*sql.Tx），保证原子性。
	//
	// 注意：在 Observer 回调中 tx.clone=0，tx.Exec() 内部 getInstance() 返回同一个 tx，
	// 原 UPDATE 的 Statement.Vars（26个参数）仍保留，clause.Expr.Build 会 append 新参数，
	// 导致参数数量错误（expected 10, got 36）。
	// 因此必须在 Exec 前手动清空 SQL 和 Vars。
	tx.Statement.SQL.Reset()
	tx.Statement.Vars = nil
	result := tx.Exec(
		`INSERT INTO event_outbox (event_id, aggregate_id, event_type, topic, partition_key, payload, queue_type, status, max_retries, execute_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		outbox.EventId, outbox.AggregateId, outbox.EventType, outbox.Topic,
		outbox.PartitionKey, outbox.Payload, outbox.QueueType, outbox.Status, outbox.MaxRetries,
		outbox.ExecuteAt, outbox.CreatedAt, outbox.UpdatedAt,
	)
	if result.Error != nil {
		Logger().Errorf("[outbox] INSERT 失败: %v", result.Error)
		return result.Error
	}
	// Redis PUBLISH 通知 Relay 立即扫描投递（goroutine 异步执行，不阻塞业务事务）
	// 失败不影响主流程，Ticker 兜底保证最终一致
	go notifyRelay(outbox.EventId)

	return nil
}

// WriteKafkaOutbox 写入 Kafka 发件箱（向后兼容便捷函数）
// 原 WriteOutbox 调用点可直接替换为此函数，参数不变
func WriteKafkaOutbox(tx *gorm.DB, aggregateId, eventType, topic, partitionKey string, payload interface{}) error {
	return WriteOutbox(tx, aggregateId, eventType, topic, partitionKey, payload, models.QueueTypeKafka, nil)
}

// WriteDelayOutbox 写入延迟事务发件箱（Redis 延迟队列专用）
// delay: 延迟时间（如 30*time.Minute）
// 写入 event_outbox 后，额外将 event_id 注册到 Redis Sorted Set（延迟调度层）
// DelayScheduler 每秒扫描 Sorted Set 中到期消息并 XADD 到 Redis Stream
// 若 Redis 不可用，ScheduleDelay 返回 nil，完全依赖 DB Relay 兜底
func WriteDelayOutbox(tx *gorm.DB, aggregateId, eventType, topic, partitionKey string, payload interface{}, delay time.Duration) error {
	executeAt := time.Now().Add(delay)
	eventId := uuid.New().String()

	// 写入 event_outbox（使用预生成的 eventId）
	err := writeOutboxWithId(tx, eventId, aggregateId, eventType, topic, partitionKey, payload, models.QueueTypeRedis, &executeAt)
	if err != nil {
		return err
	}

	// 注册到 Redis Sorted Set（异步优化，失败不影响主流程）
	if global.RedisClient != nil {
		go func() {
			if err := ScheduleDelay(global.RedisClient, eventId, executeAt); err != nil {
				Logger().Warnf("[outbox] Sorted Set 注册失败 event_id=%s: %v，将依赖 DB Relay 兜底", eventId, err)
			}
		}()
	}

	return nil
}

// notifyRelay 通过 Redis Pub/Sub 通知 Relay 有新 outbox 记录
func notifyRelay(eventId string) {
	if global.RedisClient == nil {
		return
	}
	if err := global.RedisClient.Publish(outboxNotifyChannel, eventId).Err(); err != nil {
		Logger().Warnf("[outbox] Redis 通知失败 event_id=%s: %v", eventId, err)
		// 不影响主流程，Relay 的 Ticker 兜底会处理
	}
}
