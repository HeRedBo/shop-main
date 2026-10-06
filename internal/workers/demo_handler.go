package workers

import (
	"context"

	"shop/internal/worker"
	"shop/pkg/global"
)

// DemoHandler 是一个简单的 Kafka 消费演示处理器
// 用于验证 Worker 常驻进程能正常连接 Kafka 并消费消息
type DemoHandler struct{}

// NewDemoHandler 创建 DemoHandler 实例
func NewDemoHandler() *DemoHandler {
	return &DemoHandler{}
}

// Handle 实现 worker.Handler 接口
// 收到消息后打印日志，包含 topic、key、消息长度等信息
func (h *DemoHandler) Handle(ctx context.Context, event *worker.Event) error {
	value := string(event.Value)

	// 截取前 200 个字符，避免日志过长
	preview := value
	if len(preview) > 200 {
		preview = preview[:200] + "..."
	}

	global.LOG.Infof("[DemoHandler] 收到消息 topic=%s key=%s partition=%d offset=%d length=%d value=%s",
		event.Topic,
		event.Key,
		event.Partition,
		event.Offset,
		len(event.Value),
		preview,
	)

	// 尝试判断是否为 JSON（以 { 或 [ 开头），格式化输出
	if len(value) > 0 {
		firstChar := value[0]
		if firstChar == '{' || firstChar == '[' {
			global.LOG.Debugf("[DemoHandler] JSON 消息原文长度: %d", len(value))
		}
	}

	return nil
}
