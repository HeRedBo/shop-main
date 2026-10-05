package main

import (
	"context"
	"flag"
	"os/signal"
	"syscall"

	"shop/internal/bootstrap"
	"shop/internal/cron"
	"shop/pkg/global"
)

var configPath string

func init() {
	flag.StringVar(&configPath, "config", bootstrap.DefaultConfigPath, "配置文件路径")
}

func main() {
	flag.Parse()

	// 1. 初始化全部基础组件（配置→日志→Redis→MySQL→Casbin→观察者→JWT→Kafka）
	bootstrap.BootstrapWith(configPath,
		bootstrap.WithCasbin(),
		bootstrap.WithObserver(),
		bootstrap.WithJWT(),
		bootstrap.WithKafka(),
	)
	global.LOG.Info("[cron] 基础组件初始化完成")

	// 2. 创建定时任务注册表
	registry := cron.NewRegistry()

	// 注册定时任务
	registry.Register("每日订单统计", "0 0 0 * * *", cron.OrderReportJob)
	registry.Register("每日订单统计", "0 0 1 * * *", cron.OrderStatsJob)

	// 3. 启动调度器
	registry.Start()
	global.LOG.Info("[cron] 定时任务服务已就绪")

	// 4. 阻塞等待系统信号
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()

	global.LOG.Info("[cron] 收到退出信号，开始关闭...")

	// 5. 停止调度器
	registry.Stop()

	// 6. 清理基础资源
	bootstrap.Shutdown()

	global.LOG.Info("[cron] 定时任务服务已退出")
}
