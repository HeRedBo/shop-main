package worker

import (
	"time"

	"gorm.io/gorm"
)

// DelayQueue 延迟队列写入接口
// 提供统一的延迟消息写入能力，底层通过 event_outbox 表 + Relay 投递到 Redis Stream
type DelayQueue struct{}

// NewDelayQueue 创建延迟队列实例
func NewDelayQueue() *DelayQueue {
	return &DelayQueue{}
}

// Push 写入延迟消息
// tx: 业务事务（保证原子性）
// aggregateId: 业务实体 ID（如订单号）
// eventType: 事件类型（如 "order.timeout"）
// topic: Redis Stream key（如 worker.TopicDelayOrderTimeout）
// delay: 延迟时间
// payload: 消息体
func (dq *DelayQueue) Push(tx *gorm.DB, aggregateId, eventType, topic string, delay time.Duration, payload interface{}) error {
	return WriteDelayOutbox(tx, aggregateId, eventType, topic, aggregateId, payload, delay)
}

// PushWithKey 写入延迟消息（指定路由键）
// partitionKey: 路由键（保证同一业务实体消息有序）
func (dq *DelayQueue) PushWithKey(tx *gorm.DB, aggregateId, eventType, topic, partitionKey string, delay time.Duration, payload interface{}) error {
	return WriteDelayOutbox(tx, aggregateId, eventType, topic, partitionKey, payload, delay)
}
