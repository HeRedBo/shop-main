package es

// migrate.go 实现方案第五节「es:migrate 变更分级算法」。
//
// 取代 PHP「靠 ES 400 异常猜」的做法：逐 Definition 用只读探测决定「首建 / 引导 cutover /
// 报错」，别名已就位时再读线上 mappings/settings 跑 diff.Decide 分级为 Noop / Inplace / Rebuild。
// 关键点：
//   - 逐 Definition 独立执行，单条失败只记录、不影响其他（一个坏声明不该阻塞整批迁移）；
//   - 别名指向 >1 个索引直接报错中止（修 PHP array_keys()[0] 只取首个的隐患）；
//   - 别名不存在但同名物理索引存在 => 存量未割接，写路径中止并引导 es:cutover；
//     --dry-run 下额外读该物理索引线上结构跑 Decide，把「割接后会被判成什么」如实打印出来，
//     让运维在只读预演里就看到结构漂移（本仓库线上 shop-product 缺 _source.excludes 即判 Rebuild）；
//   - ActionInplace 只下发动态 settings + 新增字段子集的 put_mapping（strict 不阻止新增字段），
//     绝不做 PHP 的无条件 close/open；ActionRebuild 委托 RunRebuild 走第六节蓝绿 8 步；
//   - --dry-run 由 auditedRebuild 硬拦一切写，可被「raw fake 零写 ops」与「httptest 只见 GET/HEAD」
//     两种方式证明。

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// MigrateVerdict 是单条 Definition 的分级结论。
type MigrateVerdict string

const (
	VerdictNoop             MigrateVerdict = "noop"               // 结构一致，无写操作
	VerdictInplace          MigrateVerdict = "inplace"            // 已原地提交动态 settings / 新增字段
	VerdictRebuild          MigrateVerdict = "rebuild"            // 已委托蓝绿重建
	VerdictFirstCreate      MigrateVerdict = "first_create"       // 别名与同名索引都不存在，首建（先建后灌、对账达标再挂别名）
	VerdictCutoverRequired  MigrateVerdict = "cutover_required"   // 别名位仍是物理索引，引导先 es:cutover
	VerdictError            MigrateVerdict = "error"              // 判定/执行失败（多目标、探测出错等）
	VerdictSkippedNotActive MigrateVerdict = "skipped_not_active" // 声明未接线，跳过
)

// MigrateOptions migrate 编排列参。Target / Sync / Compensate / DBCount 全部注入，
// 与 RunRebuild 共用同一批窄接口，装配层只需接一次线。
type MigrateOptions struct {
	Registry   *Registry
	Target     RebuildTarget // nil → NewAdminRebuild()（同时充当 migrate 自己的 Cluster）
	Sync       SyncFunc
	Compensate CompensateFunc
	DBCount    CountFunc

	// IngestCheck 是割接/迁移前置预检：在首建路径发出【任何】ES 写请求之前确认目标别名的
	// 全量灌数编排可用。由装配层注入（cmd/cli 接 sync.IngestOrchestrationReady）；nil 时跳过。
	IngestCheck func(ctx context.Context, alias string) error

	Alias            string // "" 或 "all" → 全部可执行 Definition；否则只跑该别名
	Tolerance        int64
	DryRun           bool
	ForceRebuild     bool // 跳过 diff，对已割接索引直接走蓝绿重建
	TuneIngest       bool
	KeepNewOnFailure bool
	DropOld          bool // 透传给被委托的 rebuild
	FinalizeTimeout  time.Duration

	Logf func(format string, v ...any)
}

// DefMigrateResult 单条 Definition 的迁移结论。
type DefMigrateResult struct {
	Alias   string `json:"alias"`
	Source  string `json:"source"`
	Verdict string `json:"verdict"`
	Action  string `json:"action,omitempty"` // diff 三级（noop/inplace/rebuild），存在结构判定时填
	Reason  string `json:"reason,omitempty"`

	DeclaredFingerprint string `json:"declared_fingerprint,omitempty"`
	LiveFingerprint     string `json:"live_fingerprint,omitempty"`

	Planned []string `json:"planned,omitempty"` // dry-run / 说明用的将执行请求序列

	Error   string         `json:"error,omitempty"`
	Rebuild *RebuildResult `json:"rebuild,omitempty"`
}

// MigrateResult 一次 migrate 的汇总结论。
type MigrateResult struct {
	DryRun bool                `json:"dry_run"`
	Defs   []*DefMigrateResult `json:"defs"`
}

// RunMigrate 按第五节逐条执行分级迁移。返回的 *MigrateResult 恒非 nil；
// error 在「至少一条 Definition 失败」时非 nil（聚合），但所有 Definition 都会被独立处理。
func RunMigrate(ctx context.Context, opts MigrateOptions) (*MigrateResult, error) {
	logf := opts.Logf
	if logf == nil {
		logf = func(format string, v ...any) { fmt.Printf(format+"\n", v...) }
	}
	if opts.Registry == nil {
		return nil, fmt.Errorf("es: migrate 需要 Registry")
	}
	if opts.DBCount == nil {
		opts.DBCount = productCountFunc
	}
	target := opts.Target
	if target == nil {
		target = NewAdminRebuild()
	}
	// 回填原始 target：migrate 自身写走下面的 audit，被委托的 rebuild 用裸 target 自带
	// 各自单层审计（避免双层包装导致读台账重复计数）；dry-run 下两层各自硬拒写，
	// 裸 fake / httptest 恒只见 GET/HEAD，零写证明因此与包装层数无关。
	opts.Target = target
	// 单一审计层覆盖 migrate 自身写（inplace/首建）：dry-run 下一切写路径立刻被拒，
	// 「零写操作」可被 WriteOps()==空 单点证明。
	audit := newAuditedRebuild(target, opts.DryRun)

	defs, err := pickDefinitions(opts.Registry, opts.Alias)
	if err != nil {
		return nil, err
	}

	res := &MigrateResult{DryRun: opts.DryRun}
	var failures []string
	for _, def := range defs {
		one := migrateOne(ctx, def, audit, opts, logf)
		res.Defs = append(res.Defs, one)
		if one.Error != "" {
			failures = append(failures, fmt.Sprintf("%s: %s", one.Alias, one.Error))
		}
	}
	if len(failures) > 0 {
		return res, fmt.Errorf("es: migrate 有 %d/%d 条 Definition 失败：%s",
			len(failures), len(res.Defs), strings.Join(failures, "; "))
	}
	return res, nil
}

// pickDefinitions 按 --alias 选择处理范围。""/"all" → 全部可执行声明；
// 指定别名 → 只跑该别名（未登记即报错）。返回顺序稳定（Registry.All 已按 alias 升序）。
func pickDefinitions(reg *Registry, alias string) ([]*IndexDefinition, error) {
	alias = strings.TrimSpace(alias)
	if alias == "" || strings.EqualFold(alias, "all") {
		defs := reg.ExecutableDefinitions()
		out := make([]*IndexDefinition, 0, len(reg.All()))
		out = append(out, defs...)
		return out, nil
	}
	def, ok := reg.Get(alias)
	if !ok {
		return nil, fmt.Errorf("es: 别名 %s 没有登记的索引声明（已登记：%v）", alias, reg.Aliases())
	}
	return []*IndexDefinition{def}, nil
}

// migrateOne 处理单条 Definition，任何错误都被吞进 DefMigrateResult.Error，绝不 panic 上抛，
// 以保证「单条失败不影响其他」。
func migrateOne(ctx context.Context, def *IndexDefinition, c RebuildTarget, opts MigrateOptions, logf func(string, ...any)) (out *DefMigrateResult) {
	rec := &DefMigrateResult{Alias: def.Alias(), Source: def.Source(), DeclaredFingerprint: def.Fingerprint()}
	defer func() {
		if r := recover(); r != nil {
			rec.Verdict = string(VerdictError)
			rec.Error = fmt.Sprintf("panic: %v", r)
			out = rec
		}
	}()

	if !def.Executable() {
		rec.Verdict = string(VerdictSkippedNotActive)
		rec.Reason = fmt.Sprintf("声明 %s 的 status=%s 未接线，跳过", def.Source(), def.Status())
		logf("[migrate] %s: %s", def.Alias(), rec.Reason)
		return rec
	}

	alias := def.Alias()
	// ---- 第 2 步：别名/索引存在性组合决定「首建 / 引导 cutover / 报错」----
	targets, err := c.AliasTargets(ctx, alias)
	if err != nil {
		rec.Verdict = string(VerdictError)
		rec.Error = fmt.Sprintf("读取别名 %s 指向失败: %v", alias, err)
		return rec
	}

	switch {
	case len(targets) > 1:
		rec.Verdict = string(VerdictError)
		rec.Error = fmt.Sprintf("别名 %s 指向 %d 个索引 %v，要求唯一指向单一物理索引，已中止（不猜首个）", alias, len(targets), targets)
		logf("[migrate] %s: ✗ %s", alias, rec.Error)
		return rec

	case len(targets) == 1:
		// 别名已就位 → 读线上结构跑 Decide 分级（或 --force-rebuild 直接重建）。
		rec = migrateCutovered(ctx, def, targets[0], c, opts, logf, rec)
		return rec

	default: // len(targets) == 0：别名不存在
		exists, eerr := c.IndexExists(ctx, alias)
		if eerr != nil {
			rec.Verdict = string(VerdictError)
			rec.Error = fmt.Sprintf("探测 %s 是否为物理索引失败: %v", alias, eerr)
			return rec
		}
		if !exists {
			// ---- 首建：Create(def.Name(), body 不含 aliases) 只建索引 → 灌数 → 对账达标 → 再单次 POST /_aliases 挂别名 ----
			return migrateFirstCreate(ctx, def, c, opts, logf, rec)
		}
		// ---- 别名位仍是物理索引（存量未割接）----
		return migrateCutoverRequired(ctx, def, c, opts, logf, rec)
	}
}

// migrateCutovered 处理「别名已唯一指向某物理索引」的场景：--force-rebuild 直接蓝绿；
// 否则读线上结构 diff 分级 Noop / Inplace / Rebuild。
func migrateCutovered(ctx context.Context, def *IndexDefinition, cur string, c RebuildTarget, opts MigrateOptions, logf func(string, ...any), rec *DefMigrateResult) *DefMigrateResult {
	alias := def.Alias()
	if opts.ForceRebuild {
		logf("[migrate] %s: --force-rebuild，跳过 diff 直接蓝绿重建", alias)
		return delegateRebuild(ctx, def, cur, c, opts, logf, rec)
	}
	lm, err := c.GetMapping(ctx, cur)
	if err != nil {
		rec.Verdict = string(VerdictError)
		rec.Error = fmt.Sprintf("读取 %s mappings 失败: %v", cur, err)
		return rec
	}
	ls, err := c.GetSettings(ctx, cur)
	if err != nil {
		rec.Verdict = string(VerdictError)
		rec.Error = fmt.Sprintf("读取 %s settings 失败: %v", cur, err)
		return rec
	}
	d := Decide(def, lm, ls)
	rec.Action = d.Action.String()
	rec.LiveFingerprint = d.LiveFingerprint
	rec.Reason = d.Reason

	switch d.Action {
	case ActionNoop:
		rec.Verdict = string(VerdictNoop)
		logf("[migrate] %s: 结构一致（noop），无写操作。%s", alias, d.Reason)
		return rec
	case ActionInplace:
		return migrateInplace(ctx, def, cur, c, opts, logf, rec, d)
	default: // ActionRebuild
		logf("[migrate] %s: 判定 rebuild（%s），委托蓝绿重建", alias, d.Reason)
		return delegateRebuild(ctx, def, cur, c, opts, logf, rec)
	}
}

// migrateInplace 原地路径：只下发动态 settings + 新增字段子集 put_mapping，不做 close/open。
// dry-run 只把将执行的两条写记进 Planned，真实写路径被审计层拒绝。
func migrateInplace(ctx context.Context, def *IndexDefinition, cur string, c RebuildTarget, opts MigrateOptions, logf func(string, ...any), rec *DefMigrateResult, d *Diff) *DefMigrateResult {
	alias := def.Alias()
	setBody, hasSet := d.InplacePutSettingsJSON()
	mapBody, hasMap := d.InplacePutMappingJSON()
	if !hasSet && !hasMap {
		// 判成 inplace 却无可原地下发的增量（例如仅指纹差），退化成 noop 并如实说明。
		rec.Verdict = string(VerdictNoop)
		rec.Reason = "diff=inplace 但无可原地下发的增量（仅指纹/版本差异）"
		logf("[migrate] %s: %s", alias, rec.Reason)
		return rec
	}
	if opts.DryRun {
		if hasSet {
			rec.Planned = append(rec.Planned, fmt.Sprintf("PUT /%s/_settings %s", cur, setBody))
		}
		if hasMap {
			rec.Planned = append(rec.Planned, fmt.Sprintf("PUT /%s/_mapping %s", cur, mapBody))
		}
		logf("[migrate] %s: [dry-run] 将原地提交 %d 项（本次零写操作）：%v", alias, len(rec.Planned), rec.Planned)
		rec.Verdict = string(VerdictInplace)
		return rec
	}
	if hasSet {
		if err := c.PutSettings(ctx, cur, setBody); err != nil {
			rec.Verdict = string(VerdictError)
			rec.Error = fmt.Sprintf("原地更新 settings 失败 [index=%s]: %v", cur, err)
			return rec
		}
	}
	if hasMap {
		if _, err := c.Perform(ctx, "PUT", "/"+cur+"/_mapping", mapBody); err != nil {
			rec.Verdict = string(VerdictError)
			rec.Error = fmt.Sprintf("原地追加 mapping 字段失败 [index=%s]: %v", cur, err)
			return rec
		}
	}
	rec.Verdict = string(VerdictInplace)
	logf("[migrate] %s: 已原地提交（settings=%v mapping=%v）", alias, hasSet, hasMap)
	return rec
}

// delegateRebuild 把 ActionRebuild/--force-rebuild 交给第六节蓝绿编排，注入同一批窄接口。
// rebuild 自带单层审计，其写台账落在返回的 RebuildResult.WriteOps 里。
func delegateRebuild(ctx context.Context, def *IndexDefinition, cur string, c RebuildTarget, opts MigrateOptions, logf func(string, ...any), rec *DefMigrateResult) *DefMigrateResult {
	// 蓝绿重建同样先建新索引再灌数，与首建共享「空 v1 孤儿」风险：在 RunRebuild 发出任何
	// ES 写请求之前确认全量灌数编排可用；不可用即拒绝，不建新索引、不留孤儿。
	if opts.IngestCheck != nil {
		if err := opts.IngestCheck(ctx, def.Alias()); err != nil {
			rec.Verdict = string(VerdictError)
			rec.Error = fmt.Sprintf("灌数编排不可用，已在发出任何 ES 写请求之前拒绝蓝绿重建 [alias=%s]（不建新索引、不留孤儿）: %v", def.Alias(), err)
			logf("[migrate] %s: %s", def.Alias(), rec.Error)
			return rec
		}
	}
	rb, err := RunRebuild(ctx, RebuildOptions{
		Alias:            def.Alias(),
		Definition:       def,
		Target:           opts.Target, // 裸 target：RunRebuild 自建单层审计，不与 migrate 的 audit 叠加
		Sync:             opts.Sync,
		Compensate:       opts.Compensate,
		DBCount:          opts.DBCount,
		Tolerance:        opts.Tolerance,
		DryRun:           opts.DryRun,
		DropOld:          opts.DropOld,
		KeepNewOnFailure: opts.KeepNewOnFailure,
		TuneIngest:       opts.TuneIngest,
		FinalizeTimeout:  opts.FinalizeTimeout,
		Logf:             logf,
	})
	rec.Rebuild = rb
	rec.Verdict = string(VerdictRebuild)
	if rb != nil {
		rec.Reason = rb.VerifyNote
	}
	if err != nil {
		rec.Error = err.Error()
		return rec
	}
	return rec
}

// migrateFirstCreate 首次部署：别名与同名物理索引都不存在。顺序与 es:rebuild 对齐——
// 先建索引（body 不带 aliases，读写都触达不到它）→ 灌数 → 对账达标后才用**单次**
// POST /_aliases 原子挂别名。对账不达标则索引存在但不挂别名，生产别名不指向它、线上读不受影响，
// 如实报错并给出「如何继续」（删残留重跑 migrate / 保留后走 rebuild）。
// 目标名 = def.Name()（<alias>_v<schema_version>）。
func migrateFirstCreate(ctx context.Context, def *IndexDefinition, c RebuildTarget, opts MigrateOptions, logf func(string, ...any), rec *DefMigrateResult) *DefMigrateResult {
	alias := def.Alias()
	next := def.Name()
	// 前置预检：在发出任何 ES 写请求（含首建 PUT /<next>）之前确认全量灌数编排可用；不可用即拒绝，
	// 不建索引、不留孤儿。dry-run 也先过这道闸（编排不可用则本就不该承诺“能首建+灌数”）。
	if opts.IngestCheck != nil {
		if err := opts.IngestCheck(ctx, alias); err != nil {
			rec.Verdict = string(VerdictError)
			rec.Error = fmt.Sprintf("灌数编排不可用，已在发出任何 ES 写请求之前拒绝首建 [alias=%s]（不建索引、不留孤儿）: %v", alias, err)
			logf("[migrate] %s: %s", alias, rec.Error)
			return rec
		}
	}
	dbExpected, err := opts.DBCount(ctx, alias)
	if err != nil {
		rec.Verdict = string(VerdictError)
		rec.Error = fmt.Sprintf("统计商品期望数失败: %v", err)
		return rec
	}
	// 建索引时不挂别名：直接用 BodyJSON（已剔除 _meta/aliases），与蓝绿新建同一 body。
	body, err := def.BodyJSON()
	if err != nil {
		rec.Verdict = string(VerdictError)
		rec.Error = fmt.Sprintf("es: 生成首建 body 失败: %v", err)
		return rec
	}
	// 对账达标后单次 _aliases 原子挂别名的请求体：复用 RebuildSwitchBody(alias,next,"")，
	// cur 传空即「纯 add」，与蓝绿切换共用同一份请求体生成器，不新造第二套。
	addAliasBody := mustBody(RebuildSwitchBody(alias, next, ""))
	rec.Planned = append(rec.Planned,
		fmt.Sprintf("PUT /%s  body 不含 aliases（首建只建索引，暂不挂别名）", next),
		fmt.Sprintf("灌数 目标 %s，op_type=index 幂等，期望 %d 条", next, dbExpected),
		fmt.Sprintf("POST /%s/_refresh + GET /%s/_count  对账（容差 %d）", next, next, opts.Tolerance),
		fmt.Sprintf("对账达标后 POST /_aliases 单次原子挂别名：%s", addAliasBody),
	)
	if opts.DryRun {
		rec.Verdict = string(VerdictFirstCreate)
		rec.Reason = fmt.Sprintf("别名与同名索引均不存在，将首建 %s（不挂别名）并灌数 %d 条，对账达标后再单次 _aliases 挂别名", next, dbExpected)
		logf("[migrate] %s: [dry-run] %s", alias, rec.Reason)
		return rec
	}
	logf("[migrate] %s: 首建 %s（不挂别名，对账达标后再挂）", alias, next)
	if err := c.CreateIndex(ctx, next, body); err != nil {
		rec.Verdict = string(VerdictError)
		rec.Error = fmt.Sprintf("首建索引失败 [index=%s]: %v", next, err)
		rec.Rebuild = nil
		return rec
	}
	if opts.Sync == nil {
		rec.Verdict = string(VerdictError)
		rec.Error = fmt.Sprintf("已首建 %s（未挂别名）但未注入灌数回调（SyncFunc 为 nil）：别名 %s 未指向它，线上读不受影响", next, alias)
		return rec
	}
	summary, err := opts.Sync(ctx, alias, next)
	if err != nil {
		rec.Verdict = string(VerdictError)
		rec.Error = fmt.Sprintf("首建后灌数失败 [target=%s]（未挂别名，线上读不受影响）: %v", next, err)
		return rec
	}
	if err := c.Refresh(ctx, next); err != nil {
		rec.Verdict = string(VerdictError)
		rec.Error = fmt.Sprintf("首建后对账 refresh 失败 [target=%s]（未挂别名，线上读不受影响）: %v", next, err)
		return rec
	}
	esCount, err := c.Count(ctx, next)
	if err != nil {
		rec.Verdict = string(VerdictError)
		rec.Error = fmt.Sprintf("首建后对账 count 失败 [target=%s]（未挂别名，线上读不受影响）: %v", next, err)
		return rec
	}
	// 对账口径完全沿用 ReconcileCounts（含负容差=永不可达标），不新造第二套。
	diff, ok := ReconcileCounts(dbExpected, esCount, opts.Tolerance)
	if !ok {
		// 关键：对账不达标绝不挂别名——索引存在但不对外服务，避免半空索引跑到生产别名后面。
		rec.Verdict = string(VerdictError)
		rec.Error = fmt.Sprintf("首建对账不达标（DB=%d ES=%d 差 %+d 超容差 %d）：索引 %s 已建成但未挂别名（生产别名 %s 未指向它，线上读不受影响）。"+
			"修复根因后二选一继续：① 删除该残留（es:cleanup --index %s --list-only=false）后重跑 es:migrate --alias %s 走首建；② 保留该索引、单独挂上别名后改用 es:rebuild 重新灌入并对账",
			dbExpected, esCount, diff, opts.Tolerance, next, alias, next, alias)
		return rec
	}
	// 对账达标 → 单次 _aliases 原子挂别名（走 Perform 写原语，与 inplace 同一审计路径；dry-run 已被上面的早退拦下）。
	logf("[migrate] %s: 首建对账达标，单次 POST /_aliases 挂别名 %s -> %s：%s", alias, alias, next, addAliasBody)
	if _, err := c.Perform(ctx, "POST", "/_aliases", addAliasBody); err != nil {
		rec.Verdict = string(VerdictError)
		rec.Error = fmt.Sprintf("首建对账达标但挂别名失败 [alias=%s -> %s]: %v：索引 %s 已灌入、对账达标，却未挂上别名（生产别名 %s 未指向它，线上读不受影响），"+
			"可修复后重跑 es:migrate --alias %s（首建幂等）或手工 POST /_aliases 补齐", next, alias, err, next, alias, alias)
		return rec
	}
	rec.Verdict = string(VerdictFirstCreate)
	rec.Reason = fmt.Sprintf("首建完成并灌入 %d 条（扫描 %d，失败 %d），对账达标后单次 _aliases 挂别名 %s -> %s",
		summary.Sent, summary.Scanned, summary.Failed, alias, next)
	logf("[migrate] %s: %s", alias, rec.Reason)
	return rec
}

// migrateCutoverRequired 处理「别名不存在但同名物理索引存在」的存量未割接态。
// 写路径：中止并引导 es:cutover（绝不在 migrate 里做 remove_index 那种不可逆动作）。
// dry-run：额外读该物理索引线上结构跑 Decide，把「割接后 migrate 会判成什么」如实打印，
// 这正是本地真机预演「证明预期判定是 rebuild（缺 _source.excludes）」的落点。
func migrateCutoverRequired(ctx context.Context, def *IndexDefinition, c RebuildTarget, opts MigrateOptions, logf func(string, ...any), rec *DefMigrateResult) *DefMigrateResult {
	alias := def.Alias()
	rec.Verdict = string(VerdictCutoverRequired)
	rec.Reason = fmt.Sprintf("别名位 %s 仍是物理索引（存量未割接），migrate 不做不可逆割接；请先执行 es:cutover --alias %s", alias, alias)
	logf("[migrate] %s: %s", alias, rec.Reason)

	if !opts.DryRun {
		rec.Error = "存量未割接，需先执行 es:cutover"
		return rec
	}
	// dry-run 只读预演：读同名物理索引的线上结构，展示割接后的分级判定。
	lm, err := c.GetMapping(ctx, alias)
	if err != nil {
		rec.Reason += fmt.Sprintf("（结构预览失败：%v）", err)
		return rec
	}
	ls, err := c.GetSettings(ctx, alias)
	if err != nil {
		rec.Reason += fmt.Sprintf("（结构预览失败：%v）", err)
		return rec
	}
	d := Decide(def, lm, ls)
	rec.Action = d.Action.String()
	rec.LiveFingerprint = d.LiveFingerprint
	rec.Planned = append(rec.Planned, fmt.Sprintf("[割接后预览] Decide=%s：%s", d.Action, d.Reason))
	logf("[migrate] %s: [dry-run] 割接后结构分级预览：action=%s（%s）", alias, d.Action, d.Reason)
	return rec
}
