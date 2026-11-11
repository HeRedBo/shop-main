package es

// rebuild.go 实现方案第六节「蓝绿重建与零停机切换」（es:rebuild 的编排内核）。
//
// 与 es:cutover 的分工（务必区分，二者语义相反）：
//   - cutover 的起点是「shop-product 仍是物理索引、别名不存在」，要把物理索引换成同名
//     别名，唯一安全路径是单次 _aliases 里 remove_index + add，旧物理索引连同数据一起消失；
//   - rebuild 的起点是「别名已经存在」（v0 -> v1），新建 v1、灌数、对账达标后**单次**
//     _aliases 原子切换：add v1 + remove v0（只解除别名、**保留旧索引**），因此默认可以
//     直接把别名原子切回旧索引回滚。--drop-old 是唯一会物理删除旧索引的开关。
//
// 三条不可让的语义（连续多轮踩坑，本层用类型与 defer 把死）：
//  1. 对账（Refresh + Count vs DB 期望）必须在 _aliases 切换**之前**，不达标即中止，
//     别名保持指向旧索引，线上读不受影响；且这份判定只在本层做 —— --tolerance 绝不下发给
//     内层灌数（IngestToleranceIgnore），否则 sync 会抢先失败，中止步骤错报成 sync、
//     结论行的 ES 计数是从未回读的假 0、--force 分支不可达；
//  2. _aliases 必须**单请求**原子下发（add + remove 同一次 Do）。实测拆两步：第一步之后
//     再写别名会得到 201，ES 自动建同名空索引，写入黑洞比 cutover 更彻底 —— 严禁拆分；
//  3. 灌入期调优「注册恢复先于变更」，且恢复/校验一律用 context.WithoutCancel 派生的独立
//     ctx：主 ctx 被 --timeout/信号打断时，也要把 refresh/replicas 恢复回声明值，绝不能
//     把索引留在 refresh_interval=-1 / replicas=0（sync 层刚踩过这个坑）。
//
// 解耦（照 cutover.go 已验证的做法）：灌数复用包内既有窄接口 SyncFunc，差量补偿声明成本地
// 窄接口 CompensateFunc，二者都由 cmd/cli 在装配层注入 sync 包的具体实现；本文件不 import
// internal/es/sync 的任何内部类型，sync 的签名再变只冲击 CLI 装配层。

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"shop/internal/es/admin"
)

// ---------------------------------------------------------------------------
// 窄接口：差量补偿回调 + rebuild 专属集群原语
// ---------------------------------------------------------------------------

// CompensateSummary 是差量补偿回调返回的最小统计集。
//
// 与 SyncSummary 同理，刻意不引用 sync.Stats：那个类型属于仍在重构的 #5 内部，
// 跨层引用会把它的字段变动直接传导进割接/重建编排。**Failed 必须带出来**：
// 早先版本回调只返回 error，sync.Compensate 累计的 item 级失败（含 strict_dynamic
// 结构类失败）整条被丢弃，失败 bulk 仍被置成 Compensated=true —— 虚假成功。
type CompensateSummary struct {
	Scanned int64 // 从 DB 读出的补偿行数
	Indexed int64 // 判定为「应入索引」并写入的文档数
	Deleted int64 // 判定为「不应入索引」并发出 delete 的文档数
	Failed  int64 // item 级失败文档数（>0 即补偿未收敛，绝不能当成功）
	Batches int64 // 提交的 bulk 批次
}

// CompensateFunc 是蓝绿切换「之后」的差量补偿入口，由 CLI 装配成对 sync.Compensate 的调用。
//
// since 语义（最关键）：必须是本次重建任务**首次启动时刻**，早于它的变更已被全量灌入覆盖，
// 晚于它的（灌数窗口内的改/删）必须靠这次补偿收敛。取错下界会静默漏数据。
// target 是切换后的新物理索引名（此刻别名也已指向它，二者等价，显式用物理名更直观）。
//
// 返回值必须同时给出统计与 error：error 只表达「补偿没跑完」（ctx 取消/扫描失败），
// Stats.Failed 表达「跑完了但有文档没写进去」，两类缺陷语义不同，合并成一个 error
// 就会把结构类失败吞成成功。
type CompensateFunc func(ctx context.Context, target string, since time.Time) (CompensateSummary, error)

// RebuildOps 是 rebuild 相较 Cluster 额外需要的两个原语，都带删/留语义区分：
//   - SwitchAliasKeepOld：单次 POST /_aliases，add next + remove cur（仅解除别名，保留 cur）；
//     绝不能用 Cluster.SwitchAlias（那是 cutover 的 remove_index，会连数据删掉旧索引）；
//   - DeleteIndex：物理删除 + 失效存在性缓存，仅供 --drop-old 显式调用。
type RebuildOps interface {
	SwitchAliasKeepOld(ctx context.Context, alias, next, cur string) error
	DeleteIndex(ctx context.Context, idx string) error
}

// RebuildTarget 是 rebuild 编排依赖的完整能力面：Cluster 的全部读写 + 两个 rebuild 专属原语。
type RebuildTarget interface {
	Cluster
	RebuildOps
}

// IngestToleranceIgnore 是装配层给**内层灌数**（sync.Options.Tolerance）用的哨兵值。
//
// 对账判定归 runRebuild 独占：第 5 步拿 Refresh + Count 的真实回读值与 DBCount 比，
// 容差取 --tolerance，不达标才中止，--force 才能生效。把 --tolerance 原样下发给内层灌数
// 会让 sync 在自己的收尾对账里先行失败，rebuild 只能在 AbortedStep="sync" 处退出：
// 此刻第 5 步根本没跑，结论行的 ES=0 是「从未回读」的假数，与 差=+0 达标=false 一起自相
// 矛盾，运维拿到的失败原因是错的。内层只负责灌，判定口径不接受外包。
const IngestToleranceIgnore int64 = math.MaxInt64

// AdminRebuild 是 RebuildTarget 的生产实现：Cluster 部分直接复用 AdminCluster（同包），
// 两个专属原语走 internal/es/admin 已定稿的封装，不新引任何依赖。
type AdminRebuild struct {
	AdminCluster
}

// NewAdminRebuild 返回生产实现。零值可用，构造器只为调用点可读性存在。
func NewAdminRebuild() *AdminRebuild { return &AdminRebuild{} }

// SwitchAliasKeepOld 以**单次** POST /_aliases 完成蓝绿切换：add next（挂上别名）、
// remove cur（仅解除 cur 与别名的关联，**不删** cur）。
//
// 刻意走 admin.AtomicSwitchAlias(removeEntireAlias=false)：该函数内部只发一个 svc.Do()，
// 天然满足「单请求原子」；且提交前会断言 removeIndexes(cur) 当前确实持有 alias —— rebuild
// 起点别名已存在，这个前提可满足（与 cutover 里物理索引名恒不满足的情况相反）。
func (AdminRebuild) SwitchAliasKeepOld(ctx context.Context, alias, next, cur string) error {
	if err := admin.AtomicSwitchAlias(ctx, alias, []string{next}, []string{cur}, false); err != nil {
		return fmt.Errorf("es: 蓝绿原子切换失败 [alias=%s: %s -> %s]: %w", alias, cur, next, err)
	}
	return nil
}

// DeleteIndex 物理删除索引；admin.Delete 成功后已调用 DeleteIndexCache 失效存在性缓存。
func (AdminRebuild) DeleteIndex(ctx context.Context, idx string) error {
	return admin.Delete(ctx, idx)
}

// ---------------------------------------------------------------------------
// dry-run 写操作审计（复用 auditedCluster 的读/写台账，额外审计两个专属原语）
// ---------------------------------------------------------------------------

// auditedRebuild 在 Cluster 各方法之上叠加 RebuildOps 的审计与 dry-run 硬拒，
// 与 auditedCluster 共用同一份 read/write 台账，让「--dry-run 零写操作」可被单点证明。
type auditedRebuild struct {
	*auditedCluster
	ops RebuildOps
}

func newAuditedRebuild(t RebuildTarget, dryRun bool) *auditedRebuild {
	return &auditedRebuild{
		auditedCluster: newAuditedCluster(t, dryRun),
		ops:            t,
	}
}

func (a *auditedRebuild) SwitchAliasKeepOld(ctx context.Context, alias, next, cur string) error {
	if err := a.note("write", "POST /_aliases"); err != nil {
		return err
	}
	return a.ops.SwitchAliasKeepOld(ctx, alias, next, cur)
}

func (a *auditedRebuild) DeleteIndex(ctx context.Context, idx string) error {
	if err := a.note("write", "DELETE /"+idx); err != nil {
		return err
	}
	return a.ops.DeleteIndex(ctx, idx)
}

// ---------------------------------------------------------------------------
// 纯判定 / 请求体生成（可单测内核）
// ---------------------------------------------------------------------------

// rebuildSwitchAction 固定蓝绿 _aliases 请求体的键顺序（map 序列化会随机化 key，
// 与运维排障时看到的真机样本必须逐字一致）。
type rebuildSwitchAction struct {
	Add    *switchAdd    `json:"add,omitempty"`
	Remove *switchRemove `json:"remove,omitempty"`
}
type switchAdd struct {
	Index string `json:"index"`
	Alias string `json:"alias"`
}
type switchRemove struct {
	Index string `json:"index"`
	Alias string `json:"alias"`
}

// RebuildSwitchBody 生成蓝绿切换的 POST /_aliases 请求体：add next -> alias，
// remove cur（仅解除，保留 cur）。也是 --dry-run 要打印、且与 AdminRebuild.SwitchAliasKeepOld
// 实际下发的同一对 action。顺序固定先 add 后 remove，与真机验证样本一致。
func RebuildSwitchBody(alias, next, cur string) (string, error) {
	if strings.TrimSpace(alias) == "" || strings.TrimSpace(next) == "" {
		return "", fmt.Errorf("es: 切换请求体需要非空的别名与目标索引")
	}
	if next == alias {
		return "", fmt.Errorf("es: 目标索引名 %s 不能与别名同名（ES 中二者绝对互斥）", next)
	}
	actions := []rebuildSwitchAction{{Add: &switchAdd{Index: next, Alias: alias}}}
	if cur != "" && cur != next {
		actions = append(actions, rebuildSwitchAction{Remove: &switchRemove{Index: cur, Alias: alias}})
	}
	b, err := json.Marshal(map[string]any{"actions": actions})
	if err != nil {
		return "", fmt.Errorf("es: 序列化蓝绿切换请求体失败: %w", err)
	}
	return string(b), nil
}

// RebuildRollbackBody 生成「把别名原子切回旧索引」的 POST /_aliases 请求体：
// add cur -> alias，remove next（仅解除，保留 next）。默认保留旧索引就是为了这条退路。
func RebuildRollbackBody(alias, next, cur string) (string, error) {
	if cur == "" {
		return "", fmt.Errorf("es: 旧索引名为空，无法生成回滚请求")
	}
	actions := []rebuildSwitchAction{
		{Add: &switchAdd{Index: cur, Alias: alias}},
		{Remove: &switchRemove{Index: next, Alias: alias}},
	}
	b, err := json.Marshal(map[string]any{"actions": actions})
	if err != nil {
		return "", fmt.Errorf("es: 序列化回滚请求体失败: %w", err)
	}
	return string(b), nil
}

// VerifyRebuildTarget 校验蓝绿切换结果：别名必须只指向 next（旧索引是否保留不影响此判定）。
// 与 VerifyCutoverTarget 的差别：rebuild 不强制 is_write_index（单索引隐式即为写索引）。
func VerifyRebuildTarget(rows []AliasTarget, alias, next string) (bool, string) {
	var targets []AliasTarget
	for _, r := range rows {
		if r.Alias == alias {
			targets = append(targets, r)
		}
	}
	if len(targets) == 0 {
		return false, fmt.Sprintf("别名 %s 不存在，切换未生效", alias)
	}
	if len(targets) > 1 {
		names := make([]string, 0, len(targets))
		for _, t := range targets {
			names = append(names, t.Index)
		}
		return false, fmt.Sprintf("别名 %s 指向 %d 个索引 %v，蓝绿切换后应只剩 %s", alias, len(targets), names, next)
	}
	if targets[0].Index != next {
		return false, fmt.Sprintf("别名 %s 指向 %s，与期望目标 %s 不符", alias, targets[0].Index, next)
	}
	return true, fmt.Sprintf("别名 %s -> %s 已切换（旧索引已保留，可直接回滚）", alias, next)
}

// FormatRebuildOrphanAdvisory 生成重建失败路径的孤儿索引处置建议。与 cutover 版不同：
// 这里明确「旧索引仍在、别名未动，线上读不受影响」，孤儿只是灌到一半的新索引。
func FormatRebuildOrphanAdvisory(alias, cur, next string, kept bool) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("重建中止，别名 %s 未被改动", alias))
	if cur != "" {
		sb.WriteString(fmt.Sprintf("，仍指向旧索引 %s（线上读不受影响）", cur))
	}
	sb.WriteString("\n")
	if kept {
		sb.WriteString(fmt.Sprintf("新索引 %s 已保留供排查（未删除），确认无用后手工删除：DELETE /%s\n", next, next))
		sb.WriteString("或修复原因后重跑 es:rebuild；重跑会因 HEAD /" + next + " 命中而以「目标已存在」中止，须先删除该残留。\n")
	} else {
		sb.WriteString(fmt.Sprintf("新索引 %s 若已创建需人工确认处置；本次未自动删除任何索引。\n", next))
	}
	return sb.String()
}

// ---------------------------------------------------------------------------
// 编排
// ---------------------------------------------------------------------------

// RebuildOptions 蓝绿重建编排入参。Cluster / Sync / Compensate / DBCount 全部注入，
// 让 #5 流水线与差量补偿的签名变动只冲击 CLI 装配层，也让单测无需真连 ES/MySQL。
type RebuildOptions struct {
	Alias      string
	Definition *IndexDefinition
	Target     RebuildTarget // nil → NewAdminRebuild()
	Sync       SyncFunc      // 灌数（复用 cutover.go 定义的窄接口）
	Compensate CompensateFunc
	DBCount    CountFunc // nil → ProductDocCount

	Tolerance        int64
	DryRun           bool
	DropOld          bool // 仅在切换成功后删除旧索引
	KeepNewOnFailure bool // 失败时保留新索引供排查（默认 true 由 CLI 决定）
	Force            bool // 跳过对账不达标即中止（谨慎）
	TuneIngest       bool
	FinalizeTimeout  time.Duration

	Logf func(format string, v ...any)
}

// RebuildResult 一次重建的完整结论，供 CLI 打印与 --json 序列化。
type RebuildResult struct {
	Alias                 string `json:"alias"`
	Next                  string `json:"next"`
	Cur                   string `json:"cur"`
	DeclaredSchemaVersion int    `json:"declared_schema_version"`
	DryRun                bool   `json:"dry_run"`

	PluginsInstalled []string `json:"plugins_installed"`
	PluginsRequired  []string `json:"plugins_required"`
	PluginsMissing   []string `json:"plugins_missing"`

	DBExpected          int64  `json:"db_expected"`
	Tolerance           int64  `json:"tolerance"` // 本次生效的容差（负值=永不可达标，不当 0 用）
	PlannedCreateBody   string `json:"planned_create_body,omitempty"`
	PlannedSwitchBody   string `json:"planned_switch_body,omitempty"`
	PlannedRollbackBody string `json:"planned_rollback_body,omitempty"`

	Sync       SyncSummary `json:"sync"`
	ESCount    int64       `json:"es_count"`
	Diff       int64       `json:"count_diff"`
	Reconciled bool        `json:"reconciled"`
	// ReconcileAttempted 区分「对账跑了但不达标」与「根本没能走到对账」：
	// 前者 ESCount/Diff 是真实回读值，后者全是零值，结论行必须据此如实表达。
	ReconcileAttempted bool `json:"reconcile_attempted"`
	Created            bool `json:"created"`
	Tuned              bool `json:"tuned"`
	Restored           bool `json:"settings_restored"`
	Switched           bool `json:"switched"`
	Compensated        bool `json:"compensated"`
	// Compensate 是补偿回调带回的统计（Failed>0 即「别名已切、但窗口增量没收敛完」）。
	// Compensated 只在补偿无 error 且 Failed==0 时置 true，不再对失败 bulk 放行。
	Compensate CompensateSummary `json:"compensate"`
	OldDropped bool              `json:"old_dropped"`
	Verified   bool              `json:"verified"`
	VerifyNote string            `json:"verify_note,omitempty"`

	StartedAt   time.Time `json:"started_at"`
	Orphans     []string  `json:"orphans,omitempty"`
	AbortedStep string    `json:"aborted_step,omitempty"`

	ReadOps  []string `json:"read_ops"`
	WriteOps []string `json:"write_ops"`
}

// RunRebuild 按方案第六节执行蓝绿重建。返回的 *RebuildResult 恒非 nil（即便报错），
// 以便 CLI 打印已发生的读写台账与中止步骤。
func RunRebuild(ctx context.Context, opts RebuildOptions) (*RebuildResult, error) {
	if opts.Definition == nil {
		return nil, fmt.Errorf("es: 重建需要索引声明（Definition 为 nil）")
	}
	if strings.TrimSpace(opts.Alias) == "" {
		return nil, fmt.Errorf("es: 重建需要目标别名")
	}
	if opts.Alias != opts.Definition.Alias() {
		return nil, fmt.Errorf("es: 别名 %s 与声明 %s 的 alias=%s 不一致",
			opts.Alias, opts.Definition.Source(), opts.Definition.Alias())
	}
	if !opts.Definition.Executable() {
		return nil, fmt.Errorf("es: 声明 %s 的 status=%s 未接线，拒绝重建", opts.Definition.Source(), opts.Definition.Status())
	}
	logf := opts.Logf
	if logf == nil {
		logf = func(format string, v ...any) { fmt.Printf(format+"\n", v...) }
	}
	if opts.DBCount == nil {
		opts.DBCount = ProductDocCount
	}
	target := opts.Target
	if target == nil {
		target = NewAdminRebuild()
	}

	audit := newAuditedRebuild(target, opts.DryRun)
	c := RebuildTarget(audit)

	alias := opts.Alias
	next := opts.Definition.Name() // <alias>_v<schema_version>
	// startedAt 在编排最开始就固化：它是对账与差量补偿的共同下界，语义是「本次任务首次启动」。
	// 即便灌数中途被打断、用独立 ctx 收尾，补偿仍按这个时刻扫描。
	startedAt := time.Now()
	res := &RebuildResult{
		Alias:                 alias,
		Next:                  next,
		DeclaredSchemaVersion: opts.Definition.Version(),
		DryRun:                opts.DryRun,
		PluginsRequired:       opts.Definition.RequiresPlugins(),
		Tolerance:             opts.Tolerance,
		StartedAt:             startedAt,
	}
	defer func() {
		res.ReadOps = audit.ReadOps()
		res.WriteOps = audit.WriteOps()
	}()

	// ---- 1. 前置校验（全部只读）：别名已存在且唯一、next 空闲、插件齐备 ----
	logf("[1/8] 前置校验：别名指向、目标名、插件")
	plugins, err := c.ListPlugins(ctx)
	if err != nil {
		res.AbortedStep = "precheck_plugins"
		return res, err
	}
	res.PluginsInstalled = plugins
	missing := MissingPlugins(plugins, opts.Definition.RequiresPlugins())
	res.PluginsMissing = missing
	if len(missing) > 0 {
		res.AbortedStep = "precheck_plugins"
		return res, fmt.Errorf("es: 依赖插件缺失 %v（已安装：%v），重建会建出分词器不可用的索引，已中止", missing, plugins)
	}
	curTargets, err := c.AliasTargets(ctx, alias)
	if err != nil {
		res.AbortedStep = "precheck_alias"
		return res, err
	}
	switch len(curTargets) {
	case 0:
		res.AbortedStep = "precheck_alias"
		return res, fmt.Errorf("es: 别名 %s 不存在（该名称仍是物理索引或尚未创建），蓝绿重建的前置是别名已就位；如未割接请先执行 es:cutover", alias)
	case 1:
		res.Cur = curTargets[0]
	default:
		res.AbortedStep = "precheck_alias"
		return res, fmt.Errorf("es: 别名 %s 指向 %d 个索引 %v，蓝绿重建要求别名唯一指向单一旧索引，请先人工收敛", alias, len(curTargets), curTargets)
	}
	if res.Cur == next {
		res.AbortedStep = "precheck_next"
		return res, fmt.Errorf("es: 别名已指向本次声明的目标 %s，无需重建", next)
	}
	nextExists, err := c.IndexExists(ctx, next)
	if err != nil {
		res.AbortedStep = "precheck_next"
		return res, err
	}
	if nextExists {
		res.AbortedStep = "precheck_next"
		res.Orphans = []string{next}
		return res, fmt.Errorf("es: 目标索引 %s 已存在（上次重建残留），拒绝覆盖；请先 es:cleanup 删除该残留再重跑", next)
	}

	dbExpected, err := opts.DBCount(ctx)
	if err != nil {
		res.AbortedStep = "precheck_db_count"
		return res, err
	}
	res.DBExpected = dbExpected
	// BodyJSON 已剔除 aliases，蓝绿新建本就要求「不带 aliases」，此处直接复用。
	createBody, err := opts.Definition.BodyJSON()
	if err != nil {
		res.AbortedStep = "precheck_definition"
		return res, err
	}
	res.PlannedCreateBody = createBody
	res.PlannedSwitchBody = mustBody(RebuildSwitchBody(alias, next, res.Cur))
	res.PlannedRollbackBody = mustBody(RebuildRollbackBody(alias, next, res.Cur))

	// ---- dry-run：到此为止，只打印计划（写路径已被审计层拒绝，一条都不会发）----
	if opts.DryRun {
		logf("[dry-run] 蓝绿重建计划（本次零写操作，全部只读 GET/HEAD）：")
		logf("      别名现状:   %s -> %s（旧索引将保留）", alias, res.Cur)
		logf("      1) PUT /%s            按声明新建（settings+mappings，不带 aliases）", next)
		logf("      2) PUT /%s/_settings  灌数提速 %s", next, IngestTuneBody())
		logf("      3) 灌数               目标 %s，op_type=index 幂等，期望 %d 条", next, dbExpected)
		logf("      4) POST /%s/_refresh  +  GET /%s/_count  对账（切别名之前）", next, next)
		logf("      5) POST /_aliases     %s（单请求原子，保留旧索引）", res.PlannedSwitchBody)
		logf("      6) 差量补偿           自 %s 起扫描（Unscoped，含删分支）", startedAt.Format(time.RFC3339))
		logf("      7) 保留旧索引 %s 并打印回滚请求：%s", res.Cur, res.PlannedRollbackBody)
		if opts.DropOld {
			logf("      8) DELETE /%s        --drop-old：切换成功后删除旧索引 + 失效缓存", res.Cur)
		} else {
			logf("      8) （未开启 --drop-old，旧索引 %s 保留可直接回滚）", res.Cur)
		}
		logf("[dry-run] 只读调用清单（%d 次，全部 GET/HEAD）：%v", len(audit.ReadOps()), audit.ReadOps())
		logf("[dry-run] 写调用清单：%v（空即零写操作已证明）", audit.WriteOps())
		return res, nil
	}

	// ---- 2. 新建 next（不带 aliases，读写都触达不到它）----
	logf("[2/8] 创建新索引 %s（声明：%s）", next, opts.Definition.Source())
	if err := c.CreateIndex(ctx, next, createBody); err != nil {
		res.AbortedStep = "create"
		return res, err
	}
	res.Created = true

	// 收尾独立 ctx + 幂等恢复：注册先于任何 settings 变更（sync 层教训）。
	finalize := func() (context.Context, context.CancelFunc) {
		d := opts.FinalizeTimeout
		if d <= 0 {
			d = defaultFinalizeTimeout
		}
		return context.WithTimeout(context.WithoutCancel(ctx), d)
	}
	restoreSettings := func() {
		if !res.Tuned || res.Restored {
			return
		}
		fctx, cancel := finalize()
		defer cancel()
		if err := c.PutSettings(fctx, next, IngestRestoreBody(opts.Definition.SettingsMap())); err != nil {
			logf("      ⚠️  恢复 settings 失败（收尾 ctx 已耗尽）：%v —— 请手工对 %s 恢复 refresh_interval/replicas", err, next)
			return
		}
		res.Restored = true
		logf("      settings 已恢复：%s", IngestRestoreBody(opts.Definition.SettingsMap()))
	}
	failKeepNext := func(step string, err error) (*RebuildResult, error) {
		res.AbortedStep = step
		restoreSettings()
		if opts.KeepNewOnFailure {
			res.Orphans = []string{next}
			logf("      已保留新索引 %s 供排查（别名未动，旧索引 %s 仍在服务）", next, res.Cur)
		}
		return res, err
	}

	// ---- 3. 灌入期调优（先注册恢复，再变更）----
	if opts.TuneIngest {
		if err := c.PutSettings(ctx, next, IngestTuneBody()); err != nil {
			return failKeepNext("tune_ingest", err)
		}
		res.Tuned = true
		logf("[3/8] 灌数提速已开启：refresh=-1、replicas=0（单节点健康呈 yellow，属预期）")
	}

	// ---- 4. 灌数（窄接口注入，op_type=index 幂等）----
	if opts.Sync == nil {
		return failKeepNext("sync", fmt.Errorf("es: 未注入灌数回调（SyncFunc 为 nil）"))
	}
	logf("[4/8] 灌入 %s（从 MySQL 全量，op_type=index 幂等）", next)
	summary, err := opts.Sync(ctx, next)
	res.Sync = summary
	if err != nil {
		return failKeepNext("sync", fmt.Errorf("es: 灌数失败 [target=%s]: %w", next, err))
	}
	restoreSettings()
	if summary.Failed > 0 {
		return failKeepNext("sync_failures", fmt.Errorf("es: 灌数存在 %d 条 item 级失败，切别名前中止", summary.Failed))
	}

	// ---- 5. 对账（**必须在 _aliases 切换之前**，别名保持指向旧索引）----
	logf("[5/8] 对账：DB 期望 %d 条 vs %s/_count（容差 %d，判定在本层，不由内层灌数代劳）", dbExpected, next, opts.Tolerance)
	res.ReconcileAttempted = true
	if err := c.Refresh(ctx, next); err != nil {
		return failKeepNext("reconcile_refresh", err)
	}
	esCount, err := c.Count(ctx, next)
	if err != nil {
		return failKeepNext("reconcile_count", err)
	}
	res.ESCount = esCount
	// 负容差 = 永不可达标的口径只由 ReconcileCounts 一处拥有（见其注释），
	// 这里不再自己补一道「负值归 0 / 强制 false」，两层各写一套正是审查报出的分歧源。
	cntDiff, ok := ReconcileCounts(dbExpected, esCount, opts.Tolerance)
	res.Diff = cntDiff
	res.Reconciled = ok
	logf("      ES=%d（真实回读），差值=%+d，容差=%d，达标=%v", esCount, cntDiff, opts.Tolerance, ok)
	if !ok {
		if !opts.Force {
			return failKeepNext("reconcile", fmt.Errorf(
				"es: 对账不达标（差 %+d 超容差 %d），已在切别名之前中止，别名 %s 仍指向旧索引 %s，线上读不受影响",
				cntDiff, opts.Tolerance, alias, res.Cur))
		}
		logf("      ⚠️  对账不达标但 --force 已开启，继续切换（谨慎）")
	}

	// ---- 6. 单次 _aliases 原子切换（add next + remove cur，保留旧索引）----
	logf("[6/8] 原子切换：POST /_aliases %s", res.PlannedSwitchBody)
	if err := c.SwitchAliasKeepOld(ctx, alias, next, res.Cur); err != nil {
		// 切换失败：别名可能已动也可能没动，交人工判定，不自动删任何索引。
		res.AbortedStep = "switch"
		res.Orphans = []string{next}
		return res, err
	}
	res.Switched = true

	// ---- 7. 差量补偿（切换之后，补写/补删自然落到新索引）----
	if opts.Compensate == nil {
		res.AbortedStep = "compensate"
		return res, fmt.Errorf("es: 已切换别名但未注入差量补偿回调（CompensateFunc 为 nil），"+
			"灌数窗口内的增量未收敛，请手工对 %s 跑一次 update_time >= %s 的补偿",
			next, startedAt.Format(time.RFC3339))
	}
	logf("[7/8] 差量补偿：扫描 update_time >= %s（Unscoped，含删分支），目标 %s", startedAt.Format(time.RFC3339), next)
	cstats, cerr := opts.Compensate(ctx, next, startedAt)
	res.Compensate = cstats
	if cerr != nil {
		// 补偿失败不回滚切换（线上已读新索引），只报错交人工重跑补偿（幂等）。
		res.AbortedStep = "compensate"
		return res, fmt.Errorf("es: 差量补偿失败 [target=%s]（别名已切到 %s，补偿幂等可重跑）: %w", next, next, cerr)
	}
	// Failed>0 必须如实上报：bulk 里有文档没落进新索引，窗口增量就还没收敛完，
	// 置 Compensated=true 等于给运维一份虚假成功报表。
	if cstats.Failed > 0 {
		res.AbortedStep = "compensate_failures"
		return res, fmt.Errorf("es: 差量补偿存在 %d 条 item 级失败 [target=%s，扫描 %d/写入 %d/删除 %d]"+
			"（别名已切到 %s，不回滚；补偿幂等，修复结构问题后可用同一 since 重跑）",
			cstats.Failed, next, cstats.Scanned, cstats.Indexed, cstats.Deleted, next)
	}
	res.Compensated = true

	// ---- 8. 保留旧索引 + 打印回滚指令；--drop-old 才删除 ----
	logf("[8/8] 完成。默认保留旧索引 %s，可直接回滚：", res.Cur)
	logf("      回滚请求（POST /_aliases）：%s", res.PlannedRollbackBody)
	if opts.DropOld {
		logf("      --drop-old 已开启：删除旧索引 %s 并失效存在性缓存", res.Cur)
		if err := c.DeleteIndex(ctx, res.Cur); err != nil {
			res.AbortedStep = "drop_old"
			return res, fmt.Errorf("es: 切换与补偿已完成，但删除旧索引 %s 失败（不影响读，可手工删除）: %w", res.Cur, err)
		}
		res.OldDropped = true
	}

	// ---- 校验别名指向（独立 ctx：切完正好 --timeout 到期时校验仍要跑完）----
	fctx, cancel := finalize()
	defer cancel()
	rows, err := c.ListAliasRows(fctx)
	if err != nil {
		res.AbortedStep = "verify"
		return res, fmt.Errorf("es: 切换已提交但校验别名失败：%w", err)
	}
	verified, note := VerifyRebuildTarget(rows, alias, next)
	res.Verified = verified
	res.VerifyNote = note
	if !verified {
		res.AbortedStep = "verify"
		return res, fmt.Errorf("es: 重建校验未通过：%s", note)
	}
	logf("      %s", note)
	return res, nil
}
