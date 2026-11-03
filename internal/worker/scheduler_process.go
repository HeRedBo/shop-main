// Package worker — DelayScheduler Process 工厂
// 封装 DelayScheduler 的创建逻辑，Redis 不可用时返回 nil
package worker

import (
	"shop/pkg/global"
)

// NewSchedulerProcess 创建 DelayScheduler Process
// 基于 Redis Sorted Set 的延迟调度器，作为延迟消息的主调度路径
// 如果 Redis 客户端不可用则返回 nil（降级为 DB Relay 兜底）
func NewSchedulerProcess() *Process {
	if global.RedisClient == nil {
		Logger().Warn("[worker] Redis 客户端未初始化，跳过 DelayScheduler 创建（依赖 DB Relay 兜底）")
		return nil
	}

	scheduler := NewDelayScheduler(global.RedisClient)
	return NewProcess("delay-scheduler", "scheduler", scheduler)
}
