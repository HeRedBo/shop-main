package worker

import "context"

// Runnable 通用任务抽象接口
// 屏蔽不同任务类型（队列消费、定时任务、监控上报等）的差异，统一生命周期管理
// 任何需要长期运行的守护任务都可以实现此接口
type Runnable interface {
	// Name 返回任务名称，用于日志标识
	Name() string

	// Start 启动任务
	// ctx 用于控制生命周期，收到取消信号后应停止运行
	Start(ctx context.Context) error

	// Stop 优雅关闭
	// 应等待当前处理完毕后再返回
	Stop() error
}
