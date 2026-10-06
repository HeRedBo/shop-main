package workers

import (
	"shop/internal/worker"
	"shop/pkg/global"
)

// RegisterAll 注册所有事件处理器到 Worker Engine
// 按 topic 注册对应的业务 Handler，替代 DemoHandler
func RegisterAll(engine *worker.Engine) {
	registry := engine.GetRegistry()

	productHandler := NewProductHandler()
	orderHandler := NewOrderHandler()

	// 按 topic 注册对应的 Handler
	registry.Register(worker.TopicProductEvents, productHandler)
	registry.Register(worker.TopicOrderEvents, orderHandler)

	global.LOG.Infof("[Worker] 事件处理器注册完成，topics: [%s, %s]", worker.TopicProductEvents, worker.TopicOrderEvents)
}
