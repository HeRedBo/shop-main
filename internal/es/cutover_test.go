package es

// cutover_test.go 覆盖两层：
//   1. 判定/编排的纯函数（DecideCutover、MissingPlugins、CutoverAliasBody、VerifyCutoverTarget、
//      ReconcileCounts、ResolveSchemaVersion、IngestRestoreBody）—— 割接的正确性完全由这些决定；
//   2. RunCutover 的编排时序 —— 用 fake Cluster 断言「写操作序列」「对账不达标时绝不碰别名」
//      「dry-run 零写」「收尾用独立 ctx」这几条只有跑起来才看得见的约束。
// 全部用例不依赖 ES 与 MySQL 连接。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	sharedes "github.com/HeRedBo/pkg/es"
	"github.com/olivere/elastic/v7"

	"shop/pkg/global"
)

// ---------------------------------------------------------------------------
// 测试夹具
// ---------------------------------------------------------------------------

// cutoverDefJSON 构造带 requires_plugins 的声明体（diff_test.go 的 bodyOf 不含该字段，
// 而割接前置校验必须依赖 requires_plugins 才能测）。
func cutoverDefJSON(alias string, version int, plugins []string) string {
	pj, _ := json.Marshal(plugins)
	return fmt.Sprintf(`{
		"_meta": {"alias": %q, "schema_version": %d, "status": "active", "requires_plugins": %s},
		"settings": {"refresh_interval": "1s", "number_of_shards": 3, "index": {"number_of_replicas": 1}},
		"mappings": {"dynamic": "strict", "properties": {"id": {"type": "keyword"}}}
	}`, alias, version, pj)
}

func mustCutoverDef(t *testing.T, alias string, version int, plugins ...string) *IndexDefinition {
	t.Helper()
	def, err := ParseDefinitionJSONC("testdata/cutover.jsonc", []byte(cutoverDefJSON(alias, version, plugins)), false)
	if err != nil {
		t.Fatalf("构造割接声明失败: %v", err)
	}
	return def
}

// fakeCall 一次集群调用。ctxErr/hasDeadline 必须在**调用发生那一刻**取样：
// 收尾 ctx 用完即 cancel，事后再读 Err() 只会看到 canceled，证明不了任何事。
type fakeCall struct {
	op          string
	ctx         context.Context
	ctxErr      error
	hasDeadline bool
}

// fakeCluster 是 Cluster 的内存实现，只建模割接关心的状态。
//
// honorCtx=true 时按真机行为对已取消的 ctx 返回 ctx.Err()，用于验证收尾动作
// 确实换了独立 context（默认 false，让绝大多数用例只关心调用序列）。
type fakeCluster struct {
	mu       sync.Mutex
	calls    []fakeCall
	hook     func(op string, ctx context.Context)
	honorCtx bool

	// cancelOnSwitch 在割接**成功生效之后**取消主 ctx，用来把 --timeout 精确
	// 卡在「切完但还没校验」那一刻。
	cancelOnSwitch context.CancelFunc

	plugins   []string
	indexes   map[string]bool          // 存在的物理索引
	aliasRows map[string][]AliasTarget // alias -> 指向行
	counts    map[string]int64         // _count 返回值
	settings  map[string]map[string]any
	mappings  map[string]map[string]any
	errs      map[string]error // 按 op 前缀注入失败

	createdBodies map[string]string
	putSettings   []string // 依次记录的 settings 请求体
	switchCalls   int
}

func newFakeCluster() *fakeCluster {
	return &fakeCluster{
		indexes:       map[string]bool{},
		aliasRows:     map[string][]AliasTarget{},
		counts:        map[string]int64{},
		settings:      map[string]map[string]any{},
		mappings:      map[string]map[string]any{},
		errs:          map[string]error{},
		createdBodies: map[string]string{},
	}
}

// enter 记录调用、跑 hook、按 ctx 与注入表决定失败，返回非 nil 即调用点立即返回。
func (f *fakeCluster) enter(op string, ctx context.Context) error {
	ctxErr := ctx.Err()
	_, hasDeadline := ctx.Deadline()
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{op: op, ctx: ctx, ctxErr: ctxErr, hasDeadline: hasDeadline})
	hook := f.hook
	honor := f.honorCtx
	f.mu.Unlock()

	if hook != nil {
		hook(op, ctx)
	}
	if honor && ctxErr != nil {
		return ctxErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for prefix, err := range f.errs {
		if strings.HasPrefix(op, prefix) {
			return err
		}
	}
	return nil
}

func (f *fakeCluster) opsOfKind(kind string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if (kind == "write") == isWriteOp(c.op) {
			out = append(out, c.op)
		}
	}
	return out
}

func (f *fakeCluster) allOps() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, c.op)
	}
	return out
}

// lastCallOf 返回最后一次匹配 op 的调用记录（收尾动作总在该 op 之后发生）。
func (f *fakeCluster) lastCallOf(op string) (fakeCall, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var last fakeCall
	found := false
	for _, c := range f.calls {
		if strings.HasPrefix(c.op, op) {
			last, found = c, true
		}
	}
	return last, found
}

func isWriteOp(op string) bool {
	for _, p := range []string{"PUT ", "POST ", "DELETE "} {
		if strings.HasPrefix(op, p) {
			return true
		}
	}
	return false
}

func (f *fakeCluster) IndexExists(ctx context.Context, idx string) (bool, error) {
	if err := f.enter("HEAD /"+idx, ctx); err != nil {
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.indexes[idx] {
		return true, nil
	}
	// 别名在 ES 里同样能被 HEAD 命中（解析到指向索引），故别名名单独再看一次
	if _, ok := f.aliasRows[idx]; ok {
		return true, nil
	}
	return false, nil
}

func (f *fakeCluster) AliasTargets(ctx context.Context, alias string) ([]string, error) {
	if err := f.enter("GET /_alias/"+alias, ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, r := range f.aliasRows[alias] {
		out = append(out, r.Index)
	}
	return out, nil
}

func (f *fakeCluster) ListAliasRows(ctx context.Context) ([]AliasTarget, error) {
	if err := f.enter("GET /_cat/aliases", ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []AliasTarget
	for _, rows := range f.aliasRows {
		out = append(out, rows...)
	}
	SortAliasTargets(out)
	return out, nil
}

func (f *fakeCluster) ListPlugins(ctx context.Context) ([]string, error) {
	if err := f.enter("GET /_cat/plugins", ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.plugins...), nil
}

func (f *fakeCluster) IndexStats(ctx context.Context, idx string) ([]IndexStat, error) {
	if err := f.enter("GET /_cat/indices/"+idx, ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return []IndexStat{{Index: idx, Health: "yellow", Primaries: 3, DocsCount: f.counts[idx]}}, nil
}

func (f *fakeCluster) Count(ctx context.Context, idx string) (int64, error) {
	if err := f.enter("GET /"+idx+"/_count", ctx); err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counts[idx], nil
}

func (f *fakeCluster) GetMapping(ctx context.Context, idx string) (map[string]any, error) {
	if err := f.enter("GET /"+idx+"/_mapping", ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mappings[idx], nil
}

func (f *fakeCluster) GetSettings(ctx context.Context, idx string) (map[string]any, error) {
	if err := f.enter("GET /"+idx+"/_settings", ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.settings[idx], nil
}

func (f *fakeCluster) CreateIndex(ctx context.Context, idx, bodyJSON string) error {
	if err := f.enter("PUT /"+idx, ctx); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.indexes[idx] {
		return fmt.Errorf("fake: 索引 %s 已存在（resource_already_exists_exception）", idx)
	}
	f.indexes[idx] = true
	f.createdBodies[idx] = bodyJSON
	return nil
}

func (f *fakeCluster) PutSettings(ctx context.Context, idx, bodyJSON string) error {
	if err := f.enter("PUT /"+idx+"/_settings", ctx); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.putSettings = append(f.putSettings, bodyJSON)
	return nil
}

func (f *fakeCluster) Refresh(ctx context.Context, idx string) error {
	return f.enter("POST /"+idx+"/_refresh", ctx)
}

// SwitchAlias 复刻真机语义：remove_index 会把旧物理索引连同其数据一起摘掉。
func (f *fakeCluster) SwitchAlias(ctx context.Context, alias, target string) error {
	if err := f.enter("POST /_aliases", ctx); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.switchCalls++
	if !f.indexes[target] {
		return fmt.Errorf("fake: 目标 %s 不存在，无法加别名", target)
	}
	delete(f.indexes, alias) // remove_index：旧物理索引连数据一起消失
	delete(f.counts, alias)
	f.aliasRows[alias] = []AliasTarget{{Alias: alias, Index: target, WriteIndex: "true"}}
	if f.cancelOnSwitch != nil { // 模拟 --timeout 正好卡在割接生效那一刻
		f.cancelOnSwitch()
	}
	return nil
}

func (f *fakeCluster) Perform(ctx context.Context, method, path, body string) (string, error) {
	op := strings.ToUpper(method) + " " + path
	if err := f.enter(op, ctx); err != nil {
		return "", err
	}
	return "", nil
}

// silentLogf 吞掉编排输出，保持测试日志干净。
func silentLogf(string, ...any) {}

func cutoverSync(summary SyncSummary, err error) SyncFunc {
	return func(ctx context.Context, target string) (SyncSummary, error) {
		s := summary
		s.Target = target
		return s, err
	}
}

func fixedCount(n int64) CountFunc {
	return func(ctx context.Context) (int64, error) { return n, nil }
}

// noopCompensate 是默认的差量补偿桩：不碰集群，只证明调用点被走到。
func noopCompensate(context.Context, string, time.Time) (CompensateSummary, error) {
	return CompensateSummary{}, nil
}

// snapshotAlias 直接读内存状态取样「此刻别名指向谁、旧物理索引还在不在」，
// 不走任何集群方法（不污染调用台账），用于在回调内部证明时序。
func (f *fakeCluster) snapshotAlias(alias string) (target string, oldPhysicalGone bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if rows := f.aliasRows[alias]; len(rows) == 1 {
		target = rows[0].Index
	}
	return target, !f.indexes[alias]
}

// ---------------------------------------------------------------------------
// 纯函数
// ---------------------------------------------------------------------------

func TestDecideCutover(t *testing.T) {
	tests := []struct {
		name  string
		probe AliasProbe
		want  CutoverVerdict
	}{
		{
			name:  "别名位仍是物理索引 => 可以割接",
			probe: AliasProbe{NameIsPhysicalIndex: true, TargetExists: false},
			want:  VerdictProceed,
		},
		{
			// 割接成功后 target 既存在又是别名指向，必须先判幂等而不是「目标被占」
			name:  "已是别名且指向 v1 => 幂等",
			probe: AliasProbe{NameIsPhysicalIndex: false, AliasTargets: []string{"shop-product_v1"}, TargetExists: true},
			want:  VerdictAlreadyCutover,
		},
		{
			name:  "已是别名但指向别的索引 => 仍按幂等已完成处理",
			probe: AliasProbe{AliasTargets: []string{"shop-product_v2"}, TargetExists: false},
			want:  VerdictAlreadyCutover,
		},
		{
			name:  "不是别名但 v1 已存在 => 拒绝覆盖",
			probe: AliasProbe{NameIsPhysicalIndex: true, TargetExists: true},
			want:  VerdictTargetOccupied,
		},
		{
			name:  "两边都没有 => 无割接对象",
			probe: AliasProbe{},
			want:  VerdictMissingSource,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, detail := DecideCutover(tt.probe)
			if got != tt.want {
				t.Fatalf("verdict=%s(%d), want %s；detail=%s", got, got, tt.want, detail)
			}
			if detail == "" {
				t.Errorf("判定必须附带可打印的说明")
			}
		})
	}
}

func TestMissingPlugins(t *testing.T) {
	installed := []string{"analysis-ik", "analysis-pinyin"}
	tests := []struct {
		name     string
		required []string
		want     string
	}{
		{"全部在位", []string{"analysis-ik", "analysis-pinyin"}, ""},
		{"简写也能匹配", []string{"ik", "pinyin"}, ""},
		{"缺一个", []string{"analysis-ik", "analysis-curly"}, "analysis-curly"},
		{"全缺", []string{"mapper-murmur3"}, "mapper-murmur3"},
		{"空条目忽略", []string{""}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MissingPlugins(installed, tt.required)
			if joined := strings.Join(got, ","); joined != tt.want {
				t.Errorf("MissingPlugins()=%v, want %q", got, tt.want)
			}
		})
	}
	if got := MissingPlugins(nil, []string{"analysis-ik"}); len(got) != 1 {
		t.Errorf("集群无任何插件时必须报缺失，实际 %v", got)
	}
}

func TestParsePluginComponents(t *testing.T) {
	// 真机 8.2.2 抓到的样本（同节点多插件 + 重复项）
	raw := `[{"name":"c8fc56e5f380","component":"analysis-ik"},` +
		`{"name":"c8fc56e5f380","component":"analysis-pinyin"},` +
		`{"name":"c8fc56e5f380","component":"analysis-ik"}]`
	got, err := ParsePluginComponents([]byte(raw))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	want := []string{"analysis-ik", "analysis-pinyin"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("got=%v want=%v", got, want)
	}
	if empty, err := ParsePluginComponents([]byte("")); err != nil || empty != nil {
		t.Errorf("空响应应返回 (nil,nil)，实际 (%v,%v)", empty, err)
	}
	if _, err := ParsePluginComponents([]byte("not-json")); err == nil {
		t.Errorf("非法 JSON 必须报错")
	}
}

func TestCutoverAliasBody(t *testing.T) {
	got, err := CutoverAliasBody("shop-product", "shop-product_v1")
	if err != nil {
		t.Fatalf("构造请求体失败: %v", err)
	}
	want := `{"actions":[{"remove_index":{"index":"shop-product"}},` +
		`{"add":{"index":"shop-product_v1","alias":"shop-product","is_write_index":true}}]}`
	if got != want {
		t.Errorf("请求体与真机验证样本不一致:\ngot =%s\nwant=%s", got, want)
	}

	// 结构化校验：remove_index 必须在 add 之前，add 一定带 is_write_index:true
	var parsed cutoverAliasRequest
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("回解失败: %v", err)
	}
	if len(parsed.Actions) != 2 {
		t.Fatalf("必须是一个请求里的两个 action，实际 %d", len(parsed.Actions))
	}
	if parsed.Actions[0].RemoveIndex == nil || parsed.Actions[0].RemoveIndex.Index != "shop-product" {
		t.Errorf("第一个 action 必须 remove_index 旧物理索引：%+v", parsed.Actions[0])
	}
	if parsed.Actions[1].Add == nil || !parsed.Actions[1].Add.IsWriteIndex {
		t.Errorf("add 必须显式 is_write_index=true，否则切完无写索引：%+v", parsed.Actions[1])
	}

	if _, err := CutoverAliasBody("a", "a"); err == nil {
		t.Errorf("别名与索引同名必须拒绝（ES 中二者互斥）")
	}
	if _, err := CutoverAliasBody("", "x"); err == nil {
		t.Errorf("空别名必须拒绝")
	}
}

func TestVerifyCutoverTarget(t *testing.T) {
	rows := []AliasTarget{
		{Alias: "shop-product", Index: "shop-product_v1", WriteIndex: "true"},
		{Alias: "other", Index: "other_v1", WriteIndex: "-"},
	}
	if ok, note := VerifyCutoverTarget(rows, "shop-product", "shop-product_v1"); !ok {
		t.Errorf("正确割接应判通过，note=%s", note)
	}
	if ok, _ := VerifyCutoverTarget(rows, "shop-product", "shop-product_v2"); ok {
		t.Errorf("指向错误目标却判通过")
	}
	if ok, note := VerifyCutoverTarget(nil, "shop-product", "shop-product_v1"); ok {
		t.Errorf("别名不存在应判失败，note=%s", note)
	}

	// 两个索引同时持有别名 = 割接没摘干净
	multi := []AliasTarget{
		{Alias: "shop-product", Index: "shop-product", WriteIndex: "false"},
		{Alias: "shop-product", Index: "shop-product_v1", WriteIndex: "true"},
	}
	if ok, note := VerifyCutoverTarget(multi, "shop-product", "shop-product_v1"); ok || !strings.Contains(note, "指向 2 个索引") {
		t.Errorf("多索引指向必须失败并说明，ok=%v note=%s", ok, note)
	}

	// is_write_index 显式 false => 不可写，必须失败
	notWritable := []AliasTarget{{Alias: "shop-product", Index: "shop-product_v1", WriteIndex: "false"}}
	if ok, note := VerifyCutoverTarget(notWritable, "shop-product", "shop-product_v1"); ok {
		t.Errorf("显式非写索引却判通过，note=%s", note)
	}

	// 单索引 + "-"（未显式设置）=> 隐式写索引，算通过但必须说明
	implicit := []AliasTarget{{Alias: "shop-product", Index: "shop-product_v1", WriteIndex: "-"}}
	if ok, note := VerifyCutoverTarget(implicit, "shop-product", "shop-product_v1"); !ok || !strings.Contains(note, "隐式") {
		t.Errorf("隐式写索引应通过并标注，ok=%v note=%s", ok, note)
	}
}

// TestResolveWriteIndex 覆盖 es:status 用来定位写索引的纯函数（写错索引 = 对账数字全错）。
func TestResolveWriteIndex(t *testing.T) {
	tests := []struct {
		name    string
		rows    []AliasTarget
		want    string
		wantOK  bool
		wantHas string // note 必须包含的关键信息
	}{
		{
			name:   "割接前：别名位是物理索引，_cat/aliases 里根本没这条",
			rows:   []AliasTarget{{Alias: "other", Index: "other_v1", WriteIndex: "-"}},
			wantOK: false,
		},
		{
			name:    "单索引显式 true",
			rows:    []AliasTarget{{Alias: "shop-product", Index: "shop-product_v1", WriteIndex: "true"}},
			want:    "shop-product_v1",
			wantOK:  true,
			wantHas: "显式",
		},
		{
			name:    "单索引隐式 -",
			rows:    []AliasTarget{{Alias: "shop-product", Index: "shop-product_v1", WriteIndex: "-"}},
			want:    "shop-product_v1",
			wantOK:  true,
			wantHas: "隐式",
		},
		{
			name:   "单索引显式 false 不可写",
			rows:   []AliasTarget{{Alias: "shop-product", Index: "shop-product_v1", WriteIndex: "false"}},
			wantOK: false,
		},
		{
			name: "多索引取 true 那一个",
			rows: []AliasTarget{
				{Alias: "shop-product", Index: "shop-product_v1", WriteIndex: "false"},
				{Alias: "shop-product", Index: "shop-product_v2", WriteIndex: "true"},
			},
			want:    "shop-product_v2",
			wantOK:  true,
			wantHas: "2 个索引",
		},
		{
			name: "多索引无 true 则无法判定，不猜",
			rows: []AliasTarget{
				{Alias: "shop-product", Index: "shop-product_v1", WriteIndex: "-"},
				{Alias: "shop-product", Index: "shop-product_v2", WriteIndex: "-"},
			},
			wantOK: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, note, ok := ResolveWriteIndex(tt.rows, "shop-product")
			if ok != tt.wantOK || got != tt.want {
				t.Fatalf("ResolveWriteIndex() = (%q, ok=%v)，期望 (%q, ok=%v)，note=%s", got, ok, tt.want, tt.wantOK, note)
			}
			if tt.wantHas != "" && !strings.Contains(note, tt.wantHas) {
				t.Errorf("note 应包含 %q，got=%s", tt.wantHas, note)
			}
			if !ok && note == "" {
				t.Errorf("判定失败时必须给出原因")
			}
		})
	}
}

func TestReconcileCounts(t *testing.T) {
	tests := []struct {
		name       string
		db, es, tp int64
		wantDiff   int64
		wantOK     bool
	}{
		{"完全一致", 100, 100, 0, 0, true},
		{"容差内", 100, 98, 2, -2, true},
		{"超容差", 100, 90, 2, -10, false},
		{"ES 多出也要中止", 100, 105, 0, 5, false},
		{"负容差=永不可达标（差 1）", 100, 101, -1, 1, false},
		// 必修④的破坏即红用例：旧实现把负值归 0 当「严格相等」，这里会误判达标；
		// 统一后的口径是「负值永不可达标」，且 --force 分支必须可达。
		{"负容差即使完全一致也不达标", 100, 100, -1, 0, false},
		{"空表对空索引", 0, 0, 0, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			diff, ok := ReconcileCounts(tt.db, tt.es, tt.tp)
			if diff != tt.wantDiff || ok != tt.wantOK {
				t.Errorf("got (%d,%v), want (%d,%v)", diff, ok, tt.wantDiff, tt.wantOK)
			}
		})
	}
}

func TestResolveSchemaVersion(t *testing.T) {
	metaNum := map[string]any{"_meta": map[string]any{"schema_version": json.Number("7")}}
	if v, src := ResolveSchemaVersion(metaNum, "shop-product_v3"); v != 7 || src != "mappings._meta" {
		t.Errorf("_meta 必须优先于名称尾缀，got (%d,%s)", v, src)
	}
	metaFloat := map[string]any{"_meta": map[string]any{"schema_version": float64(4)}}
	if v, src := ResolveSchemaVersion(metaFloat, "whatever"); v != 4 || src != "mappings._meta" {
		t.Errorf("float64 形态的 _meta 也应识别，got (%d,%s)", v, src)
	}
	if v, src := ResolveSchemaVersion(map[string]any{}, "shop-product_v12"); v != 12 || src != "index_name_suffix" {
		t.Errorf("无 _meta 应回退尾缀，got (%d,%s)", v, src)
	}
	if v, src := ResolveSchemaVersion(nil, "shop-product_2"); v != 2 || src != "index_name_suffix" {
		t.Errorf("无 v 前缀的尾缀也要兼容，got (%d,%s)", v, src)
	}
	if v, src := ResolveSchemaVersion(nil, "shop-product"); v != 0 || src != "unknown" {
		t.Errorf("两者都无时应 unknown 而不是猜，got (%d,%s)", v, src)
	}
	// _meta 存在但版本号非法 => 仍要回退
	badMeta := map[string]any{"_meta": map[string]any{"schema_version": json.Number("0")}}
	if v, src := ResolveSchemaVersion(badMeta, "shop-product_v1"); v != 1 || src != "index_name_suffix" {
		t.Errorf("非法 _meta 必须回退，got (%d,%s)", v, src)
	}
}

func TestIngestRestoreBody(t *testing.T) {
	declared := map[string]any{
		"refresh_interval": "30s",
		"index":            map[string]any{"number_of_replicas": json.Number("2")},
	}
	got := IngestRestoreBody(declared)
	if !strings.Contains(got, `"refresh_interval":"30s"`) || !strings.Contains(got, `"number_of_replicas":2`) {
		t.Errorf("应从声明取回动态项，got=%s", got)
	}
	// 线上扁平 dotted 回读形态同样要取到
	flat := map[string]any{"index.refresh_interval": "5s", "index.number_of_replicas": json.Number("0")}
	if got := IngestRestoreBody(flat); !strings.Contains(got, `"refresh_interval":"5s"`) {
		t.Errorf("扁平形态取值失败，got=%s", got)
	}
	// 缺失声明值 => 回落 1s / 1（与 product.jsonc 一致）
	if got := IngestRestoreBody(nil); got != `{"index":{"number_of_replicas":1,"refresh_interval":"1s"}}` {
		t.Errorf("缺省回落错误，got=%s", got)
	}
	// 提速体必须同时关 refresh 与副本
	if tune := IngestTuneBody(); !strings.Contains(tune, `"refresh_interval":"-1"`) || !strings.Contains(tune, `"number_of_replicas":0`) {
		t.Errorf("提速 settings 不完整：%s", tune)
	}
}

func TestFormatOrphanAdvisory(t *testing.T) {
	if got := FormatOrphanAdvisory("shop-product", nil); got != "" {
		t.Errorf("无孤儿应返回空串，got=%q", got)
	}
	got := FormatOrphanAdvisory("shop-product", []string{"shop-product_v1"})
	for _, must := range []string{"shop-product_v1", "不会自动删除", "回滚只能重跑", "手工删除"} {
		if !strings.Contains(got, must) {
			t.Errorf("孤儿建议缺少关键信息 %q：\n%s", must, got)
		}
	}
	// 不得出现 rebuild 语义的错误承诺
	if strings.Contains(got, "保留旧索引回滚") || strings.Contains(got, "可直接切回旧索引") {
		t.Errorf("文案作出了错误承诺：\n%s", got)
	}
}

func TestIsReadOnlyMethod(t *testing.T) {
	for _, m := range []string{"GET", "get", " HEAD "} {
		if !IsReadOnlyMethod(m) {
			t.Errorf("%q 应属只读白名单", m)
		}
	}
	for _, m := range []string{"POST", "PUT", "DELETE", ""} {
		if IsReadOnlyMethod(m) {
			t.Errorf("%q 不该被当成只读", m)
		}
	}
}

func TestProductDocCountRequiresDB(t *testing.T) {
	// 单元测试进程未初始化 MySQL：必须返回带指引的 error 而不是 panic
	if _, err := ProductDocCount(context.Background()); err == nil {
		t.Skip("当前进程 MySQL 已初始化，跳过无 DB 断言")
	} else if !strings.Contains(err.Error(), "MySQL") {
		t.Errorf("错误信息应指向 MySQL 未初始化，got=%v", err)
	}
}

func TestCutoverVerdictString(t *testing.T) {
	tests := []struct {
		v    CutoverVerdict
		want string
	}{
		{VerdictProceed, "proceed"},
		{VerdictAlreadyCutover, "already_alias"},
		{VerdictTargetOccupied, "target_occupied"},
		{VerdictMissingSource, "source_missing"},
		{CutoverVerdict(99), "proceed"},
	}
	for _, tt := range tests {
		if got := tt.v.String(); got != tt.want {
			t.Errorf("%d.String()=%s, want %s", tt.v, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// 编排
// ---------------------------------------------------------------------------

func baseCutoverOpts(f *fakeCluster, def *IndexDefinition) CutoverOptions {
	f.plugins = []string{"analysis-ik", "analysis-pinyin"}
	f.indexes["shop-product"] = true // 割接前提：别名位仍是物理索引
	return CutoverOptions{
		Alias:      def.Alias(),
		Definition: def,
		Cluster:    f,
		Sync:       cutoverSync(SyncSummary{Scanned: 100, Sent: 100}, nil),
		Compensate: noopCompensate,
		DBCount:    fixedCount(100),
		Logf:       silentLogf,
	}
}

func TestRunCutover_HappyPath(t *testing.T) {
	def := mustCutoverDef(t, "shop-product", 1, "analysis-ik", "analysis-pinyin")
	f := newFakeCluster()
	opts := baseCutoverOpts(f, def)
	opts.TuneIngest = true
	f.counts["shop-product_v1"] = 100

	res, err := RunCutover(context.Background(), opts)
	if err != nil {
		t.Fatalf("割接应成功: %v", err)
	}
	if res.Verdict != "proceed" || !res.Created || !res.Switched || !res.Verified {
		t.Fatalf("结果状态不正确: %+v", res)
	}
	if res.Idempotent || res.DryRun {
		t.Errorf("既非幂等也非 dry-run: %+v", res)
	}
	if res.Diff != 0 || !res.Reconciled || !res.Tuned || !res.Restored {
		t.Errorf("对账/提速恢复状态不符: %+v", res)
	}
	if len(res.Orphans) != 0 || res.AbortedStep != "" {
		t.Errorf("成功路径不该留下孤儿，got=%v step=%s", res.Orphans, res.AbortedStep)
	}

	// 建索引请求体必须是声明体（剔除 _meta 与 aliases）
	body := f.createdBodies["shop-product_v1"]
	if strings.Contains(body, "_meta") || strings.Contains(body, `"aliases"`) {
		t.Errorf("create 请求体应剔除 _meta 与 aliases，got=%s", body)
	}

	// 写操作序列：建索引 -> 提速 -> 恢复 -> refresh -> 割接（_aliases 必须是最后一条写）
	wantWrites := []string{
		"PUT /shop-product_v1",
		"PUT /shop-product_v1/_settings",
		"PUT /shop-product_v1/_settings",
		"POST /shop-product_v1/_refresh",
		"POST /_aliases",
	}
	gotWrites := f.opsOfKind("write")
	if strings.Join(gotWrites, ",") != strings.Join(wantWrites, ",") {
		t.Errorf("写操作序列不符:\ngot =%v\nwant=%v", gotWrites, wantWrites)
	}
	// 第一次 settings 是提速（-1/0），第二次是恢复（声明值 1s/1）
	if len(f.putSettings) != 2 || !strings.Contains(f.putSettings[0], `"-1"`) {
		t.Fatalf("提速 settings 不符：%v", f.putSettings)
	}
	if !strings.Contains(f.putSettings[1], `"1s"`) || !strings.Contains(f.putSettings[1], `"number_of_replicas":1`) {
		t.Errorf("恢复 settings 应回到声明值：%v", f.putSettings)
	}

	// 割接后旧物理索引被 remove_index 连数据一起摘除（真机语义）
	if f.indexes["shop-product"] {
		t.Errorf("割接后旧物理索引应已消失")
	}
	if got := f.aliasRows["shop-product"]; len(got) != 1 || got[0].Index != "shop-product_v1" || got[0].WriteIndex != "true" {
		t.Errorf("别名指向/写标记不符：%v", got)
	}
	if f.switchCalls != 1 {
		t.Errorf("_aliases 只能调用一次，实际 %d 次", f.switchCalls)
	}
	// 收尾校验确实读了别名清单
	if !strings.Contains(strings.Join(f.allOps(), ","), "GET /_cat/aliases") {
		t.Errorf("必须校验别名指向：%v", f.allOps())
	}
	if len(res.WriteOps) != len(wantWrites) || len(res.ReadOps) == 0 {
		t.Errorf("审计清单不完整：writes=%v reads=%v", res.WriteOps, res.ReadOps)
	}
}

func TestRunCutover_DryRunProvesZeroWrites(t *testing.T) {
	def := mustCutoverDef(t, "shop-product", 1, "analysis-ik", "analysis-pinyin")
	f := newFakeCluster()
	opts := baseCutoverOpts(f, def)
	opts.DryRun = true
	opts.TuneIngest = true
	compensated := false
	opts.Compensate = func(context.Context, string, time.Time) (CompensateSummary, error) {
		compensated = true
		return CompensateSummary{}, nil
	}

	res, err := RunCutover(context.Background(), opts)
	if err != nil {
		t.Fatalf("dry-run 不应报错: %v", err)
	}
	// 补偿也是写原语：预演模式下一条都不会发（含差量补偿）。
	if compensated {
		t.Errorf("dry-run 绝不该调用差量补偿回调")
	}
	if !res.DryRun {
		t.Errorf("结果应标记 dry_run")
	}
	// 硬证据：fake 里既没有新建索引也没有别名变更，审计层写清单为空
	if got := f.opsOfKind("write"); len(got) != 0 || len(res.WriteOps) != 0 {
		t.Errorf("dry-run 出现写操作: fake=%v audit=%v", got, res.WriteOps)
	}
	if _, ok := f.createdBodies["shop-product_v1"]; ok {
		t.Errorf("dry-run 竟创建了目标索引")
	}
	if len(f.aliasRows) != 0 {
		t.Errorf("dry-run 竟改动了别名：%v", f.aliasRows)
	}
	if res.PlannedAliasesBody == "" || res.PlannedCreateBody == "" {
		t.Errorf("dry-run 必须打印将要执行的请求体")
	}
	if res.StartedAt.IsZero() {
		t.Errorf("编排起点必须固化（它就是补偿下界，取错会静默漏数据）")
	}
	if res.DBExpected != 100 {
		t.Errorf("dry-run 仍要给出对账期望值，got=%d", res.DBExpected)
	}
	// 只读调用应覆盖：插件探测 + HEAD 别名位 + GET /_alias + HEAD 目标名
	reads := strings.Join(f.allOps(), ",")
	for _, want := range []string{"GET /_cat/plugins", "HEAD /shop-product", "GET /_alias/shop-product", "HEAD /shop-product_v1"} {
		if !strings.Contains(reads, want) {
			t.Errorf("dry-run 缺少只读调用 %s：%v", want, f.allOps())
		}
	}
}

func TestDryRunAuditRejectsEveryWritePrimitive(t *testing.T) {
	// 审计层的硬拦截：编排逻辑一旦误调写接口，立刻失败，而不是静默执行
	a := newAuditedCluster(newFakeCluster(), true)
	writes := map[string]func() error{
		"CreateIndex": func() error { return a.CreateIndex(context.Background(), "x", "{}") },
		"PutSettings": func() error { return a.PutSettings(context.Background(), "x", "{}") },
		"Refresh":     func() error { return a.Refresh(context.Background(), "x") },
		"SwitchAlias": func() error { return a.SwitchAlias(context.Background(), "a", "b") },
		"PerformPUT":  func() error { _, err := a.Perform(context.Background(), "PUT", "/x", "{}"); return err },
		"PerformPOST": func() error { _, err := a.Perform(context.Background(), "POST", "/_aliases", "{}"); return err },
	}
	for name, fn := range writes {
		if err := fn(); err == nil {
			t.Errorf("dry-run 下 %s 必须被拒绝", name)
		} else if !strings.Contains(err.Error(), "dry-run") {
			t.Errorf("%s 的拒绝理由应写明 dry-run：%v", name, err)
		}
	}
	if len(a.WriteOps()) != 0 {
		t.Errorf("被拒绝的写不该计入写清单：%v", a.WriteOps())
	}
	if _, err := a.Perform(context.Background(), "GET", "/x", ""); err != nil {
		t.Errorf("dry-run 下 GET 应放行：%v", err)
	}
	if _, err := a.Count(context.Background(), "x"); err != nil {
		t.Errorf("dry-run 下 _count 应放行：%v", err)
	}
	if len(a.ReadOps()) == 0 {
		t.Errorf("只读调用应被记录")
	}

	// 非 dry-run 时同一批写必须放行并登记
	b := newAuditedCluster(newFakeCluster(), false)
	if err := b.CreateIndex(context.Background(), "x", "{}"); err != nil {
		t.Fatalf("非 dry-run 不应拦截: %v", err)
	}
	if len(b.WriteOps()) != 1 {
		t.Errorf("写清单=%v", b.WriteOps())
	}
}

func TestRunCutover_AlreadyAliasIsIdempotent(t *testing.T) {
	def := mustCutoverDef(t, "shop-product", 1, "analysis-ik", "analysis-pinyin")
	f := newFakeCluster()
	f.plugins = def.RequiresPlugins()
	f.aliasRows["shop-product"] = []AliasTarget{{Alias: "shop-product", Index: "shop-product_v1", WriteIndex: "true"}}
	f.indexes["shop-product_v1"] = true

	res, err := RunCutover(context.Background(), CutoverOptions{
		Alias: "shop-product", Definition: def, Cluster: f,
		Sync: cutoverSync(SyncSummary{}, nil), DBCount: fixedCount(100), Logf: silentLogf,
	})
	if err != nil {
		t.Fatalf("已是别名必须退出 0，got err=%v", err)
	}
	if !res.Idempotent || res.Verdict != "already_alias" {
		t.Errorf("应标记幂等：%+v", res)
	}
	if len(f.opsOfKind("write")) != 0 {
		t.Errorf("幂等路径不得有任何写操作：%v", f.opsOfKind("write"))
	}
}

func TestRunCutover_TargetOccupiedRefusesOverwrite(t *testing.T) {
	def := mustCutoverDef(t, "shop-product", 1, "analysis-ik", "analysis-pinyin")
	f := newFakeCluster()
	f.plugins = def.RequiresPlugins()
	f.indexes["shop-product"] = true
	f.indexes["shop-product_v1"] = true

	res, err := RunCutover(context.Background(), CutoverOptions{
		Alias: "shop-product", Definition: def, Cluster: f,
		Sync: cutoverSync(SyncSummary{}, nil), DBCount: fixedCount(10), Logf: silentLogf,
	})
	if err == nil {
		t.Fatalf("目标已存在必须报错")
	}
	if res.Verdict != "target_occupied" || res.AbortedStep != "precheck_target" {
		t.Errorf("判定/中止步骤不符：%+v", res)
	}
	if len(f.opsOfKind("write")) != 0 {
		t.Errorf("拒绝覆盖时不得有写操作：%v", f.opsOfKind("write"))
	}
	if !strings.Contains(err.Error(), "拒绝覆盖") {
		t.Errorf("错误信息应说明拒绝覆盖：%v", err)
	}
}

func TestRunCutover_MissingPluginAborts(t *testing.T) {
	def := mustCutoverDef(t, "shop-product", 1, "analysis-ik", "analysis-pinyin")
	f := newFakeCluster()
	f.plugins = []string{"analysis-ik"} // pinyin 缺失
	f.indexes["shop-product"] = true

	res, err := RunCutover(context.Background(), CutoverOptions{
		Alias: "shop-product", Definition: def, Cluster: f,
		Sync: cutoverSync(SyncSummary{}, nil), DBCount: fixedCount(10), Logf: silentLogf,
	})
	if err == nil || res.AbortedStep != "precheck_plugins" {
		t.Fatalf("插件缺失必须在前置校验中止：%+v err=%v", res, err)
	}
	if strings.Join(res.PluginsMissing, ",") != "analysis-pinyin" {
		t.Errorf("缺失清单应点名 pinyin：%+v", res.PluginsMissing)
	}
	if len(f.opsOfKind("write")) != 0 {
		t.Errorf("插件缺失不得写入：%v", f.opsOfKind("write"))
	}
}

func TestRunCutover_ReconcileFailStopsBeforeAliasSwitch(t *testing.T) {
	def := mustCutoverDef(t, "shop-product", 1, "analysis-ik", "analysis-pinyin")
	f := newFakeCluster()
	opts := baseCutoverOpts(f, def)
	opts.Tolerance = 2
	opts.TuneIngest = true           // 开了提速，任何中止路径都必须把 settings 恢复回去
	f.counts["shop-product_v1"] = 90 // 期望 100，差 10 > 容差

	res, err := RunCutover(context.Background(), opts)
	if err == nil {
		t.Fatalf("对账不达标必须报错")
	}
	if res.Switched || f.switchCalls != 0 {
		t.Errorf("对账失败却已切别名")
	}
	for _, op := range f.opsOfKind("write") {
		if op == "POST /_aliases" {
			t.Fatalf("对账不达标仍下发了 _aliases：%v", f.opsOfKind("write"))
		}
	}
	if res.AbortedStep != "reconcile" || res.Reconciled {
		t.Errorf("中止步骤/对账标记不符：%+v", res)
	}
	if res.Diff != -10 {
		t.Errorf("差值应如实报告，got=%d", res.Diff)
	}
	if len(res.Orphans) != 1 || res.Orphans[0] != "shop-product_v1" {
		t.Errorf("应列出孤儿索引：%v", res.Orphans)
	}
	// 线上别名位必须完好无损：既没被摘也没被挂
	if !f.indexes["shop-product"] || len(f.aliasRows) != 0 {
		t.Errorf("中止不得影响线上别名位：indexes=%v aliases=%v", f.indexes, f.aliasRows)
	}
	if !res.Restored {
		t.Errorf("中止时也必须恢复 settings")
	}
}

func TestRunCutover_SyncFailureStillRestoresSettings(t *testing.T) {
	def := mustCutoverDef(t, "shop-product", 1, "analysis-ik", "analysis-pinyin")
	f := newFakeCluster()
	opts := baseCutoverOpts(f, def)
	opts.TuneIngest = true
	opts.Sync = cutoverSync(SyncSummary{Sent: 3}, errors.New("boom"))

	res, err := RunCutover(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("灌数失败应上抛：%v", err)
	}
	if res.AbortedStep != "sync" || !res.Restored || res.Switched {
		t.Errorf("灌数失败必须恢复 settings 且不切别名：%+v", res)
	}
	if len(res.Orphans) != 1 {
		t.Errorf("应记录孤儿索引：%v", res.Orphans)
	}
	if strings.Join(f.opsOfKind("write"), ",") != "PUT /shop-product_v1,PUT /shop-product_v1/_settings,PUT /shop-product_v1/_settings" {
		t.Errorf("失败路径的写清单不符：%v", f.opsOfKind("write"))
	}
}

func TestRunCutover_ItemLevelFailuresAbortBeforeSwitch(t *testing.T) {
	def := mustCutoverDef(t, "shop-product", 1, "analysis-ik", "analysis-pinyin")
	f := newFakeCluster()
	opts := baseCutoverOpts(f, def)
	opts.Sync = cutoverSync(SyncSummary{Sent: 99, Failed: 1}, nil)
	f.counts["shop-product_v1"] = 100 // 即便 _count 恰好对齐，也要因 item 失败中止

	res, err := RunCutover(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "item 级失败") {
		t.Fatalf("存在 item 失败必须中止：%v", err)
	}
	if res.Switched || res.AbortedStep != "sync_failures" {
		t.Errorf("不应进入切别名：%+v", res)
	}
}

// TestRunCutover_RestoreUsesDetachedContext 覆盖 --timeout 场景：
// 主 ctx 在灌数一刻到期，恢复 settings 仍必须用独立 ctx 成功下发，
// 且此时绝不切别名。
func TestRunCutover_RestoreUsesDetachedContext(t *testing.T) {
	def := mustCutoverDef(t, "shop-product", 1, "analysis-ik", "analysis-pinyin")
	f := newFakeCluster()
	f.honorCtx = true
	opts := baseCutoverOpts(f, def)
	opts.TuneIngest = true
	f.counts["shop-product_v1"] = 100

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// 灌数完成的同一刻超时到：主 ctx 立即取消
	opts.Sync = func(ctx context.Context, target string) (SyncSummary, error) {
		cancel()
		return SyncSummary{Target: target, Sent: 100}, nil
	}

	res, err := RunCutover(ctx, opts)
	if err == nil {
		t.Fatalf("主 ctx 已取消，后续读请求应失败并中止：%+v", res)
	}
	if res.Switched {
		t.Errorf("超时后绝不能切别名")
	}
	if !res.Restored {
		t.Fatalf("收尾恢复必须成功：%+v", res)
	}
	// 最后一次 settings 调用（恢复）发生时主 ctx 已取消，它用的 ctx 必须仍可用
	restore, ok := f.lastCallOf("PUT /shop-product_v1/_settings")
	if !ok {
		t.Fatalf("未记录到 settings 调用")
	}
	if restore.ctx == ctx {
		t.Errorf("收尾不得复用主 ctx")
	}
	if restore.ctxErr != nil {
		t.Errorf("收尾调用发生一刻其 ctx 不应已取消：%v", restore.ctxErr)
	}
	if !restore.hasDeadline {
		t.Errorf("收尾 ctx 应带独立超时")
	}
	if ctx.Err() == nil {
		t.Errorf("测试前提失效：主 ctx 应已取消")
	}
}

// TestRunCutover_VerifyUsesDetachedContext 让超时正好落在割接成功那一刻：
// 校验别名的收尾读必须用独立 ctx 跑完，不能把已经成功的割接误报成失败。
func TestRunCutover_VerifyUsesDetachedContext(t *testing.T) {
	def := mustCutoverDef(t, "shop-product", 1, "analysis-ik", "analysis-pinyin")
	f := newFakeCluster()
	f.honorCtx = true
	opts := baseCutoverOpts(f, def)
	f.counts["shop-product_v1"] = 100

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// 主 ctx 在 POST /_aliases 生效之后立即取消（模拟 --timeout 卡在切完之后）
	f.cancelOnSwitch = cancel

	res, err := RunCutover(ctx, opts)
	if err != nil {
		t.Fatalf("割接已生效，校验用独立 ctx 应跑完：%v", err)
	}
	if !res.Switched || !res.Verified {
		t.Fatalf("结果不符：%+v", res)
	}
	if f.switchCalls != 1 {
		t.Errorf("割接必须且只能下发一次 POST /_aliases，got=%d", f.switchCalls)
	}
	verify, ok := f.lastCallOf("GET /_cat/aliases")
	if !ok {
		t.Fatalf("未记录到别名校验调用")
	}
	if verify.ctx == ctx {
		t.Errorf("校验不得复用主 ctx")
	}
	if verify.ctxErr != nil {
		t.Errorf("校验调用发生一刻其 ctx 不应已取消：%v", verify.ctxErr)
	}
	if !verify.hasDeadline {
		t.Errorf("校验 ctx 应带独立超时")
	}
	if ctx.Err() == nil {
		t.Errorf("测试前提失效：主 ctx 应已取消")
	}
}

func TestRunCutover_VerifyFailureIsReported(t *testing.T) {
	def := mustCutoverDef(t, "shop-product", 1, "analysis-ik", "analysis-pinyin")
	f := newFakeCluster()
	opts := baseCutoverOpts(f, def)
	f.counts["shop-product_v1"] = 100
	// 割接请求「成功」但别名指向被外部改成 v9：校验必须兜住这种不一致
	opts.Cluster = misdirectingCluster{inner: f}

	res, err := RunCutover(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "校验未通过") {
		t.Fatalf("指向异常必须报错：%v", err)
	}
	if !res.Switched || res.Verified || res.AbortedStep != "verify" {
		t.Errorf("应标记已切但未通过校验：%+v", res)
	}
}

// misdirectingCluster 只覆写 SwitchAlias：请求照常成功，但别名指向落到了别的索引，
// 用来构造「集群侧被并发干预」的校验失败场景。
type misdirectingCluster struct {
	inner *fakeCluster
}

func (m misdirectingCluster) IndexExists(ctx context.Context, idx string) (bool, error) {
	return m.inner.IndexExists(ctx, idx)
}
func (m misdirectingCluster) AliasTargets(ctx context.Context, alias string) ([]string, error) {
	return m.inner.AliasTargets(ctx, alias)
}
func (m misdirectingCluster) ListAliasRows(ctx context.Context) ([]AliasTarget, error) {
	return m.inner.ListAliasRows(ctx)
}
func (m misdirectingCluster) ListPlugins(ctx context.Context) ([]string, error) {
	return m.inner.ListPlugins(ctx)
}
func (m misdirectingCluster) IndexStats(ctx context.Context, idx string) ([]IndexStat, error) {
	return m.inner.IndexStats(ctx, idx)
}
func (m misdirectingCluster) Count(ctx context.Context, idx string) (int64, error) {
	return m.inner.Count(ctx, idx)
}
func (m misdirectingCluster) GetMapping(ctx context.Context, idx string) (map[string]any, error) {
	return m.inner.GetMapping(ctx, idx)
}
func (m misdirectingCluster) GetSettings(ctx context.Context, idx string) (map[string]any, error) {
	return m.inner.GetSettings(ctx, idx)
}
func (m misdirectingCluster) CreateIndex(ctx context.Context, idx, body string) error {
	return m.inner.CreateIndex(ctx, idx, body)
}
func (m misdirectingCluster) PutSettings(ctx context.Context, idx, body string) error {
	return m.inner.PutSettings(ctx, idx, body)
}
func (m misdirectingCluster) Refresh(ctx context.Context, idx string) error {
	return m.inner.Refresh(ctx, idx)
}
func (m misdirectingCluster) Perform(ctx context.Context, method, path, body string) (string, error) {
	return m.inner.Perform(ctx, method, path, body)
}
func (m misdirectingCluster) SwitchAlias(ctx context.Context, alias, target string) error {
	m.inner.mu.Lock()
	defer m.inner.mu.Unlock()
	m.inner.switchCalls++
	delete(m.inner.indexes, alias)
	m.inner.aliasRows[alias] = []AliasTarget{{Alias: alias, Index: alias + "_v9", WriteIndex: "true"}}
	return nil
}

func TestRunCutover_InputValidation(t *testing.T) {
	def := mustCutoverDef(t, "shop-product", 1, "analysis-ik", "analysis-pinyin")
	f := newFakeCluster()

	tests := []struct {
		name   string
		opts   CutoverOptions
		wantIn string
	}{
		{"缺声明", CutoverOptions{Alias: "a", Cluster: f}, "Definition"},
		{"缺集群", CutoverOptions{Alias: "a", Definition: def}, "Cluster"},
		{"别名空", CutoverOptions{Definition: def, Cluster: f}, "别名"},
		{"别名与声明不符", CutoverOptions{Alias: "other", Definition: def, Cluster: f}, "不一致"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.opts.Sync = cutoverSync(SyncSummary{}, nil)
			tt.opts.Logf = silentLogf
			_, err := RunCutover(context.Background(), tt.opts)
			if err == nil || !strings.Contains(err.Error(), tt.wantIn) {
				t.Errorf("got %v, want 含 %q", err, tt.wantIn)
			}
		})
	}

	t.Run("未接线的 draft 声明拒绝割接", func(t *testing.T) {
		draft := mustCutoverDef(t, "shop-product", 1, "analysis-ik", "analysis-pinyin")
		draft.status = StatusDraft
		_, err := RunCutover(context.Background(), CutoverOptions{
			Alias: "shop-product", Definition: draft, Cluster: f, Logf: silentLogf,
		})
		if err == nil || !strings.Contains(err.Error(), "未接线") {
			t.Errorf("draft 声明必须拒绝割接，got=%v", err)
		}
	})

	t.Run("未注入灌数回调", func(t *testing.T) {
		f2 := newFakeCluster()
		opts := baseCutoverOpts(f2, def)
		opts.Sync = nil
		res, err := RunCutover(context.Background(), opts)
		if err == nil || !strings.Contains(err.Error(), "SyncFunc") {
			t.Fatalf("缺回调必须报错：%v", err)
		}
		if res.Switched || len(res.Orphans) != 1 {
			t.Errorf("缺回调时不得切别名并应记录孤儿：%+v", res)
		}
	})
}

func TestRunCutover_TimeoutPropagatesToSync(t *testing.T) {
	// 割接层不自己掐表：--timeout 通过 ctx 传给灌数回调，由同步层收敛
	def := mustCutoverDef(t, "shop-product", 1, "analysis-ik", "analysis-pinyin")
	f := newFakeCluster()
	opts := baseCutoverOpts(f, def)
	opts.TuneIngest = false

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	opts.Sync = func(ctx context.Context, target string) (SyncSummary, error) {
		select {
		case <-ctx.Done():
			return SyncSummary{Target: target}, ctx.Err()
		case <-time.After(500 * time.Millisecond):
			return SyncSummary{Target: target}, nil
		}
	}
	_, err := RunCutover(ctx, opts)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("超时应从 Sync 的 ctx 如实上抛，got=%v", err)
	}
	for _, op := range f.opsOfKind("write") {
		if op == "POST /_aliases" {
			t.Errorf("超时后绝不该切别名：%v", f.opsOfKind("write"))
		}
	}
}

// ---------------------------------------------------------------------------
// 必修①：割接后的差量补偿（remove_index 之后执行，下界＝编排起点 startedAt）
// ---------------------------------------------------------------------------

// TestRunCutover_CompensateRunsAfterRemoveIndexWithStartedAtLowerBound 钉住设计里「窗口内变更
// 靠差量补偿收敛」那一步：旧物理索引被 remove_index 连数据一起摘走之后，灌数窗口内的改/删
// 永久不可回滚，只能靠这一次以 startedAt 为下界、以新 write index 为目标的扫描收敛。
// 删掉编排里的补偿调用 → ran=false 红；把补偿提到切换之前 → 取样时旧索引还在、别名还指着
// 物理索引名 → 红；把下界改成切换时刻 time.Now() → since 晚于灌数开始 → 红。
func TestRunCutover_CompensateRunsAfterRemoveIndexWithStartedAtLowerBound(t *testing.T) {
	def := mustCutoverDef(t, "shop-product", 1, "analysis-ik", "analysis-pinyin")
	f := newFakeCluster()
	opts := baseCutoverOpts(f, def)
	f.counts["shop-product_v1"] = 100

	var (
		ran           bool
		targetAtComp  string
		aliasAtComp   string
		oldGoneAtComp bool
		sinceAtComp   time.Time
		nowAtComp     time.Time
		syncStartedAt time.Time
	)
	opts.Sync = func(ctx context.Context, target string) (SyncSummary, error) {
		syncStartedAt = time.Now()
		return SyncSummary{Target: target, Scanned: 100, Sent: 100}, nil
	}
	opts.Compensate = func(_ context.Context, target string, since time.Time) (CompensateSummary, error) {
		ran = true
		targetAtComp, sinceAtComp, nowAtComp = target, since, time.Now()
		aliasAtComp, oldGoneAtComp = f.snapshotAlias("shop-product")
		return CompensateSummary{Scanned: 2, Indexed: 1, Deleted: 1}, nil
	}

	res, err := RunCutover(context.Background(), opts)
	if err != nil {
		t.Fatalf("割接应成功: %v", err)
	}
	if !ran {
		t.Fatal("割接后必须执行一次差量补偿（旧索引已被 remove_index，窗口增量除了它就没人补）")
	}
	if !oldGoneAtComp {
		t.Errorf("补偿必须发生在 remove_index 生效之后，调用时旧物理索引仍存在")
	}
	if aliasAtComp != "shop-product_v1" {
		t.Errorf("补偿调用时别名应已指向新 write index，取样=%q", aliasAtComp)
	}
	if targetAtComp != "shop-product_v1" {
		t.Errorf("补偿目标必须是新的 write index，got=%q", targetAtComp)
	}
	if !sinceAtComp.Equal(res.StartedAt) {
		t.Errorf("补偿下界必须是编排固化的 startedAt，got=%v want=%v", sinceAtComp, res.StartedAt)
	}
	// 下界取成切换时刻的缺陷就集中在这一行：窗口（灌数开始~切换完成）内的变更会被静默漏掉。
	if sinceAtComp.After(syncStartedAt) {
		t.Errorf("下界不得晚于灌数开始时刻（用切换时刻就会漏窗口内变更）：since=%v syncStart=%v", sinceAtComp, syncStartedAt)
	}
	if sinceAtComp.After(nowAtComp) {
		t.Errorf("下界不可能晚于补偿执行时刻：%v vs %v", sinceAtComp, nowAtComp)
	}
	if !res.Compensated || res.Compensate.Scanned != 2 || res.Compensate.Deleted != 1 {
		t.Errorf("补偿结果必须进报表，got compensated=%v stats=%+v", res.Compensated, res.Compensate)
	}
}

// TestRunCutover_MissingCompensateAbortsAfterSwitch：未注入补偿回调时不能当成功退出 ——
// 窗口增量没人收敛，且旧索引已消失，只能点名人工指令。
func TestRunCutover_MissingCompensateAbortsAfterSwitch(t *testing.T) {
	def := mustCutoverDef(t, "shop-product", 1, "analysis-ik", "analysis-pinyin")
	f := newFakeCluster()
	opts := baseCutoverOpts(f, def)
	opts.Compensate = nil
	f.counts["shop-product_v1"] = 100

	res, err := RunCutover(context.Background(), opts)
	if err == nil {
		t.Fatal("未注入 CompensateFunc 必须报错（窗口增量未收敛不能当成功）")
	}
	if res.AbortedStep != "compensate" || !res.Switched || res.Compensated {
		t.Errorf("应停在 compensate、别名已切走但未标记补偿完成：%+v", res)
	}
	if !strings.Contains(err.Error(), "shop-product_v1") || !strings.Contains(err.Error(), "update_time") {
		t.Errorf("报错必须点名目标与补偿口径，让人工可接手，got=%v", err)
	}
	if got, _ := f.snapshotAlias("shop-product"); got != "shop-product_v1" {
		t.Errorf("补偿缺失不得回滚割接（本就没有可回滚对象），别名应已指向新索引，got=%q", got)
	}
}

// TestRunCutover_CompensateFailureIsNotSwallowedAsSuccess：补偿阶段的两类缺陷都必须如实上报
// —— error（没跑完）与 Stats.Failed>0（跑完但有文档没写进去），旧版本只取 error 会把后者吞掉。
func TestRunCutover_CompensateFailureIsNotSwallowedAsSuccess(t *testing.T) {
	t.Run("补偿报错", func(t *testing.T) {
		def := mustCutoverDef(t, "shop-product", 1, "analysis-ik", "analysis-pinyin")
		f := newFakeCluster()
		opts := baseCutoverOpts(f, def)
		f.counts["shop-product_v1"] = 100
		opts.Compensate = func(context.Context, string, time.Time) (CompensateSummary, error) {
			return CompensateSummary{Scanned: 3, Indexed: 1}, errors.New("bulk boom")
		}

		res, err := RunCutover(context.Background(), opts)
		if err == nil || !strings.Contains(err.Error(), "bulk boom") {
			t.Fatalf("补偿失败必须上抛：%v", err)
		}
		if res.AbortedStep != "compensate" || !res.Switched || res.Compensated {
			t.Errorf("不得被吞成成功：%+v", res)
		}
		if res.Compensate.Scanned != 3 {
			t.Errorf("部分统计也要如实保留在结果里，got %+v", res.Compensate)
		}
	})

	t.Run("补偿有 item 级失败但无 error", func(t *testing.T) {
		def := mustCutoverDef(t, "shop-product", 1, "analysis-ik", "analysis-pinyin")
		f := newFakeCluster()
		opts := baseCutoverOpts(f, def)
		f.counts["shop-product_v1"] = 100
		opts.Compensate = func(context.Context, string, time.Time) (CompensateSummary, error) {
			return CompensateSummary{Scanned: 5, Indexed: 2, Failed: 3}, nil
		}

		res, err := RunCutover(context.Background(), opts)
		if err == nil || !strings.Contains(err.Error(), "3 条 item 级失败") {
			t.Fatalf("Failed>0 必须如实报错（旧版本把结构类失败整条丢弃）：%v", err)
		}
		if res.Compensated || res.AbortedStep != "compensate_failures" {
			t.Errorf("失败 bulk 绝不能置 Compensated=true：%+v", res)
		}
		if res.Compensate.Failed != 3 {
			t.Errorf("报表里必须看到失败数，got %+v", res.Compensate)
		}
	})
}

// TestRunCutover_NegativeToleranceAbortsAndForceIsReachable 钉住必修③+④在本层的合成：
// 负容差=永不可达标（即使 ES 与 DB 一模一样），不达标默认中止在切别名之前；
// --force 必须可达（旧实现把负值归 0 当严格相等，闸门被静默绕过）。
func TestRunCutover_NegativeToleranceAbortsAndForceIsReachable(t *testing.T) {
	def := mustCutoverDef(t, "shop-product", 1, "analysis-ik", "analysis-pinyin")

	// A：容差 -1，计数完全对齐 —— 仍必须不达标并中止，别名位原封不动。
	fA := newFakeCluster()
	optsA := baseCutoverOpts(fA, def)
	optsA.Tolerance = -1
	fA.counts["shop-product_v1"] = 100
	resA, errA := RunCutover(context.Background(), optsA)
	if errA == nil || resA.AbortedStep != "reconcile" {
		t.Fatalf("负容差必须永不可达标并中止在切别名之前，got step=%q err=%v", resA.AbortedStep, errA)
	}
	if resA.Reconciled || resA.Switched || resA.Tolerance != -1 {
		t.Errorf("应如实记 达标=false、未切换、容差不被归 0：%+v", resA)
	}
	if _, gone := fA.snapshotAlias("shop-product"); !resA.Switched && gone {
		t.Errorf("中止不得碰线上索引位：%+v", resA)
	}
	if !fA.indexes["shop-product"] {
		t.Errorf("对账失败路径旧物理索引必须还在")
	}

	// B：同一现场 + --force 必须真正放行，且补偿照旧执行（force 不能跳过补偿）。
	fB := newFakeCluster()
	optsB := baseCutoverOpts(fB, def)
	optsB.Tolerance = -1
	optsB.Force = true
	fB.counts["shop-product_v1"] = 100
	compensated := false
	optsB.Compensate = func(context.Context, string, time.Time) (CompensateSummary, error) {
		compensated = true
		return CompensateSummary{}, nil
	}
	resB, errB := RunCutover(context.Background(), optsB)
	if errB != nil {
		t.Fatalf("--force 必须能越过不达标继续割接（旧实现里该分支不可达）: %v", errB)
	}
	if !resB.Switched || resB.Reconciled || !resB.Verified {
		t.Errorf("--force 下应已割接并校验通过，但如实记 达标=false，got %+v", resB)
	}
	if !compensated || !resB.Compensated {
		t.Errorf("--force 只作用于对账闸门，绝不能顺带跳过差量补偿：%+v", resB)
	}
	if got, gone := fB.snapshotAlias("shop-product"); got != "shop-product_v1" || !gone {
		t.Errorf("割接后别名应指向新索引且旧物理索引已消失，got=%q 旧索引没了=%v", got, gone)
	}
}

// ---------------------------------------------------------------------------
// AdminCluster.SwitchAlias 的真实下发形态（httptest 假 ES + 注入 global.EsClient）
// ---------------------------------------------------------------------------

// recordedReq 一次被假 ES 收到的请求。
type recordedReq struct {
	method string
	path   string
	body   string
}

// newRecordingESTool 起一台只记录请求的假 ES，恒回 respBody。
// 记录必须加锁：handler 跑在独立 goroutine，-race 下裸 slice 会报数据竞争。
func newRecordingESTool(t *testing.T, respBody string) (*elastic.Client, func() []recordedReq) {
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
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(respBody))
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

// injectSharedESClient 让 AdminCluster 走真实生产路径：admin.Raw() 读的就是 global.EsClient。
// 用 t.Cleanup 归还，否则同包后续用例（以及 -count=2 的第 2 遍）会拿到已 close 的服务器。
func injectSharedESClient(t *testing.T, cli *elastic.Client) {
	t.Helper()
	prev := global.EsClient
	global.EsClient = &sharedes.Client{Client: cli}
	t.Cleanup(func() { global.EsClient = prev })
}

func describeReqs(reqs []recordedReq) string {
	parts := make([]string, 0, len(reqs))
	for _, r := range reqs {
		parts = append(parts, r.method+" "+r.path+" "+r.body)
	}
	return "[" + strings.Join(parts, " | ") + "]"
}

// actionsByKey 把 _aliases 请求体拍成 {"remove_index":{...},"add":{...}}，用于语义比对
// （JSON 字段顺序在 olivere 与手写结构体之间不保证一致，按字节比会误报）。
func actionsByKey(t *testing.T, body string) map[string]map[string]any {
	t.Helper()
	var payload struct {
		Actions []map[string]map[string]any `json:"actions"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("解析 _aliases 请求体失败: %v\nbody=%s", err, body)
	}
	out := make(map[string]map[string]any, len(payload.Actions))
	for _, a := range payload.Actions {
		for kind, spec := range a {
			out[kind] = spec
		}
	}
	return out
}

// TestAdminClusterSwitchAliasSendsSingleAliasesRequest 钉的是生产请求层：割接必须**恰好一次**
// POST /_aliases，且同一 body 里同时含 remove_index 与带 is_write_index 的 add。
//
// 为什么 CutoverAliasBody 的用例不够：它只钉住「计划请求体」这个字符串。把
// AdminCluster.SwitchAlias 拆成「先 POST remove_index，再 POST add」两步，既有全部用例
// 依旧全绿，而真机上第一步之后写别名会拿到 201 并让 ES 自动建出一个同名空索引 ——
// 比注释里说的黑洞更彻底（后续写入全部落进空索引）。
func TestAdminClusterSwitchAliasSendsSingleAliasesRequest(t *testing.T) {
	cli, seen := newRecordingESTool(t, `{"acknowledged":true}`)
	injectSharedESClient(t, cli)

	if err := (AdminCluster{}).SwitchAlias(context.Background(), "shop-product", "shop-product_v1"); err != nil {
		t.Fatalf("SwitchAlias() 报错: %v", err)
	}

	reqs := seen()
	var aliasCalls []recordedReq
	for _, r := range reqs {
		if r.path == "/_aliases" {
			aliasCalls = append(aliasCalls, r)
		}
	}
	// ① 恰为 1 次。整包个数也断言 ==1：多发出一条任何请求都说明下发形态被改过。
	if len(aliasCalls) != 1 {
		t.Fatalf("POST /_aliases 必须恰好 1 次（拆两步会留下写入黑洞），实际 %d 次：%s",
			len(aliasCalls), describeReqs(reqs))
	}
	if len(reqs) != 1 {
		t.Fatalf("割接不该发出第二个请求：%s", describeReqs(reqs))
	}
	if got := aliasCalls[0].method; got != http.MethodPost {
		t.Errorf("_aliases 必须是 POST，got=%s", got)
	}

	// ② 同一 body 同时含 remove_index 与带 is_write_index 的 add
	byKey := actionsByKey(t, aliasCalls[0].body)
	removed, ok := byKey["remove_index"]
	if !ok {
		t.Fatalf("body 缺 remove_index：%s", aliasCalls[0].body)
	}
	added, ok := byKey["add"]
	if !ok {
		t.Fatalf("body 缺 add：%s", aliasCalls[0].body)
	}
	if _, bad := byKey["remove"]; bad {
		t.Errorf("不允许用 alias 级 remove（会把别名整个摘掉）：%s", aliasCalls[0].body)
	}
	if len(byKey) != 2 {
		t.Errorf("只能有 remove_index + add 两个 action，got=%v", aliasCalls[0].body)
	}
	if removed["index"] != "shop-product" {
		t.Errorf("remove_index 该摘旧物理索引，got=%v", removed["index"])
	}
	if added["index"] != "shop-product_v1" || added["alias"] != "shop-product" {
		t.Errorf("add 指向错误：%#v", added)
	}
	if wi, isBool := added["is_write_index"].(bool); !isBool || !wi {
		t.Errorf("add 必须同时带 is_write_index=true，got=%v", added["is_write_index"])
	}

	// ③ 生产下发体与 dry-run 打印的计划体同源同义：给运维看的预览必须就是真会发出去的东西
	want, err := CutoverAliasBody("shop-product", "shop-product_v1")
	if err != nil {
		t.Fatalf("CutoverAliasBody 报错: %v", err)
	}
	if got, exp := byKey, actionsByKey(t, want); !reflect.DeepEqual(got, exp) {
		t.Errorf("实际下发体与 dry-run 计划体不一致\n  实际: %#v\n  计划: %#v", got, exp)
	}
}
