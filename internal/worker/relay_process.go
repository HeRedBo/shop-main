// Package worker — Relay Process 工厂
// 封装 Relay 的创建逻辑，将配置解析和实例化集中管理
package worker

import (
	"shop/conf"
)

// NewRelayProcess 创建 Relay Process
// Relay 负责扫描 event_outbox 表并投递消息到 Kafka/Redis
func NewRelayProcess(workerCfg conf.WorkerConfig) *Process {
	relayConfig := ParseRelayConfig(workerCfg)
	relay := NewRelay(relayConfig)
	return NewProcess("relay", "relay", relay)
}
