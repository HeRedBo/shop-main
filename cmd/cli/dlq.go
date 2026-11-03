package main

import (
	"fmt"
	"time"

	"shop/internal/models"
	"shop/pkg/global"

	"github.com/spf13/cobra"
)

var (
	dlqTopic  string
	dlqAll    bool
	dlqDays   int
	dlqLimit  int
	dlqStatus string
)

var dlqCmd = &cobra.Command{
	Use:   "dlq",
	Short: "死信队列管理",
	Long:  "管理死信队列（Dead Letter Queue），支持查看、重试、丢弃等操作",
}

var dlqListCmd = &cobra.Command{
	Use:   "list",
	Short: "查看死信列表",
	Run: func(cmd *cobra.Command, args []string) {
		runDlqList()
	},
}

var dlqRetryCmd = &cobra.Command{
	Use:   "retry [id]",
	Short: "重试死信消息",
	Args:  cobra.MaximumNArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		if dlqAll {
			runDlqRetryAll()
		} else if len(args) > 0 {
			runDlqRetry(args[0])
		} else {
			fmt.Println("请指定死信 ID 或使用 --all 重试全部")
			return
		}
	},
}

var dlqDiscardCmd = &cobra.Command{
	Use:   "discard [id]",
	Short: "丢弃死信消息",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		runDlqDiscard(args[0])
	},
}

var dlqCleanCmd = &cobra.Command{
	Use:   "clean",
	Short: "清理过期死信记录",
	Run: func(cmd *cobra.Command, args []string) {
		runDlqClean()
	},
}

var dlqStatsCmd = &cobra.Command{
	Use:   "stats",
	Short: "死信队列统计",
	Run: func(cmd *cobra.Command, args []string) {
		runDlqStats()
	},
}

func init() {
	rootCmd.AddCommand(dlqCmd)
	dlqCmd.AddCommand(dlqListCmd)
	dlqCmd.AddCommand(dlqRetryCmd)
	dlqCmd.AddCommand(dlqDiscardCmd)
	dlqCmd.AddCommand(dlqCleanCmd)
	dlqCmd.AddCommand(dlqStatsCmd)

	// dlq list flags
	dlqListCmd.Flags().StringVar(&dlqTopic, "topic", "", "按 topic 筛选")
	dlqListCmd.Flags().StringVar(&dlqStatus, "status", "PENDING", "按状态筛选 (PENDING/RESOLVED)")
	dlqListCmd.Flags().IntVar(&dlqLimit, "limit", 20, "显示条数")

	// dlq retry flags
	dlqRetryCmd.Flags().BoolVar(&dlqAll, "all", false, "重试全部 PENDING 死信")
	dlqRetryCmd.Flags().StringVar(&dlqTopic, "topic", "", "按 topic 筛选（配合 --all 使用）")

	// dlq clean flags
	dlqCleanCmd.Flags().IntVar(&dlqDays, "days", 30, "清理多少天前的 RESOLVED 记录")
}

// runDlqList 查看死信列表
func runDlqList() {
	query := global.Db.Model(&models.EventDeadLetter{})

	if dlqTopic != "" {
		query = query.Where("topic = ?", dlqTopic)
	}
	if dlqStatus != "" {
		query = query.Where("status = ?", dlqStatus)
	}

	var letters []models.EventDeadLetter
	if err := query.Order("created_at DESC").Limit(dlqLimit).Find(&letters).Error; err != nil {
		fmt.Printf("查询死信列表失败: %v\n", err)
		return
	}

	if len(letters) == 0 {
		fmt.Println("没有找到死信记录")
		return
	}

	fmt.Println()
	fmt.Printf("%-36s  %-20s  %-30s  %-8s  %-10s  %s\n",
		"EVENT_ID", "TOPIC", "AGGREGATE_ID", "RETRIES", "STATUS", "CREATED_AT")
	fmt.Println("────────────────────────────────────  ────────────────────  ──────────────────────────────  ────────  ──────────  ──────────────────")

	for _, l := range letters {
		fmt.Printf("%-36s  %-20s  %-30s  %-8d  %-10s  %s\n",
			l.EventId, truncateStr(l.Topic, 20), truncateStr(l.AggregateId, 30),
			l.RetryCount, l.Status, l.CreatedAt.Format("2006-01-02 15:04"))
	}
	fmt.Printf("\n共 %d 条记录\n", len(letters))
}

// runDlqRetry 重试单条死信
func runDlqRetry(id string) {
	var letter models.EventDeadLetter
	if err := global.Db.Where("event_id = ? AND status = ?", id, models.DeadLetterStatusPending).First(&letter).Error; err != nil {
		fmt.Printf("未找到 PENDING 状态的死信: id=%s\n", id)
		return
	}

	// 重新写入 event_outbox，由 Relay 重新投递
	now := time.Now()
	outbox := models.EventOutbox{
		EventId:      letter.EventId,
		AggregateId:  letter.AggregateId,
		EventType:    letter.EventType,
		Topic:        letter.Topic,
		PartitionKey: letter.PartitionKey,
		Payload:      letter.Payload,
		QueueType:    models.QueueTypeKafka, // 重试默认走 Kafka
		Status:       models.OutboxStatusWait,
		MaxRetries:   5,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := global.Db.Create(&outbox).Error; err != nil {
		fmt.Printf("重试写入 outbox 失败: %v\n", err)
		return
	}

	// 更新死信状态为 RESOLVED
	global.Db.Model(&letter).Updates(map[string]interface{}{
		"status":      models.DeadLetterStatusResolved,
		"resolved_at": now,
		"updated_at":  now,
	})

	fmt.Printf("死信已重新投递: event_id=%s, topic=%s\n", letter.EventId, letter.Topic)
}

// runDlqRetryAll 重试全部 PENDING 死信
func runDlqRetryAll() {
	query := global.Db.Model(&models.EventDeadLetter{}).Where("status = ?", models.DeadLetterStatusPending)
	if dlqTopic != "" {
		query = query.Where("topic = ?", dlqTopic)
	}

	var letters []models.EventDeadLetter
	query.Find(&letters)

	if len(letters) == 0 {
		fmt.Println("没有 PENDING 状态的死信")
		return
	}

	successCount := 0
	for _, l := range letters {
		now := time.Now()
		outbox := models.EventOutbox{
			EventId:      l.EventId,
			AggregateId:  l.AggregateId,
			EventType:    l.EventType,
			Topic:        l.Topic,
			PartitionKey: l.PartitionKey,
			Payload:      l.Payload,
			QueueType:    models.QueueTypeKafka,
			Status:       models.OutboxStatusWait,
			MaxRetries:   5,
			CreatedAt:    now,
			UpdatedAt:    now,
		}
		if err := global.Db.Create(&outbox).Error; err != nil {
			fmt.Printf("重试失败 event_id=%s: %v\n", l.EventId, err)
			continue
		}
		global.Db.Model(&l).Updates(map[string]interface{}{
			"status":      models.DeadLetterStatusResolved,
			"resolved_at": now,
			"updated_at":  now,
		})
		successCount++
	}

	fmt.Printf("批量重试完成: 成功 %d/%d\n", successCount, len(letters))
}

// runDlqDiscard 丢弃死信
func runDlqDiscard(id string) {
	now := time.Now()
	result := global.Db.Model(&models.EventDeadLetter{}).
		Where("event_id = ? AND status = ?", id, models.DeadLetterStatusPending).
		Updates(map[string]interface{}{
			"status":      models.DeadLetterStatusResolved,
			"resolved_at": now,
			"updated_at":  now,
		})
	if result.Error != nil {
		fmt.Printf("丢弃失败: %v\n", result.Error)
		return
	}
	if result.RowsAffected == 0 {
		fmt.Printf("未找到 PENDING 状态的死信: id=%s\n", id)
		return
	}
	fmt.Printf("死信已丢弃: id=%s\n", id)
}

// runDlqClean 清理过期死信
func runDlqClean() {
	threshold := time.Now().AddDate(0, 0, -dlqDays)
	result := global.Db.Where("status = ? AND resolved_at < ?", models.DeadLetterStatusResolved, threshold).
		Delete(&models.EventDeadLetter{})
	if result.Error != nil {
		fmt.Printf("清理失败: %v\n", result.Error)
		return
	}
	fmt.Printf("清理完成: 删除 %d 条过期死信记录（%d 天前已解决）\n", result.RowsAffected, dlqDays)
}

// runDlqStats 死信统计
func runDlqStats() {
	type TopicStats struct {
		Topic  string
		Status string
		Count  int64
	}

	var stats []TopicStats
	global.Db.Model(&models.EventDeadLetter{}).
		Select("topic, status, COUNT(*) as count").
		Group("topic, status").
		Scan(&stats)

	if len(stats) == 0 {
		fmt.Println("没有死信记录")
		return
	}

	fmt.Println()
	fmt.Printf("%-30s  %-12s  %s\n", "TOPIC", "STATUS", "COUNT")
	fmt.Println("──────────────────────────────  ────────────  ──────")

	var totalPending, totalResolved int64
	for _, s := range stats {
		fmt.Printf("%-30s  %-12s  %d\n", s.Topic, s.Status, s.Count)
		if s.Status == models.DeadLetterStatusPending {
			totalPending += s.Count
		} else {
			totalResolved += s.Count
		}
	}
	fmt.Println("──────────────────────────────  ────────────  ──────")
	fmt.Printf("%-30s  %-12s  %d\n", "TOTAL", "PENDING", totalPending)
	fmt.Printf("%-30s  %-12s  %d\n", "TOTAL", "RESOLVED", totalResolved)

	// 告警
	if totalPending > 100 {
		fmt.Printf("\n⚠️  警告: PENDING 死信数量 (%d) 超过阈值 (100)，请及时处理！\n", totalPending)
	}
}

// truncateStr 截断字符串，超长部分用 ... 代替
func truncateStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}
