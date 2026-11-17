package main

// es_migrate.go 是 internal/es.RunMigrate 的 CLI 装配层（方案第五节变更分级迁移）。
//
// migrate 逐 Definition 只读探测 + diff 分级（Noop / Inplace / Rebuild / 首建 / 引导 cutover），
// 其中 Rebuild 分支委托第六节蓝绿重建，因此这里同样要把 sync.SyncProducts / sync.Compensate
// 适配成 es 的窄接口注入。护栏与 es:rebuild 一致：非 TTY 且无 --yes 在触达 ES 之前退出码 1、零 ES 调用。
//
// --dry-run 是只读预演：不仅不改结构，还会对「别名位仍是物理索引」的存量场景额外读线上结构、
// 跑 Decide 打印「割接后会被判成什么」（本仓库线上 shop-product 缺 _source.excludes → 判 rebuild）。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"shop/internal/es"
	"shop/internal/es/admin"
	"shop/internal/es/sync"
	"shop/pkg/global"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

var esMigrateCmd = &cobra.Command{
	Use:   "es:migrate",
	Short: "索引结构迁移：逐定义分级 Noop / Inplace / Rebuild / 首建 / 引导 cutover",
	Long: "取代 PHP「靠 ES 400 异常猜」的做法，按声明与线上结构做显式 diff，逐 Definition 独立分级：\n" +
		"  - 别名与同名物理索引都不存在   -> 首建 <alias>_vN：先建索引（body 不带别名）→ 灌数 → 对账达标后才**单次** _aliases 挂别名（不达标则索引存在但不挂别名）；\n" +
		"  - 别名不存在但同名物理索引存在 -> 存量未割接：中止并引导先执行 es:cutover（migrate 不做不可逆割接）；\n" +
		"    （--dry-run 下会额外读该物理索引结构、预览割接后的分级判定）\n" +
		"  - 别名指向多于 1 个索引        -> 直接报错中止（不猜首个，修 PHP array_keys()[0] 隐患）；\n" +
		"  - 别名唯一指向某索引           -> diff：Noop 无操作 / Inplace 原地追加 / Rebuild 委托 es:rebuild 蓝绿。\n" +
		"\n" +
		"顺序纪律：新增字段先跑 es:migrate（走 Inplace 的 put_mapping 追加字段）成功，再部署产出该字段的代码；\n" +
		"反向会导致写入全量失败。Inplace 只下发动态 settings + 新增字段子集，不做无条件 close/open。\n" +
		"\n" +
		"逐 Definition 独立执行，单条失败只记录、不影响其他。--dry-run 只读、写路径被审计层硬拒。\n" +
		"非 TTY 且未带 --yes 时直接退出码 1（不猜、不默认放行）。",
	Example: "  # 全量预演：只读，逐条打印分级与将执行的请求，零写操作\n" +
		"  go run ./cmd/cli es es:migrate --alias all --dry-run\n" +
		"  # 只迁移某别名，结构漂移需重建时委托蓝绿（非交互须带 --yes）\n" +
		"  go run ./cmd/cli es es:migrate --alias shop-product --yes --timeout 90m",
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runESMigrate(cmd)
	},
}

// migrateSpec 是 es:migrate 的 flags 快照。
type migrateSpec struct {
	Alias      string
	IndicesDir string
	Tolerance  int64
	DryRun     bool
	AssumeYes  bool
	AsJSON     bool
	Timeout    time.Duration
	FinalizeTO time.Duration

	ForceRebuild     bool
	DropOld          bool
	TuneIngest       bool
	KeepNewOnFailure bool
	Resume           bool
	ChunkSize        int
	Concurrency      int
	BulkSize         int
	MaxFailures      int
}

func init() {
	f := esMigrateCmd.Flags()
	f.String("alias", "all", "目标别名（all → 全部已登记且可执行的声明）")
	f.String("indices-dir", "", "索引声明目录（空 → 内置默认）")
	f.Int64("tolerance", 0, "对账容差（透传给被委托的 rebuild / 首建对账）")
	f.Bool("dry-run", false, "只读预演：仅 GET/HEAD，逐条打印分级与将执行的请求序列")
	f.Bool("yes", false, "跳过交互确认（非 TTY 环境必须显式带上）")
	f.Bool("json", false, "以 JSON 输出迁移结论（进度日志改走 stderr）")
	f.Duration("timeout", 90*time.Minute, "整体 ctx 超时（透传给 rebuild 的收尾独立 ctx 由 --finalize-timeout 决定）")
	f.Duration("finalize-timeout", 60*time.Second, "被委托 rebuild 的收尾独立超时")
	f.Bool("force-rebuild", false, "跳过 diff，对已割接索引直接走蓝绿重建")
	f.Bool("drop-old", false, "透传给被委托的 rebuild：切换成功后删除旧索引（默认保留可回滚）")
	f.Bool("tune-ingest", true, "透传给被委托的 rebuild：灌数期临时关 refresh/副本")
	f.Bool("keep-new-on-failure", true, "透传给被委托的 rebuild：失败时保留新索引供排查")
	f.Bool("resume", false, "灌数阶段断点续传（透传给 rebuild / 首建的 SyncProducts）")
	f.Int("chunk-size", 1000, "灌数流水线每批读取行数")
	f.Int("concurrency", 4, "灌数流水线 scanner/writer 各自的 goroutine 数")
	f.Int("bulk-size", 3000, "单个 bulk 的最大文档数")
	f.Int("max-failures", 100, "灌数 item 级失败累计超过该值即中断")

	esRootCmd.AddCommand(esMigrateCmd)
}

func parseMigrateSpec(cmd *cobra.Command) (migrateSpec, error) {
	f := cmd.Flags()
	s := migrateSpec{}
	s.Alias, _ = f.GetString("alias")
	s.IndicesDir, _ = f.GetString("indices-dir")
	s.Tolerance, _ = f.GetInt64("tolerance")
	s.DryRun, _ = f.GetBool("dry-run")
	s.AssumeYes, _ = f.GetBool("yes")
	s.AsJSON, _ = f.GetBool("json")
	s.Timeout, _ = f.GetDuration("timeout")
	s.FinalizeTO, _ = f.GetDuration("finalize-timeout")
	s.ForceRebuild, _ = f.GetBool("force-rebuild")
	s.DropOld, _ = f.GetBool("drop-old")
	s.TuneIngest, _ = f.GetBool("tune-ingest")
	s.KeepNewOnFailure, _ = f.GetBool("keep-new-on-failure")
	s.Resume, _ = f.GetBool("resume")
	s.ChunkSize, _ = f.GetInt("chunk-size")
	s.Concurrency, _ = f.GetInt("concurrency")
	s.BulkSize, _ = f.GetInt("bulk-size")
	s.MaxFailures, _ = f.GetInt("max-failures")
	if s.Timeout < 0 || s.FinalizeTO < 0 {
		return s, fmt.Errorf("--timeout / --finalize-timeout 不能为负")
	}
	return s, nil
}

func runESMigrate(cmd *cobra.Command) error {
	if !global.CONFIG.Elasticsearch.Enabled {
		fmt.Println("Elasticsearch 未启用（elasticsearch.enabled=false），跳过迁移")
		return nil
	}
	spec, err := parseMigrateSpec(cmd)
	if err != nil {
		return err
	}
	reg, err := es.LoadRegistry(spec.IndicesDir)
	if err != nil {
		return fmt.Errorf("加载索引声明失败: %w", err)
	}
	if reg.Len() == 0 {
		return fmt.Errorf("索引声明目录为空，没有可迁移的 Definition")
	}

	// 护栏先于任何 ES 连接/请求：--dry-run 无需确认；实写需 --yes 或 TTY 交互确认。
	if !spec.DryRun && !spec.AssumeYes {
		if !cutoverStdinIsInteractive() {
			return fmt.Errorf("非 TTY 环境必须显式传 --yes 才能执行迁移（可能触发蓝绿重建/首建写操作）")
		}
		printMigrateSummary(spec, reg)
		if !confirmYes() {
			return fmt.Errorf("用户取消迁移")
		}
	} else if spec.DryRun {
		fmt.Printf("[dry-run] 只读预演，不写 ES、不改结构、不动别名\n")
	}

	if _, err := admin.Client(); err != nil {
		return err
	}

	sigCtx, sigCancel := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer sigCancel()
	ctx := sigCtx
	if spec.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, spec.Timeout)
		defer cancel()
	}

	logf := func(format string, v ...any) { fmt.Printf(format+"\n", v...) }
	if spec.AsJSON {
		logf = func(format string, v ...any) { fmt.Fprintf(os.Stderr, format+"\n", v...) }
	}

	res, runErr := es.RunMigrate(ctx, es.MigrateOptions{
		Registry:         reg,
		Target:           es.NewAdminRebuild(),
		Sync:             newMigrateSyncFunc(spec),
		Compensate:       newMigrateCompensateFunc(spec),
		DBCount:          sync.DBCountByAlias,
		Alias:            spec.Alias,
		Tolerance:        spec.Tolerance,
		DryRun:           spec.DryRun,
		ForceRebuild:     spec.ForceRebuild,
		TuneIngest:       spec.TuneIngest,
		KeepNewOnFailure: spec.KeepNewOnFailure,
		DropOld:          spec.DropOld,
		FinalizeTimeout:  spec.FinalizeTO,
		Logf:             logf,
	})

	if res != nil {
		if spec.AsJSON {
			if out, merr := json.MarshalIndent(res, "", "  "); merr == nil {
				fmt.Println(string(out))
			} else {
				logf("⚠️  结论序列化失败：%v", merr)
			}
		} else {
			printMigrateResult(res)
		}
	}
	return runErr
}

// newMigrateSyncFunc / newMigrateCompensateFunc 与 es_rebuild.go 的同名适配器同构：
// 把 sync 包的两个入口收敛成 es 的窄接口。rebuild 与 migrate 各自独立装配，避免互相牵扯。
// 二者都按编排传下来的 alias 查表取实现（sync.SyncByAlias / sync.CompensateByAlias /
// sync.DBCountByAlias），不再恒调商品入口 —— --alias all 逐条循环时每条都落到自己业务的数据源与口径。
//
// Tolerance 固定用 es.IngestToleranceIgnore，**绝不把 --tolerance 下发给内层灌数**（与
// es_rebuild.go / es_cutover.go 同一口径）：内层只负责灌，它的收尾对账一旦按同一个容差判定，
// 就会抢在 migrate 本层闸门之前失败 —— 中止步骤错报成 sync、结论的 ES 计数是从未回读的假 0。
// 对账判定归 migrate 本层闸门：Rebuild 分支把 opts.Tolerance 转发给 runRebuild 第 5 步
// （ReconcileCounts），首建分支在 internal/es/migrate.go 用真实 opts.Tolerance 直接判定；
// --tolerance 的用户可见语义（达标/不达标、负值=永不可达标）只在 ReconcileCounts 单点拥有，本层不改写。
func newMigrateSyncFunc(spec migrateSpec) es.SyncFunc {
	return func(ctx context.Context, alias, target string) (es.SyncSummary, error) {
		stats := &sync.SyncStats{}
		opts := migrateIngestOptions(spec, target)
		opts.Stats = stats
		// 灌数进度条：migrate 是多阶段编排，标题必须带上正在处理的别名并点明「这是灌数」，
		// 否则会被误读成整个 migrate 的进度。非 TTY / --json 下不构造渲染器。
		stopProg := startIngestProgress(stats, fmt.Sprintf("ES 灌数(%s)", alias), spec.AsJSON)
		defer stopProg()
		_, err := sync.SyncByAlias(ctx, alias, opts)
		return es.SyncSummary{
			Target:  target,
			Scanned: stats.ScannedDocs.Load(),
			Sent:    stats.SentDocs.Load(),
			Failed:  stats.FailedDocs.Load(),
			Batches: stats.Batches.Load(),
			Elapsed: stats.Elapsed(),
		}, err
	}
}

// migrateIngestOptions 构造下发给内层灌数（sync.SyncByAlias）的 Options。
// 单独提成纯函数，是为了让「容差固定为哨兵 IngestToleranceIgnore、绝不透传 spec.Tolerance」
// 这一关键装配可被单测直接钉住（见 cmd/cli/es_migrate_test.go 的破坏即红用例）。
// 其余字段与提取前逐字一致：TuneIngest=false（settings 生命周期归 rebuild 编排独占）、
// AssumeYes=true、DryRun=false、Timeout=0（统一由外层 --timeout 的 ctx 控制）。
func migrateIngestOptions(spec migrateSpec, target string) sync.Options {
	return sync.Options{
		Index:       target,
		ChunkSize:   spec.ChunkSize,
		Concurrency: spec.Concurrency,
		BulkSize:    spec.BulkSize,
		MaxFailures: spec.MaxFailures,
		Tolerance:   es.IngestToleranceIgnore, // 对账归 migrate 本层闸门，不外包给灌数层
		IndicesDir:  spec.IndicesDir,
		AssumeYes:   true,
		TuneIngest:  false,
		Resume:      spec.Resume,
		DryRun:      false,
		Timeout:     0,
	}
}

func newMigrateCompensateFunc(spec migrateSpec) es.CompensateFunc {
	return func(ctx context.Context, alias, target string, since time.Time) (es.CompensateSummary, error) {
		st, err := sync.CompensateByAlias(ctx, alias, target, since, sync.CompensateOptions{
			ChunkSize: spec.ChunkSize,
			BulkSize:  spec.BulkSize,
		})
		// Stats 必须带出（与 es_rebuild.go 同一接法）：只返回 error 会把 item 级失败吞成成功。
		return es.CompensateSummary{
			Scanned: st.Scanned,
			Indexed: st.Indexed,
			Deleted: st.Deleted,
			Failed:  st.Failed,
			Batches: st.Batches,
		}, err
	}
}

func printMigrateSummary(spec migrateSpec, reg *es.Registry) {
	scope := spec.Alias
	if scope == "" {
		scope = "all"
	}
	fmt.Println("!!!!!!!!!!!!!!!! es:migrate !!!!!!!!!!!!!!!!")
	fmt.Printf("处理范围:     %s（已登记别名：%v）\n", scope, reg.Aliases())
	fmt.Printf("强制重建:     %v   对账容差: %d\n", spec.ForceRebuild, spec.Tolerance)
	fmt.Printf("透传 rebuild: drop-old=%v tune-ingest=%v keep-new-on-failure=%v\n",
		spec.DropOld, spec.TuneIngest, spec.KeepNewOnFailure)
	fmt.Printf("超时:         %s\n", spec.Timeout)
	fmt.Println("可能动作:     首建（先建后灌、对账达标再挂别名）/ 原地 put_mapping+settings / 委托蓝绿重建（保留旧索引）。")
	fmt.Println("!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!")
}

func printMigrateResult(r *es.MigrateResult) {
	fmt.Println()
	fmt.Println("========== es:migrate 结论 ==========")
	for _, d := range r.Defs {
		line := fmt.Sprintf("%-16s %-16s", d.Alias, d.Verdict)
		if d.Action != "" {
			line += fmt.Sprintf(" action=%-8s", d.Action)
		}
		if d.Error != "" {
			line += " ✗ " + d.Error
		} else if d.Reason != "" {
			line += " " + d.Reason
		}
		fmt.Println(line)
		if len(d.Planned) > 0 {
			for _, p := range d.Planned {
				fmt.Println("    - " + p)
			}
		}
		// 被委托 rebuild 的关键统计。
		if d.Rebuild != nil {
			rb := d.Rebuild
			fmt.Printf("    └─ rebuild: next=%s cur=%s switched=%v reconciled=%v compensated=%v verified=%v dropped_old=%v\n",
				rb.Next, rb.Cur, rb.Switched, rb.Reconciled, rb.Compensated, rb.Verified, rb.OldDropped)
		}
	}
	if r.DryRun {
		fmt.Println("(dry-run：以上均为只读预演，未发起任何写操作)")
	}
	fmt.Println("=====================================")
}
