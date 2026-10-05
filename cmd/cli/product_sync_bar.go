package main

import (
	"encoding/json"
	"fmt"
	"time"

	"shop/internal/models"
	"shop/pkg/chunk"
	"shop/pkg/global"
	"shop/pkg/logging"

	"github.com/schollz/progressbar/v3"
	"github.com/spf13/cobra"
	"gorm.io/gorm"
)

var productSyncBarCmd = &cobra.Command{
	Use:   "product:sync-bar",
	Short: "商品数据分块同步（带进度条）",
	Long:  "使用主键游标分块查询商品数据，实时显示进度条，逐批输出到日志文件",
	Run: func(cmd *cobra.Command, args []string) {
		runProductSyncBar(cmd)
	},
}

func init() {
	productSyncBarCmd.Flags().String("status", "", "商品状态: on_sale(上架) / off_sale(下架)")
	productSyncBarCmd.Flags().Bool("hot", false, "是否热卖")
	productSyncBarCmd.Flags().Bool("benefit", false, "是否优惠")
	productSyncBarCmd.Flags().Bool("best", false, "是否精品")
	productSyncBarCmd.Flags().Bool("new", false, "是否新品")
	productSyncBarCmd.Flags().Bool("good", false, "是否良品")
	productSyncBarCmd.Flags().Bool("postage", false, "是否包邮")
	productSyncBarCmd.Flags().Bool("sub", false, "是否订阅")
	productSyncBarCmd.Flags().Bool("integral", false, "是否积分兑换")
	productSyncBarCmd.Flags().Int("chunk-size", 100, "每批查询数量（默认 100）")

	rootCmd.AddCommand(productSyncBarCmd)
}

func runProductSyncBar(cmd *cobra.Command) {
	logger := logging.GetLogger("product_sync")

	// 读取 flags
	status, _ := cmd.Flags().GetString("status")
	hot, _ := cmd.Flags().GetBool("hot")
	benefit, _ := cmd.Flags().GetBool("benefit")
	best, _ := cmd.Flags().GetBool("best")
	isNew, _ := cmd.Flags().GetBool("new")
	good, _ := cmd.Flags().GetBool("good")
	postage, _ := cmd.Flags().GetBool("postage")
	sub, _ := cmd.Flags().GetBool("sub")
	integral, _ := cmd.Flags().GetBool("integral")
	chunkSize, _ := cmd.Flags().GetInt("chunk-size")

	if chunkSize < 1 {
		chunkSize = 100
	}

	// 构建查询条件
	buildQuery := func() *gorm.DB {
		q := global.Db.Model(&models.StoreProduct{}).Where("is_del = ?", 0)

		switch status {
		case "on_sale":
			q = q.Where("is_show = ?", 1)
		case "off_sale":
			q = q.Where("is_show = ?", 0)
		}

		boolFlags := map[string]bool{
			"hot":      hot,
			"benefit":  benefit,
			"best":     best,
			"new":      isNew,
			"good":     good,
			"postage":  postage,
			"sub":      sub,
			"integral": integral,
		}
		columnMap := map[string]string{
			"hot":      "is_hot",
			"benefit":  "is_benefit",
			"best":     "is_best",
			"new":      "is_new",
			"good":     "is_good",
			"postage":  "is_postage",
			"sub":      "is_sub",
			"integral": "is_integral",
		}
		for key, val := range boolFlags {
			if val {
				q = q.Where(fmt.Sprintf("%s = ?", columnMap[key]), 1)
			}
		}
		return q
	}

	startTime := time.Now()
	var bar *progressbar.ProgressBar
	var processedTotal int64

	err := chunk.ChunkByIdWithTotal(buildQuery(), &models.StoreProduct{}, chunkSize,
		func(batch []models.StoreProduct, batchNum int, total int64) error {
			// 第一批时创建进度条
			if bar == nil {
				bar = progressbar.NewOptions64(total,
					progressbar.OptionSetDescription("商品数据同步"),
					progressbar.OptionShowCount(),
					progressbar.OptionShowIts(),
					progressbar.OptionSetWidth(40),
					progressbar.OptionSetTheme(progressbar.Theme{Saucer: "█", SaucerPadding: "░", BarStart: "▒", BarEnd: "▓"}),
				)
			}

			// 组装导出数据
			exportItems := make([]ProductExportItem, 0, len(batch))
			for _, p := range batch {
				item := ProductExportItem{
					Id:         p.Id,
					StoreName:  p.StoreName,
					Price:      p.Price,
					Stock:      p.Stock,
					IsShow:     derefInt8(p.IsShow),
					IsHot:      derefInt8(p.IsHot),
					IsBest:     derefInt8(p.IsBest),
					IsNew:      derefInt8(p.IsNew),
					IsBenefit:  derefInt8(p.IsBenefit),
					IsGood:     derefInt8(p.IsGood),
					IsPostage:  derefInt8(p.IsPostage),
					CateId:     p.CateId,
					Sales:      p.Sales,
					Browse:     p.Browse,
					CreateTime: p.CreateTime.Format(time.RFC3339),
				}
				exportItems = append(exportItems, item)
			}

			// 序列化为 JSON 并写入日志
			jsonData, err := json.Marshal(exportItems)
			if err != nil {
				return fmt.Errorf("batch %d JSON 序列化失败: %w", batchNum, err)
			}

			logger.Infow("商品数据同步",
				"batch", batchNum,
				"batch_size", len(batch),
				"data", string(jsonData),
			)

			processedTotal += int64(len(batch))
			_ = bar.Add(len(batch))
			return nil
		},
	)

	if err != nil {
		logger.Errorw("分块查询失败", "error", err)
		fmt.Printf("分块查询失败: %v\n", err)
		return
	}

	if bar != nil {
		_ = bar.Finish()
	}

	elapsed := time.Since(startTime)

	// 终端统计信息
	fmt.Println()
	fmt.Println("========== 商品数据同步 ==========")
	fmt.Printf("处理总数:   %d\n", processedTotal)
	fmt.Printf("每批数量:   %d\n", chunkSize)
	fmt.Printf("总耗时:     %s\n", elapsed.Round(time.Millisecond))
	fmt.Printf("平均速度:   %.1f 条/秒\n", float64(processedTotal)/elapsed.Seconds())
	fmt.Println("数据已输出到日志文件 (product_sync)")
	fmt.Println("==================================")

	_ = logger.Sync()
}
