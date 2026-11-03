package models

import "time"

// Outbox 状态常量
const (
	OutboxStatusWait    = "WAIT"
	OutboxStatusSending = "SENDING"
	OutboxStatusSent    = "SENT"
	OutboxStatusDead    = "DEAD"
)

// Queue Type 常量
const (
	QueueTypeKafka = "kafka"
	QueueTypeRedis = "redis"
)

// EventOutbox 事务发件箱表
// 用于 Transactional Outbox 模式：业务操作与事件记录写入同一个数据库事务
type EventOutbox struct {
	Id           int64      `gorm:"primaryKey" json:"id"`
	EventId      string     `gorm:"type:varchar(36);uniqueIndex;not null" json:"event_id"`                          // UUID，消费端幂等键
	AggregateId  string     `gorm:"type:varchar(64);index;not null" json:"aggregate_id"`                            // 业务实体 ID（如订单号）
	EventType    string     `gorm:"type:varchar(64);index;not null" json:"event_type"`                              // 事件类型（如 order.created）
	Topic        string     `gorm:"type:varchar(128);not null" json:"topic"`                                        // Kafka topic / Redis stream key
	PartitionKey string     `gorm:"column:partition_key;type:varchar(64);not null;default:''" json:"partition_key"` // Kafka 分区键 / Redis 路由键
	Payload      string     `gorm:"type:text;not null" json:"payload"`                                              // 事件数据（JSON 字符串）
	QueueType    string     `gorm:"type:varchar(16);not null;default:'kafka';index" json:"queue_type"`              // 目标队列类型: "kafka" / "redis"
	Status       string     `gorm:"type:varchar(16);not null;default:WAIT;index" json:"status"`                     // WAIT/SENDING/SENT/DEAD
	RetryCount   int        `gorm:"not null;default:0" json:"retry_count"`                                          // 重试次数
	MaxRetries   int        `gorm:"not null;default:5" json:"max_retries"`                                          // 最大重试次数
	ErrorMsg     string     `gorm:"type:text" json:"error_msg"`                                                     // 最近一次错误信息
	SendingAt    *time.Time `gorm:"index" json:"sending_at"`                                                        // 开始投递时间（租约）
	ExecuteAt    *time.Time `gorm:"index" json:"execute_at"`                                                        // 延迟执行时间（Redis 延迟队列专用，Kafka 为 NULL）
	CreatedAt    time.Time  `gorm:"not null;index" json:"created_at"`                                               // 创建时间（由业务代码设置）
	UpdatedAt    time.Time  `gorm:"not null" json:"updated_at"`                                                     // 更新时间（由业务代码设置）
}

func (EventOutbox) TableName() string {
	return "event_outbox"
}
