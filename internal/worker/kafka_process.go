// Package worker — Kafka Process 工厂
// 封装 saramaConfig 创建、handlerTimeout 解析、默认值兜底、ConsumerGroup 创建等逻辑
// 将 cmd/worker/main.go 和 engine.go 中的重复逻辑统一提取到此工厂函数
package worker

import (
	"fmt"
	"time"

	"shop/conf"

	"github.com/IBM/sarama"
)

// NewKafkaProcesses 根据配置创建所有 Kafka Consumer Process
// 返回 []*Process，每个 Process 包装一个独立的 Consumer（含独立 ConsumerGroup）
// ConsumerGroup 创建失败时日志告警并跳过该 handler，不会整体失败
func NewKafkaProcesses(workerCfg conf.WorkerConfig, kafkaCfg conf.Kafka, registry *Registry) []*Process {
	var processes []*Process

	// 1. 创建 sarama ConsumerGroup 配置
	saramaConfig := sarama.NewConfig()
	saramaConfig.Consumer.Group.Rebalance.GroupStrategies = []sarama.BalanceStrategy{sarama.NewBalanceStrategyRoundRobin()}
	saramaConfig.Consumer.Offsets.Initial = sarama.OffsetNewest
	saramaConfig.Consumer.Offsets.AutoCommit.Enable = false // 手动提交 offset

	// 2. 解析全局 handler-timeout 配置
	handlerTimeout := ParseHandlerTimeout(workerCfg.HandlerTimeout)

	// 3. 新模式：per-Handler 独立 Consumer
	if len(workerCfg.Handlers) > 0 {
		for _, h := range workerCfg.Handlers {
			config := buildConsumerConfig(h, workerCfg, handlerTimeout)

			// 为每个 Consumer 创建独立的 ConsumerGroup
			// （sarama.ConsumerGroup 同一时间只能有一个活跃的 Consume() 调用）
			consumerGroup, err := sarama.NewConsumerGroup(kafkaCfg.Hosts, workerCfg.GroupId, saramaConfig)
			if err != nil {
				Logger().Errorf("[worker] 创建 ConsumerGroup 失败 (topic=%s): %v，跳过该 handler", h.Topic, err)
				continue
			}

			consumer := NewConsumerWithConfig(consumerGroup, config, registry)
			processName := fmt.Sprintf("kafka-%s", h.Topic)
			processes = append(processes, NewProcess(processName, "kafka", consumer))

			Logger().Infof("[worker] 创建 Kafka Consumer Process: topic=%s, concurrency=%d, buffer-size=%d",
				h.Topic, config.Concurrency, config.BufferSize)
		}
		return processes
	}

	// 4. 旧模式兼容：单 Consumer 订阅所有 topics
	if len(workerCfg.Topics) == 0 {
		Logger().Warn("[worker] 未配置任何 Handler 或 Topic，跳过 Kafka Process 创建")
		return nil
	}

	concurrency := workerCfg.Concurrency
	if concurrency <= 0 {
		concurrency = defaultConcurrency
		Logger().Warn("[worker] concurrency 未配置或为 0，使用默认值 1")
	}

	consumerGroup, err := sarama.NewConsumerGroup(kafkaCfg.Hosts, workerCfg.GroupId, saramaConfig)
	if err != nil {
		Logger().Errorf("[worker] 创建 ConsumerGroup 失败 (topics=%v): %v", workerCfg.Topics, err)
		return nil
	}

	consumer := NewConsumerWithConfig(consumerGroup, ConsumerConfig{
		Topics:         workerCfg.Topics,
		Concurrency:    concurrency,
		HandlerTimeout: handlerTimeout,
	}, registry)
	processes = append(processes, NewProcess("kafka-consumer", "kafka", consumer))

	Logger().Infof("[worker] 创建 Kafka Consumer Process（兼容模式）: topics=%v, concurrency=%d",
		workerCfg.Topics, concurrency)

	return processes
}

// buildConsumerConfig 根据 HandlerConfig + WorkerConfig 全局默认值构建 ConsumerConfig
func buildConsumerConfig(h conf.HandlerConfig, workerCfg conf.WorkerConfig, handlerTimeout time.Duration) ConsumerConfig {
	config := ConsumerConfig{
		Topic:          h.Topic,
		Concurrency:    h.Concurrency,
		BufferSize:     h.BufferSize,
		Ordered:        h.Ordered,
		HandlerTimeout: handlerTimeout,
	}

	// 使用全局默认值兜底：per-handler > worker 级 > 硬编码默认值
	if config.Concurrency <= 0 {
		config.Concurrency = workerCfg.DefaultConcurrency
		if config.Concurrency <= 0 {
			config.Concurrency = workerCfg.Concurrency
			if config.Concurrency <= 0 {
				config.Concurrency = defaultConcurrency
			}
		}
	}
	if config.BufferSize <= 0 {
		config.BufferSize = workerCfg.DefaultBufferSize
		if config.BufferSize <= 0 {
			config.BufferSize = defaultTaskBufferSize
		}
	}

	return config
}
