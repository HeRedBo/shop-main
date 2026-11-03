package main

import (
	"context"
	"flag"
	"fmt"
	"os/signal"
	"syscall"

	"shop/internal/bootstrap"
	"shop/internal/worker"
	"shop/internal/workers"
	"shop/pkg/global"
)

var configPath string

func init() {
	flag.StringVar(&configPath, "config", bootstrap.DefaultConfigPath, "配置文件路径")
}

// Worker 入口 - ProcessManager 统一管理模式
//
// 架构说明：
//   main → bootstrap → ProcessManager → 统一管理 Kafka/Redis/Relay/Scheduler
//   Handler 注册：workers.RegisterAllToManager(pm) → pm.GetRegistry()
//   各 Process 通过工厂函数创建，内部封装所有配置和资源管理细节
//
// Process 类型：
//   Kafka Consumer   — 从 Kafka topic 拉取消息，双缓冲架构（Poller + Worker Pool）
//   Redis Consumer   — 从 Redis Stream 拉取消息，双缓冲架构（与 Kafka Consumer 一致）
//   Relay            — 扫描 event_outbox 表，投递消息到 Kafka/Redis（通知驱动 + Ticker 兜底）
//   DelayScheduler   — 基于 Redis Sorted Set 的延迟调度器（主调度路径）

func main() {
	// 1. 解析命令行参数
	flag.Parse()

	// 2. 基础组件初始化（配置→日志→Redis→MySQL→KafkaProducer）
	bootstrap.BootstrapWith(configPath,
		bootstrap.WithKafka(), // Relay 投递 outbox 需要 Sync Producer
	)
	defer bootstrap.Shutdown()

	logger := global.GetLogger("worker")

	// 3. Worker 配置检查
	workerCfg := global.CONFIG.Worker
	if !workerCfg.Enabled {
		logger.Info("[worker] Worker 模块未启用，跳过")
		return
	}

	// 4. 创建 ProcessManager（传入共享的 Registry）
	registry := worker.NewRegistry()
	pm := worker.NewProcessManager(registry)

	// 5. 注册所有 Handler 到 Registry
	workers.RegisterAllToManager(pm)

	// 6. 声明式添加 Process（工厂内部封装所有创建细节）
	pm.AddProcesses(worker.NewKafkaProcesses(workerCfg, global.CONFIG.Kafka, registry)...)
	pm.AddProcesses(worker.NewRedisProcesses(workerCfg, registry)...)
	pm.AddProcess(worker.NewRelayProcess(workerCfg))
	pm.AddProcess(worker.NewSchedulerProcess())

	// 7. 启动 ProcessManager（统一管理所有 Process：Kafka/Redis Consumer + Relay + DelayScheduler）
	if err := pm.Start(); err != nil {
		logger.Fatalf("[worker] ProcessManager 启动失败: %v", err)
	}
	logger.Info("[worker] Worker 服务已启动（ProcessManager 模式）")

	// 8. 阻塞等待系统信号
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()

	// 9. 优雅关闭（逆序关闭所有 Process：DelayScheduler → Relay → Redis Consumer → Kafka Consumer）
	logger.Info("[worker] 收到退出信号，开始关闭...")
	pm.Stop()

	fmt.Println("[worker] Worker 服务已安全退出")
}
