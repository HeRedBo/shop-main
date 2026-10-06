package workers

import (
	"shop/internal/worker"
	"shop/pkg/global"
)

// RegisterAll 注册所有事件处理器到 Worker Engine
// 按 topic 注册对应的业务 Handler
func RegisterAll(engine *worker.Engine) {
	registry := engine.GetRegistry()

	// 注册商品事件 Handler
	registry.Register(worker.TopicProductEvents, NewProductHandler())

	// 注册订单事件 Handler
	registry.Register(worker.TopicOrderEvents, NewOrderHandler())

	global.LOG.Infof("[Worker] 事件处理器注册完成，topics: [%s, %s]", worker.TopicProductEvents, worker.TopicOrderEvents)
}
