package worker

import "context"

// Handler 事件处理器接口
// 参数 *Message 是统一消息模型，屏蔽 Kafka/Redis 等队列差异
type Handler interface {
	Handle(ctx context.Context, msg *Message) error
}

// HandlerFunc 函数类型实现 Handler 接口，方便用闭包注册处理器
type HandlerFunc func(ctx context.Context, msg *Message) error

func (f HandlerFunc) Handle(ctx context.Context, msg *Message) error {
	return f(ctx, msg)
}
