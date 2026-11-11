package sync

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	golangsync "sync"
	"testing"
	"time"

	"shop/internal/es"
	"shop/internal/models"
)

// TestProductScanColumns_ContainsIsShow 验证列裁剪包含 is_show（BuildIndexDoc 资格判定依赖）。
func TestProductScanColumns_ContainsIsShow(t *testing.T) {
	if !strings.Contains(productScanColumns, "is_show") {
		t.Errorf("productScanColumns 缺少 is_show 列（BuildIndexDoc 资格判定依赖该列）")
	}
}

// TestProductScanColumns_NoUpdatedAt 验证不引用 updated_at（项目时间列是 update_time）。
func TestProductScanColumns_NoUpdatedAt(t *testing.T) {
	if strings.Contains(productScanColumns, "updated_at") {
		t.Errorf("productScanColumns 不应包含 updated_at（项目时间列是 update_time）")
	}
}

// TestProductScanColumns_ContainsExpectedColumns 验证包含所有 17 个索引列。
func TestProductScanColumns_ContainsExpectedColumns(t *testing.T) {
	expected := []string{
		"id", "store_name", "store_info", "keyword", "cate_id", "price", "sales",
		"ficti", "is_hot", "is_benefit", "is_best", "is_new", "is_postage", "is_good",
		"description", "create_time", "update_time",
	}
	for _, col := range expected {
		if !strings.Contains(productScanColumns, col) {
			t.Errorf("productScanColumns 缺少列 %s", col)
		}
	}
}

// TestProductAssemble_SkipsIneligible 验证 Assemble 丢弃不应入索引的行（is_show!=1）。
func TestProductAssemble_SkipsIneligible(t *testing.T) {
	def := &es.IndexDefinition{} // 空声明，Assemble 不依赖它
	pp := &productProvider{def: def}

	// 构造一批数据：第一行 is_show=1（应入索引），第二行 is_show=0（应丢弃）
	show1 := int8(1)
	show0 := int8(0)
	batch := productBatch{
		Rows: []models.StoreProduct{
			{BaseModel: models.BaseModel{Id: 1}, IsShow: &show1},
			{BaseModel: models.BaseModel{Id: 2}, IsShow: &show0},
		},
	}

	docs, err := pp.Assemble(batch)
	if err != nil {
		t.Fatalf("Assemble 失败: %v", err)
	}
	// 期望只有一行（is_show=1 的那行）
	if len(docs) != 1 {
		t.Errorf("Assemble 应丢弃 is_show=0 的行，期望 1 个 doc，实际 %d", len(docs))
	}
	if len(docs) > 0 && docs[0].ID != "1" {
		t.Errorf("Assemble 产出的 doc ID 应为 '1'，实际 %q", docs[0].ID)
	}
}

// TestProductAssemble_EmptyBatch 验证空批次不报错。
func TestProductAssemble_EmptyBatch(t *testing.T) {
	def := &es.IndexDefinition{}
	pp := &productProvider{def: def}

	batch := productBatch{Rows: []models.StoreProduct{}}
	docs, err := pp.Assemble(batch)
	if err != nil {
		t.Fatalf("Assemble 空批次失败: %v", err)
	}
	if len(docs) != 0 {
		t.Errorf("空批次应产出 0 个 doc，实际 %d", len(docs))
	}
}

// TestProductIDs 验证 productIDs 辅助函数。
func TestProductIDs(t *testing.T) {
	rows := []models.StoreProduct{
		{BaseModel: models.BaseModel{Id: 10}},
		{BaseModel: models.BaseModel{Id: 20}},
		{BaseModel: models.BaseModel{Id: 30}},
	}
	ids := productIDs(rows)
	if len(ids) != 3 {
		t.Fatalf("期望 3 个 id，实际 %d", len(ids))
	}
	if ids[0] != 10 || ids[1] != 20 || ids[2] != 30 {
		t.Errorf("id 提取错误，期望 [10,20,30]，实际 %v", ids)
	}
}

// TestCheckpoint_Persistence 验证 checkpoint 实际落盘（缺陷 #2 回归测试）。
// 必修①后游标口径：单区间计划 (0,1000] 内的连续右端——区间内 chunk 游标升序，
// 因此 observeRange 推进到的就是本区间已连续读出的最大 id。
func TestCheckpoint_Persistence(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/test.checkpoint.json"
	sink := newCheckpointSink(path, "test-index")
	single := IDRange{Lo: 0, Hi: 1000}
	sink.planRanges([]IDRange{single})

	// 初始无文件
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("初始时 checkpoint 不应存在")
	}

	// observeRange 后应创建文件
	sink.observeRange(single, 100)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Fatalf("observeRange 后 checkpoint 应已创建")
	}

	// 读取并验证内容
	cp, err := loadCheckpoint(path)
	if err != nil {
		t.Fatalf("读取 checkpoint 失败: %v", err)
	}
	if cp.Target != "test-index" {
		t.Errorf("Target 应为 'test-index'，实际 %q", cp.Target)
	}
	if cp.LastID != 100 {
		t.Errorf("LastID 应为 100，实际 %d", cp.LastID)
	}

	// 再次 observeRange 应更新
	sink.observeRange(single, 200)
	cp2, err := loadCheckpoint(path)
	if err != nil {
		t.Fatalf("再次读取 checkpoint 失败: %v", err)
	}
	if cp2.LastID != 200 {
		t.Errorf("更新后 LastID 应为 200，实际 %d", cp2.LastID)
	}

	// 第五轮新增断言 1：resume 读回后必须强制 k=1（单区间单游标）。
	pp := &productProvider{}
	opts := &Options{Concurrency: 8, RangeMultiplier: 8}
	applyResumeState(cp2, pp, opts)
	if opts.Concurrency != 1 || opts.RangeMultiplier != 1 {
		t.Errorf("resume 后应强制 k=1（Concurrency=%d, RangeMultiplier=%d）",
			opts.Concurrency, opts.RangeMultiplier)
	}
	// 第五轮新增断言 2：不跳过未完成区间——minExclusiveID 必须等于最后 observe 的 LastID，
	// 保证 resume 后只扫描 id > 200 的区间，不会跳过 100~200 这段未完成区域。
	if pp.minExclusiveID != 200 {
		t.Errorf("minExclusiveID 应为最后 observe 的 200，实际 %d（未完成区间被跳过）",
			pp.minExclusiveID)
	}
}

// TestCheckpoint_ResumeFields 验证 checkpoint 含 StartedAt 与 Total（缺陷 #6 回归）。
func TestCheckpoint_ResumeFields(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/test.checkpoint.json"
	sink := newCheckpointSink(path, "test-index")
	single := IDRange{Lo: 0, Hi: 1000}
	sink.planRanges([]IDRange{single})
	sink.setTotal(500)
	sink.observeRange(single, 42)

	cp, err := loadCheckpoint(path)
	if err != nil {
		t.Fatalf("读取 checkpoint 失败: %v", err)
	}
	if cp.Total != 500 {
		t.Errorf("Total 应为 500，实际 %d", cp.Total)
	}
	if cp.StartedAt.IsZero() {
		t.Error("StartedAt 不应为零值")
	}
	if time.Since(cp.StartedAt) > 5*time.Second {
		t.Errorf("StartedAt 应接近当前时间，实际 %v", cp.StartedAt)
	}
	if cp.LastID != 42 {
		t.Errorf("LastID 应为 42，实际 %d", cp.LastID)
	}
}

// TestCheckpoint_ResumePreservesStartedAtAndTotal 验证 resume 时 restoreCheckpointSink
// 原样保留已加载的 StartedAt 与 Total（第四轮缺陷 #3 回归）。
func TestCheckpoint_ResumePreservesStartedAtAndTotal(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/test.checkpoint.json"

	// 模拟首次启动写入的 checkpoint：StartedAt 在 1 小时前，Total=1000
	originalStarted := time.Now().Add(-1 * time.Hour)
	cp1 := checkpoint{
		Target:    "test-index",
		StartedAt: originalStarted,
		Total:     1000,
		LastID:    500,
	}
	b, err := json.Marshal(cp1)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}

	// resume：加载并恢复
	loaded, lerr := loadCheckpoint(path)
	if lerr != nil {
		t.Fatalf("加载 checkpoint 失败: %v", lerr)
	}
	sink := restoreCheckpointSink(path, loaded)
	resumed := IDRange{Lo: 500, Hi: 1000} // resume 后游标下界之上的单区间计划
	sink.planRanges([]IDRange{resumed})
	sink.observeRange(resumed, 600) // 继续推进 LastID

	// 验证 StartedAt 被原样保留（不是 time.Now()）
	if !sink.cp.StartedAt.Equal(originalStarted) {
		t.Errorf("StartedAt 应被原样保留为 %v，实际 %v", originalStarted, sink.cp.StartedAt)
	}
	// 验证 Total 被保留
	if sink.cp.Total != 1000 {
		t.Errorf("Total 应为 1000，实际 %d", sink.cp.Total)
	}
	// 验证 LastID 已更新
	if sink.cp.LastID != 600 {
		t.Errorf("LastID 应为 600，实际 %d", sink.cp.LastID)
	}

	// 落盘后再读回，确认持久化一致性
	cp2, err := loadCheckpoint(path)
	if err != nil {
		t.Fatalf("重新读取 checkpoint 失败: %v", err)
	}
	if !cp2.StartedAt.Equal(originalStarted) {
		t.Errorf("落盘后 StartedAt 应保留为 %v，实际 %v", originalStarted, cp2.StartedAt)
	}
	if cp2.Total != 1000 {
		t.Errorf("落盘后 Total 应为 1000，实际 %d", cp2.Total)
	}
}

// ---------------------------------------------------------------------------
// 对账决策防护（第四轮缺陷 #2：resume 对账零防护）
// ---------------------------------------------------------------------------

// TestReconcileDecision_ResumeNotSkipped 验证 resume 场景不跳过对账。
// 直接调用 shouldSkipReconcile（生产代码 :345 的判定函数），若有人加 || opts.Resume
// 或删 k=1 双置导致 resume 被错误跳过，本测试即变红。
func TestReconcileDecision_ResumeNotSkipped(t *testing.T) {
	tests := []struct {
		name     string
		opts     Options
		wantSkip bool
	}{
		{"resume 无 ids/since 必须对账", Options{Resume: true}, false},
		{"全量无 resume 必须对账", Options{}, false},
		{"ids 补数跳过对账", Options{IDs: []int64{1, 2}}, true},
		{"since 补数跳过对账", Options{Since: time.Now()}, true},
		{"resume + ids 仍跳过", Options{Resume: true, IDs: []int64{1}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldSkipReconcile(tt.opts); got != tt.wantSkip {
				t.Errorf("shouldSkipReconcile(%+v) = %v, want %v", tt.opts, got, tt.wantSkip)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 第五轮新增：fail-closed / 编排路径 / checkpoint 增强防护
// ---------------------------------------------------------------------------

// TestIsIngestTargetTunable_FailClosedOnError 验证可注入 lookup 返回 error 时
// isIngestTargetTunableWithLookup 必须 fail-closed（ok=false, err!=nil）。
// 关键：Registry 必须持有已注册别名，使 alias 循环实际执行；否则 continue 与 return
// 行为等价，无法捕获「fail-closed 被改成 continue」的回归（Leo 第四轮破坏 E1b 空洞）。
func TestIsIngestTargetTunable_FailClosedOnError(t *testing.T) {
	reg, err := es.LoadRegistry("")
	if err != nil {
		t.Fatalf("装载内嵌 Registry 失败: %v", err)
	}
	if len(reg.Aliases()) == 0 {
		t.Fatal("Registry 必须持有已已注册别名，否则无法测试 alias 循环内的 fail-closed")
	}
	fakeLookup := func(ctx context.Context, index string) ([]string, error) {
		// alias 查询失败，target 查询成功（返回空别名 → 不调优应判为可安全调优）。
		// 这样只有 fail-closed 的 return error 路径才能让函数返回 ok=false；
		// 若被改成 continue，alias 错误被跳过，target 查无别名 → ok=true，测试失败。
		for _, alias := range reg.Aliases() {
			if index == alias {
				return nil, fmt.Errorf("ES 连接失败模拟")
			}
		}
		return nil, nil // target 查询成功，无别名
	}
	ok, err := isIngestTargetTunableWithLookup(context.Background(), reg, "any-target", fakeLookup)
	if ok {
		t.Fatal("lookup 返回 error 时必须 fail-closed 返回 ok=false")
	}
	if err == nil {
		t.Fatal("lookup 返回 error 时必须返回非 nil error")
	}
	if !strings.Contains(err.Error(), "灌入调优安全判定失败") {
		t.Errorf("error 应包含 '灌入调优安全判定失败'，实际: %v", err)
	}
}

// TestIsIngestTargetTunable_ESAliasAuthority 验证 IndexAliases 权威判定：
// 当 target 在 ES 侧持有别名（即使不在 Registry 中），必须判定为不可调优。
func TestIsIngestTargetTunable_ESAliasAuthority(t *testing.T) {
	reg := &es.Registry{} // 空 Registry，无注册别名
	fakeLookup := func(ctx context.Context, index string) ([]string, error) {
		return nil, nil // Registry 别名为空，第一道不触发
	}
	// 注入 indexAliasesLookup 以模拟 ES 侧 target 持有别名
	origIAL := indexAliasesLookup
	indexAliasesLookup = func(ctx context.Context, index string) ([]string, error) {
		if index == "target-with-es-alias" {
			return []string{"es-side-alias"}, nil
		}
		return nil, nil
	}
	defer func() { indexAliasesLookup = origIAL }()

	ok, err := isIngestTargetTunableWithLookup(context.Background(), reg, "target-with-es-alias", fakeLookup)
	if err != nil {
		t.Fatalf("不应返回 error，实际: %v", err)
	}
	if ok {
		t.Fatal("target 在 ES 侧持有别名时必须判定为不可调优（ok=false）")
	}
}

// TestBuildReconcileDecision_ResumeNotPartial 验证 buildReconcileDecision：
// Resume=true 但无 ids/since 时 IsPartialSync=false（resume 不是局部补数）。
func TestBuildReconcileDecision_ResumeNotPartial(t *testing.T) {
	opts := Options{Resume: true}
	d := buildReconcileDecision(opts)
	if d.IsPartialSync {
		t.Fatal("Resume=true 但无 ids/since 时 IsPartialSync 应为 false")
	}
	if !d.Resume {
		t.Fatal("Resume 字段应被透传")
	}
	// 同时验证 shouldReconcile：IsPartialSync=false → 必须对账
	if !shouldReconcile(d) {
		t.Fatal("resume 全量必须执行对账（shouldReconcile=true）")
	}
}

// TestBuildReconcileDecision_IdsPartial 验证 ids 补数 → IsPartialSync=true → 跳过对账。
func TestBuildReconcileDecision_IdsPartial(t *testing.T) {
	opts := Options{IDs: []int64{1, 2, 3}}
	d := buildReconcileDecision(opts)
	if !d.IsPartialSync {
		t.Fatal("有 ids 时 IsPartialSync 应为 true")
	}
	if shouldReconcile(d) {
		t.Fatal("ids 补数应跳过对账（shouldReconcile=false）")
	}
}

// TestSyncProducts_ResumeForcesSingleRange 验证 resume 后 LastID>0 时强制 Concurrency=1
// 且 RangeMultiplier=1（k=1 单区间单游标）。保护 :282-283 的 k=1 双置。
func TestSyncProducts_ResumeForcesSingleRange(t *testing.T) {
	cp := checkpoint{Target: "test", LastID: 100, Total: 500}
	pp := &productProvider{}
	opts := &Options{Concurrency: 4, RangeMultiplier: 4}
	applyResumeState(cp, pp, opts)
	if pp.minExclusiveID != 100 {
		t.Errorf("minExclusiveID 应为 100，实际 %d", pp.minExclusiveID)
	}
	if opts.Concurrency != 1 {
		t.Errorf("resume 后 Concurrency 应为 1，实际 %d", opts.Concurrency)
	}
	if opts.RangeMultiplier != 1 {
		t.Errorf("resume 后 RangeMultiplier 应为 1，实际 %d", opts.RangeMultiplier)
	}
}

// TestApplyResumeState_ZeroLastID_NoOp 验证 LastID=0 时 applyResumeState 不做任何修改。
func TestApplyResumeState_ZeroLastID_NoOp(t *testing.T) {
	cp := checkpoint{Target: "test", LastID: 0, Total: 500}
	pp := &productProvider{}
	opts := &Options{Concurrency: 4, RangeMultiplier: 4}
	applyResumeState(cp, pp, opts)
	if pp.minExclusiveID != 0 {
		t.Errorf("LastID=0 时 minExclusiveID 应保持 0，实际 %d", pp.minExclusiveID)
	}
	if opts.Concurrency != 4 {
		t.Errorf("LastID=0 时 Concurrency 应保持 4，实际 %d", opts.Concurrency)
	}
}

// TestCheckpoint_Persistence_IncludesTotalAndK1 验证 checkpoint 落盘后 Total>0
// 且 resume 读回后能正确恢复 k=1 模式（第五轮新增断言）。
func TestCheckpoint_Persistence_IncludesTotalAndK1(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/test.checkpoint.json"
	sink := newCheckpointSink(path, "test-index")
	single := IDRange{Lo: 0, Hi: 1000}
	sink.planRanges([]IDRange{single})
	sink.setTotal(300)
	sink.observeRange(single, 50)

	cp, err := loadCheckpoint(path)
	if err != nil {
		t.Fatalf("读取 checkpoint 失败: %v", err)
	}
	// 断言 Total > 0（第五轮 P0 修复：生产路径 setTotal 调用点存在）
	if cp.Total != 300 {
		t.Errorf("Total 应为 300，实际 %d", cp.Total)
	}
	if cp.Total <= 0 {
		t.Fatal("落盘文件 total 必须 > 0")
	}
	// 断言 resume 后 k=1
	pp := &productProvider{}
	opts := &Options{Concurrency: 8, RangeMultiplier: 8}
	applyResumeState(cp, pp, opts)
	if opts.Concurrency != 1 || opts.RangeMultiplier != 1 {
		t.Errorf("resume 后应强制 k=1（Concurrency=%d, RangeMultiplier=%d）", opts.Concurrency, opts.RangeMultiplier)
	}
	k := ComputeRangeCount(opts.Concurrency, opts.RangeMultiplier)
	if k != 1 {
		t.Errorf("resume 后 k 应为 1，实际 %d", k)
	}
}

// TestExecResumeReconcile_FullScope 验证 execResumeReconcile 的 save/restore 模式：
// 保护 SyncProducts :351 的调用点——若有人删除该调用或破坏 save/restore，本测试变红。
//
// 破坏矩阵：
//   - 删除 SyncProducts 中 execResumeReconcile 调用 → 测试仍通过但生产代码不再 save/restore（需 code review）
//   - 删除 execResumeReconcile 内部的 pp.minExclusiveID = saved → 断言 2 变红
//   - 删除 execResumeReconcile 内部的 pp.minExclusiveID = 0 → 断言 1 变红
func TestExecResumeReconcile_FullScope(t *testing.T) {
	// 断言 1：execResumeReconcile 内部必须清零 minExclusiveID（全量口径）。
	t.Run("clearsMinExclusiveID", func(t *testing.T) {
		pp := &productProvider{minExclusiveID: 15}
		var captured int64 = -1
		// 用 fake reconcile 捕获调用时的 minExclusiveID
		origReconcile := reconcileForTest
		reconcileForTest = func(ctx context.Context, pp *productProvider, target string, stats *SyncStats, tol int64) error {
			captured = pp.minExclusiveID
			return nil
		}
		defer func() { reconcileForTest = origReconcile }()

		_ = execResumeReconcile(context.Background(), pp, "test", &SyncStats{}, 0)
		if captured != 0 {
			t.Fatalf("reconcile 调用时 minExclusiveID 应为 0（全量口径），实际 %d", captured)
		}
	})

	// 断言 2：execResumeReconcile 完成后必须恢复原始 minExclusiveID。
	t.Run("restoresMinExclusiveID", func(t *testing.T) {
		pp := &productProvider{minExclusiveID: 15}
		origReconcile := reconcileForTest
		reconcileForTest = func(ctx context.Context, pp *productProvider, target string, stats *SyncStats, tol int64) error {
			return nil
		}
		defer func() { reconcileForTest = origReconcile }()

		_ = execResumeReconcile(context.Background(), pp, "test", &SyncStats{}, 0)
		if pp.minExclusiveID != 15 {
			t.Fatalf("execResumeReconcile 应恢复 minExclusiveID=15，实际 %d", pp.minExclusiveID)
		}
	})
}

// ---------------------------------------------------------------------------
// tune-ingest 安全判定测试（第四轮缺陷 #5：零覆盖）
// ---------------------------------------------------------------------------

// TestCheckIngestTunable_TargetIsAlias target 本身就是别名名 → 不调优
func TestCheckIngestTunable_TargetIsAlias(t *testing.T) {
	aliasNames := []string{"shop-product"}
	aliasMap := map[string][]string{"shop-product": {"shop-product-v1"}}
	ok, reason := checkIngestTunable(aliasNames, aliasMap, "shop-product")
	if ok {
		t.Fatal("target 是别名名 'shop-product'，不应判定为可调优")
	}
	if !strings.Contains(reason, "本身就是已注册别名") {
		t.Errorf("跳过原因应包含 '本身就是已注册别名'，实际 %q", reason)
	}
}

// TestCheckIngestTunable_TargetPointedByAlias target 被某别名指向的物理索引 → 不调优
func TestCheckIngestTunable_TargetPointedByAlias(t *testing.T) {
	aliasNames := []string{"shop-product"}
	aliasMap := map[string][]string{"shop-product": {"shop-product-v1"}}
	ok, reason := checkIngestTunable(aliasNames, aliasMap, "shop-product-v1")
	if ok {
		t.Fatal("target 'shop-product-v1' 被别名 'shop-product' 指向，不应判定为可调优")
	}
	if !strings.Contains(reason, "被别名") {
		t.Errorf("跳过原因应包含 '被别名'，实际 %q", reason)
	}
}

// TestCheckIngestTunable_TargetPointedByThirdPartyAlias
// target 是第三方别名指向的物理索引 → 不调优
func TestCheckIngestTunable_TargetPointedByThirdPartyAlias(t *testing.T) {
	aliasNames := []string{"shop-product", "shop-order"}
	aliasMap := map[string][]string{
		"shop-product": {"shop-product-v1"},
		"shop-order":   {"shop-order-v1"},
	}
	ok, _ := checkIngestTunable(aliasNames, aliasMap, "shop-order-v1")
	if ok {
		t.Fatal("target 'shop-order-v1' 被第三方别名 'shop-order' 指向，不应判定为可调优")
	}
}

// TestCheckIngestTunable_TargetIsStandaloneIndex target 完全无别名挂载 → 调优
func TestCheckIngestTunable_TargetIsStandaloneIndex(t *testing.T) {
	aliasNames := []string{"shop-product"}
	aliasMap := map[string][]string{"shop-product": {"shop-product-v1"}}
	ok, _ := checkIngestTunable(aliasNames, aliasMap, "shop-product-next")
	if !ok {
		t.Fatal("target 'shop-product-next' 未挂载任何别名，应判定为可调优")
	}
}

// TestCheckIngestTunable_EmptyRegistry 无任何别名注册 → 调优
func TestCheckIngestTunable_EmptyRegistry(t *testing.T) {
	ok, _ := checkIngestTunable(nil, nil, "any-target")
	if !ok {
		t.Fatal("无别名注册时任何 target 应判定为可调优")
	}
}

// TestCheckIngestTunable_MultipleAliasesPointingTarget 多个别名都指向同一 target → 不调优
func TestCheckIngestTunable_MultipleAliasesPointingTarget(t *testing.T) {
	aliasNames := []string{"alias-a", "alias-b"}
	aliasMap := map[string][]string{
		"alias-a": {"target-index"},
		"alias-b": {"target-index", "other-index"},
	}
	ok, reason := checkIngestTunable(aliasNames, aliasMap, "target-index")
	if ok {
		t.Fatal("target 被多个别名指向，不应判定为可调优")
	}
	if reason == "" {
		t.Error("应给出跳过原因")
	}
}

// ---------------------------------------------------------------------------
// 第七轮新增：调用点钉测（Fix ① B2/B5）+ 调优泄漏防护（Fix ② C1）
// ---------------------------------------------------------------------------

// TestSyncProducts_ResumeCallsExecReconcile 锁定 SyncProducts :351 的 execResumeReconcile 调用点。
// 测试经 SyncProducts 端到端执行，替换 execResumeReconcile 为 spy：
//   - 断言 spy 被 SyncProducts 调用（resume 路径确实经由 execResumeReconcile）。
//   - 破坏矩阵 B2：删掉 :351 调用或换成裸 reconcile() → spy 不被调用 → called=false → 测试变红。
func TestSyncProducts_ResumeCallsExecReconcile(t *testing.T) {
	origExec := execResumeReconcile
	origBounds := boundsLookup
	origCpDir := checkpointBaseDir
	defer func() {
		execResumeReconcile = origExec
		boundsLookup = origBounds
		checkpointBaseDir = origCpDir
	}()

	var called bool
	execResumeReconcile = func(ctx context.Context, pp *productProvider, target string, stats *SyncStats, tolerance int64) error {
		called = true
		return nil
	}
	boundsLookup = func(ctx context.Context, pp *productProvider) (Bounds, error) {
		return Bounds{Min: 0, Max: 0, Total: 0}, nil
	}

	// 创建临时 registry
	dir := t.TempDir()
	defJSON := `{"_meta":{"alias":"shop-product","schema_version":1},"settings":{},"mappings":{"properties":{"id":{"type":"long"}}}}`
	if err := os.WriteFile(dir+"/product.jsonc", []byte(defJSON), 0o644); err != nil {
		t.Fatalf("写入临时 registry 失败: %v", err)
	}

	// 注入 checkpoint 基目录为临时目录，避免写入真实 runtime/
	cpDir := t.TempDir()
	checkpointBaseDir = cpDir
	cp := checkpoint{Target: "test-idx", LastID: 10, Total: 100, StartedAt: time.Now()}
	cpB, err := json.Marshal(cp)
	if err != nil {
		t.Fatalf("序列化 checkpoint 失败: %v", err)
	}
	if err := os.WriteFile(cpDir+"/test-idx.checkpoint.json", cpB, 0o644); err != nil {
		t.Fatalf("写入 checkpoint 文件失败: %v", err)
	}

	stats, syncErr := SyncProducts(context.Background(), Options{
		IndicesDir: dir,
		Index:      "test-idx",
		Resume:     true,
	})
	// spy 断言前置：不依赖 SyncProducts 是否报错，直接检查 called。
	if !called {
		t.Errorf("execResumeReconcile 未被 SyncProducts 调用——:337 调用点可能已被删除或替换为裸 reconcile()")
	}
	if syncErr != nil {
		t.Fatalf("SyncProducts 不应返回错误: %v", syncErr)
	}
	if stats == nil {
		t.Fatal("stats 不应为 nil")
	}
}

// TestSyncProducts_FreshRunWritesTotal 锁定 SyncProducts :292 的 setTotalFromBounds 调用点。
// 测试经 SyncProducts 端到端执行，替换 setTotalFromBounds 为 spy：
//   - 断言 spy 被 SyncProducts 调用（非 resume 路径确实调用了 setTotalFromBounds）。
//   - 破坏矩阵 B5：删掉 :292 调用 → spy 不被调用 → called=false → 测试变红。
func TestSyncProducts_FreshRunWritesTotal(t *testing.T) {
	origSet := setTotalFromBounds
	origBounds := boundsLookup
	origReconcile := reconcile
	origCpDir := checkpointBaseDir
	defer func() {
		setTotalFromBounds = origSet
		boundsLookup = origBounds
		reconcile = origReconcile
		checkpointBaseDir = origCpDir
	}()

	var called bool
	setTotalFromBounds = func(ctx context.Context, pp *productProvider) {
		called = true
		pp.ck.setTotal(999)
	}
	boundsLookup = func(ctx context.Context, pp *productProvider) (Bounds, error) {
		return Bounds{Min: 0, Max: 0, Total: 0}, nil
	}
	reconcile = func(ctx context.Context, pp *productProvider, target string, stats *SyncStats, tolerance int64) error {
		return nil
	}

	// 创建临时 registry
	dir := t.TempDir()
	defJSON := `{"_meta":{"alias":"shop-product","schema_version":1},"settings":{},"mappings":{"properties":{"id":{"type":"long"}}}}`
	if err := os.WriteFile(dir+"/product.jsonc", []byte(defJSON), 0o644); err != nil {
		t.Fatalf("写入临时 registry 失败: %v", err)
	}

	// 注入 checkpoint 基目录为临时目录，避免写入真实 runtime/
	checkpointBaseDir = t.TempDir()

	stats, err := SyncProducts(context.Background(), Options{
		IndicesDir: dir,
		Index:      "test-idx",
	})
	if err != nil {
		t.Fatalf("SyncProducts 不应返回错误: %v", err)
	}
	if stats == nil {
		t.Fatal("stats 不应为 nil")
	}
	if !called {
		t.Fatal("setTotalFromBounds 未被 SyncProducts 调用——:278 调用点可能已被删除")
	}
}

// TestApplyIngestTuning_PartialFailureRollsBack 锁定 applyIngestTuning 的 defer-before-mutate：
// 每项 apply 之前先登记 defer 回滚；后续项失败时，Go defer 机制自动回滚所有已生效项。
//
// 测试设计：注入 fake putSettings，第 2 项 apply 失败。
//   - 正确代码（defer-before-mutate + idx<appliedCount）：
//     调用序列 = apply(refresh) → defer#0登记 → apply(replicas,失败) → defer#1登记 →
//     return 触发 defer → defer#1(idx=1 < appliedCount=1 = false, 跳过) →
//     defer#0(idx=0 < appliedCount=1 = true, 回滚 refresh) → 共 3 次调用。
//   - 破坏矩阵 C1：去掉 defer-before-mutate（改回成功后才登记 restore）→
//     第 2 项失败时其 defer 未登记 → 仅第 1 项 defer 触发 → 同样 3 次调用，
//     但回滚发生在函数返回时而非 return 前（语义差异，调用数相同）。
//   - 破坏矩阵 ②-条件回退：把 idx<appliedCount 改回 appliedCount>0 →
//     defer#1 也触发无意义回滚 → 共 4 次调用 → 断言 totalCalls==3 变红。
func TestApplyIngestTuning_PartialFailureRollsBack(t *testing.T) {
	origPutSettings := putSettings
	defer func() { putSettings = origPutSettings }()

	var (
		mu        golangsync.Mutex
		callCount int
		callLog   []string // 记录每次调用的 settingsJSON
	)
	putSettings = func(ctx context.Context, idx, settingsJSON string) error {
		mu.Lock()
		defer mu.Unlock()
		callCount++
		callLog = append(callLog, settingsJSON)
		// 第 2 次调用（number_of_replicas apply）返回错误
		if callCount == 2 {
			return fmt.Errorf("模拟 settings 应用失败")
		}
		return nil
	}

	def := &es.IndexDefinition{}
	restore, err := applyIngestTuning(context.Background(), "test-idx", def)

	// apply 应返回 error（第 2 项失败）
	if err == nil {
		t.Fatal("第 2 项 apply 失败时 applyIngestTuning 应返回 error")
	}
	if restore != nil {
		t.Fatal("第 2 项 apply 失败时不应返回 restore 函数")
	}

	// 关键断言：只回滚真正已生效的项（idx < appliedCount）。
	// 总调用数 = 2（apply）+ 1（仅 refresh 的 defer 回滚）= 3。
	// 若回滚条件退化为 appliedCount>0，未生效的 replicas 也会被回滚 → 总调用数 = 4。
	mu.Lock()
	defer mu.Unlock()
	if callCount != 3 {
		t.Fatalf("只回滚已生效项时应有 3 次调用（2 apply + 1 rollback），实际 %d 次；调用序列: %v", callCount, callLog)
	}
	// 回滚调用中应包含 refresh_interval 的恢复
	foundRefresh := false
	for i := 2; i < len(callLog); i++ { // 跳过前 2 次 apply 调用
		if strings.Contains(callLog[i], "refresh_interval") {
			foundRefresh = true
			break
		}
	}
	if !foundRefresh {
		t.Errorf("回滚应包含 refresh_interval 恢复，实际调用序列: %v", callLog)
	}
}

// TestApplyIngestTuning_PartialFailureRollsBackAppliedOnly 锁定回滚范围精确性：
// 第 1 项（refresh_interval）apply 成功、第 2 项（number_of_replicas）apply 失败时，
// 必须且仅回滚第 1 项。若回滚条件过宽（appliedCount>0 而非 idx<appliedCount），
// 会把未生效的 replicas 也 PUT 一次声明值，本测试通过「禁止出现 replicas restore」断言捕获。
//
// 破坏自证：
//   - 条件改回 appliedCount>0 → replicas 的 restore 出现 → foundReplicasRestore=true → 测试 RED
//   - 删掉 restore 调用 → refresh 的 restore 不出现 → foundRefreshRestore=false → 测试 RED
func TestApplyIngestTuning_PartialFailureRollsBackAppliedOnly(t *testing.T) {
	origPutSettings := putSettings
	defer func() { putSettings = origPutSettings }()

	var (
		mu        golangsync.Mutex
		callCount int
		callLog   []string
	)
	putSettings = func(ctx context.Context, idx, settingsJSON string) error {
		mu.Lock()
		defer mu.Unlock()
		callCount++
		callLog = append(callLog, settingsJSON)
		if callCount == 2 {
			return fmt.Errorf("模拟 settings 应用失败")
		}
		return nil
	}

	def := &es.IndexDefinition{}
	_, err := applyIngestTuning(context.Background(), "test-idx", def)
	if err == nil {
		t.Fatal("第 2 项 apply 失败时应返回 error")
	}

	mu.Lock()
	defer mu.Unlock()

	// 断言 1：refresh_interval 的 restore 必须出现（已生效项必须回滚）
	foundRefreshRestore := false
	// 断言 2：number_of_replicas 的 restore 不得出现（未生效项不应回滚）
	foundReplicasRestore := false
	for i := 2; i < len(callLog); i++ { // 跳过前 2 次 apply 调用
		if strings.Contains(callLog[i], "refresh_interval") {
			foundRefreshRestore = true
		}
		if strings.Contains(callLog[i], "number_of_replicas") {
			foundReplicasRestore = true
		}
	}
	if !foundRefreshRestore {
		t.Errorf("已生效的 refresh_interval 必须被回滚，实际调用序列: %v", callLog)
	}
	if foundReplicasRestore {
		t.Errorf("未生效的 number_of_replicas 不应被回滚（回滚范围过宽），实际调用序列: %v", callLog)
	}
}

// TestApplyIngestTuning_IndependentContext 锁定 applyIngestTuning 使用独立 context：
// 父 ctx 已取消时 apply 仍应成功（因为内部使用 context.Background）。
// 破坏矩阵：apply 改回使用父 ctx → fake 检查 ctx.Err() → 返回错误 → 测试变红。
func TestApplyIngestTuning_IndependentContext(t *testing.T) {
	origPutSettings := putSettings
	defer func() { putSettings = origPutSettings }()

	putSettings = func(ctx context.Context, idx, settingsJSON string) error {
		if ctx.Err() != nil {
			return fmt.Errorf("context 已取消，PutSettings 拒绝执行")
		}
		return nil // 模拟成功
	}

	// 使用已取消的父 ctx
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立即取消

	def := &es.IndexDefinition{}
	restore, err := applyIngestTuning(ctx, "test-idx", def)
	if err != nil {
		t.Fatalf("父 ctx 已取消时 applyIngestTuning 仍应成功（使用独立 ctx），实际错误: %v", err)
	}
	if restore == nil {
		t.Fatal("restore 函数不应为 nil")
	}
}

// TestSetTotalFromBounds_CalledByProductionPath 锁定 setTotalFromBounds 调用点：
// SyncProducts :291 必须经由此函数设置 Total。若有人删除 setTotal(b.Total) 行，
// 断言 checkpoint.Total=0 即变红。
func TestSetTotalFromBounds_CalledByProductionPath(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/test.checkpoint.json"
	pp := &productProvider{ck: newCheckpointSink(path, "test-index")}

	// 替换 boundsLookup 为 fake（避免真实 DB 连接）
	origLookup := boundsLookup
	boundsLookup = func(ctx context.Context, pp *productProvider) (Bounds, error) {
		return Bounds{Min: 1, Max: 100, Total: 50}, nil
	}
	defer func() { boundsLookup = origLookup }()

	setTotalFromBounds(context.Background(), pp)

	// 验证 in-memory 状态（setTotal 只更新内存，observe 才落盘）
	if pp.ck.cp.Total != 50 {
		t.Fatalf("setTotalFromBounds 必须调用 setTotal(Bounds.Total)：期望 Total=50，实际 %d", pp.ck.cp.Total)
	}

	// 破坏矩阵：删除 setTotalFromBounds 内部的 setTotal 调用 → Total=0 → 断言变红
	if pp.ck.cp.Total <= 0 {
		t.Fatal("checkpoint Total 必须 > 0")
	}
}

// TestIsIngestTargetTunable_TargetLookupErrorFailClosed 锁定 :560-563 的 target 侧
// fail-closed：当 IndexAliases(target) 返回 error 时，必须 fail-closed（ok=false, err!=nil）。
// 若有人吞掉该 error（如改成 continue 或 return true, nil），本测试变红。
func TestIsIngestTargetTunable_TargetLookupErrorFailClosed(t *testing.T) {
	reg := &es.Registry{} // 空 Registry，跳过 alias 循环，直接到 target 查询
	fakeLookup := func(ctx context.Context, index string) ([]string, error) {
		return nil, nil // 第一道 Registry 循环不触发
	}
	// 注入 indexAliasesLookup 模拟 target 查询失败
	origIAL := indexAliasesLookup
	indexAliasesLookup = func(ctx context.Context, index string) ([]string, error) {
		if index == "broken-target" {
			return nil, fmt.Errorf("ES 连接超时模拟")
		}
		return nil, nil
	}
	defer func() { indexAliasesLookup = origIAL }()

	ok, err := isIngestTargetTunableWithLookup(context.Background(), reg, "broken-target", fakeLookup)
	if ok {
		t.Fatal("target 查询返回 error 时必须 fail-closed 返回 ok=false")
	}
	if err == nil {
		t.Fatal("target 查询返回 error 时必须返回非 nil error")
	}
	if !strings.Contains(err.Error(), "灌入调优安全判定失败") {
		t.Errorf("error 应包含 '灌入调优安全判定失败'，实际: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 必修③ 回归防护：调优安全判定失败必须降级为「跳过调优」，不得阻断灌数
// ---------------------------------------------------------------------------

// tuneRegJSON 是最小 registry 声明：带已登记别名 shop-product，使 alias 循环真实执行。
const tuneRegJSON = `{"_meta":{"alias":"shop-product","schema_version":1},"settings":{},"mappings":{"properties":{"id":{"type":"long"}}}}`

// writeTuneRegistry 写一个临时 registry 目录（t.TempDir，绝不碰真实 runtime/）。
func writeTuneRegistry(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/product.jsonc", []byte(tuneRegJSON), 0o644); err != nil {
		t.Fatalf("写入临时 registry 失败: %v", err)
	}
	return dir
}

// tuneGuards 集中管理必修③ 用例要替换的包级注入点，Cleanup 负责还原。
type tuneGuards struct {
	aliasIdx   func(context.Context, string) ([]string, error)
	indexAlias func(context.Context, string) ([]string, error)
	put        func(context.Context, string, string) error
	bounds     func(context.Context, *productProvider) (Bounds, error)
	reconcile  func(context.Context, *productProvider, string, *SyncStats, int64) error
	cpDir      string
}

func (g tuneGuards) install(t *testing.T) {
	t.Helper()
	oAlias, oIndex, oPut := aliasIndexesLookup, indexAliasesLookup, putSettings
	oBounds, oRec, oCp := boundsLookup, reconcile, checkpointBaseDir
	aliasIndexesLookup, indexAliasesLookup, putSettings = g.aliasIdx, g.indexAlias, g.put
	boundsLookup, reconcile, checkpointBaseDir = g.bounds, g.reconcile, g.cpDir
	t.Cleanup(func() {
		aliasIndexesLookup, indexAliasesLookup, putSettings = oAlias, oIndex, oPut
		boundsLookup, reconcile, checkpointBaseDir = oBounds, oRec, oCp
	})
}

// zeroBounds 是必修③ 用例共用的边界：空表使 Run 优雅短路，无需 MySQL。
func zeroBounds(context.Context, *productProvider) (Bounds, error) {
	return Bounds{Min: 0, Max: 0, Total: 0}, nil
}

// noopReconcile 替换对账出口（生产 reconcile 会真连 ES）。
func noopReconcile(context.Context, *productProvider, string, *SyncStats, int64) error {
	return nil
}

// TestSyncProducts_TuningCheckFailureSkipsTuningAndContinues 锁定必修③ 主诉求：
// 登记别名缺失 / 别名查询异常（例如 orders 草稿索引）时，安全判定自身报错，
// 只能降级为「跳过灌入期调优 + 告警」，绝不能把整条灌数路径判死。
//
// 鉴别力：把调用点改回 `if terr != nil { return nil, terr }`，本测试立即变红
// （SyncProducts 返回错误、reconcile 未被调用）。
func TestSyncProducts_TuningCheckFailureSkipsTuningAndContinues(t *testing.T) {
	t.Helper()
	var reconciled bool
	var puts []string
	tuneGuards{
		aliasIdx: func(_ context.Context, alias string) ([]string, error) {
			return nil, fmt.Errorf("alias [%s] 不存在（登记别名缺失模拟）", alias)
		},
		indexAlias: func(context.Context, string) ([]string, error) { return nil, nil },
		put: func(_ context.Context, idx, body string) error {
			puts = append(puts, idx+" "+body)
			return nil
		},
		bounds: zeroBounds,
		reconcile: func(context.Context, *productProvider, string, *SyncStats, int64) error {
			reconciled = true
			return nil
		},
		cpDir: t.TempDir(),
	}.install(t)

	_, err := SyncProducts(context.Background(), Options{
		IndicesDir: writeTuneRegistry(t),
		Index:      "zz_test_idx",
		TuneIngest: true,
	})
	if err != nil {
		t.Fatalf("调优安全判定失败必须降级为跳过调优，不得阻断灌数，实际错误: %v", err)
	}
	if len(puts) != 0 {
		t.Errorf("判定失败时必须零 PUT /_settings，实际发起 %d 次: %v", len(puts), puts)
	}
	if !reconciled {
		t.Error("灌数主流程未走完（reconcile 未被调用）—— 整条灌数被判定失败阻断了")
	}
}

// TestRunDryRun_TuningCheckFailureDoesNotBlock 同理钉住 dry-run 路径：判定失败
// 只能降级展示「跳过调优」，预演本身必须成功返回（旧实现 return stats, terr 会变红）。
func TestRunDryRun_TuningCheckFailureDoesNotBlock(t *testing.T) {
	puts := 0
	tuneGuards{
		aliasIdx: func(context.Context, string) ([]string, error) {
			return nil, fmt.Errorf("alias 不存在（登记别名缺失模拟）")
		},
		indexAlias: func(context.Context, string) ([]string, error) { return nil, nil },
		put:        func(context.Context, string, string) error { puts++; return nil },
		bounds:     zeroBounds,
		reconcile:  noopReconcile,
		cpDir:      t.TempDir(),
	}.install(t)

	_, err := SyncProducts(context.Background(), Options{
		IndicesDir: writeTuneRegistry(t),
		Index:      "zz_test_idx",
		TuneIngest: true,
		DryRun:     true,
	})
	if err != nil {
		t.Fatalf("dry-run 不应因调优判定失败而报错: %v", err)
	}
	if puts != 0 {
		t.Errorf("dry-run 必须零 PUT settings，实际 %d 次", puts)
	}
}

// TestSyncProducts_TargetHoldingAliasStillSkipsTuning 确认必修③ 没有削弱真机钉过的
// 安全边界：target 在 ES 侧持有别名（含第三方未登记别名）时依然跳过调优、零 PUT，
// 且这是「跳过」而不是「失败」。
func TestSyncProducts_TargetHoldingAliasStillSkipsTuning(t *testing.T) {
	var puts int
	tuneGuards{
		aliasIdx: func(context.Context, string) ([]string, error) { return nil, nil },
		indexAlias: func(_ context.Context, idx string) ([]string, error) {
			if idx == "zz_test_idx" {
				return []string{"third-party-alias"}, nil
			}
			return nil, nil
		},
		put:       func(context.Context, string, string) error { puts++; return nil },
		bounds:    zeroBounds,
		reconcile: noopReconcile,
		cpDir:     t.TempDir(),
	}.install(t)

	if _, err := SyncProducts(context.Background(), Options{
		IndicesDir: writeTuneRegistry(t),
		Index:      "zz_test_idx",
		TuneIngest: true,
	}); err != nil {
		t.Fatalf("target 持别名应跳过调优并照常灌数，实际错误: %v", err)
	}
	if puts != 0 {
		t.Errorf("target 持有别名时 PUT /_settings 次数 = %d, 期望 0", puts)
	}
}

// TestSyncProducts_TimeoutStillRunsRestoreOnIndependentCtx 锁定必修③ 第二条：
// 上游 ctx（--timeout）已打断时，apply 与 restore 仍必须落定 settings —— 不能
// 因为「apply 用上游 ctx 失败返回、restore 未注册」把索引留在 refresh_interval=-1。
//
// 鉴别力：把调用点 apply 改回 `applyIngestTuning(ctx, ...)` 且 tuningContext 退回
// `context.WithTimeout(ctx, ...)`，restore 的 PUT 会看到已取消 ctx → 断言变红。
func TestSyncProducts_TimeoutStillRunsRestoreOnIndependentCtx(t *testing.T) {
	type rec struct {
		body string
		dead bool
	}
	var mu golangsync.Mutex
	var puts []rec
	tuneGuards{
		aliasIdx:   func(context.Context, string) ([]string, error) { return nil, nil },
		indexAlias: func(context.Context, string) ([]string, error) { return nil, nil },
		put: func(ctx context.Context, _, body string) error {
			mu.Lock()
			defer mu.Unlock()
			puts = append(puts, rec{body: body, dead: ctx.Err() != nil})
			return nil
		},
		bounds:    zeroBounds,
		reconcile: noopReconcile,
		cpDir:     t.TempDir(),
	}.install(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 模拟 --timeout 已打断

	if _, err := SyncProducts(ctx, Options{
		IndicesDir: writeTuneRegistry(t),
		Index:      "zz_test_idx",
		TuneIngest: true,
	}); err != nil {
		t.Fatalf("SyncProducts 失败: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	var applied, restored, onDead int
	for _, p := range puts {
		if p.dead {
			onDead++
		}
		switch {
		case strings.Contains(p.body, `"-1"`) || strings.Contains(p.body, "number_of_replicas\":0"):
			applied++
		default:
			restored++
		}
	}
	if applied != 2 {
		t.Errorf("apply 的 PUT 次数 = %d, 期望 2（refresh_interval + replicas 分开下发）", applied)
	}
	if restored != 2 {
		t.Errorf("restore 的 PUT 次数 = %d, 期望 2（上游已取消仍必须恢复）", restored)
	}
	if onDead != 0 {
		t.Errorf("%d/%d 次 settings PUT 跑在已取消 ctx 上 —— 必修③ 独立 ctx 未生效", onDead, len(puts))
	}
}

// TestTuningContext_InheritsValuesNotCancellation 钉住 tuningContext 本身语义：
// 断开取消传播（WithoutCancel），但保留上游 ctx 的值，并带显式超时。
// 退化回 context.Background() 会丢值 → 断言变红。
func TestTuningContext_InheritsValuesNotCancellation(t *testing.T) {
	type key struct{}
	parent, cancel := context.WithCancel(context.WithValue(context.Background(), key{}, "trace-1"))
	cancel()

	ctx, cancelTuning := tuningContext(parent)
	defer cancelTuning()

	if ctx.Err() != nil {
		t.Errorf("派生 ctx 不应继承取消，实际 Err=%v", ctx.Err())
	}
	if ctx.Value(key{}) != "trace-1" {
		t.Error("派生 ctx 丢失了上游 ctx 的值（须用 WithoutCancel 而非 Background）")
	}
	if _, ok := ctx.Deadline(); !ok {
		t.Error("派生 ctx 必须带显式超时")
	}
}

// TestApplyIngestTuning_CancelledUpstreamLeavesNoTuning 钉住必修③ 「apply 阶段被打断仍不遗留调优态」：
// 上游 ctx（--timeout）已打断、且第 2 项 apply 失败时，第 1 项已生效的
// refresh_interval=-1 必须在**未取消的独立 ctx** 上回滚掉——此时 restore 尚未注册，
// defer 回滚是唯一能把索引从调优态拉回来的路径。
//
// 破坏矩阵：
//   - tuningContext 退回 context.WithTimeout(ctx, ...)（不断取消）→ 回滚 PUT 的 ctx 已死 → 变红；
//   - 删掉 defer 里的回滚 PUT → 调用序列只有 2 次 → 变红；
//   - 回滚条件退化成 appliedCount>0 → 未生效的 replicas 也被 PUT → 末段断言变红。
func TestApplyIngestTuning_CancelledUpstreamLeavesNoTuning(t *testing.T) {
	origPutSettings := putSettings
	defer func() { putSettings = origPutSettings }()

	type putRec struct {
		body string
		dead bool
	}
	var (
		mu   golangsync.Mutex
		puts []putRec
	)
	putSettings = func(ctx context.Context, _, body string) error {
		mu.Lock()
		defer mu.Unlock()
		puts = append(puts, putRec{body: body, dead: ctx.Err() != nil})
		if len(puts) == 2 {
			return fmt.Errorf("模拟 apply 阶段被 --timeout 打断")
		}
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 上游已打断：任何复用上游 ctx 的 settings 操作都会必然失败

	restore, err := applyIngestTuning(ctx, "zz_test_idx", &es.IndexDefinition{})
	if err == nil {
		t.Fatal("第 2 项 apply 失败时必须返回 error")
	}
	if restore != nil {
		t.Fatal("apply 未全部成功时不应返回 restore（回滚必须靠 defer）")
	}

	mu.Lock()
	defer mu.Unlock()
	// 调用序列：apply(refresh) 成功 → apply(replicas) 失败 → defer 回滚 refresh。
	if len(puts) != 3 {
		t.Fatalf("应有 3 次 PUT（2 apply + 1 回滚），实际 %d 次: %v", len(puts), puts)
	}
	rollback := puts[2].body
	if !strings.Contains(rollback, "refresh_interval") {
		t.Errorf("必须回滚已生效的 refresh_interval，实际调用序列: %v", puts)
	}
	if strings.Contains(rollback, `"-1"`) {
		t.Errorf("回滚应写回声明值而非 -1，实际: %s", rollback)
	}
	if puts[2].dead {
		t.Errorf("回滚 PUT 跑在已取消 ctx 上 —— target 会留在 refresh_interval=-1 调优态无人恢复")
	}
	for i := 2; i < len(puts); i++ {
		if strings.Contains(puts[i].body, "number_of_replicas") {
			t.Errorf("未生效的 number_of_replicas 不应被回滚（回滚范围过宽），实际调用序列: %v", puts)
		}
	}
}
