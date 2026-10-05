package cron

import (
	"time"

	"gorm.io/gorm"

	"shop/internal/models"
	"shop/pkg/global"
	"shop/pkg/logging"
)

// OrderReportJob 每日订单统计任务
// 查询前一天（昨天 00:00:00 ~ 23:59:59）的订单数据，统计各状态数量并写入 order_report 日志
func OrderReportJob() {
	now := time.Now()
	yesterday := now.AddDate(0, 0, -1)
	startOfDay := time.Date(yesterday.Year(), yesterday.Month(), yesterday.Day(), 0, 0, 0, 0, now.Location())
	endOfDay := startOfDay.Add(24*time.Hour - time.Second)

	// 时间范围条件：昨天创建的所有订单（is_del 由 GORM soft_delete 插件自动过滤）
	timeCondition := "create_time BETWEEN ? AND ?"

	// 每次查询使用独立的 Model 实例，避免 GORM 链式调用累积 WHERE 条件
	model := func() *gorm.DB {
		return global.Db.Model(&models.StoreOrder{})
	}

	// 总下单数
	var totalOrders int64
	model().Where(timeCondition, startOfDay, endOfDay).Count(&totalOrders)

	// 已支付订单数
	var paidOrders int64
	model().Where(timeCondition, startOfDay, endOfDay).Where("paid = ?", 1).Count(&paidOrders)

	// 未支付订单数
	var unpaidOrders int64
	model().Where(timeCondition, startOfDay, endOfDay).Where("paid = ?", 0).Count(&unpaidOrders)

	// 待发货：已支付 && status=0 && refund_status=0
	var pendingShip int64
	model().Where(timeCondition, startOfDay, endOfDay).
		Where("paid = ? AND status = ? AND refund_status = ?", 1, 0, 0).Count(&pendingShip)

	// 待收货：已支付 && status=1 && refund_status=0
	var pendingReceive int64
	model().Where(timeCondition, startOfDay, endOfDay).
		Where("paid = ? AND status = ? AND refund_status = ?", 1, 1, 0).Count(&pendingReceive)

	// 已完成：已支付 && status=3 && refund_status=0
	var completed int64
	model().Where(timeCondition, startOfDay, endOfDay).
		Where("paid = ? AND status = ? AND refund_status = ?", 1, 3, 0).Count(&completed)

	// 退款中：refund_status=1
	var refunding int64
	model().Where(timeCondition, startOfDay, endOfDay).
		Where("refund_status = ?", 1).Count(&refunding)

	// 已退款：refund_status=2
	var refunded int64
	model().Where(timeCondition, startOfDay, endOfDay).
		Where("refund_status = ?", 2).Count(&refunded)

	// 组装统计结果
	type OrderReport struct {
		Date           string `json:"date"`            // 统计日期
		TotalOrders    int64  `json:"total_orders"`    // 总下单数
		PaidOrders     int64  `json:"paid_orders"`     // 已支付
		UnpaidOrders   int64  `json:"unpaid_orders"`   // 未支付
		PendingShip    int64  `json:"pending_ship"`    // 待发货
		PendingReceive int64  `json:"pending_receive"` // 待收货
		Completed      int64  `json:"completed"`       // 已完成
		Refunding      int64  `json:"refunding"`       // 退款中
		Refunded       int64  `json:"refunded"`        // 已退款
	}

	report := OrderReport{
		Date:           yesterday.Format("2006-01-02"),
		TotalOrders:    totalOrders,
		PaidOrders:     paidOrders,
		UnpaidOrders:   unpaidOrders,
		PendingShip:    pendingShip,
		PendingReceive: pendingReceive,
		Completed:      completed,
		Refunding:      refunding,
		Refunded:       refunded,
	}

	logger := logging.GetLogger("order_report")
	logger.Infow("每日订单统计",
		"date", report.Date,
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
