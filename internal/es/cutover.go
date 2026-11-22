package es

// 本文件实现 Phase 6「同名别名割接」（es:cutover）的编排层，只保留割接专属的
// 判定、请求体生成与编排。各编排共用的基础设施已按职责拆出（同包内纯搬家）：
//   - cluster.go：Cluster 接口 / AdminCluster 实现 / 集群读结果解释（es:status 的探测原语也在那）；
//   - audit.go：auditedCluster dry-run 写操作审计层；
//   - ingest.go：SyncFunc/CountFunc 窄接口、DB 期望数口径、ReconcileCounts、灌入调优 body。
//
// 为什么「割接」是与 es:rebuild 分开的独立动作：
//   - rebuild 的起点是「别名已经存在」（v0 -> v1），可以只摘别名、保留旧索引回滚；
//   - cutover 的起点是「shop-product 仍然是个物理索引」，要把它变成同名别名。
// ES 8.2.2 真机实测：物理索引名与别名**绝对互斥** —— 给物理索引 shop-product 挂
// 同名别名会得到 400 invalid_alias_name_exception，反向 PUT /shop-product 会得到
// 400 invalid_index_name_exception "already exists as alias"，_close 也解除不了冲突。
// 因此唯一可行路径是在**单次** POST /_aliases 里同时提交
//   {remove_index:{index:"shop-product"}} + {add:{index:"shop-product_v1",alias:"shop-product",is_write_index:true}}
// 原子生效（olivere v7.0.32 无论挂多少个 action 都只发一次 POST /_aliases），
// 拆成两步会留下「既没有索引也没有别名」的写入黑洞窗口 —— 本层严禁拆分。
//
// 由此推出的、必须如实告知运维的回滚语义：割接请求本身就把旧物理索引连同其数据一起
// 摘除，割接后不存在「保留旧索引直接回滚」这种选项，回滚只能重跑 MySQL 全量灌 v1。
// 本文件的帮助文案、--dry-run 输出与日志都不得承诺相反内容。
//
// 与 Phase 4 的解耦：第 3 步「灌 v1」复用全量同步流水线，但 internal/es/sync 的 API
// 仍在重构，所以本层只依赖 SyncFunc 这个窄接口，由 cmd/cli/es_cutover.go 在装配时注入
// 具体形态；cutover.go 不 import internal/es/sync，签名再变也只冲击 CLI 装配层。
// 第 6 步「差量补偿」同理，复用 rebuild.go 的窄接口 CompensateFunc（同包定义，不引入依赖）。
//
// 已定决策（不再自行探测/推翻）：不用 _clone（olivere v7.0.32 无 CloneIndex），
// 不用 snapshot/restore（本地集群无 path.repo，且 GET /_snapshot/_all 无仓库时返回
// 404 repository_missing_exception 而不是空对象）。集群健康在单节点上恒为 yellow，
// 属正常态，本层任何判定都不把 yellow 当故障。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// 纯判定函数（割接编排的可测内核）
// ---------------------------------------------------------------------------

// CutoverVerdict 是前置探测后得出的割接结论。
type CutoverVerdict int

const (
	// VerdictProceed 可以割接：别名位还被物理索引占着，且目标名空闲。
	VerdictProceed CutoverVerdict = iota
	// VerdictAlreadyCutover 幂等：目标名已作为别名存在，什么都不做，退出 0。
	VerdictAlreadyCutover
	// VerdictTargetOccupied 目标 v1 索引已存在：绝不覆盖，报错中止。
	VerdictTargetOccupied
	// VerdictMissingSource 别名与物理索引都不存在：无割接对象，报错中止。
	VerdictMissingSource
)

func (v CutoverVerdict) String() string {
	switch v {
	case VerdictAlreadyCutover:
		return "already_alias"
	case VerdictTargetOccupied:
		return "target_occupied"
	case VerdictMissingSource:
		return "source_missing"
	default:
		return "proceed"
	}
}

// AliasProbe 是前置只读探测的原始事实集合，判定与 IO 分离才能单测。
type AliasProbe struct {
	// NameIsPhysicalIndex：HEAD /<name> 命中，且 GET /_alias/<name> 无条目 => 名称仍被物理索引占据。
	NameIsPhysicalIndex bool
	AliasTargets        []string // GET /_alias/<alias> 指向的物理索引；非空 => 已是别名
	TargetExists        bool     // HEAD /<target> 是否已存在
}

// DecideCutover 给前置探测结论。判定顺序不能换：
// 割接成功后 target 既存在又正是别名指向，必须先认「幂等已完成」而不是「目标被占」。
func DecideCutover(p AliasProbe) (CutoverVerdict, string) {
	switch {
	case len(p.AliasTargets) > 0:
		return VerdictAlreadyCutover, fmt.Sprintf(
			"别名位已是别名，指向 %s；割接已完成，不做任何变更", strings.Join(p.AliasTargets, ", "))
	case p.TargetExists:
		return VerdictTargetOccupied, "目标索引已存在，拒绝覆盖"
	case !p.NameIsPhysicalIndex:
		return VerdictMissingSource, "别名位上既没有别名也没有物理索引，无割接对象"
	default:
		return VerdictProceed, "别名位仍是物理索引，可以执行同名割接"
	}
}

// cutoverAliasAction / cutoverRemoveIndex / cutoverAddAlias 用结构体而非 map
// 固定 _aliases 请求体的键顺序（map 序列化会随机化 key，运维对不上方案正文的样本会误判）。
type cutoverRemoveIndex struct {
	Index string `json:"index"`
}

type cutoverAddAlias struct {
	Index        string `json:"index"`
	Alias        string `json:"alias"`
	IsWriteIndex bool   `json:"is_write_index"`
}

type cutoverAliasAction struct {
	RemoveIndex *cutoverRemoveIndex `json:"remove_index,omitempty"`
	Add         *cutoverAddAlias    `json:"add,omitempty"`
}

type cutoverAliasRequest struct {
	Actions []cutoverAliasAction `json:"actions"`
}

// CutoverAliasBody 生成割接的 POST /_aliases 请求体（也是 dry-run 要打印的内容）。
//
// 顺序固定为先 remove_index 后 add，与真机验证过的样本逐字一致。它描述的是
// AdminCluster.SwitchAlias 实际下发的同一对 action，两处必须保持同语义 ——
// 真要拆成两个请求就会留下写入黑洞，本层不提供拆分能力。
func CutoverAliasBody(alias, target string) (string, error) {
	if strings.TrimSpace(alias) == "" || strings.TrimSpace(target) == "" {
		return "", fmt.Errorf("es: 割接请求体需要非空的别名与目标索引")
	}
	if alias == target {
		// 物理索引名与别名互斥，同名 add 必然 400 invalid_alias_name_exception。
		return "", fmt.Errorf("es: 目标索引名 %s 不能与别名同名（ES 中二者绝对互斥）", alias)
	}
	req := cutoverAliasRequest{Actions: []cutoverAliasAction{
		{RemoveIndex: &cutoverRemoveIndex{Index: alias}},
		{Add: &cutoverAddAlias{Index: target, Alias: alias, IsWriteIndex: true}},
	}}
	b, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("es: 序列化割接请求体失败: %w", err)
	}
	return string(b), nil
}

// VerifyCutoverTarget 校验割接结果：别名必须只指向 target，且 target 是写索引。
// 纯函数，入参是 _cat/aliases 的解析结果。
func VerifyCutoverTarget(rows []AliasTarget, alias, target string) (bool, string) {
	var targets []AliasTarget
	for _, r := range rows {
		if r.Alias == alias {
			targets = append(targets, r)
		}
	}
	if len(targets) == 0 {
		return false, fmt.Sprintf("别名 %s 不存在，割接未生效", alias)
	}
	if len(targets) > 1 {
		names := make([]string, 0, len(targets))
		for _, t := range targets {
			names = append(names, t.Index)
		}
		return false, fmt.Sprintf("别名 %s 指向 %d 个索引 %v，割接后应只剩 %s", alias, len(targets), names, target)
	}
	got := targets[0]
	if got.Index != target {
		return false, fmt.Sprintf("别名 %s 指向 %s，与期望目标 %s 不符", alias, got.Index, target)
	}
	effective, explicit := writeIndexEffective(got.WriteIndex, []string{got.Index})
	if !effective {
		return false, fmt.Sprintf("别名 %s -> %s 已生效，但 is_write_index=%q 非写索引", alias, target, got.WriteIndex)
	}
	if !explicit {
		return true, fmt.Sprintf("别名 %s -> %s 正确（单索引隐式写索引，_cat 回读 %q）", alias, target, got.WriteIndex)
	}
	return true, fmt.Sprintf("别名 %s -> %s 正确，is_write_index=true 已生效", alias, target)
}

// FormatOrphanAdvisory 生成「已建但未切走」的孤儿索引处置建议。
//
// 刻意不自动删：误删一次 15 分片索引的代价远高于留一个待人工确认的孤儿。
// 文案必须如实 —— 割接完成后旧索引连数据一起消失，回滚只能重跑全量，
// 这里不承诺任何「保留旧索引」的退路。
func FormatOrphanAdvisory(alias string, orphans []string) string {
	if len(orphans) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("孤儿索引清单（已创建但未切走别名 %s，本命令不会自动删除）：\n", alias))
	for _, o := range orphans {
		sb.WriteString("  - " + o + "\n")
	}
	sb.WriteString("处置建议（二选一，都需人工执行）：\n")
	for _, o := range orphans {
		sb.WriteString(fmt.Sprintf("  1) 确认数据无用后手工删除：DELETE /%s\n", o))
		sb.WriteString(fmt.Sprintf("  2) 修复原因后重跑本命令：重跑会因 HEAD /%s 命中而以「目标索引已存在，拒绝覆盖」中止，\n", o))
		sb.WriteString("     必须先删除该索引（op_type=index 幂等，但结构必须由本命令按声明新建）。\n")
	}
	sb.WriteString("回滚说明：同名割接由单请求 remove_index 完成，旧物理索引连同数据一起摘除，\n")
	sb.WriteString("        不存在「保留旧索引直接切回」的选项；回滚只能重跑 MySQL 全量灌入目标索引。\n")
	return sb.String()
}

// ---------------------------------------------------------------------------
// 编排
// ---------------------------------------------------------------------------

// defaultFinalizeTimeout 收尾（恢复 settings / 校验别名）独立上下文的默认超时。
const defaultFinalizeTimeout = 60 * time.Second

// CutoverOptions 割接编排入参。
//
// Cluster 与 Sync 都由调用方注入：Cluster 让 dry-run 能拦写，Sync 让 #5 的
// API 变动只冲击 CLI 装配层。DBCount 注入是为了对账口径可单测；nil 时回落 ProductDocCount。
// Compensate 与 rebuild 第 7 步同构：割接把旧物理索引连数据一起摘走，灌数窗口内的
// 改/删只能靠这一次差量补偿收敛，不注入就是丢数据（见 RunCutover 第 6 步）。
type CutoverOptions struct {
	Alias      string // 目标别名，必须与 Definition.Alias() 一致
	Definition *IndexDefinition
	Cluster    Cluster
	Sync       SyncFunc
	DBCount    CountFunc
	Compensate CompensateFunc // 切换之后收敛窗口增量；nil 时在切换后报错中止

	// IngestCheck 是割接前置预检：在发出【任何】ES 写请求之前确认目标别名的全量灌数编排可用。
	// 由装配层注入（cmd/cli 接 sync.IngestOrchestrationReady）；nil 时跳过（保持内层单测既有行为）。
	// 目的：杜绝「先 PUT /<alias>_v1 建出空索引、再在灌数步中止」留下的孤儿索引。
	IngestCheck func(ctx context.Context, alias string) error

	Tolerance       int64 // 对账容差：|ES - DB| > 该值即在切别名之前中止；负值=永不可达标
	Force           bool  // 对账不达标仍继续割接（与 es:rebuild --force 同语义，谨慎）
	DryRun          bool  // 只读预演：任何写调用都会被审计层拒绝
	TuneIngest      bool  // 灌数期临时关 refresh/副本（仅对无别名的新建物理索引安全）
	FinalizeTimeout time.Duration

	Logf func(format string, v ...any)
}

// CutoverResult 割接一次执行的完整结论，供 CLI 打印与 --json 序列化。
type CutoverResult struct {
	Alias                 string `json:"alias"`
	Target                string `json:"target"`
	DeclaredSchemaVersion int    `json:"declared_schema_version"`
	Verdict               string `json:"verdict"`
	DryRun                bool   `json:"dry_run"`
	Idempotent            bool   `json:"idempotent"`

	PluginsInstalled []string `json:"plugins_installed"`
	PluginsRequired  []string `json:"plugins_required"`
	PluginsMissing   []string `json:"plugins_missing"`

	AliasTargetsBefore []string `json:"alias_targets_before"`

	DBExpected         int64  `json:"db_expected"`
	Tolerance          int64  `json:"tolerance"` // 本次生效的容差（负值=永不可达标，不当 0 用）
	Force              bool   `json:"force"`     // --force：对账不达标仍继续割接
	PlannedCreateBody  string `json:"planned_create_body,omitempty"`
	PlannedAliasesBody string `json:"planned_aliases_body,omitempty"`

	Sync        SyncSummary `json:"sync"`
	ESCount     int64       `json:"es_count"`
	Diff        int64       `json:"count_diff"`
	Reconciled  bool        `json:"reconciled"`
	Created     bool        `json:"created"`
	Tuned       bool        `json:"tuned"`
	Restored    bool        `json:"settings_restored"`
	Switched    bool        `json:"switched"`
	Verified    bool        `json:"verified"`
	VerifyNote  string      `json:"verify_note,omitempty"`
	Orphans     []string    `json:"orphans,omitempty"`
	AbortedStep string      `json:"aborted_step,omitempty"`

	// Compensate 是窗口增量补偿的统计；Compensated 只在补偿无 error 且 Failed==0 时置 true。
	// 旧物理索引已被 remove_index 摘除，窗口增量没收敛就是丢了，不能报成成功（与 rebuild 同口径）。
	Compensate  CompensateSummary `json:"compensate"`
	Compensated bool              `json:"compensated"`
	StartedAt   time.Time         `json:"started_at"` // 编排起点，也是差量补偿的下界

	ReadOps  []string `json:"read_ops"`
	WriteOps []string `json:"write_ops"`
}

// RunCutover 按已定稿的 7 步执行同名别名割接：前置校验 -> 建 target -> 灌数 ->
// 对账（闸门，可 --force）-> 单次 _aliases 原子割接 -> **差量补偿** -> 校验别名。
//
// 差量补偿为什么不可省略：第 5 步的 remove_index 把旧物理索引连同数据一起摘除，
// 灌数窗口（startedAt 至切换完成）内 MySQL 发生的增/改/删既没进 target，也再也回不来
// —— 只能靠这一次以 startedAt 为下界、以新 write index 为目标的扫描收敛。
//
// 收尾动作（恢复 settings、校验别名）一律用 context.WithoutCancel(ctx) 派生的独立
// context：--timeout 到期时主 ctx 已取消，若复用同一 ctx，恢复请求会直接 context canceled
// 失败，把「refresh 还关着、副本还是 0」的索引遗留在集群里。
func RunCutover(ctx context.Context, opts CutoverOptions) (*CutoverResult, error) {
	if opts.Definition == nil {
		return nil, fmt.Errorf("es: 割接需要索引声明（Definition 为 nil）")
	}
	if opts.Cluster == nil {
		return nil, fmt.Errorf("es: 割接需要集群原语（Cluster 为 nil）")
	}
	if strings.TrimSpace(opts.Alias) == "" {
		return nil, fmt.Errorf("es: 割接需要目标别名")
	}
	if opts.Alias != opts.Definition.Alias() {
		return nil, fmt.Errorf("es: 别名 %s 与声明 %s 的 alias=%s 不一致",
			opts.Alias, opts.Definition.Source(), opts.Definition.Alias())
	}
	if !opts.Definition.Executable() {
		return nil, fmt.Errorf("es: 声明 %s 的 status=%s 未接线，拒绝割接", opts.Definition.Source(), opts.Definition.Status())
	}
	logf := opts.Logf
	if logf == nil {
		logf = func(format string, v ...any) { fmt.Printf(format+"\n", v...) }
	}
	if opts.DBCount == nil {
		opts.DBCount = productCountFunc
	}

	// ---- 割接前置预检：在任何 ES 读写（连审计层都不建）之前确认全量灌数编排可用 ----
	// 编排不可用却继续往下跑，第 2 步就会 PUT 出空的 <alias>_v1，第 3 步灌数才失败——留下孤儿索引、
	// 别名不切。故这里 fail-fast：不可用即拒绝，一条 ES 请求都不发（零写、且零孤儿）。
	if opts.IngestCheck != nil {
		if err := opts.IngestCheck(ctx, opts.Alias); err != nil {
			return nil, fmt.Errorf("es: 割接前置预检未通过 [alias=%s]，已在发出任何 ES 写请求之前拒绝（不建索引、不留孤儿）: %w", opts.Alias, err)
		}
	}

	audit := newAuditedCluster(opts.Cluster, opts.DryRun)
	c := Cluster(audit)

	alias := opts.Alias
	target := opts.Definition.Name() // <alias>_v<schema_version>
	// startedAt 在编排最开始就固化，与 rebuild 同源同一语义：本次任务首次启动时刻，
	// 也是差量补偿的唯一下界（绝不取切换时刻 —— 那会把灌数窗口内的变更静默漏掉）。
	startedAt := time.Now()
	res := &CutoverResult{
		Alias:                 alias,
		Target:                target,
		DeclaredSchemaVersion: opts.Definition.Version(),
		DryRun:                opts.DryRun,
		PlannedAliasesBody:    mustBody(CutoverAliasBody(alias, target)),
		PluginsRequired:       opts.Definition.RequiresPlugins(),
		Tolerance:             opts.Tolerance,
		Force:                 opts.Force,
		StartedAt:             startedAt,
	}
	defer func() {
		res.ReadOps = audit.ReadOps()
		res.WriteOps = audit.WriteOps()
	}()

	// ---- 1. 前置校验（全部只读）----
	logf("[1/7] 前置校验：插件、别名状态、目标名")
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
		return res, fmt.Errorf("es: 依赖插件缺失 %v（已安装：%v），割接会建出一个分词器不可用的索引，已中止",
			missing, plugins)
	}

	// HEAD 命中说明名字在用（物理索引与别名都可能命中），GET /_alias 才把它定性：
	// 只有「名字是别名」才有条目 —— 互斥命名让这两个只读请求足以判清三种状态。
	nameHit, err := c.IndexExists(ctx, alias)
	if err != nil {
		res.AbortedStep = "precheck_alias"
		return res, err
	}
	targets, err := c.AliasTargets(ctx, alias)
	if err != nil {
		res.AbortedStep = "precheck_alias"
		return res, err
	}
	targetExists, err := c.IndexExists(ctx, target)
	if err != nil {
		res.AbortedStep = "precheck_target"
		return res, err
	}
	probe := AliasProbe{
		NameIsPhysicalIndex: nameHit && len(targets) == 0,
		AliasTargets:        targets,
		TargetExists:        targetExists,
	}
	verdict, detail := DecideCutover(probe)
	res.Verdict = verdict.String()
	res.AliasTargetsBefore = probe.AliasTargets
	logf("      判定=%s（%s）", verdict, detail)

	switch verdict {
	case VerdictAlreadyCutover:
		res.Idempotent = true
		logf("      别名已就位，指向 %v；割接无需重复执行，退出码 0", probe.AliasTargets)
		if probe.AliasTargets[0] != target {
			logf("      注意：当前指向的不是本次声明的 %s，本命令不做任何变更", target)
		}
		return res, nil
	case VerdictTargetOccupied:
		res.AbortedStep = "precheck_target"
		res.Orphans = []string{target}
		return res, fmt.Errorf("es: 目标索引 %s 已存在，拒绝覆盖（确认无用后手工删除再重跑）", target)
	case VerdictMissingSource:
		res.AbortedStep = "precheck_source"
		return res, fmt.Errorf("es: 别名位 %s 上既无别名也无物理索引，没有可割接的对象", alias)
	}

	dbExpected, err := opts.DBCount(ctx, alias)
	if err != nil {
		res.AbortedStep = "precheck_db_count"
		return res, err
	}
	res.DBExpected = dbExpected
	createBody, err := opts.Definition.BodyJSON()
	if err != nil {
		res.AbortedStep = "precheck_definition"
		return res, err
	}
	res.PlannedCreateBody = createBody

	// ---- dry-run：到此为止，只打印计划 ----
	if opts.DryRun {
		logf("[dry-run] 计划执行的写操作（本次一条都不会发出）：")
		logf("      1) PUT /%s            按声明建索引（settings+mappings，不带 aliases）", target)
		logf("      2) PUT /%s/_settings  灌数提速 %s", target, IngestTuneBody())
		logf("      3) 灌数               目标 %s，op_type=index 幂等，期望 %d 条", target, dbExpected)
		logf("      4) POST /%s/_refresh  对账前刷新", target)
		logf("      5) POST /_aliases     %s", res.PlannedAliasesBody)
		logf("      6) PUT /%s/_settings  恢复 %s", target, IngestRestoreBody(opts.Definition.SettingsMap()))
		logf("      7) 差量补偿           目标 %s，下界 %s（编排起点固化，非切换时刻）——窗口内变更只能靠它收敛",
			target, startedAt.Format(time.RFC3339))
		if opts.Force {
			logf("      注：本次 --force 已开启，对账不达标也不会中止（仍会执行第 7 步补偿）")
		}
		logf("[dry-run] 只读调用清单（%d 次，全部 GET/HEAD）：%v", len(audit.ReadOps()), audit.ReadOps())
		logf("[dry-run] 写调用清单：%v（空即零写操作已证明）", audit.WriteOps())
		logf("[dry-run] 回滚语义：割接由单请求 remove_index 摘除旧物理索引，数据同步消失，" +
			"回滚只能重跑 MySQL 全量灌入目标索引；不存在「保留旧索引可回滚」。")
		return res, nil
	}

	// ---- 2. 按声明 mapping 建 v1 ----
	logf("[2/7] 创建目标索引 %s（声明：%s）", target, opts.Definition.Source())
	if err := c.CreateIndex(ctx, target, createBody); err != nil {
		res.AbortedStep = "create"
		return res, err
	}
	res.Created = true

	// ---- 3. 灌数（窄接口注入）----
	if opts.TuneIngest {
		if err := c.PutSettings(ctx, target, IngestTuneBody()); err != nil {
			res.AbortedStep = "tune_ingest"
			res.Orphans = []string{target}
			return res, err
		}
		res.Tuned = true
		logf("      灌数提速已开启：refresh=-1、replicas=0（单节点健康会呈 yellow，属预期）")
	}
	// 收尾用的独立 context：主 ctx 取消/超时后仍要把 settings 恢复回去。
	finalize := func() (context.Context, context.CancelFunc) {
		d := opts.FinalizeTimeout
		if d <= 0 {
			d = defaultFinalizeTimeout
		}
		return context.WithTimeout(context.WithoutCancel(ctx), d)
	}
	// restoreSettings 幂等且总用独立 ctx，任何失败路径都必须先跑它。
	restoreSettings := func() {
		if !res.Tuned || res.Restored {
			return
		}
		fctx, cancel := finalize()
		defer cancel()
		if err := c.PutSettings(fctx, target, IngestRestoreBody(opts.Definition.SettingsMap())); err != nil {
			logf("      ⚠️  恢复 settings 失败（收尾 ctx 也已耗尽）：%v —— 请手工对 %s 恢复 refresh_interval/replicas",
				err, target)
			return
		}
		res.Restored = true
		logf("      settings 已恢复：%s", IngestRestoreBody(opts.Definition.SettingsMap()))
	}

	if opts.Sync == nil {
		res.AbortedStep = "sync"
		restoreSettings()
		res.Orphans = []string{target}
		return res, fmt.Errorf("es: 未注入灌数回调（SyncFunc 为 nil）")
	}
	logf("[3/7] 灌入 %s（从 MySQL 全量，op_type=index 幂等）", target)
	summary, err := opts.Sync(ctx, alias, target)
	res.Sync = summary
	if err != nil {
		res.AbortedStep = "sync"
		restoreSettings()
		res.Orphans = []string{target}
		return res, fmt.Errorf("es: 灌数失败 [target=%s]: %w", target, err)
	}
	restoreSettings()
	if summary.Failed > 0 {
		res.AbortedStep = "sync_failures"
		res.Orphans = []string{target}
		return res, fmt.Errorf("es: 灌数存在 %d 条 item 级失败，切别名前中止", summary.Failed)
	}

	// ---- 4. 对账（差异超阈值必须在切别名之前中止）----
	// 口径差异如实记账（本次不改变判定）：dbExpected 取自灌数**之前**，esCount 是灌数**之后**的
	// _count，两者本就不是同一时点；窗口内新增/下架会造成假失败。因此给 --force 退路，
	// 语义与 es:rebuild 完全对齐：不达标仍继续割接，但结论里 switched/reconciled 照实记录。
	logf("[4/7] 对账：DB 期望 %d 条 vs %s/_count（容差 %d，判定在本层）", dbExpected, target, opts.Tolerance)
	if err := c.Refresh(ctx, target); err != nil {
		res.AbortedStep = "reconcile_refresh"
		res.Orphans = []string{target}
		return res, err
	}
	esCount, err := c.Count(ctx, target)
	if err != nil {
		res.AbortedStep = "reconcile_count"
		res.Orphans = []string{target}
		return res, err
	}
	res.ESCount = esCount
	diff, ok := ReconcileCounts(dbExpected, esCount, opts.Tolerance)
	res.Diff = diff
	res.Reconciled = ok
	logf("      ES=%d，差值=%+d，容差=%d，达标=%v", esCount, diff, opts.Tolerance, ok)
	if !ok {
		if !opts.Force {
			res.AbortedStep = "reconcile"
			res.Orphans = []string{target}
			return res, fmt.Errorf("es: 对账不达标（差 %+d 超容差 %d），已在切别名之前中止，别名 %s 未被改动",
				diff, opts.Tolerance, alias)
		}
		logf("      ⚠️  对账不达标但 --force 已开启，继续割接（谨慎）")
	}

	// ---- 5. 单次 _aliases 原子割接 ----
	logf("[5/7] 原子割接：POST /_aliases %s", res.PlannedAliasesBody)
	if err := c.SwitchAlias(ctx, alias, target); err != nil {
		res.AbortedStep = "switch"
		res.Orphans = []string{target}
		return res, err
	}
	res.Switched = true

	// ---- 6. 差量补偿（remove_index 之后、校验之前）----
	// 与 --force / 对账闸门的先后关系（必须如实）：对账闸门与 --force 都只作用于第 4 步
	// 「切不切」，一旦切完就已无可回滚对象，第 6 步无论对账是否达标、是否 --force 都必跑，
	// 也绝不能被 --force 跳过；它失败不回滚割接（旧索引已连数据消失），但必须如实报错。
	if opts.DryRun {
		// 写原语拦截不依赖「dry-run 在第 5 步之前就 return」这条约定：万一将来有人
		// 把补偿提到预演分支之前，这里直接硬拒，而不是静默写入。
		res.AbortedStep = "compensate"
		return res, fmt.Errorf("es: dry-run 拒绝差量补偿（写操作）")
	}
	if opts.Compensate == nil {
		res.AbortedStep = "compensate"
		return res, fmt.Errorf("es: 已割接但未注入差量补偿回调（CompensateFunc 为 nil），"+
			"旧物理索引已被 remove_index，灌数窗口内的增量无法回补，请立即手工对 %s 跑一次 update_time >= %s 的补偿",
			target, startedAt.Format(time.RFC3339))
	}
	logf("[6/7] 差量补偿：扫描 update_time >= %s（Unscoped，含删分支），目标 %s", startedAt.Format(time.RFC3339), target)
	cstats, cerr := opts.Compensate(ctx, alias, target, startedAt)
	res.Compensate = cstats
	if cerr != nil {
		res.AbortedStep = "compensate"
		return res, fmt.Errorf("es: 差量补偿失败 [target=%s]（割接已生效，补偿幂等可重跑）: %w", target, cerr)
	}
	if cstats.Failed > 0 {
		// item 级失败不能被吞成 success：这些文档在旧索引里已经连索引一起消失。
		res.AbortedStep = "compensate_failures"
		return res, fmt.Errorf("es: 差量补偿存在 %d 条 item 级失败 [target=%s，扫描 %d/写入 %d/删除 %d]"+
			"（割接已生效且旧索引已消失，不回滚；修复结构问题后可用同一 since 幂等重跑）",
			cstats.Failed, target, cstats.Scanned, cstats.Indexed, cstats.Deleted)
	}
	res.Compensated = true

	// ---- 7. 校验别名指向与写标记（收尾用独立 ctx：切完正好 --timeout 到期时，
	//        校验仍要跑完，否则会把已经成功的割接误报成失败）----
	logf("[7/7] 校验别名指向")
	fctx, cancel := finalize()
	defer cancel()
	rows, err := c.ListAliasRows(fctx)
	if err != nil {
		res.AbortedStep = "verify"
		return res, fmt.Errorf("es: 割接已提交但校验别名失败：%w", err)
	}
	verified, note := VerifyCutoverTarget(rows, alias, target)
	res.Verified = verified
	res.VerifyNote = note
	if !verified {
		res.AbortedStep = "verify"
		return res, fmt.Errorf("es: 割接校验未通过：%s", note)
	}
	logf("      %s", note)
	return res, nil
}

// mustBody 生成 dry-run 用的计划请求体；失败退化成占位说明而不是把编排打断
// （割接主流程会另行重新调用 CutoverAliasBody 并如实报错）。
func mustBody(body string, err error) string {
	if err != nil {
		return "<invalid: " + err.Error() + ">"
	}
	return body
}
