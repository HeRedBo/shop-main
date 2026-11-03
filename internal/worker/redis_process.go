// Package worker — Redis Stream Consumer Process 工厂
// 封装 RedisStreamConsumer 的创建逻辑，Redis 不可用或未配置时返回 nil
package worker

import (
	"fmt"
	"time"

	"shop/conf"
	"shop/pkg/global"
)

// NewRedisProcesses 根据配置创建所有 Redis Stream Consumer Process
// 每个 Stream 配置对应一个独立的 RedisStreamConsumer（与 Kafka per-handler 模式一致）
// 如果 Redis 不可用或未配置则返回空切片
func NewRedisProcesses(workerCfg conf.WorkerConfig, registry *Registry) []*Process {
	// 1. 检查 Redis 客户端是否可用
	if global.RedisClient == nil {
		Logger().Warn("[worker] Redis 客户端未初始化，跳过 Redis Stream Consumer 创建")
		return nil
	}

	redisCfg := workerCfg.RedisStream
	if !redisCfg.Enabled {
		Logger().Info("[worker] Redis Stream Consumer 未启用，跳过")
		return nil
	}

	if len(redisCfg.Streams) == 0 {
		Logger().Warn("[worker] Redis Stream Consumer 已启用但未配置任何 Stream，跳过")
		return nil
	}

	// 2. 解析全局默认值
	handlerTimeout := ParseHandlerTimeout(redisCfg.HandlerTimeout)
	if handlerTimeout <= 0 {
		handlerTimeout = ParseHandlerTimeout(workerCfg.HandlerTimeout)
	}

	blockTimeout := parseBlockTimeout(redisCfg.BlockTimeout)
	maxRetries := redisCfg.MaxRetries
	if maxRetries <= 0 {
		maxRetries = 5
	}

	globalConcurrency := redisCfg.Concurrency
	if globalConcurrency <= 0 {
		globalConcurrency = workerCfg.DefaultConcurrency
		if globalConcurrency <= 0 {
			globalConcurrency = workerCfg.Concurrency
			if globalConcurrency <= 0 {
				globalConcurrency = defaultConcurrency
			}
		}
	}

	globalBufferSize := redisCfg.BufferSize
	if globalBufferSize <= 0 {
		globalBufferSize = workerCfg.DefaultBufferSize
		if globalBufferSize <= 0 {
			globalBufferSize = defaultTaskBufferSize
		}
	}

	// 3. 为每个 Stream 创建独立的 RedisStreamConsumer
	var processes []*Process
	for _, streamCfg := range redisCfg.Streams {
		consumerCfg := buildRedisConsumerConfig(streamCfg, globalConcurrency, globalBufferSize, blockTimeout, handlerTimeout, maxRetries)

		consumer := NewRedisStreamConsumer(global.RedisClient, consumerCfg, registry)
		processName := fmt.Sprintf("redis-%s", streamCfg.StreamKey)
		processes = append(processes, NewProcess(processName, "redis", consumer))

		Logger().Infof("[worker] 创建 Redis Stream Consumer Process: stream=%s, group=%s, consumer=%s, concurrency=%d, buffer-size=%d",
			streamCfg.StreamKey, streamCfg.ConsumerGroup, streamCfg.ConsumerName,
			consumerCfg.Concurrency, consumerCfg.BufferSize)
	}

	return processes
}

// buildRedisConsumerConfig 根据 RedisStreamItemConfig + 全局默认值构建 RedisConsumerConfig
func buildRedisConsumerConfig(
	streamCfg conf.RedisStreamItemConfig,
	globalConcurrency, globalBufferSize int,
	blockTimeout, handlerTimeout time.Duration,
	maxRetries int,
) RedisConsumerConfig {
	concurrency := streamCfg.Concurrency
	if concurrency <= 0 {
		concurrency = globalConcurrency
	}

	bufferSize := streamCfg.BufferSize
	if bufferSize <= 0 {
		bufferSize = globalBufferSize
	}

	return RedisConsumerConfig{
		StreamKey:      streamCfg.StreamKey,
		ConsumerGroup:  streamCfg.ConsumerGroup,
		ConsumerName:   streamCfg.ConsumerName,
		Concurrency:    concurrency,
		BufferSize:     bufferSize,
		BlockTimeout:   blockTimeout,
		HandlerTimeout: handlerTimeout,
		MaxRetries:     maxRetries,
	}
}

// parseBlockTimeout 解析 XReadGroup BLOCK 超时配置
func parseBlockTimeout(timeoutStr string) time.Duration {
	if timeoutStr == "" {
		return 5 * time.Second
	}
	d, err := time.ParseDuration(timeoutStr)
	if err != nil {
		Logger().Warnf("[worker] redis-stream block-timeout 解析失败，使用默认值 5s: %v", err)
		return 5 * time.Second
	}
	if d <= 0 {
		return 5 * time.Second
	}
	return d
}
