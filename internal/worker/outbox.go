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

// WriteOutbox 写入事务发件箱
// 在业务事务中调用，保证业务数据与事件记录原子性
// 参数：
//   - tx: 当前 GORM 事务（与业务操作同一个事务）
//   - aggregateId: 业务实体 ID（如订单号、商品 ID）
//   - eventType: 事件类型（如 "order.created"、"product.updated"）
//   - topic: Kafka topic
//   - partitionKey: Kafka 分区键（保证同一业务实体消息有序）
//   - payload: 事件数据（会被 JSON 序列化）
func WriteOutbox(tx *gorm.DB, aggregateId, eventType, topic, partitionKey string, payload interface{}) error {
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("序列化 payload 失败: %w", err)
	}

	now := time.Now()
	outbox := models.EventOutbox{
		EventId:      uuid.New().String(),
		AggregateId:  aggregateId,
		EventType:    eventType,
		Topic:        topic,
		PartitionKey: partitionKey,
		Payload:      string(payloadJSON),
		Status:       models.OutboxStatusWait,
		MaxRetries:   5,
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
		`INSERT INTO event_outbox (event_id, aggregate_id, event_type, topic, partition_key, payload, status, max_retries, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		outbox.EventId, outbox.AggregateId, outbox.EventType, outbox.Topic,
		outbox.PartitionKey, outbox.Payload, outbox.Status, outbox.MaxRetries,
		outbox.CreatedAt, outbox.UpdatedAt,
	)
	if result.Error != nil {
		global.LOG.Errorf("[outbox] INSERT 失败: %v", result.Error)
		return result.Error
	}
	// Redis PUBLISH 通知 Relay 立即扫描投递（goroutine 异步执行，不阻塞业务事务）
	// 失败不影响主流程，Ticker 兜底保证最终一致
	go notifyRelay(outbox.EventId)

	return nil
}

// notifyRelay 通过 Redis Pub/Sub 通知 Relay 有新 outbox 记录
func notifyRelay(eventId string) {
	if global.RedisClient == nil {
		return
	}
	if err := global.RedisClient.Publish(outboxNotifyChannel, eventId).Err(); err != nil {
		global.LOG.Warnf("[outbox] Redis 通知失败 event_id=%s: %v", eventId, err)
		// 不影响主流程，Relay 的 Ticker 兜底会处理
	}
}
