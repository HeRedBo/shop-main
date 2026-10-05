package order_report_service

import (
	"fmt"
	"time"

	"shop/pkg/global"
)

// OrderReport 订单统计报表数据结构
type OrderReport struct {
	ReportDate     string `json:"report_date"`     // 统计日期
	TotalOrders    int64  `json:"total_orders"`    // 总下单数
	PaidOrders     int64  `json:"paid_orders"`     // 已支付订单数
	UnpaidOrders   int64  `json:"unpaid_orders"`   // 未支付订单数
	PendingShip    int64  `json:"pending_ship"`    // 待发货
	PendingReceive int64  `json:"pending_receive"` // 待收货
	Completed      int64  `json:"completed"`       // 已完成
	Refunding      int64  `json:"refunding"`       // 退款中
	Refunded       int64  `json:"refunded"`        // 已退款
}

// GetOrderReport 获取指定时间范围内的订单统计报表
// 使用单条 SQL + CASE WHEN 一次性查询所有状态统计
// db: GORM 数据库实例
// startDate: 开始时间（包含）
// endDate: 结束时间（不包含）
func GetOrderReport(startDate, endDate time.Time) (*OrderReport, error) {
	var report OrderReport

	err := global.Db.Table("store_order").
		Select(`
			? as report_date,
			COUNT(*) as total_orders,
			SUM(CASE WHEN paid = 1 THEN 1 ELSE 0 END) as paid_orders,
			SUM(CASE WHEN paid = 0 THEN 1 ELSE 0 END) as unpaid_orders,
			SUM(CASE WHEN paid = 1 AND status = 0 AND refund_status = 0 THEN 1 ELSE 0 END) as pending_ship,
			SUM(CASE WHEN paid = 1 AND status = 1 AND refund_status = 0 THEN 1 ELSE 0 END) as pending_receive,
			SUM(CASE WHEN paid = 1 AND status = 3 AND refund_status = 0 THEN 1 ELSE 0 END) as completed,
			SUM(CASE WHEN refund_status = 1 THEN 1 ELSE 0 END) as refunding,
			SUM(CASE WHEN refund_status = 2 THEN 1 ELSE 0 END) as refunded
		`, startDate.Format("2006-01-02")).
		Where("create_time >= ? AND create_time < ?", startDate, endDate).
		Scan(&report).Error

	if err != nil {
		return nil, fmt.Errorf("获取订单报表失败: %w", err)
	}

	return &report, nil
}

// GetOrderReportByDate 获取指定日期的订单统计（便捷方法）
func GetOrderReportByDate(date time.Time) (*OrderReport, error) {
	start := time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, date.Location())
	end := start.AddDate(0, 0, 1)
	return GetOrderReport(start, end)
}

// GetYesterdayReport 获取昨天的订单统计（供 cron 使用）
func GetYesterdayReport() (*OrderReport, error) {
	yesterday := time.Now().AddDate(0, 0, -1)
	return GetOrderReportByDate(yesterday)
}
