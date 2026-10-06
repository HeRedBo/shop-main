package models

import "time"

// ProcessedEvent 幂等消费记录表
// 用于消费端幂等保障：记录已处理的事件 ID，防止重复消费
type ProcessedEvent struct {
	Id          int64     `gorm:"primaryKey" json:"id"`
	EventId     string    `gorm:"type:varchar(36);uniqueIndex;not null" json:"event_id"`    // 事件唯一 ID（与 outbox 的 event_id 对应）
	EventType   string    `gorm:"type:varchar(64);not null" json:"event_type"`               // 事件类型
	AggregateId string    `gorm:"type:varchar(64);not null" json:"aggregate_id"`             // 业务实体 ID
	ProcessedAt time.Time `gorm:"not null" json:"processed_at"`                              // 处理时间
	CreatedAt   time.Time `gorm:"not null;index" json:"created_at"`
}

func (ProcessedEvent) TableName() string {
	return "processed_events"
}
