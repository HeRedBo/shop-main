package cron

import (
	"shop/internal/service/order_report_service"
	"shop/pkg/logging"
)

// OrderStatsJob 每日订单统计定时任务
// 每天凌晨执行，统计前一天的订单数据
func OrderStatsJob() {
	logger := logging.GetLogger("order_report")

	report, err := order_report_service.GetYesterdayReport()
	if err != nil {
		logger.Errorw("订单统计任务执行失败", "error", err)
		return
	}

	logger.Infow("每日订单统计",
		"date", report.ReportDate,
		"total_orders", report.TotalOrders,
		"paid_orders", report.PaidOrders,
		"unpaid_orders", report.UnpaidOrders,
		"pending_ship", report.PendingShip,
		"pending_receive", report.PendingReceive,
		"completed", report.Completed,
		"refunding", report.Refunding,
		"refunded", report.Refunded,
	)
}
