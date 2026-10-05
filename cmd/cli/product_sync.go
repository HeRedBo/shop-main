package main

import (
	"encoding/json"
	"fmt"
	"time"

	"shop/internal/models"
	"shop/pkg/global"
	"shop/pkg/logging"

	"github.com/spf13/cobra"
	"gorm.io/gorm"
)

// ProductExportItem 商品导出数据结构
type ProductExportItem struct {
	Id         int64   `json:"id"`
	StoreName  string  `json:"store_name"`
	Price      float64 `json:"price"`
	Stock      int     `json:"stock"`
	IsShow     int8    `json:"is_show"`
	IsHot      int8    `json:"is_hot"`
	IsBest     int8    `json:"is_best"`
	IsNew      int8    `json:"is_new"`
	IsBenefit  int8    `json:"is_benefit"`
	IsGood     int8    `json:"is_good"`
	IsPostage  int8    `json:"is_postage"`
	CateId     int     `json:"cate_id"`
	Sales      int     `json:"sales"`
	Browse     int     `json:"browse"`
	CreateTime string  `json:"create_time"`
}

var productSyncCmd = &cobra.Command{
	Use:   "product:sync",
	Short: "商品数据查询同步",
	Long:  "按状态查询商品数据，组装批量数据结构，输出到日志文件",
	Run: func(cmd *cobra.Command, args []string) {
		runProductSync(cmd)
	},
}

func init() {
	// 状态筛选
	productSyncCmd.Flags().String("status", "", "商品状态: on_sale(上架) / off_sale(下架)")
	// 布尔类型标记
	productSyncCmd.Flags().Bool("hot", false, "是否热卖")
	productSyncCmd.Flags().Bool("benefit", false, "是否优惠")
	productSyncCmd.Flags().Bool("best", false, "是否精品")
	productSyncCmd.Flags().Bool("new", false, "是否新品")
	productSyncCmd.Flags().Bool("good", false, "是否良品")
	productSyncCmd.Flags().Bool("postage", false, "是否包邮")
	productSyncCmd.Flags().Bool("sub", false, "是否订阅")
	productSyncCmd.Flags().Bool("integral", false, "是否积分兑换")
	// 分页与限制
	productSyncCmd.Flags().Int("limit", 0, "限制返回数量（默认 0 不限制）")
	productSyncCmd.Flags().Int("page", 1, "页码（默认 1）")
	productSyncCmd.Flags().Int("page-size", 100, "每页数量（默认 100）")

	rootCmd.AddCommand(productSyncCmd)
}

func runProductSync(cmd *cobra.Command) {
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
	limit, _ := cmd.Flags().GetInt("limit")
	page, _ := cmd.Flags().GetInt("page")
	pageSize, _ := cmd.Flags().GetInt("page-size")

	// 构建查询条件的辅助函数，确保 Count 和 Find 使用独立的 query 对象
	buildQuery := func() *gorm.DB {
		q := global.Db.Model(&models.StoreProduct{}).Where("is_del = ?", 0)

		// status 条件
		switch status {
		case "on_sale":
			q = q.Where("is_show = ?", 1)
		case "off_sale":
			q = q.Where("is_show = ?", 0)
		}

		// bool 类型 flag，为 true 时加入条件
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

	// 先查询总数
	var total int64
	buildQuery().Count(&total)

	if total == 0 {
		fmt.Println("未查询到符合条件的商品数据")
		logger.Info("未查询到符合条件的商品数据")
		return
	}

	// 分页参数下界校验
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 100
	}

	// 分页逻辑：如果指定了 --limit 则优先使用 limit
	query := buildQuery()
	if limit > 0 {
		query = query.Limit(limit)
	} else {
		offset := (page - 1) * pageSize
		query = query.Offset(offset).Limit(pageSize)
	}

	// 查询商品数据
	var products []models.StoreProduct
	if err := query.Order("id desc").Find(&products).Error; err != nil {
		logger.Errorw("查询商品数据失败", "error", err)
		fmt.Printf("查询商品数据失败: %v\n", err)
		return
	}

	// 组装导出数据
	exportItems := make([]ProductExportItem, 0, len(products))
	for _, p := range products {
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

	// 序列化为 JSON
	jsonData, err := json.MarshalIndent(exportItems, "", "  ")
	if err != nil {
		logger.Errorw("JSON 序列化失败", "error", err)
		fmt.Printf("JSON 序列化失败: %v\n", err)
		return
	}

	// 输出到日志文件
	logger.Infow("商品数据同步",
		"total", total,
		"current_batch", len(exportItems),
		"page", page,
		"page_size", pageSize,
		"limit", limit,
		"data", string(jsonData),
	)

	// 终端输出统计信息
	fmt.Println("========== 商品数据同步 ==========")
	fmt.Printf("符合条件的商品总数: %d\n", total)
	fmt.Printf("当前批次数量:       %d\n", len(exportItems))
	if limit > 0 {
		fmt.Printf("限制返回数量:       %d\n", limit)
	} else {
		fmt.Printf("当前页码:           %d\n", page)
		fmt.Printf("每页数量:           %d\n", pageSize)
	}
	fmt.Println("数据已输出到日志文件 (product_sync)")
	fmt.Println("==================================")

	// 确保 logger 缓冲刷新
	_ = logger.Sync()
}

// derefInt8 安全解引用 *int8，nil 时返回 0
func derefInt8(p *int8) int8 {
	if p == nil {
		return 0
	}
	return *p
}
