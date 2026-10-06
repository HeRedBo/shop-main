package worker

import (
	"context"
	"sync"
)

// Registry 事件处理器注册表
// key 为 topic 名称，value 为对应的 Handler
type Registry struct {
	mu       sync.RWMutex
	handlers map[string]Handler // key: topic 或 event_type
}

// NewRegistry 创建一个新的 Handler 注册表
func NewRegistry() *Registry {
	return &Registry{
		handlers: make(map[string]Handler),
	}
}

// Register 注册一个 Handler 处理指定 topic 的消息
func (r *Registry) Register(topic string, handler Handler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handlers[topic] = handler
}

// RegisterFunc 以函数形式注册 Handler，方便用闭包快速注册
func (r *Registry) RegisterFunc(topic string, fn func(ctx context.Context, event *Event) error) {
	r.Register(topic, HandlerFunc(fn))
}

// GetHandler 获取指定 topic 的 Handler，第二个返回值表示是否找到
func (r *Registry) GetHandler(topic string) (Handler, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	h, ok := r.handlers[topic]
	return h, ok
}
