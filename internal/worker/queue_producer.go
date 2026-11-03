package worker

import "context"

// QueueProducer 队列生产者抽象接口
// 屏蔽不同队列（Kafka/Redis）的投递差异
type QueueProducer interface {
	// Produce 投递单条消息到队列
	Produce(ctx context.Context, msg *Message) error

	// ProduceBatch 批量投递消息
	ProduceBatch(ctx context.Context, msgs []*Message) error

	// Close 关闭生产者，释放资源
	Close() error
}
