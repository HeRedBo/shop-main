package main

// es_cutover.go 是 internal/es.RunCutover 的 CLI 装配层：
//   - 从 Registry 取索引声明，把 sync.SyncProducts 适配成 es.SyncFunc 窄接口
//     （#5 的签名再变，只有这个文件需要跟着改，割接编排不受冲击）；
//   - 负责护栏：--yes / TTY 交互确认、统一 --timeout、--dry-run、退出码；
//   - 负责失败路径的孤儿索引处置建议打印。

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

var esCutoverCmd = &cobra.Command{
	Use:   "es:cutover",
	Short: "同名割接：把物理索引换成同名别名（一次性、不可逆）",
	Long: "把仍占着别名位的**物理索引**（如 shop-product）一次性割接成同名别名 + 新物理索引 shop-product_vN。\n" +
		"\n" +
		"为什么需要单独一个命令：ES 里物理索引名与别名绝对互斥，既不能给 shop-product 挂\n" +
		"同名别名（400 invalid_alias_name_exception），也不能先建同名索引再改（400 invalid_index_name_exception），\n" +
		"_close 也解除不了冲突。唯一安全路径是**单次** POST /_aliases 里同时提交\n" +
		"  remove_index{shop-product} + add{shop-product_vN -> shop-product, is_write_index:true}\n" +
		"原子生效。本命令不会把它拆成两步（拆了就会留下既无索引也无别名的写入黑洞）。\n" +
		"\n" +
		"回滚语义（必须如实理解）：割接请求本身就是 remove_index，旧物理索引连同其数据在同一次\n" +
		"原子操作里消失。**不存在「保留旧索引直接切回」**，出问题时的恢复手段只有一个：\n" +
		"重跑本命令（或 es:sync-products）从 MySQL 全量重灌目标索引。这与 es:rebuild（别名已存在、\n" +
		"可保留旧索引回滚）是两套完全不同的语义。\n" +
		"\n" +
		"幂等与安全护栏：\n" +
		"  - 别名位已经是别名 => 打印现状并以退出码 0 结束，不做任何变更；\n" +
		"  - 目标索引已存在 => 拒绝覆盖并中止，绝不 DROP；\n" +
		"  - 对账（|ES - DB| > --tolerance）在切别名之前中止，别名不会被动过；\n" +
		"  - --dry-run 只做只读请求（GET/HEAD），写调用被审计层硬拦截，并打印将要下发的 _aliases 请求体；\n" +
		"  - 非 TTY 且未带 --yes 时直接退出码 1，避免脚本误触发；\n" +
		"  - 对账不达标可用 --force 放行（语义与 es:rebuild --force 完全一致）；\n" +
		"  - 割接生效后自动补跑一次差量补偿（下界=本次任务启动时刻），窗口内的增/改/删只能靠它收敛；\n" +
		"    补偿失败 / 有 item 级失败都会如实报错，不会被吞成成功。\n" +
		"\n" +
		"灌数走 op_type=index 幂等写入（create 对重复 _id 会 409 version_conflict_engine_exception）。\n" +
		"单节点集群健康恒为 yellow，属正常态，本命令不把它当故障。",
	Example: "  # 先预演：确认计划与 _aliases 请求体，零写操作\n" +
		"  go run ./cmd/cli es es:cutover --alias shop-product --dry-run\n" +
		"  # 正式执行（非交互环境必须带 --yes）\n" +
		"  go run ./cmd/cli es es:cutover --alias shop-product --yes --timeout 60m",
	// 刻意与其他 es:* 命令不一致：割接失败时刷屏 usage 会把「已不行」这条真错埋掉，
	// 也会让脚本误读成参数错误。usage 仍可通过 --help 拿到。
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runESCutover(cmd)
	},
}

// cutoverSpec 是 es:cutover 的 flags 快照。
type cutoverSpec struct {
	Alias      string
	IndicesDir string
	Tolerance  int64
	Force      bool
	DryRun     bool
	AssumeYes  bool
	AsJSON     bool
	Timeout    time.Duration
	FinalizeTO time.Duration

	TuneIngest  bool
	ChunkSize   int
	Concurrency int
	BulkSize    int
	MaxFailures int
}

func init() {
	f := esCutoverCmd.Flags()
	f.String("alias", "shop-product", "目标别名（割接后指向 <alias>_v<schema_version>）")
	f.String("indices-dir", "", "索引声明目录（空 → 内置默认）")
	f.Int64("tolerance", 0, "对账容差：|ES count - DB expected| > 该值即在切别名之前中止；负值=永不可达标，不会当 0 用")
	f.Bool("force", false, "对账不达标仍继续割接（危险，正常勿用；不能跳过切换后的差量补偿）")
	f.Bool("dry-run", false, "只读预演：仅 GET/HEAD，打印将执行的 _aliases 请求体")
	f.Bool("yes", false, "跳过交互确认（非 TTY 环境必须显式带上）")
	f.Bool("json", false, "以 JSON 输出割接结论（进度日志改走 stderr）")
	f.Duration("timeout", 60*time.Minute, "整体 ctx 超时；到期后恢复 settings / 别名校验仍用独立收尾 ctx 跑完")
	f.Duration("finalize-timeout", 60*time.Second, "收尾动作（恢复 settings、校验别名）的独立超时")
	f.Bool("tune-ingest", true, "灌数期临时关 refresh/副本（仅对不持有别名的新建物理索引安全）")
	// 灌数流水线参数：只暴露真正需要按机器规格调的几项，其余走 sync 内置默认。
	f.Int("chunk-size", 1000, "同步流水线每批读取行数")
	f.Int("concurrency", 4, "同步流水线 scanner/writer 各自的 goroutine 数")
	f.Int("bulk-size", 3000, "单个 bulk 的最大文档数")
	f.Int("max-failures", 100, "item 级失败累计超过该值即中断灌数（进而在切别名之前中止）")

	esRootCmd.AddCommand(esCutoverCmd)
}

func parseCutoverSpec(cmd *cobra.Command) (cutoverSpec, error) {
	f := cmd.Flags()
	s := cutoverSpec{}
	s.Alias, _ = f.GetString("alias")
	s.IndicesDir, _ = f.GetString("indices-dir")
	s.Tolerance, _ = f.GetInt64("tolerance")
	s.Force, _ = f.GetBool("force")
	s.DryRun, _ = f.GetBool("dry-run")
	s.AssumeYes, _ = f.GetBool("yes")
	s.AsJSON, _ = f.GetBool("json")
	s.Timeout, _ = f.GetDuration("timeout")
	s.FinalizeTO, _ = f.GetDuration("finalize-timeout")
	s.TuneIngest, _ = f.GetBool("tune-ingest")
	s.ChunkSize, _ = f.GetInt("chunk-size")
	s.Concurrency, _ = f.GetInt("concurrency")
	s.BulkSize, _ = f.GetInt("bulk-size")
	s.MaxFailures, _ = f.GetInt("max-failures")
	if s.Alias == "" {
		return s, fmt.Errorf("--alias 不能为空")
	}
	if s.Timeout < 0 || s.FinalizeTO < 0 {
		return s, fmt.Errorf("--timeout / --finalize-timeout 不能为负")
	}
	return s, nil
}

func runESCutover(cmd *cobra.Command) error {
	// enabled=false 时 initES 跳过初始化，此处按「无事可做」正常退出而不是报错。
	if !global.CONFIG.Elasticsearch.Enabled {
		fmt.Println("Elasticsearch 未启用（elasticsearch.enabled=false），跳过割接")
		return nil
	}
	if _, err := admin.Client(); err != nil {
		return err
	}
	spec, err := parseCutoverSpec(cmd)
	if err != nil {
		return err
	}

	// 声明必须来自 Registry，不接受命令行手搓 mapping：割接建出来的索引结构必须与
	// es:migrate / es:status 判定的指纹同源。
	reg, err := es.LoadRegistry(spec.IndicesDir)
	if err != nil {
		return fmt.Errorf("加载索引声明失败: %w", err)
	}
	def, ok := reg.Get(spec.Alias)
	if !ok {
		return fmt.Errorf("别名 %s 没有登记的索引声明（已登记别名：%v）", spec.Alias, reg.Aliases())
	}
	if !def.Executable() {
		return fmt.Errorf("声明 %s 的 status=%s 未接线，拒绝割接", def.Source(), def.Status())
	}
	target := def.Name()
	if target == spec.Alias {
		// 声明层不可能出现（版本号缺失时 Parse 就报错），留这道断言防止未来改动把
		// 「同名 add」这种必然 400 的请求体放进割接流程。
		return fmt.Errorf("目标索引名 %s 与别名同名，ES 中二者互斥，无法割接", target)
	}

	// 护栏：非 --yes 之外还要交互确认；非 TTY 直接退出码 1（不猜、不默认放行）。
	if !spec.DryRun && !spec.AssumeYes {
		if !cutoverStdinIsInteractive() {
			return fmt.Errorf("非 TTY 环境必须显式传 --yes 才能执行割接（本操作会摘除旧物理索引）")
		}
		printCutoverSummary(spec, def)
		if !confirmYes() {
			return fmt.Errorf("用户取消割接")
		}
	} else if spec.DryRun {
		fmt.Printf("[dry-run] 只读预演，不写 ES、不改 settings、不动别名\n")
	}

	// 统一超时：信号取消 + --timeout 叠加；--timeout 只作用于主 ctx，
	// 收尾动作（恢复 settings、校验别名）由编排层用 context.WithoutCancel 派生独立 ctx。
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
		// --json：stdout 只留一份可解析的结论，过程日志转 stderr。
		logf = func(format string, v ...any) { fmt.Fprintf(os.Stderr, format+"\n", v...) }
	}

	res, runErr := es.RunCutover(ctx, es.CutoverOptions{
		Alias:           spec.Alias,
		Definition:      def,
		Cluster:         es.NewAdminCluster(),
		Sync:            newCutoverSyncFunc(spec),
		Compensate:      newCutoverCompensateFunc(spec),
		DBCount:         sync.DBCountByAlias,
		Tolerance:       spec.Tolerance,
		Force:           spec.Force,
		DryRun:          spec.DryRun,
		TuneIngest:      spec.TuneIngest,
		FinalizeTimeout: spec.FinalizeTO,
		Logf:            logf,
	})

	if res != nil {
		if spec.AsJSON {
			if out, merr := json.MarshalIndent(res, "", "  "); merr == nil {
				fmt.Println(string(out))
			} else {
				logf("⚠️  结论序列化失败：%v", merr)
			}
		} else {
			printCutoverResult(res)
		}
		// 第 7 步：失败路径点名孤儿索引，只给处置建议，绝不自动删。
		if advisory := es.FormatOrphanAdvisory(spec.Alias, res.Orphans); advisory != "" {
			fmt.Fprintln(os.Stderr, "\n"+advisory)
		}
	}
	return runErr
}

// cutoverStdinIsInteractive 判标准输入是不是「人给的终端」，比包内已有的
// isStdinTTY()（只看 ModeCharDevice，属 #5 文件，不在我的改动范围）更严：
// /dev/null 也是字符设备，本地真机复现过：
//
//	go run ./cmd/cli es es:cutover --alias shop-product < /dev/null   # 未带 --yes 仍进交互分支
//
// 对一个会摘掉旧物理索引的命令，这种误判不能靠「confirmYes 读到 EOF 正好拒绝」兼得。
// 这里额外把 stdin 与 os.DevNull 做设备+inode 比对（os.SameFile），命中即按非交互处理。
// 不依赖 term.IsTerminal：macOS 的 tcgetattr 对 /dev/null 同样返回成功。
func cutoverStdinIsInteractive() bool {
	fi, err := os.Stdin.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return false // 管道 / 文件重定向
	}
	if devNull, oerr := os.Open(os.DevNull); oerr == nil {
		defer devNull.Close()
		if nullFI, serr := devNull.Stat(); serr == nil && os.SameFile(fi, nullFI) {
			return false // 字符设备，但输入被丢弃，不是人
		}
	}
	return true
}

// newCutoverSyncFunc 把按别名查表的灌数入口（sync.SyncByAlias）适配成 es.SyncFunc ——
// 全项目唯一一处割接层与 #5 流水线之间的耦合点（一期只有 shop-product 一条表项）。
//
// 三个刻意设置：
//   - TuneIngest=false：settings 的生命周期由割接编排独占（提速 + 无论成败都要恢复），
//     嵌套再开一次会在灌数结束时把 refresh 恢复回去，白丢提速窗口；
//   - Resume=false：断点文件按 target 命名，割接是一次性动作，不该留续传状态；
//   - Timeout=0：统一由外层 --timeout 的 ctx 控制，避免两层超时互相打架；
//   - Tolerance 固定用 es.IngestToleranceIgnore，**绝不把 --tolerance 下发给内层灌数**：
//     对账判定归割接编排第 4 步，下发下去会让 sync 抢在自己收尾对账里失败，中止步骤错报成
//     sync，而 --force 分支根本不可达（与 es:rebuild 同一结论，真机实测坐实过）。
func newCutoverSyncFunc(spec cutoverSpec) es.SyncFunc {
	return func(ctx context.Context, alias, target string) (es.SyncSummary, error) {
		stats := &sync.SyncStats{}
		_, err := sync.SyncByAlias(ctx, alias, sync.Options{
			Index:       target,
			ChunkSize:   spec.ChunkSize,
			Concurrency: spec.Concurrency,
			BulkSize:    spec.BulkSize,
			MaxFailures: spec.MaxFailures,
			Tolerance:   es.IngestToleranceIgnore, // 对账归割接编排，不外包给灌数层
			IndicesDir:  spec.IndicesDir,
			AssumeYes:   true,
			TuneIngest:  false,
			Resume:      false,
			DryRun:      false,
			Timeout:     0,
			Stats:       stats,
		})
		summary := es.SyncSummary{
			Target:  target,
			Scanned: stats.ScannedDocs.Load(),
			Sent:    stats.SentDocs.Load(),
			Failed:  stats.FailedDocs.Load(),
			Batches: stats.Batches.Load(),
			Elapsed: stats.Elapsed(),
		}
		return summary, err
	}
}

// newCutoverCompensateFunc 把 sync.CompensateByAlias 适配成 es.CompensateFunc（割接后的差量补偿注入点）。
// since 由 RunCutover 固化为编排起点（与 rebuild 同一语义），这里绝不重新取 time.Now()；
// Stats 必须原样带出，否则 bulk 里的 item 级失败（含 strict_dynamic）会被吞成成功。
func newCutoverCompensateFunc(spec cutoverSpec) es.CompensateFunc {
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

// printCutoverSummary 打印影响面摘要（交互确认前）。
func printCutoverSummary(spec cutoverSpec, def *es.IndexDefinition) {
	body, _ := es.CutoverAliasBody(spec.Alias, def.Name())
	fmt.Println("!!!!!!!!!!!!!!!! es:cutover !!!!!!!!!!!!!!!!")
	fmt.Printf("别名:         %s（当前仍是物理索引）\n", spec.Alias)
	fmt.Printf("目标索引:     %s（声明 %s，schema_version=%d）\n", def.Name(), def.Source(), def.Version())
	fmt.Printf("依赖插件:     %v\n", def.RequiresPlugins())
	fmt.Printf("对账容差:     %d（DB 口径 is_del=0 AND is_show=1；负值=永不可达标）\n", spec.Tolerance)
	fmt.Printf("--force:      %v（对账不达标仍继续割接，但绝不跳过切换后的差量补偿）\n", spec.Force)
	fmt.Printf("灌数提速:     %v\n", spec.TuneIngest)
	fmt.Printf("超时:         %s（收尾另给 %s 独立 ctx）\n", spec.Timeout, spec.FinalizeTO)
	fmt.Printf("将下发:       POST /_aliases %s\n", body)
	fmt.Println("风险提示:     remove_index 与 add 同请求原子生效，旧物理索引连同数据一起消失；")
	fmt.Println("              不存在「保留旧索引直接切回」，恢复手段只有重跑全量灌入。")
	fmt.Println("              灌数窗口内的增/改/删靠切换后的一次差量补偿收敛（补偿不做即丢数据）。")
	fmt.Println("!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!")
}

// printCutoverResult 打印割接结论。
func printCutoverResult(r *es.CutoverResult) {
	fmt.Println()
	fmt.Println("========== es:cutover 结论 ==========")
	fmt.Printf("别名/目标:    %s -> %s\n", r.Alias, r.Target)
	fmt.Printf("判定:         %s", r.Verdict)
	if r.Idempotent {
		fmt.Printf("（幂等：未做任何变更）")
	}
	fmt.Println()
	if r.DryRun {
		fmt.Printf("模式:         dry-run（只读）\n")
		fmt.Printf("读调用:       %d 次 %v\n", len(r.ReadOps), r.ReadOps)
		fmt.Printf("写调用:       %d 次 %v（空即零写操作）\n", len(r.WriteOps), r.WriteOps)
	} else {
		fmt.Printf("建索引:       %v  提速: %v  已恢复: %v\n", r.Created, r.Tuned, r.Restored)
		fmt.Printf("灌数:         扫描 %d，写入 %d，失败 %d，批次 %d，耗时 %s\n",
			r.Sync.Scanned, r.Sync.Sent, r.Sync.Failed, r.Sync.Batches, r.Sync.Elapsed.Round(time.Millisecond))
		fmt.Printf("对账:         DB=%d ES=%d 差=%+d 容差=%d 达标=%v\n", r.DBExpected, r.ESCount, r.Diff, r.Tolerance, r.Reconciled)
		if !r.Reconciled && r.Force && r.Switched {
			fmt.Printf("⚠️  对账不达标但 --force 已开启，已继续割接（实况：switched=%v verified=%v）\n", r.Switched, r.Verified)
		}
		fmt.Printf("差量补偿:     compensated=%v（扫描 %d/写入 %d/删除 %d/失败 %d，下界 %s）\n",
			r.Compensated, r.Compensate.Scanned, r.Compensate.Indexed,
			r.Compensate.Deleted, r.Compensate.Failed, r.StartedAt.Format(time.RFC3339))
		if r.Compensate.Failed > 0 {
			fmt.Printf("⚠️  补偿有 %d 条 item 级失败：旧索引已被 remove_index，这些文档只能人工回补\n",
				r.Compensate.Failed)
		}
		fmt.Printf("割接/校验:    switched=%v verified=%v\n", r.Switched, r.Verified)
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
