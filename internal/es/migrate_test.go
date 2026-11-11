package es

// migrate_test.go 钉第五节分级算法的六个判定面（全部用 fake / httptest，不连真 ES/MySQL）：
//   - 首建：先建（body 不带 aliases）→ 灌数 → 对账达标后才**单次** _aliases 挂别名；
//     对账不达标则索引存在但**绝不挂别名**（顺序改回「先挂别名」时下面两条用例必须变红）；
//   - 存量未割接：写路径零写并引导 es:cutover；--dry-run 额外给出「割接后会被判成什么」的
//     结构预览（本仓库线上缺 _source.excludes，预览必须是 rebuild）；
//   - 别名指向 >1：报错、绝不「取首个」；
//   - inplace：只下发动态 settings + 新增字段子集，不新建索引、不切别名；
//   - 逐 Definition 独立：单条失败不阻断其余（聚合报错里点名 1/2）；
//   - --dry-run 在**生产实现路径**上也只发 GET/HEAD（httptest 逐条请求验收方法）。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/olivere/elastic/v7"
)

// ---------------------------------------------------------------------------
// 夹具
// ---------------------------------------------------------------------------

func migrateDefJSON(alias string, version int, settings, mappings string) string {
	return fmt.Sprintf(`{
		"_meta": {"alias": %q, "schema_version": %d, "status": "active"},
		"settings": %s,
		"mappings": %s
	}`, alias, version, settings, mappings)
}

func mustMigrateDef(t *testing.T, alias string, version int, settings, mappings string) *IndexDefinition {
	t.Helper()
	def, err := ParseDefinitionJSONC("testdata/"+alias+".jsonc", []byte(migrateDefJSON(alias, version, settings, mappings)), false)
	if err != nil {
		t.Fatalf("构造迁移声明失败: %v", err)
	}
	return def
}

func mustRegistry(t *testing.T, defs ...*IndexDefinition) *Registry {
	t.Helper()
	r := NewRegistry()
	if err := r.Register(defs...); err != nil {
		t.Fatalf("注册声明失败: %v", err)
	}
	return r
}

// 一套「线上只缺 _source.excludes」的声明/回读体：结构差只落在 mapping 顶层 _source 上，
// 判定必须唯一地指向 rebuild（静态结构，无法原地改）。
const (
	migSettingsJSON  = `{"index":{"number_of_shards":"3","number_of_replicas":"1","refresh_interval":"1s"}}`
	migMapsJSON      = `{"dynamic":"strict","_source":{"excludes":["store_info","description"]},"properties":{"id":{"type":"keyword"}}}`
	migLiveMapsJSON  = `{"dynamic":"strict","properties":{"id":{"type":"keyword"}}}`
	migLiveSettsJSON = `{"index":{"number_of_shards":"3","number_of_replicas":"1","refresh_interval":"1s","uuid":"nQ2lYmQxT3a-FAKE"}}`
)

func liveMapsOf(t *testing.T) map[string]any {
	t.Helper()
	return objOf(t, migLiveMapsJSON)
}

func liveSettingsOf(t *testing.T) map[string]any {
	t.Helper()
	return objOf(t, migLiveSettsJSON)
}

func verdictOf(res *MigrateResult, alias string) *DefMigrateResult {
	for _, d := range res.Defs {
		if d.Alias == alias {
			return d
		}
	}
	return nil
}

func migrateOpts(reg *Registry, f RebuildTarget, dryRun bool) MigrateOptions {
	return MigrateOptions{
		Registry: reg,
		Target:   f,
		Sync:     cutoverSync(SyncSummary{Scanned: 5, Sent: 5}, nil),
		Compensate: func(context.Context, string, time.Time) (CompensateSummary, error) {
			return CompensateSummary{}, nil
		},
		DBCount:   fixedCount(5),
		Tolerance: 0,
		DryRun:    dryRun,
		Logf:      silentLogf,
	}
}

// ---------------------------------------------------------------------------
// 首建
// ---------------------------------------------------------------------------

// firstCreateSpy 在 fakeRebuildTarget 之上额外捕获每一次 POST /_aliases 的请求体，
// 用来直接证明「首建到底挂没挂别名、挂了几次」——这是本用例的破坏即红探针。
// RunMigrate 用 newAuditedRebuild 包装注入的 Target，其 Perform 落到这里，
// 记录后再委托给内层 fake（内层 fake 只登记 op 不改别名状态）。
type firstCreateSpy struct {
	*fakeRebuildTarget
	mu         sync.Mutex
	aliasCalls []string // 每次 POST /_aliases 实际下发的 body（按发生顺序）
}

func newFirstCreateSpy() *firstCreateSpy {
	return &firstCreateSpy{fakeRebuildTarget: newFakeRebuildTarget()}
}

func (s *firstCreateSpy) Perform(ctx context.Context, method, path, body string) (string, error) {
	if strings.EqualFold(method, "POST") && path == "/_aliases" {
		s.mu.Lock()
		s.aliasCalls = append(s.aliasCalls, body)
		s.mu.Unlock()
	}
	return s.fakeRebuildTarget.Perform(ctx, method, path, body)
}

func (s *firstCreateSpy) snapshotAliasCalls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.aliasCalls...)
}

// createBodyHasAliases 解析 PUT 建索引 body 是否携带顶层 aliases（回归「先挂别名」的探针）。
func createBodyHasAliases(t *testing.T, body string) bool {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("建索引 body 非法 JSON: %v\n%s", err, body)
	}
	_, has := m["aliases"]
	return has
}

// 破坏即红用例②：对账达标时，别名必须以**单次** _aliases 纯 add 请求挂上，且一定发生在
// 对账 count **之后**（先建不带别名 → 灌数 → 对账 → 达标才挂别名）。
func TestRunMigrate_FirstCreateAttachesAliasAfterReconcileViaSingleAliases(t *testing.T) {
	const alias = "shop-product"
	def := mustMigrateDef(t, alias, 1, migSettingsJSON, migMapsJSON)
	spy := newFirstCreateSpy() // 别名与同名索引都不存在 => 首建
	next := def.Name()
	spy.counts[next] = 5 // migrateOpts 的 DBCount 固定 5，对账达标

	res, err := RunMigrate(context.Background(), migrateOpts(mustRegistry(t, def), spy, false))
	if err != nil {
		t.Fatalf("首建失败: %v", err)
	}
	rec := verdictOf(res, alias)
	if rec == nil || rec.Verdict != string(VerdictFirstCreate) {
		t.Fatalf("判定应为首建，got %+v", rec)
	}
	if !spy.indexExists(next) {
		t.Fatalf("首建必须真的把 %s 建出来", next)
	}
	// 建索引时绝不挂别名：body 不含 aliases（把顺序改回「先挂别名」此处即红）。
	if createBodyHasAliases(t, spy.createdBodies[next]) {
		t.Errorf("首建 body 绝不能含 aliases（必须等对账达标后单独挂别名），got=%s", spy.createdBodies[next])
	}
	// 达标后别名以**单次** _aliases 请求挂上。
	calls := spy.snapshotAliasCalls()
	if len(calls) != 1 {
		t.Fatalf("对账达标后必须且只能发一次 POST /_aliases 挂别名，got %d 次：%v", len(calls), calls)
	}
	var got struct {
		Actions []struct {
			Add struct {
				Index string `json:"index"`
				Alias string `json:"alias"`
			} `json:"add"`
			Remove json.RawMessage `json:"remove"`
		} `json:"actions"`
	}
	if err := json.Unmarshal([]byte(calls[0]), &got); err != nil {
		t.Fatalf("解析 _aliases 请求体失败: %v\n%s", err, calls[0])
	}
	if len(got.Actions) != 1 || got.Actions[0].Add.Index != next || got.Actions[0].Add.Alias != alias {
		t.Errorf("必须是单次纯 add {%q -> %q}，不应含 remove，got=%s", alias, next, calls[0])
	}
	if len(got.Actions) == 1 && got.Actions[0].Remove != nil {
		t.Errorf("首建挂别名不得含 remove（没有旧索引可摘），got=%s", calls[0])
	}
	// 时序：挂别名必须在对账 count 之后（先对账达标、再挂别名）。
	ops := spy.allOps()
	if !opsHave(ops, "POST /"+next+"/_refresh") || opPos(ops, "GET /"+next+"/_count") < 0 {
		t.Errorf("首建必须先 Refresh + Count 对账：%v", ops)
	}
	pCount, pAlias := opPos(ops, "GET /"+next+"/_count"), opPos(ops, "POST /_aliases")
	if pAlias < 0 || pCount < 0 || pAlias < pCount {
		t.Errorf("挂别名 POST /_aliases 必须在对账 count 之后：%v", ops)
	}
}

// 破坏即红用例①：对账不达标时，别名**绝不能被添加**。若有人把顺序改回「建索引即挂别名」
// （create body 携带 aliases）或「先挂别名再对账」（出现 POST /_aliases），下面两条断言必红。
func TestRunMigrate_FirstCreateReconcileFailLeavesAliasUnattached(t *testing.T) {
	const alias = "shop-product"
	def := mustMigrateDef(t, alias, 1, migSettingsJSON, migMapsJSON)
	spy := newFirstCreateSpy()
	next := def.Name()
	spy.counts[next] = 3 // DB 期望 5，差 -2 超容差 0 => 对账不达标

	res, err := RunMigrate(context.Background(), migrateOpts(mustRegistry(t, def), spy, false))
	if err == nil {
		t.Fatal("对账不达标必须报错")
	}
	rec := verdictOf(res, alias)
	if rec == nil || rec.Verdict != string(VerdictError) {
		t.Fatalf("判定应为 error，got %+v", rec)
	}
	// 索引已建成（灌到一半也要保留供排查）。
	if !spy.indexExists(next) {
		t.Errorf("对账不达标仍应保留已建索引 %s 供排查", next)
	}
	// 核心：既不能靠 create body 挂别名，也不能发 POST /_aliases。
	if createBodyHasAliases(t, spy.createdBodies[next]) {
		t.Errorf("对账不达标时 create body 绝不能带 aliases：%s", spy.createdBodies[next])
	}
	if calls := spy.snapshotAliasCalls(); len(calls) != 0 {
		t.Errorf("对账不达标绝不能挂别名，却发了 %d 次 POST /_aliases：%v", len(calls), calls)
	}
	if opsHave(spy.allOps(), "POST /_aliases") {
		t.Errorf("对账不达标绝不能出现 POST /_aliases 写操作：%v", spy.allOps())
	}
	if spy.aliasIndexOf(alias) != "" || spy.holds(alias, next) {
		t.Errorf("对账不达标后别名 %s 不该指向 %s", alias, next)
	}
	// 报错必须如实点名残留索引并给出「如何继续」。
	if !strings.Contains(rec.Error, next) {
		t.Errorf("报错必须点名残留索引 %s，got=%q", next, rec.Error)
	}
	if !strings.Contains(rec.Error, "重跑") && !strings.Contains(rec.Error, "es:rebuild") {
		t.Errorf("报错必须说明如何继续（重跑 es:migrate / 走 es:rebuild），got=%q", rec.Error)
	}
}

// ---------------------------------------------------------------------------
// 存量未割接：写路径中止，dry-run 给割接后预览
// ---------------------------------------------------------------------------

func TestRunMigrate_CutoverRequiredNeverWritesOnWritePath(t *testing.T) {
	const alias = "shop-product"
	def := mustMigrateDef(t, alias, 2, migSettingsJSON, migMapsJSON)
	f := newFakeRebuildTarget()
	f.indexes[alias] = true // 别名位仍是物理索引（存量未割接）

	res, err := RunMigrate(context.Background(), migrateOpts(mustRegistry(t, def), f, false))
	if err == nil {
		t.Fatal("存量未割接必须报错引导 es:cutover")
	}
	rec := verdictOf(res, alias)
	if rec.Verdict != string(VerdictCutoverRequired) {
		t.Errorf("判定应为 cutover_required，got=%q", rec.Verdict)
	}
	if !strings.Contains(rec.Error, "es:cutover") && !strings.Contains(rec.Reason, "es:cutover") {
		t.Errorf("必须给出可执行的下一步指引（es:cutover），got error=%q reason=%q", rec.Error, rec.Reason)
	}
	if writes := f.opsOfKind("write"); len(writes) > 0 {
		t.Errorf("migrate 不做不可逆割接，写路径必须零写请求：%v", writes)
	}
	if f.indexExists(def.Name()) {
		t.Errorf("未割接时不该顺手建出 v2")
	}
}

func TestRunMigrate_DryRunPreviewsRebuildAfterCutoverForMissingSourceExcludes(t *testing.T) {
	const alias = "shop-product"
	def := mustMigrateDef(t, alias, 2, migSettingsJSON, migMapsJSON)
	f := newFakeRebuildTarget()
	f.indexes[alias] = true
	f.mappings[alias] = liveMapsOf(t)     // 线上：没有 _source.excludes
	f.settings[alias] = liveSettingsOf(t) // settings 与声明等价，差只落在 _source 上

	res, err := RunMigrate(context.Background(), migrateOpts(mustRegistry(t, def), f, true))
	if err != nil {
		t.Fatalf("dry-run 不该报错: %v", err)
	}
	rec := verdictOf(res, alias)
	if rec.Verdict != string(VerdictCutoverRequired) {
		t.Fatalf("判定仍应是 cutover_required，got=%q", rec.Verdict)
	}
	// #8 会重点打的靶：线上缺 _source.excludes，割接后 migrate 必须判 rebuild。
	if rec.Action != ActionRebuild.String() {
		t.Errorf("割接后结构预览必须判 rebuild（_source.excludes 属静态结构），got=%q reason=%q\n  planned=%v",
			rec.Action, rec.Reason, rec.Planned)
	}
	if len(rec.Planned) == 0 || !strings.Contains(rec.Planned[0], "rebuild") {
		t.Errorf("dry-run 要把预览结论打进 Planned，got=%v", rec.Planned)
	}
	if rec.Error != "" {
		t.Errorf("dry-run 不该把预览记成失败，got=%q", rec.Error)
	}
	if len(f.opsOfKind("write")) > 0 || f.indexExists(def.Name()) {
		t.Errorf("dry-run 零写操作已被破坏：%v", f.allOps())
	}
}

// ---------------------------------------------------------------------------
// 别名多目标：绝不猜首个
// ---------------------------------------------------------------------------

func TestRunMigrate_MultipleAliasTargetsAbortWithoutGuessing(t *testing.T) {
	const alias = "shop-product"
	def := mustMigrateDef(t, alias, 2, migSettingsJSON, migMapsJSON)
	f := newFakeRebuildTarget()
	f.indexes[alias+"_v1"] = true
	f.indexes[alias+"_v2"] = true
	f.aliasRows[alias] = []AliasTarget{
		{Alias: alias, Index: alias + "_v1"},
		{Alias: alias, Index: alias + "_v2"},
	}

	res, err := RunMigrate(context.Background(), migrateOpts(mustRegistry(t, def), f, false))
	if err == nil {
		t.Fatal("多目标必须报错（修 PHP array_keys()[0] 只取首个的隐患）")
	}
	rec := verdictOf(res, alias)
	if rec.Verdict != string(VerdictError) {
		t.Errorf("判定应为 error，got=%q", rec.Verdict)
	}
	if !strings.Contains(rec.Error, "唯一") {
		t.Errorf("报错要点名「唯一指向」，got=%q", rec.Error)
	}
	if len(f.opsOfKind("write")) > 0 {
		t.Errorf("多目标中止前不得有任何写操作：%v", f.allOps())
	}
}

// ---------------------------------------------------------------------------
// inplace：只下发增量
// ---------------------------------------------------------------------------

func TestRunMigrate_InplaceSubmitsOnlyDynamicDelta(t *testing.T) {
	const alias = "shop-product"
	cur := alias + "_v1"
	def := mustMigrateDef(t, alias, 1,
		`{"index":{"number_of_shards":"3","number_of_replicas":"1","refresh_interval":"1s"}}`,
		`{"dynamic":"strict","properties":{"id":{"type":"keyword"},"sales":{"type":"long"}}}`)
	f := newFakeRebuildTarget()
	f.indexes[cur] = true
	f.aliasRows[alias] = []AliasTarget{{Alias: alias, Index: cur, WriteIndex: "true"}}
	// 线上：refresh_interval 被改成 30s（动态项，可原地），且还没有 sales 字段。
	f.mappings[cur] = objOf(t, `{"dynamic":"strict","properties":{"id":{"type":"keyword"}}}`)
	f.settings[cur] = objOf(t, `{"index.number_of_shards":"3","index.number_of_replicas":"1","index.refresh_interval":"30s"}`)

	res, err := RunMigrate(context.Background(), migrateOpts(mustRegistry(t, def), f, false))
	if err != nil {
		t.Fatalf("inplace 失败: %v", err)
	}
	rec := verdictOf(res, alias)
	if rec.Verdict != string(VerdictInplace) || rec.Action != ActionInplace.String() {
		t.Fatalf("判定应为 inplace，got=%+v", rec)
	}
	if len(f.putSettings) != 1 || !strings.Contains(f.putSettings[0], "1s") {
		t.Errorf("只应下发一条动态 settings 增量（恢复声明值 1s），got=%v", f.putSettings)
	}
	if strings.Contains(f.putSettings[0], "number_of_shards") {
		t.Errorf("静态项绝不进 inplace settings：%s", f.putSettings[0])
	}
	ops := f.allOps()
	mapOp := "PUT /" + cur + "/_mapping"
	if opPos(ops, mapOp) < 0 {
		t.Errorf("put_mapping 必须只发新增字段子集到现有索引，ops=%v", ops)
	}
	if opsHave(ops, "/_aliases") || opsHave(ops, "POST /"+alias+"_v2") || f.indexExists(alias+"_v2") {
		t.Errorf("inplace 路径不得建索引或切别名：%v", ops)
	}
	if opsHave(ops, "/_close") || opsHave(ops, "/_open") {
		t.Errorf("绝不照搬 PHP 的无条件 close/open：%v", ops)
	}
}

// ---------------------------------------------------------------------------
// 逐 Definition 独立
// ---------------------------------------------------------------------------

func TestRunMigrate_SingleDefFailureDoesNotBlockOthers(t *testing.T) {
	const badAlias, goodAlias = "shop-orders", "shop-product"
	bad := mustMigrateDef(t, badAlias, 3, migSettingsJSON, migMapsJSON)
	good := mustMigrateDef(t, goodAlias, 1, migSettingsJSON, migMapsJSON)
	f := newFakeRebuildTarget()

	// 坏的那条：别名指向两个索引 => error
	f.indexes[badAlias+"_v1"] = true
	f.indexes[badAlias+"_v2"] = true
	f.aliasRows[badAlias] = []AliasTarget{
		{Alias: badAlias, Index: badAlias + "_v1"},
		{Alias: badAlias, Index: badAlias + "_v2"},
	}
	// 好的那条：别名唯一 + 线上结构与声明逐字一致 => noop
	goodCur := good.Name()
	f.indexes[goodCur] = true
	f.aliasRows[goodAlias] = []AliasTarget{{Alias: goodAlias, Index: goodCur, WriteIndex: "true"}}
	f.mappings[goodCur] = good.MappingsMap()
	f.settings[goodCur] = good.SettingsMap()

	res, err := RunMigrate(context.Background(), migrateOpts(mustRegistry(t, bad, good), f, false))
	if err == nil {
		t.Fatal("有一条失败就必须聚合报错")
	}
	if !strings.Contains(err.Error(), "1/2") {
		t.Errorf("聚合报错要点名失败比例，got=%v", err)
	}
	if len(res.Defs) != 2 {
		t.Fatalf("两条 Definition 都必须被独立处理，got %d 条：%+v", len(res.Defs), res.Defs)
	}
	if rec := verdictOf(res, badAlias); rec == nil || rec.Verdict != string(VerdictError) {
		t.Errorf("坏条目应为 error，got %+v", rec)
	}
	if rec := verdictOf(res, goodAlias); rec == nil || rec.Verdict != string(VerdictNoop) {
		t.Errorf("好条目不该被坏邻居拖累，应为 noop，got %+v", rec)
	}
}

// ---------------------------------------------------------------------------
// --dry-run 在生产实现路径上同样零写（httptest 逐请求验方法）
// ---------------------------------------------------------------------------

func newRoutingESTool(t *testing.T, routes map[string]string) (*elastic.Client, func() []recordedReq) {
	t.Helper()
	var (
		mu   sync.Mutex
		reqs []recordedReq
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		reqs = append(reqs, recordedReq{method: r.Method, path: r.URL.Path, body: string(raw)})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		body, ok := routes[r.URL.Path]
		if !ok {
			// 别名不存在：真机回 404 alias_exception，admin.AliasIndexes 会当成空指向。
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"type":"alias_exception","reason":"alias [x] not found"},"status":404}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	cli, err := elastic.NewClient(
		elastic.SetURL(srv.URL),
		elastic.SetSniff(false),
		elastic.SetHealthcheck(false),
		elastic.SetMaxRetries(0),
	)
	if err != nil {
		t.Fatalf("构造测试用 ES 客户端失败: %v", err)
	}
	return cli, func() []recordedReq {
		mu.Lock()
		defer mu.Unlock()
		return append([]recordedReq(nil), reqs...)
	}
}

func TestRunMigrate_DryRunAgainstProductionClientSendsOnlyReads(t *testing.T) {
	const alias = "shop-product"
	def := mustMigrateDef(t, alias, 2, migSettingsJSON, migMapsJSON)
	routes := map[string]string{
		"/" + alias:                `{}`, // HEAD 命中：别名位仍是个物理索引
		"/" + alias + "/_mapping":  fmt.Sprintf(`{"%s":{"mappings":%s}}`, alias, migLiveMapsJSON),
		"/" + alias + "/_settings": fmt.Sprintf(`{"%s":{"settings":%s}}`, alias, migLiveSettsJSON),
	}
	cli, seen := newRoutingESTool(t, routes)
	injectSharedESClient(t, cli)

	res, err := RunMigrate(context.Background(), migrateOpts(mustRegistry(t, def), NewAdminRebuild(), true))
	if err != nil {
		t.Fatalf("dry-run 失败: %v", err)
	}
	rec := verdictOf(res, alias)
	if rec == nil || rec.Verdict != string(VerdictCutoverRequired) || rec.Action != ActionRebuild.String() {
		t.Fatalf("生产路径 dry-run 判定不如预期，got %+v", rec)
	}

	reqs := seen()
	if len(reqs) == 0 {
		t.Fatal("一条请求都没发，用例本身失效")
	}
	for _, r := range reqs {
		if r.method != http.MethodGet && r.method != http.MethodHead {
			t.Errorf("dry-run 只允许 GET/HEAD，出现写请求：%s", describeReqs(reqs))
			break
		}
	}
	// 至少要把「HEAD 探测别名位、读结构」都真的走到，否则零写是因为没跑到判定点。
	if opPos(reqMethodsAndPaths(reqs), "HEAD /"+alias) < 0 {
		t.Errorf("应探测过 %s 是否为物理索引，got=%s", alias, describeReqs(reqs))
	}

	// 第二个只读预演现场：别名与同名索引都不存在 → 走**首建**分支。
	// 这一分支是 migrate 在 dry-run 下唯一会尝试写（PUT 建索引 + 灌数）的路径，
	// 拿它做锚点：一旦首建的 dry-run 早退被拆，下面立刻出现 PUT / 红。
	firstAlias := "shop-newcome"
	first := mustMigrateDef(t, firstAlias, 1, migSettingsJSON, migMapsJSON)
	cli2, seen2 := newRoutingESTool(t, nil) // 无任何路由：HEAD/GET 全 404 —— 别名与索引都不存在
	injectSharedESClient(t, cli2)
	if _, err := RunMigrate(context.Background(), migrateOpts(mustRegistry(t, first), NewAdminRebuild(), true)); err != nil {
		t.Fatalf("首建 dry-run 失败: %v", err)
	}
	for _, r := range seen2() {
		if r.method != http.MethodGet && r.method != http.MethodHead {
			t.Errorf("首建预演出现写请求：%s", describeReqs(seen2()))
			break
		}
	}
	if opPos(reqMethodsAndPaths(seen2()), "HEAD /"+firstAlias) < 0 {
		t.Errorf("首建预演应探测过 %s，got=%s", firstAlias, describeReqs(seen2()))
	}
}

func reqMethodsAndPaths(reqs []recordedReq) []string {
	out := make([]string, 0, len(reqs))
	for _, r := range reqs {
		out = append(out, r.method+" "+r.path)
	}
	return out
}
