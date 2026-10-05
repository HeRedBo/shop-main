package main

import (
	"fmt"
	"strings"
	"time"

	"shop/internal/service/order_report_service"
	"shop/pkg/logging"

	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

var (
	reportDate  string
	reportStart string
	reportEnd   string
	reportAll   bool
)

var orderReportCmd = &cobra.Command{
	Use:   "order:report",
	Short: "查询订单统计报表",
	Long:  "查询订单统计数据，支持按日期、日期范围或全量统计模式",
	Run: func(cmd *cobra.Command, args []string) {
		logger := logging.GetLogger("order_report")

		switch {
		case reportAll:
			runAllReport(logger)
		case reportDate != "":
			runDateReport(reportDate, logger)
		case reportStart != "" && reportEnd != "":
			runRangeReport(reportStart, reportEnd, logger)
		default:
			runYesterdayReport(logger)
		}
	},
}

func init() {
	rootCmd.AddCommand(orderReportCmd)
	orderReportCmd.Flags().StringVar(&reportDate, "date", "", "指定日期，格式 YYYY-MM-DD")
	orderReportCmd.Flags().StringVar(&reportStart, "start", "", "开始日期，格式 YYYY-MM-DD")
	orderReportCmd.Flags().StringVar(&reportEnd, "end", "", "结束日期，格式 YYYY-MM-DD")
	orderReportCmd.Flags().BoolVar(&reportAll, "all", false, "全量统计模式（按月汇总）")
}

// runYesterdayReport 默认模式：查询昨天的订单统计
func runYesterdayReport(logger *zap.SugaredLogger) {
	report, err := order_report_service.GetYesterdayReport()
	if err != nil {
		logger.Errorw("查询昨日订单报表失败", "error", err)
		fmt.Printf("查询失败: %v\n", err)
		return
	}
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	printReport(report, yesterday)
	logger.Infow("查询昨日订单报表成功",
		"date", yesterday,
		"total_orders", report.TotalOrders,
		"paid_orders", report.PaidOrders,
	)
}

// runDateReport 查询指定日期的订单统计
func runDateReport(dateStr string, logger *zap.SugaredLogger) {
	date, err := time.Parse("2006-01-02", dateStr)
	if err != nil {
		fmt.Printf("日期格式错误: %s，请使用 YYYY-MM-DD 格式\n", dateStr)
		return
	}

	report, err := order_report_service.GetOrderReportByDate(date)
	if err != nil {
		logger.Errorw("查询订单报表失败", "date", dateStr, "error", err)
		fmt.Printf("查询失败: %v\n", err)
		return
	}
	printReport(report, dateStr)
	logger.Infow("查询订单报表成功",
		"date", dateStr,
		"total_orders", report.TotalOrders,
		"paid_orders", report.PaidOrders,
	)
}

// runRangeReport 查询日期范围的订单统计
func runRangeReport(startStr, endStr string, logger *zap.SugaredLogger) {
	startDate, err := time.Parse("2006-01-02", startStr)
	if err != nil {
		fmt.Printf("开始日期格式错误: %s，请使用 YYYY-MM-DD 格式\n", startStr)
		return
	}
	endDate, err := time.Parse("2006-01-02", endStr)
	if err != nil {
		fmt.Printf("结束日期格式错误: %s，请使用 YYYY-MM-DD 格式\n", endStr)
		return
	}
	if endDate.Before(startDate) {
		fmt.Println("结束日期不能早于开始日期")
		return
	}

	// 按天循环统计
	fmt.Println()
	printBoxHeader(fmt.Sprintf("订单统计报表（%s ~ %s）", startStr, endStr))

	var totalReport order_report_service.OrderReport
	current := startDate
	for !current.After(endDate.AddDate(0, 0, -1)) {
		dateStr := current.Format("2006-01-02")
		report, err := order_report_service.GetOrderReportByDate(current)
		if err != nil {
			logger.Errorw("查询订单报表失败", "date", dateStr, "error", err)
			fmt.Printf("  %s: 查询失败 - %v\n", dateStr, err)
			current = current.AddDate(0, 0, 1)
			continue
		}
		totalReport.TotalOrders += report.TotalOrders
		totalReport.PaidOrders += report.PaidOrders
		totalReport.UnpaidOrders += report.UnpaidOrders
		totalReport.PendingShip += report.PendingShip
		totalReport.PendingReceive += report.PendingReceive
		totalReport.Completed += report.Completed
		totalReport.Refunding += report.Refunding
		totalReport.Refunded += report.Refunded

		fmt.Printf("║  %-12s  总单: %-6d 已支付: %-6d 未支付: %-6d 已完成: %-6d ║\n",
			dateStr, report.TotalOrders, report.PaidOrders, report.UnpaidOrders, report.Completed)
		current = current.AddDate(0, 0, 1)
	}
	printBoxFooter()

	fmt.Println()
	fmt.Println("── 汇总 ──")
	printReport(&totalReport, fmt.Sprintf("%s ~ %s", startStr, endStr))

	logger.Infow("查询范围订单报表成功",
		"start", startStr,
		"end", endStr,
		"total_orders", totalReport.TotalOrders,
		"paid_orders", totalReport.PaidOrders,
	)
}

// runAllReport 全量统计模式：按月汇总
func runAllReport(logger *zap.SugaredLogger) {
	fmt.Println()
	fmt.Println("══ 全量统计模式（按月汇总） ══")
	fmt.Println()

	// 从系统上线年份开始（按实际业务设定）
	startYear := 2020
	startMonth := time.January
	now := time.Now()

	var totalReport order_report_service.OrderReport
	year := startYear
	month := startMonth

	for {
		monthStart := time.Date(year, month, 1, 0, 0, 0, 0, time.Local)
		monthEnd := monthStart.AddDate(0, 1, 0)

		if monthStart.After(now) {
			break
		}

		monthStr := monthStart.Format("2006-01")
		report, err := order_report_service.GetOrderReport(monthStart, monthEnd)
		if err != nil {
			logger.Errorw("查询月度订单报表失败", "month", monthStr, "error", err)
			fmt.Printf("  %s: 查询失败 - %v\n", monthStr, err)
			month = month + 1
			if month > 12 {
				month = 1
				year++
			}
			continue
		}

		if report.TotalOrders > 0 {
			fmt.Printf("  %s  总单: %-6d 已支付: %-6d 未支付: %-6d 已完成: %-6d\n",
				monthStr, report.TotalOrders, report.PaidOrders, report.UnpaidOrders, report.Completed)
		}

		totalReport.TotalOrders += report.TotalOrders
		totalReport.PaidOrders += report.PaidOrders
		totalReport.UnpaidOrders += report.UnpaidOrders
		totalReport.PendingShip += report.PendingShip
		totalReport.PendingReceive += report.PendingReceive
		totalReport.Completed += report.Completed
		totalReport.Refunding += report.Refunding
		totalReport.Refunded += report.Refunded

		month = month + 1
		if month > 12 {
			month = 1
			year++
		}
	}

	fmt.Println()
	fmt.Println("── 总计 ──")
	printReport(&totalReport, "全量汇总")

	logger.Infow("全量统计完成",
		"total_orders", totalReport.TotalOrders,
		"paid_orders", totalReport.PaidOrders,
	)
}

// printReport 打印单个统计报表
func printReport(report *order_report_service.OrderReport, dateLabel string) {
	fmt.Println()
	printBoxHeader(fmt.Sprintf("订单统计报表  统计日期: %s", dateLabel))
	fmt.Printf("║  总下单数:    %-24d ║\n", report.TotalOrders)
	fmt.Printf("║  已支付:      %-24d ║\n", report.PaidOrders)
	fmt.Printf("║  未支付:      %-24d ║\n", report.UnpaidOrders)
	fmt.Printf("║  待发货:      %-24d ║\n", report.PendingShip)
	fmt.Printf("║  待收货:      %-24d ║\n", report.PendingReceive)
	fmt.Printf("║  已完成:      %-24d ║\n", report.Completed)
	fmt.Printf("║  退款中:      %-24d ║\n", report.Refunding)
	fmt.Printf("║  已退款:      %-24d ║\n", report.Refunded)
	printBoxFooter()
}

func printBoxHeader(title string) {
	width := 40
	fmt.Printf("╔%s╗\n", strings.Repeat("═", width))
	titleRunes := []rune(title)
	padding := (width - len(titleRunes)) / 2
	if padding < 0 {
		padding = 0
	}
	fmt.Printf("║%s%s%s║\n", strings.Repeat(" ", padding), title, strings.Repeat(" ", width-padding-len(titleRunes)))
	fmt.Printf("╠%s╣\n", strings.Repeat("═", width))
}

func printBoxFooter() {
	fmt.Printf("╚%s╝\n", strings.Repeat("═", 40))
}
