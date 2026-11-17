package main

// es_rebuild.go 是 internal/es.RunRebuild 的 CLI 装配层（方案第六节蓝绿重建）。
//
// 职责与 es_cutover.go 对称，但语义相反：rebuild 的起点是「别名已存在」，切换时**保留旧索引**、
// 默认打印回滚指令，只有 --drop-old 才物理删除。这里只负责：
//   - 从 Registry 取声明，把 sync.SyncProducts / sync.Compensate 适配成 es 的窄接口
//     SyncFunc / CompensateFunc（#5 与差量补偿的签名再变，只有这个文件跟着改）；
//   - 护栏：--yes / TTY 交互确认、统一 --timeout、--dry-run、退出码；
//   - 失败路径打印孤儿索引（新索引）处置建议。
//
// rebuild.go 不 import internal/es/sync，二者只在这里接线。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"shop/internal/es"
	"shop/internal/es/admin"
	"shop/internal/es/sync"
	"shop/pkg/global"

	"github.com/spf13/cobra"
)

var esRebuildCmd = &cobra.Command{
	Use:   "es:rebuild",
	Short: "蓝绿重建：新建 <alias>_vN 灌数对账后单次原子切别名（默认保留旧索引）",
	Long: "对**别名已经存在**的索引做蓝绿重建：按声明新建 <alias>_v<schema_version>、全量灌入、\n" +
		"Refresh+Count 对账达标后，用**单次** POST /_aliases 原子把别名从旧索引切到新索引。\n" +
		"\n" +
		"与 es:cutover 的根本区别（务必区分）：\n" +
		"  - cutover：别名位仍是物理索引，靠 remove_index 换成同名别名，旧索引连同数据一起消失，不可回滚；\n" +
		"  - rebuild：别名已就位，remove 只**解除旧索引与别名的关联、保留旧索引**，因此默认可直接把\n" +
		"    别名原子切回旧索引回滚。--drop-old 是唯一会物理删除旧索引的开关。\n" +
		"\n" +
		"三条不可让的安全语义（本命令已按此实现）：\n" +
		"  - 对账（|ES count - DB 期望| > --tolerance）在切别名**之前**中止，别名保持指向旧索引，线上读不受影响；\n" +
		"  - 别名切换是**单请求原子**（add new + remove old 同一次下发），绝不拆两步；\n" +
		"  - 灌入期 refresh=-1/replicas=0 的临时调优，无论成功、失败还是被 --timeout/信号打断，\n" +
		"    都用独立 context 恢复回声明值，绝不留给线上。\n" +
		"\n" +
		"切换成功后执行一次差量补偿（update_time >= 任务启动时刻，含写/删双分支）收敛灌数窗口内的增量。\n" +
		"灌数与补偿一律 op_type=index 幂等（create 对重复 _id 会 409）。单节点健康恒 yellow，属正常态。",
	Example: "  # 预演：只读，打印 8 步计划与 _aliases 请求体，零写操作\n" +
		"  go run ./cmd/cli es es:rebuild --alias shop-product --dry-run\n" +
		"  # 正式重建（非交互环境必须带 --yes），完成后保留旧索引以便回滚\n" +
		"  go run ./cmd/cli es es:rebuild --alias shop-product --yes --timeout 90m\n" +
		"  # 确认新索引无误后再回收旧索引磁盘\n" +
		"  go run ./cmd/cli es es:rebuild --alias shop-product --drop-old --yes",
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runESRebuild(cmd)
	},
}

// rebuildSpec 是 es:rebuild 的 flags 快照。
type rebuildSpec struct {
	Alias      string
	IndicesDir string
	Tolerance  int64
	DryRun     bool
	AssumeYes  bool
	AsJSON     bool
	Timeout    time.Duration
	FinalizeTO time.Duration

	DropOld          bool
	KeepNewOnFailure bool
	Force            bool
	TuneIngest       bool
	Resume           bool
	ChunkSize        int
	Concurrency      int
	BulkSize         int
	MaxFailures      int
}

func init() {
	f := esRebuildCmd.Flags()
	f.String("alias", "", "目标别名（必填，rebuild 的前置是该别名已存在）")
	f.String("indices-dir", "", "索引声明目录（空 → 内置默认）")
	f.Int64("tolerance", 0, "对账容差（只作用于 rebuild 自己的对账步骤）：|ES - DB| > 该值即在切别名之前中止；负值=永不可达标，不会当 0 用")
	f.Bool("dry-run", false, "只读预演：仅 GET/HEAD，打印 8 步计划与 _aliases 请求体")
	f.Bool("yes", false, "跳过交互确认（非 TTY 环境必须显式带上）")
	f.Bool("json", false, "以 JSON 输出重建结论（进度日志改走 stderr）")
	f.Duration("timeout", 90*time.Minute, "整体 ctx 超时；到期后恢复 settings / 校验别名仍用独立收尾 ctx 跑完")
	f.Duration("finalize-timeout", 60*time.Second, "收尾动作（恢复 settings、校验别名）的独立超时")
	f.Bool("drop-old", false, "切换+补偿成功后删除旧物理索引（默认保留以便随时回滚）")
	f.Bool("keep-new-on-failure", true, "失败时保留灌到一半的新索引供排查（关闭则仅提示，不自动删）")
	f.Bool("force", false, "对账不达标仍继续切换（危险，正常勿用）")
	f.Bool("tune-ingest", true, "灌数期临时关 refresh/副本（仅对不持有别名的新建物理索引安全）")
	f.Bool("resume", false, "灌数阶段断点续传（复用 runtime/es_sync 的 checkpoint）")
	f.Int("chunk-size", 1000, "灌数流水线每批读取行数")
	f.Int("concurrency", 4, "灌数流水线 scanner/writer 各自的 goroutine 数")
	f.Int("bulk-size", 3000, "单个 bulk 的最大文档数")
	f.Int("max-failures", 100, "灌数 item 级失败累计超过该值即中断（进而在切别名之前中止）")

	esRootCmd.AddCommand(esRebuildCmd)
}

func parseRebuildSpec(cmd *cobra.Command) (rebuildSpec, error) {
	f := cmd.Flags()
	s := rebuildSpec{}
	s.Alias, _ = f.GetString("alias")
	s.IndicesDir, _ = f.GetString("indices-dir")
	s.Tolerance, _ = f.GetInt64("tolerance")
	s.DryRun, _ = f.GetBool("dry-run")
	s.AssumeYes, _ = f.GetBool("yes")
	s.AsJSON, _ = f.GetBool("json")
	s.Timeout, _ = f.GetDuration("timeout")
	s.FinalizeTO, _ = f.GetDuration("finalize-timeout")
	s.DropOld, _ = f.GetBool("drop-old")
	s.KeepNewOnFailure, _ = f.GetBool("keep-new-on-failure")
	s.Force, _ = f.GetBool("force")
	s.TuneIngest, _ = f.GetBool("tune-ingest")
	s.Resume, _ = f.GetBool("resume")
	s.ChunkSize, _ = f.GetInt("chunk-size")
	s.Concurrency, _ = f.GetInt("concurrency")
	s.BulkSize, _ = f.GetInt("bulk-size")
	s.MaxFailures, _ = f.GetInt("max-failures")
	if s.Alias == "" {
		return s, fmt.Errorf("--alias 不能为空（rebuild 需要已存在的别名）")
	}
	if s.Timeout < 0 || s.FinalizeTO < 0 {
		return s, fmt.Errorf("--timeout / --finalize-timeout 不能为负")
	}
	return s, nil
}

func runESRebuild(cmd *cobra.Command) error {
	// 与 cutover 一致：enabled=false 时按「无事可做」正常退出而不是报错。
	if !global.CONFIG.Elasticsearch.Enabled {
		fmt.Println("Elasticsearch 未启用（elasticsearch.enabled=false），跳过重建")
		return nil
	}
	spec, err := parseRebuildSpec(cmd)
	if err != nil {
		return err
	}

	reg, err := es.LoadRegistry(spec.IndicesDir)
	if err != nil {
		return fmt.Errorf("加载索引声明失败: %w", err)
	}
	def, ok := reg.Get(spec.Alias)
	if !ok {
		return fmt.Errorf("别名 %s 没有登记的索引声明（已登记别名：%v）", spec.Alias, reg.Aliases())
	}
	if !def.Executable() {
		return fmt.Errorf("声明 %s 的 status=%s 未接线，拒绝重建", def.Source(), def.Status())
	}

	// 护栏必须在任何 ES 连接/请求之前：非 TTY 且无 --yes 直接退出码 1、零 ES 调用。
	if !spec.DryRun && !spec.AssumeYes {
		if !cutoverStdinIsInteractive() {
			return fmt.Errorf("非 TTY 环境必须显式传 --yes 才能执行蓝绿重建")
		}
		printRebuildSummary(spec, def)
		if !confirmYes() {
			return fmt.Errorf("用户取消重建")
		}
	} else if spec.DryRun {
		fmt.Printf("[dry-run] 只读预演，不写 ES、不改 settings、不动别名\n")
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

	res, runErr := es.RunRebuild(ctx, es.RebuildOptions{
		Alias:            spec.Alias,
		Definition:       def,
		Target:           es.NewAdminRebuild(),
		Sync:             newRebuildSyncFunc(spec),
		Compensate:       newRebuildCompensateFunc(spec),
		DBCount:          sync.DBCountByAlias,
		Tolerance:        spec.Tolerance,
		DryRun:           spec.DryRun,
		DropOld:          spec.DropOld,
		KeepNewOnFailure: spec.KeepNewOnFailure,
		Force:            spec.Force,
		TuneIngest:       spec.TuneIngest,
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
			printRebuildResult(res)
		}
		// 失败路径点名孤儿（灌到一半的新索引），只给建议，绝不自动删。
		if res.AbortedStep != "" && len(res.Orphans) > 0 {
			for _, o := range res.Orphans {
				fmt.Fprintln(os.Stderr, "\n"+es.FormatRebuildOrphanAdvisory(res.Alias, res.Cur, o, spec.KeepNewOnFailure))
			}
		}
	}
	return runErr
}

// newRebuildSyncFunc 把按别名查表后的灌数入口（sync.SyncByAlias）适配成 es.SyncFunc（灌数注入点）。
//
// TuneIngest=false：refresh/replicas 的生命周期由 rebuild 编排独占（提速 + 无论成败都恢复），
// 嵌套再开会在灌数结束时提前恢复，白丢提速窗口；Timeout=0：统一由外层 --timeout 的 ctx 控制。
//
// Tolerance 固定用 es.IngestToleranceIgnore，**绝不把 --tolerance 下发给内层**：内层只负责灌，
// 它的收尾对账一旦按同一个容差判定，就会抢在 rebuild 第 5 步之前失败 —— 中止步骤错报成 sync、
// 结论行的 ES 计数是从未回读的假 0、--force 分支不可达（真机实测坐实的缺陷）。
func newRebuildSyncFunc(spec rebuildSpec) es.SyncFunc {
	return func(ctx context.Context, alias, target string) (es.SyncSummary, error) {
		stats := &sync.SyncStats{}
		// 灌数进度条：标题带正在重建的别名 + 「灌数」阶段名（rebuild 是 8 步编排，不能让八步共用一条进度条）；
		// 非 TTY / --json 下不构造渲染器。
		stopProg := startIngestProgress(stats, fmt.Sprintf("ES 灌数(%s)", alias), spec.AsJSON)
		defer stopProg()
		_, err := sync.SyncByAlias(ctx, alias, sync.Options{
			Index:       target,
			ChunkSize:   spec.ChunkSize,
			Concurrency: spec.Concurrency,
			BulkSize:    spec.BulkSize,
			MaxFailures: spec.MaxFailures,
			Tolerance:   es.IngestToleranceIgnore, // 对账归 rebuild，不外包给灌数层
			IndicesDir:  spec.IndicesDir,
			AssumeYes:   true,
			TuneIngest:  false,
			Resume:      spec.Resume,
			DryRun:      false,
			Timeout:     0,
			Stats:       stats,
		})
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

// newRebuildCompensateFunc 把 sync.CompensateByAlias 适配成 es.CompensateFunc（差量补偿注入点）。
// since 由 rebuild 编排固化为「任务首次启动时刻」，透传给扫描下界，绝不在这里重新取 time.Now()。
//
// Stats 必须原样带出：sync.Compensate 里的 Failed 含 strict_dynamic 这类结构级失败，
// 早先只取 error 把它们整条丢掉了，失败 bulk 仍被报成 Compensated=true（审查必修②）。
func newRebuildCompensateFunc(spec rebuildSpec) es.CompensateFunc {
	return func(ctx context.Context, alias, target string, since time.Time) (es.CompensateSummary, error) {
		st, err := sync.CompensateByAlias(ctx, alias, target, since, sync.CompensateOptions{
			ChunkSize: spec.ChunkSize,
			BulkSize:  spec.BulkSize,
		})
		return es.CompensateSummary{
			Scanned: st.Scanned,
			Indexed: st.Indexed,
			Deleted: st.Deleted,
			Failed:  st.Failed,
			Batches: st.Batches,
		}, err
	}
}

func printRebuildSummary(spec rebuildSpec, def *es.IndexDefinition) {
	fmt.Println("!!!!!!!!!!!!!!!! es:rebuild !!!!!!!!!!!!!!!!")
	fmt.Printf("别名:         %s（前置：必须已存在且唯一指向一个旧索引）\n", spec.Alias)
	fmt.Printf("新索引:       %s（声明 %s，schema_version=%d）\n", def.Name(), def.Source(), def.Version())
	fmt.Printf("依赖插件:     %v\n", def.RequiresPlugins())
	fmt.Printf("对账容差:     %d（DB 口径 is_del=0 AND is_show=1，由 rebuild 第 5 步真实回读后判定，内层灌数不做对账）\n", spec.Tolerance)
	fmt.Printf("灌数提速:     %v   失败保留新索引: %v\n", spec.TuneIngest, spec.KeepNewOnFailure)
	fmt.Printf("删除旧索引:   %v（默认保留，可直接回滚）\n", spec.DropOld)
	fmt.Printf("超时:         %s（收尾另给 %s 独立 ctx）\n", spec.Timeout, spec.FinalizeTO)
	fmt.Println("回滚语义:     rebuild 保留旧索引，出问题可把别名原子切回；--drop-old 后旧索引不可回滚。")
	fmt.Println("!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!")
}

func printRebuildResult(r *es.RebuildResult) {
	fmt.Println()
	fmt.Println("========== es:rebuild 结论 ==========")
	fmt.Printf("别名:         %s\n", r.Alias)
	if r.Cur != "" {
		fmt.Printf("旧索引:       %s\n", r.Cur)
	}
	fmt.Printf("新索引:       %s（声明 schema_version=%d）\n", r.Next, r.DeclaredSchemaVersion)
	if len(r.PluginsMissing) > 0 {
		fmt.Printf("缺失插件:     %v\n", r.PluginsMissing)
	}
	if r.DryRun {
		fmt.Printf("模式:         dry-run（只读）\n")
		fmt.Printf("读调用:       %d 次 %v\n", len(r.ReadOps), r.ReadOps)
		fmt.Printf("写调用:       %d 次 %v（空即零写操作）\n", len(r.WriteOps), r.WriteOps)
	} else {
		fmt.Printf("建索引:       %v  提速: %v  已恢复: %v\n", r.Created, r.Tuned, r.Restored)
		fmt.Printf("灌数:         扫描 %d，写入 %d，失败 %d，批次 %d，耗时 %s\n",
			r.Sync.Scanned, r.Sync.Sent, r.Sync.Failed, r.Sync.Batches, r.Sync.Elapsed.Round(time.Millisecond))
		// 只有真跑过对账才能报 ES/差值/达标；否则这三个值全零会给出“差=+0 达标=false”这种
		// 自相矛盾的假结论，必须如实说“未执行”并点名真正中止在哪一步。
		if r.ReconcileAttempted {
			fmt.Printf("对账:         DB=%d ES=%d 差=%+d 容差=%d 达标=%v\n", r.DBExpected, r.ESCount, r.Diff, r.Tolerance, r.Reconciled)
		} else {
			fmt.Printf("对账:         未执行（已中止于 %s，ES 计数未回读，不能当对账结论）\n", r.AbortedStep)
		}
		fmt.Printf("切换:         switched=%v  补偿=%v（扫描 %d/写入 %d/删除 %d/失败 %d）  保留旧索引=%v（已删旧索引=%v）\n",
			r.Switched, r.Compensated, r.Compensate.Scanned, r.Compensate.Indexed,
			r.Compensate.Deleted, r.Compensate.Failed, !r.OldDropped, r.OldDropped)
		if r.Compensate.Failed > 0 {
			fmt.Printf("⚠️  补偿有 %d 条 item 级失败：窗口增量未收敛完，补偿幂等，修复后可用同一 started_at 重跑\n",
				r.Compensate.Failed)
		}
		if r.Switched && r.PlannedRollbackBody != "" {
			fmt.Printf("回滚请求:     POST /_aliases %s\n", r.PlannedRollbackBody)
		}
		fmt.Printf("校验:         verified=%v\n", r.Verified)
		if r.VerifyNote != "" {
			fmt.Printf("校验说明:     %s\n", r.VerifyNote)
		}
		if len(r.WriteOps) > 0 {
			fmt.Printf("写调用:       %v\n", r.WriteOps)
		}
	}
	if r.AbortedStep != "" {
		fmt.Printf("中止步骤:     %s\n", r.AbortedStep)
	}
	fmt.Println("=====================================")
}
