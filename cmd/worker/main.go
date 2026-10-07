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

func main() {
	flag.Parse()

	// 1. 初始化基础组件（配置→日志→Redis→MySQL→KafkaProducer）
	// 注意：ConsumerGroup 由 Worker Engine 内部创建，每个 Consumer 独立实例
	bootstrap.BootstrapWith(configPath,
		bootstrap.WithKafka(), // Relay 投递 outbox 需要 Sync Producer
	)
	global.LOG.Info("[worker] 基础组件初始化完成")

	// 2. 创建 Worker Engine
	engine := worker.NewEngine(global.CONFIG.Worker)

	// 3. 注册所有 Handler
	workers.RegisterAll(engine)

	// 4. 启动引擎
	if err := engine.Start(); err != nil {
		global.LOG.Errorf("[worker] 引擎启动失败: %v", err)
		panic(err)
	}
	global.LOG.Info("[worker] Worker 服务已启动")

	// 5. 阻塞等待系统信号
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()

	global.LOG.Info("[worker] 收到退出信号，开始关闭...")

	// 6. 停止引擎
	engine.Stop()

	// 7. 清理基础资源
	bootstrap.Shutdown()

	fmt.Println("[worker] Worker 服务已安全退出")
}
