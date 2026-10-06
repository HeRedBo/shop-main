package worker

import "context"

// Event 表示一条消费的事件
type Event struct {
	Topic     string            // 消息所属 Topic
	Key       string            // 消息 Key
	Value     []byte            // 消息体
	Headers   map[string]string // 消息头
	EventType string            // 从 payload 或 header 中解析的事件类型
	Offset    int64             // Kafka Offset
	Partition int32             // Kafka Partition
}

// Handler 事件处理器接口
type Handler interface {
	Handle(ctx context.Context, event *Event) error
}

// HandlerFunc 函数类型实现 Handler 接口，方便用闭包注册处理器
type HandlerFunc func(ctx context.Context, event *Event) error

func (f HandlerFunc) Handle(ctx context.Context, event *Event) error {
	return f(ctx, event)
}
