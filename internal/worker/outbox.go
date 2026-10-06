package worker

import (
	"encoding/json"
	"fmt"
	"time"

	"shop/internal/models"
	"shop/pkg/global"

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

	if err := tx.Create(&outbox).Error; err != nil {
		return err
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
