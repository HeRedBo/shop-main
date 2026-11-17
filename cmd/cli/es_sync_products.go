package main

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"shop/internal/es/sync"
	"shop/pkg/global"

	"github.com/spf13/cobra"
)

var esSyncProductsCmd = &cobra.Command{
	Use:   "es:sync-products",
	Short: "全量同步商品数据到 Elasticsearch",
	Long: "从 MySQL 分片扫描 store_product 表，并发灌入 ES 索引。\n" +
		"支持断点续传（--resume）、只读预演（--dry-run）、复跑失败（--retry-failed）、\n" +
		"灌入调优（--tune-ingest）、对账（--tolerance）等能力。",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runESSyncProducts(cmd)
	},
}

func init() {
	f := esSyncProductsCmd.Flags()
	f.String("index", "shop-product", "灌入目标（别名或物理索引名）")
	f.Int("chunk-size", 1000, "pkg/chunk 每批读取行数")
	f.Int("concurrency", 4, "scanner 与 writer 各自的 goroutine 数")
	f.Int("range-multiplier", 4, "区间数 = 该值 × concurrency")
	f.Int("bulk-size", 3000, "单个 bulk 的最大文档数")
	f.Int("bulk-max-mb", 0, "单个 bulk 的最大字节数（MB；0 → admin 默认）")
	f.Int64("limit", 0, "仅灌入前 N 行（小样本实测）")
	f.String("ids", "", "仅灌入指定 id 集合（逗号分隔）")
	f.String("since", "", "仅灌入 update_time >= Since（RFC3339 或 2006-01-02）")
	f.Int64("tolerance", 0, "对账容差：|ES count - DB expected| > 该值即非零退出")
	f.Int("max-failures", 100, "item 级失败累计超过该值即中断整任务")
	f.String("retry-failed", "", "复跑失败明细 JSONL 的路径")
	f.Bool("resume", false, "断点续传：读写 runtime/es_sync/<target>.checkpoint.json")
	f.Bool("dry-run", false, "只读预演：打印执行计划（区间/并发/对账请求序列），仍会访问 MySQL 并只读探测 ES，但不发起任何写请求、不改 settings")
	f.Bool("tune-ingest", true, "灌入物理索引时临时关 refresh/副本")
	f.Int("gogc", 0, ">0 时 debug.SetGCPercent(gogc)")
	f.String("pprof", ":0", "pprof 监听地址（:0 表示不启动）")
	f.Duration("timeout", 30*time.Minute, "整体 ctx 超时")
	f.Bool("yes", false, "跳过交互确认")
	f.String("indices-dir", "", "索引声明目录（空 → 内置默认）")
	f.String("failure-dir", "", "失败明细落盘目录（空 → admin 默认）")

	esRootCmd.AddCommand(esSyncProductsCmd)
}

// runESSyncProducts 是 es:sync-products 的入口：解析 flags → 构造 Options → 启动进度条 →
// 调 sync.SyncProducts(ctx, opts)。
func runESSyncProducts(cmd *cobra.Command) error {
	opts, err := parseSyncOpts(cmd)
	if err != nil {
		return err
	}

	// 未带 --yes：打印影响面摘要 + 二次确认（非 TTY 直接退出 1）。
	if !opts.AssumeYes && !opts.DryRun {
		if !isStdinTTY() {
			return fmt.Errorf("非 TTY 环境必须显式传 --yes 才能执行同步")
		}
		printSyncSummary(opts)
		if !confirmYes() {
			return fmt.Errorf("用户取消同步")
		}
	}

	// --pprof：在指定地址启动 pprof 服务（:0 表示不启动）。
	if addr := opts.PprofAddr(); addr != "" {
		go func() { _ = http.ListenAndServe(addr, nil) }()
		fmt.Printf("[pprof] 监听 %s/debug/pprof/\n", addr)
	}

	// 信号 + 超时：signal.NotifyContext(SIGINT,SIGTERM) 叠加 --timeout。
	sigCtx, sigCancel := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer sigCancel()
	ctx := sigCtx
	if opts.Timeout > 0 {
		var tCancel context.CancelFunc
		ctx, tCancel = context.WithTimeout(ctx, opts.Timeout)
		defer tCancel()
	}

	// 进度条：独立 goroutine 拉原子快照渲染，渲染失败绝不中止同步。
	// 统计容器提前创建，同时交给灌数入口（进度条与同步共享同一份快照）。
	// 非 TTY（管道 / 重定向 / CI）下不构造渲染器：progressbar v3.19.1 不会自己降级。
	stats := &sync.SyncStats{}
	stopProg := startIngestProgress(stats, "ES 全量同步", false)
	defer stopProg()

	opts.Stats = stats
	// 命令名即业务域：es:sync-products 只服务商品别名，按别名查表取数据源；
	// --index 解析到别的别名（如二期转 active 的订单）时会被当场拒绝，不会再静默灌商品数据。
	_, runErr := sync.SyncByAlias(ctx, sync.ProductAlias, opts.ToSync())

	if runErr != nil {
		return runErr
	}
	printSyncResult(stats)
	return nil
}

// syncOptions 是 CLI 侧的中间结构（与 sync.Options 字段一一对应，但 CLI 需要解析 flags
// 与做交互确认，故保留独立类型以便扩展）。
type syncOptions struct {
	Index string
	// StrictIndex 由 --index 是否被显式传入（Changed）决定：真→目标必须是已登记且 active 的索引，
	// 否则报错；假（未传，用默认 shop-product）→ 沿用内层友好兜底。区分「未传用默认」与「传了但不存在」。
	StrictIndex     bool
	ChunkSize       int
	Concurrency     int
	RangeMultiplier int
	BulkSize        int
	BulkMaxMB       int
	Limit           int64
	IDs             []int64
	Since           time.Time
	Tolerance       int64
	MaxFailures     int
	RetryFailed     string
	Resume          bool
	DryRun          bool
	TuneIngest      bool
	AssumeYes       bool
	GoGC            int
	Pprof           string
	Timeout         time.Duration
	IndicesDir      string
	FailureDir      string
	Stats           *sync.SyncStats
}

// ToSync 把 CLI 中间结构转成 sync.Options（编程式入口）。
func (o syncOptions) ToSync() sync.Options {
	return sync.Options{
		Index:             o.Index,
		StrictIndexLookup: o.StrictIndex,
		ChunkSize:         o.ChunkSize,
		Concurrency:       o.Concurrency,
		RangeMultiplier:   o.RangeMultiplier,
		BulkSize:          o.BulkSize,
		BulkMaxBytes:      o.BulkMaxMB * 1024 * 1024,
		Limit:             o.Limit,
		IDs:               o.IDs,
		Since:             o.Since,
		Tolerance:         o.Tolerance,
		MaxFailures:       o.MaxFailures,
		RetryFailed:       o.RetryFailed,
		Resume:            o.Resume,
		DryRun:            o.DryRun,
		TuneIngest:        o.TuneIngest,
		AssumeYes:         o.AssumeYes,
		GoGC:              o.GoGC,
		Timeout:           o.Timeout,
		IndicesDir:        o.IndicesDir,
		FailureDir:        o.FailureDir,
		Stats:             o.Stats,
	}
}

// PprofAddr 返回 pprof 监听地址（:0 表示不启动）。
func (o syncOptions) PprofAddr() string {
	addr := strings.TrimSpace(o.Pprof)
	if addr == "" || addr == ":0" {
		return ""
	}
	return addr
}

// parseSyncOpts 从 cobra flags 解析出 syncOptions。
func parseSyncOpts(cmd *cobra.Command) (syncOptions, error) {
	f := cmd.Flags()
	o := syncOptions{}
	o.Index, _ = f.GetString("index")
	// 区分「未传 --index（用默认 shop-product，宽松）」与「显式传了名字（严格：必须命中 active 定义）」。pflag.Changed 仅反映本次调用是否显式赋值。
	o.StrictIndex = f.Changed("index")

	// 四个受配置影响的数值参数按「显式 CLI flag > config elasticsearch 段 > 代码内置默认」
	// 三级优先级解析。flag 默认值保持原样（chunk 1000 / concurrency 4 / range-multiplier 4 /
	// bulk-max-mb 0），仅当用户未显式指定（Changed=false）时才回落到配置值，语义不变。
	rawChunk, _ := f.GetInt("chunk-size")
	rawConc, _ := f.GetInt("concurrency")
	rawRange, _ := f.GetInt("range-multiplier")
	rawBulkMB, _ := f.GetInt("bulk-max-mb")
	esCfg := global.CONFIG.Elasticsearch
	resolved := sync.ResolveSyncParams(
		sync.SyncParamFlags{
			ChunkSize:       sync.IntFlag{Changed: f.Changed("chunk-size"), Value: rawChunk},
			Concurrency:     sync.IntFlag{Changed: f.Changed("concurrency"), Value: rawConc},
			RangeMultiplier: sync.IntFlag{Changed: f.Changed("range-multiplier"), Value: rawRange},
			BulkMaxMB:       sync.IntFlag{Changed: f.Changed("bulk-max-mb"), Value: rawBulkMB},
		},
		sync.SyncParamConfig{
			ChunkSize:       esCfg.DefaultChunk,
			Concurrency:     esCfg.SyncConcurrency,
			RangeMultiplier: esCfg.RangeMultiplier,
			BulkMaxMB:       esCfg.BulkMaxMB,
		},
	)
	for _, w := range resolved.Warnings {
		fmt.Fprintf(os.Stderr, "[es:sync-products] 配置告警: %s\n", w)
	}
	o.ChunkSize = resolved.ChunkSize
	o.Concurrency = resolved.Concurrency
	o.RangeMultiplier = resolved.RangeMultiplier
	o.BulkMaxMB = resolved.BulkMaxMB

	o.BulkSize, _ = f.GetInt("bulk-size")
	o.Limit, _ = f.GetInt64("limit")
	o.Tolerance, _ = f.GetInt64("tolerance")
	o.MaxFailures, _ = f.GetInt("max-failures")
	o.RetryFailed, _ = f.GetString("retry-failed")
	o.Resume, _ = f.GetBool("resume")
	o.DryRun, _ = f.GetBool("dry-run")
	o.TuneIngest, _ = f.GetBool("tune-ingest")
	o.AssumeYes, _ = f.GetBool("yes")
	o.GoGC, _ = f.GetInt("gogc")
	o.Pprof, _ = f.GetString("pprof")
	o.Timeout, _ = f.GetDuration("timeout")
	o.IndicesDir, _ = f.GetString("indices-dir")
	o.FailureDir, _ = f.GetString("failure-dir")

	if idsStr, _ := f.GetString("ids"); idsStr != "" {
		for _, s := range strings.Split(idsStr, ",") {
			s = strings.TrimSpace(s)
			if s == "" {
				continue
			}
			n, err := strconv.ParseInt(s, 10, 64)
			if err != nil {
				return o, fmt.Errorf("--ids 解析失败 [%q]: %w", s, err)
			}
			o.IDs = append(o.IDs, n)
		}
	}
	if sinceStr, _ := f.GetString("since"); sinceStr != "" {
		t, err := time.Parse(time.RFC3339, sinceStr)
		if err != nil {
			t, err = time.Parse("2006-01-02", sinceStr)
			if err != nil {
				return o, fmt.Errorf("--since 解析失败（期望 RFC3339 或 2006-01-02）: %w", err)
			}
		}
		o.Since = t
	}
	return o, nil
}

// isStdinTTY 判断标准输入是否为终端（非 TTY 直接退出 1，避免脚本误操作）。
func isStdinTTY() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// printSyncSummary 打印影响面摘要（未带 --yes 时调用）。
func printSyncSummary(o syncOptions) {
	fmt.Println("========== es:sync-products ==========")
	fmt.Printf("目标索引:     %s\n", o.Index)
	fmt.Printf("并发:         scanner=%d, writer=%d\n", o.Concurrency, o.Concurrency)
	fmt.Printf("区间:         %d (= %d × %d)\n", o.RangeMultiplier*o.Concurrency, o.RangeMultiplier, o.Concurrency)
	fmt.Printf("chunk/bulk:   chunk=%d, bulk<=%d 条 / %d MB\n", o.ChunkSize, o.BulkSize, o.BulkMaxMB)
	if o.Limit > 0 {
		fmt.Printf("限额:         仅灌入前 %d 行\n", o.Limit)
	}
	if len(o.IDs) > 0 {
		fmt.Printf("指定 id:      %v\n", o.IDs)
	}
	if !o.Since.IsZero() {
		fmt.Printf("增量起点:      %s\n", o.Since.Format(time.RFC3339))
	}
	if o.Resume {
		fmt.Println("断点续传:     启用")
	}
	if o.DryRun {
		fmt.Println("只读预演:     启用（不写 ES、不改 settings）")
	}
	if o.TuneIngest {
		fmt.Println("灌入调优:     临时关 refresh/副本（仅物理索引）")
	}
	fmt.Printf("对账容差:     %d\n", o.Tolerance)
	fmt.Printf("失败阈值:     %d\n", o.MaxFailures)
	fmt.Printf("超时:         %s\n", o.Timeout)
	fmt.Println("========================================")
}

// confirmYes 读标准输入，用户输入 y/yes 返回 true。
func confirmYes() bool {
	fmt.Print("确认执行？[y/N] ")
	reader := bufio.NewReader(os.Stdin)
	line, _ := reader.ReadString('\n')
	line = strings.TrimSpace(strings.ToLower(line))
	return line == "y" || line == "yes"
}

// printSyncResult 打印同步结果摘要。
func printSyncResult(s *sync.SyncStats) {
	fmt.Println()
	fmt.Println("========== 同步完成 ==========")
	fmt.Printf("计划文档数:   %d\n", s.TotalDocs.Load())
	fmt.Printf("成功写入:     %d\n", s.SentDocs.Load())
	fmt.Printf("失败文档:     %d\n", s.FailedDocs.Load())
	fmt.Printf("bulk 批次:    %d\n", s.Batches.Load())
	fmt.Printf("总耗时:       %s\n", s.Elapsed().Round(time.Millisecond))
	fmt.Printf("平均速率:     %.1f docs/sec\n", s.DocsPerSec())
	if r := s.RestoreReplicaElapsed(); r > 0 {
		fmt.Printf("恢复副本耗时: %s\n", r.Round(time.Millisecond))
	}
	fmt.Println("================================")
}
