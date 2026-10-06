package worker

import (
	"encoding/json"
	"fmt"
	"time"

	"shop/internal/models"

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

	return tx.Create(&outbox).Error
}
