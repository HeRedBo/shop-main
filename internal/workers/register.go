package workers

import (
	"shop/internal/worker"
	"shop/pkg/global"
)

// RegisterAll 注册所有事件处理器
// 当前使用 DemoHandler 进行基础流程验证
// 验证完成后切换为 ProductHandler / OrderHandler
func RegisterAll(engine *worker.Engine) {
	registry := engine.GetRegistry()

	// ===== 验证模式：使用 DemoHandler =====
	//demo := NewDemoHandler()
	//registry.Register(worker.TopicProductEvents, demo)
	//registry.Register(worker.TopicOrderEvents, demo)
	//
	//global.LOG.Info("[Worker] Demo 处理器注册完成（验证模式）")

	// ===== 生产模式：使用真实 Handler（验证完成后启用） =====
	productHandler := NewProductHandler()
	orderHandler := NewOrderHandler()
	registry.Register(worker.TopicProductEvents, productHandler)
	registry.Register(worker.TopicOrderEvents, orderHandler)
	global.LOG.Info("[Worker] 事件处理器注册完成")
}
