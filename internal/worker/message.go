package worker

import "time"

// Message 统一消息模型，屏蔽不同队列（Kafka/Redis）的差异
type Message struct {
	// 通用字段
	Source    string            // 来源标识: "kafka" / "redis"
	Topic     string            // 统一术语 (Kafka topic / Redis stream key)
	Key       string            // 路由键
	Value     []byte            // 消息体
	Headers   map[string]string // 元数据 (event_id, event_type, aggregate_id 等)
	EventType string            // 事件类型（从 Headers 或 payload 中解析）

	// 队列特有字段（按需使用）
	Delay time.Duration // 延迟时间（Redis 特有，Kafka 为 0）

	// 原始偏移量（用于 ACK，不同队列类型不同）
	RawOffset interface{} // Kafka: int64 offset, Redis: Stream ID string
}

// Event 保留为 Message 的类型别名，向后兼容
type Event = Message
