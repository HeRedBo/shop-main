package worker

import "time"

// Middleware 是一个处理事件的中间件，接收一个 Handler 并返回一个新的 Handler
type Middleware func(Handler) Handler

// Chain 将多个 Middleware 串联，最后一个为实际 Handler
// 执行顺序：middlewares[0] → middlewares[1] → ... → h
func Chain(h Handler, middlewares ...Middleware) Handler {
	// 从后往前包装，确保执行顺序从左到右
	for i := len(middlewares) - 1; i >= 0; i-- {
		h = middlewares[i](h)
	}
	return h
}

// ─────────────────────────────────────────────────────────────
// 内置中间件工厂函数（仅声明签名，具体实现后续阶段补充）
// ─────────────────────────────────────────────────────────────

// RecoveryMiddleware panic 恢复中间件，防止单个消息处理 panic 导致 Worker goroutine 退出
func RecoveryMiddleware() Middleware {
	// TODO: 后续阶段实现
	return func(h Handler) Handler { return h }
}

// LoggingMiddleware 日志记录中间件，记录每条消息的处理耗时和结果
func LoggingMiddleware() Middleware {
	// TODO: 后续阶段实现
	return func(h Handler) Handler { return h }
}

// TimeoutMiddleware 超时控制中间件，为每条消息处理设置最大执行时间
func TimeoutMiddleware(timeout time.Duration) Middleware {
	// TODO: 后续阶段实现
	return func(h Handler) Handler { return h }
}
