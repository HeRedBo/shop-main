package models

import "time"

// DeadLetter 状态常量
const (
	DeadLetterStatusPending  = "PENDING"
	DeadLetterStatusResolved = "RESOLVED"
)

// EventDeadLetter 死信表
// 超过最大重试次数的事件记录
type EventDeadLetter struct {
	Id          int64      `gorm:"primaryKey" json:"id"`
	EventId     string     `gorm:"type:varchar(36);uniqueIndex;not null" json:"event_id"`     // UUID，消费端幂等键
	AggregateId string     `gorm:"type:varchar(64);not null" json:"aggregate_id"`              // 业务实体 ID
	EventType   string     `gorm:"type:varchar(64);not null" json:"event_type"`                // 事件类型
	Topic        string     `gorm:"type:varchar(128);not null" json:"topic"`                    // Kafka topic
	PartitionKey string     `gorm:"column:partition_key;type:varchar(64);not null;default:''" json:"partition_key"` // Kafka 分区键
	Payload      string     `gorm:"type:text;not null" json:"payload"`                          // 事件数据（JSON 字符串）
	ErrorMsg    string     `gorm:"type:text;not null" json:"error_msg"`                        // 最终错误信息
	RetryCount  int        `gorm:"not null" json:"retry_count"`                                // 已重试次数
	Status      string     `gorm:"type:varchar(16);not null;default:PENDING" json:"status"`    // PENDING/RESOLVED
	ResolvedAt  *time.Time `json:"resolved_at"`                                                // 解决时间
	CreatedAt   time.Time  `gorm:"not null" json:"created_at"`                                 // 创建时间（由业务代码设置）
	UpdatedAt   time.Time  `gorm:"not null" json:"updated_at"`                                 // 更新时间（由业务代码设置）
}

func (EventDeadLetter) TableName() string {
	return "event_dead_letter"
}
