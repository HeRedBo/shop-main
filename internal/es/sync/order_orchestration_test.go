package sync

// order_orchestration_test.go 是任务 #42「打通订单全量灌数编排」的正面证据与破坏矩阵。
//
// 覆盖四条语义（任一条被改坏 → 相应用例立刻变红）：
//  1. orderProvider 满足 ingestTarget 接口（编译期 + 运行期双锁），能被 syncIndexTarget
//     的形态守卫放行；
//  2. checkpoint 写入链路：setCheckpoint → primeTotal → PlanRanges → observeRange /
//     markRangeDone → 落盘文件反映 Total 与最小连续覆盖右边界；
//  3. resume 续传链路：loadCheckpoint → setCheckpoint(restoreCheckpointSink) → applyResume
//     把游标下界推进到 minExclusiveID、强制 k=1，且 filter 把游标如实带进 SQL；
//  4. 按别名分层的口径描述（dbCounterScopes）与预检护栏（IngestOrchestrationReady）：
//     商品=is_del=0 AND is_show=1、订单=is_del=0、未登记别名一律显式报错。
//
// 全部不连真实 MySQL/ES：
//   - Bounds 通过注入 orderBoundsLookup 拦截（同商品 boundsLookup 的注入模式）；
//   - SQL 形态用 delta_test.go 已证过的 dryRunDialector / newDeltaDryRunDB 现算；
//   - checkpointBaseDir 指向 t.TempDir()，绝不碰真实 runtime/。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"shop/internal/es"

	"gorm.io/gorm"
)

// isolateOrderBounds 把订单边界查询换成固定值，并返回 called 供断言编排真的走了这条口。
// 与 isolatePipelineSinks 商品侧同构，但只覆盖订单：本文件的用例刻意不去动商品 sink，
// 免得商品侧的破坏矩阵被误伤。
func isolateOrderBounds(t *testing.T, b Bounds, err error) (called *bool, lastOpts *Options) {
	t.Helper()
	called = new(bool)
	orig := orderBoundsLookup
	t.Cleanup(func() { orderBoundsLookup = orig })
	orderBoundsLookup = func(context.Context, *orderProvider) (Bounds, error) {
		*called = true
		return b, err
	}
	return called, nil
}

// isolateCpDir 把 checkpoint 落盘目录限定到 t.TempDir()，返回 base 供拼路径。
func isolateCpDir(t *testing.T) string {
	t.Helper()
	orig := checkpointBaseDir
	dir := t.TempDir()
	checkpointBaseDir = dir
	t.Cleanup(func() { checkpointBaseDir = orig })
	return dir
}

// ---------------------------------------------------------------------------
// ① orderProvider 满足 ingestTarget —— 编译期已在 order.go 自检，这里补运行期断言
// ---------------------------------------------------------------------------

func TestOrderProvider_ImplementsIngestTarget(t *testing.T) {
	op := newOrderProvider(nil, lookupOrderDefForSync(t), Options{})
	// 若将来给 ingestTarget 加方法而忘了在 orderProvider 上补齐，这里会立刻红。
	var _ ingestTarget = op
	if _, ok := interface{}(op).(ingestTarget); !ok {
		t.Fatal("orderProvider 必须实现 ingestTarget（否则 syncIndexTarget 会拒订单走编排）")
	}
	// 三张注册表的 providers[shop-order] 构造出来的运行时形态必须是 *orderProvider。
	f, ok := providers[OrderAlias]
	if !ok {
		t.Fatalf("providers 表缺 %s 表项", OrderAlias)
	}
	if _, ok := f(nil, lookupOrderDefForSync(t), Options{}).(ingestTarget); !ok {
		t.Fatal("providers[shop-order] 构造的 provider 必须满足 ingestTarget（订单已接入编排）")
	}
}

// ---------------------------------------------------------------------------
// ② checkpoint 写入 + ③ resume 续传 + setTotal —— 订单编排链路的正面证据
// ---------------------------------------------------------------------------

// TestOrderProvider_IngestTargetChain_CheckpointResumeSetTotal 串起订单编排链路最容易
// 被无声改坏的三段语义：
//   - setCheckpoint 挂 sink → primeTotal 从 Bounds 取 Total → 落盘文件反映 Total；
//   - PlanRanges 转交区间计划 + observeRange 逐批进度 + markRangeDone 只在 err==nil
//     时登记完整覆盖 → 落盘 LastID 沿最小连续覆盖右边界推进；
//   - applyResume 把 LastID 变成 minExclusiveID、强制 k=1，且 filter 把游标带进 SQL。
//
// 破坏矩阵（任一即变红）：
//   - 删 orderProvider.primeTotal → cp.Total 保持 0，与商品语义脱钩；
//   - 删 orderProvider.setCheckpoint → 后续所有 sink 调用都 no-op，文件不生成；
//   - 把 markRangeDone 改成无条件 completeRange → 中途报错也推游标；
//   - 删 applyResume 里的 setCursor / k=1 收敛 → minExclusiveID 停在 0 / 并发未收敛；
//   - 把 filter 的 minExclusiveID 分支删掉 → SQL 不带 id > ?，resume 白拿。
func TestOrderProvider_IngestTargetChain_CheckpointResumeSetTotal(t *testing.T) {
	def := lookupOrderDefForSync(t)
	ctx := context.Background()
	isolateCpDir(t)
	_, _ = isolateOrderBounds(t, Bounds{Min: 1, Max: 100, Total: 42}, nil)

	// --- 写入：setCheckpoint + primeTotal + PlanRanges + observeRange + markRangeDone ---
	path := filepath.Join(t.TempDir(), "cp.json")
	op := newOrderProvider(nil, def, Options{})
	sink := newCheckpointSink(path, "shop-order_v1")
	op.setCheckpoint(sink)

	// primeTotal：从 Bounds 拿 Total（42）写入 sink。此时文件尚未生成（sink 只在游标
	// 前进时落盘），必须靠后续的 observeRange/completeRange 触发写入才能验证 Total。
	op.primeTotal(ctx)

	plan := cpTestPlan() // (0,25] (25,50] (50,75] (75,100]
	op.PlanRanges(plan)

	// 未登记计划 → 保守不落盘；这里已登记 → 首次区间内进度即落盘。
	op.ck.observeRange(plan[0], 12)
	cpFile, err := loadCheckpoint(path)
	if err != nil {
		t.Fatalf("observeRange 后应落盘，读取失败: %v", err)
	}
	if cpFile.Total != 42 {
		t.Errorf("primeTotal 必须把 Bounds.Total=%d 写入 checkpoint，实际 cp.Total=%d", 42, cpFile.Total)
	}
	if cpFile.LastID != 12 {
		t.Errorf("区间内进度应反映到 LastID=12，实际 %d", cpFile.LastID)
	}
	if cpFile.Target != "shop-order_v1" {
		t.Errorf("checkpoint Target 字段未如实落盘，实际 %q", cpFile.Target)
	}

	// markRangeDone 的守门语义：err!=nil 时不得登记完整覆盖。
	op.markRangeDone(plan[0], errors.New("context canceled"))
	if f := cpLastID(t, path); f != 12 {
		t.Errorf("ScanRange 报错时不得登记完整覆盖，游标应保持 12，实际 %d", f)
	}
	// 正常走完 → 游标推到 plan[0].Hi=25；再走完 plan[1] → 推到 50。
	op.markRangeDone(plan[0], nil)
	if f := cpLastID(t, path); f != 25 {
		t.Errorf("plan[0] 完整覆盖应把游标推到 25，实际 %d", f)
	}
	op.ck.observeRange(plan[1], 40)
	op.markRangeDone(plan[1], nil)
	if f := cpLastID(t, path); f != 50 {
		t.Errorf("plan[0]+plan[1] 连续覆盖应推到 50，实际 %d", f)
	}
	// plan[2]/plan[3] 未覆盖 → 迟到的低位进度不得让游标回退。
	op.ck.observeRange(plan[3], 90)
	if f := cpLastID(t, path); f != 50 {
		t.Errorf("plan[3] 单独前进不得回退到未覆盖空洞之前，游标应保持 50，实际 %d", f)
	}

	// --- resume：加载 checkpoint → 构造新 provider → applyResume → 下界进入 filter ---
	cp, err := loadCheckpoint(path)
	if err != nil {
		t.Fatalf("resume 前加载 checkpoint 失败: %v", err)
	}
	if cp.Total != 42 || cp.LastID != 50 {
		t.Fatalf("resume 前 checkpoint 异常：Total=%d LastID=%d（期望 42/50）", cp.Total, cp.LastID)
	}

	op2 := newOrderProvider(newDeltaDryRunDB(t), def, Options{})
	op2.setCheckpoint(restoreCheckpointSink(path, cp))
	runOpts := &Options{Concurrency: 4, RangeMultiplier: 4}
	op2.applyResume(cp, runOpts)

	if op2.minExclusiveID != 50 {
		t.Errorf("applyResume 必须把 LastID=%d 挂到 minExclusiveID，实际 %d", 50, op2.minExclusiveID)
	}
	if runOpts.Concurrency != 1 || runOpts.RangeMultiplier != 1 {
		t.Errorf("resume 必须强制 k=1 单区间模式，实际 concurrency=%d rangeMultiplier=%d",
			runOpts.Concurrency, runOpts.RangeMultiplier)
	}
	// resume 后 Total 不得被 primeTotal 用「带下界的 Bounds」覆盖：sink 从 cp 继承 42。
	if op2.ck.cp.Total != 42 {
		t.Errorf("restoreCheckpointSink 必须保留原始 Total=42，实际 %d", op2.ck.cp.Total)
	}
	// 结构性保证：filter 必须把 minExclusiveID 折进 SQL "id > ?" —— resume 才有意义。
	res := op2.filter(ctx).Select(orderScanColumns).Find(&[]esStoreOrderShim{})
	if res.Error != nil {
		t.Fatalf("DryRun 生成 SQL 失败: %v", res.Error)
	}
	sqlStr := strings.ToLower(strings.ReplaceAll(res.Statement.SQL.String(), "`", ""))
	if !strings.Contains(sqlStr, "id > ") {
		t.Errorf("resume 后 filter WHERE 必须含主键游标 `id > ?`，实际 SQL: %s", sqlStr)
	}
	if !strings.Contains(sqlStr, "is_del") {
		t.Errorf("订单 filter 必须显式 is_del=0（资格口径），实际 SQL: %s", sqlStr)
	}
	if strings.Contains(sqlStr, "is_show") {
		t.Errorf("订单没有 is_show 维度，SQL 出现即为把商品口径抄错，实际 SQL: %s", sqlStr)
	}
}

// esStoreOrderShim 是 filter 断言用的 Find 目标占位（避免测试引入 models 包）。
// 只要 GORM DryRun 能把 SQL 落到 Statement.SQL 上即可，字段用不到。
type esStoreOrderShim struct{ Id int64 }

// TableName 让 GORM 生成可读表名（不改变断言语义）。
func (esStoreOrderShim) TableName() string { return "store_order" }

// ---------------------------------------------------------------------------
// ④ SyncByAlias("shop-order") 端到端路由：过去命中「尚未接入」，现在必须放行到编排
// ---------------------------------------------------------------------------

// TestSyncByAlias_OrderAliasRoutesThroughOrchestration 是「打通订单编排」的最短正面证据。
// 改造前：syncIndexTarget 里 `p.(*productProvider)` 断言失败 → 报「尚未接入全量灌数编排」。
// 改造后：ingestTarget 接口 + orderProvider 实现 → 放行到 runDryRun，orderBoundsLookup 命中。
//
// 破坏矩阵：把 orderProvider 上任何一个 ingestTarget 方法删掉 → 类型断言失败 → 立即变红。
func TestSyncByAlias_OrderAliasRoutesThroughOrchestration(t *testing.T) {
	isolateCpDir(t)
	called, _ := isolateOrderBounds(t, Bounds{Min: 1, Max: 80, Total: 9}, nil)

	_, err := SyncByAlias(context.Background(), OrderAlias, Options{
		IndicesDir: "",
		Index:      OrderAlias,
		DryRun:     true,
		TuneIngest: false,
		AssumeYes:  true,
	})
	if err != nil {
		t.Fatalf("订单编排必须打通，不得再命中「尚未接入全量灌数编排」，got=%v", err)
	}
	if strings.Contains(errString(err), "尚未接入全量灌数编排") {
		t.Fatalf("回归：又命中形态守卫，说明 orderProvider 未实现 ingestTarget")
	}
	if !*called {
		t.Fatal("SyncByAlias(shop-order) 必须经 runDryRun 触达 orderBoundsLookup，说明数据源正确路由到订单")
	}
}

// errString 让 nil 与 "" 语义不混淆（避免上面 strings.Contains 在 err==nil 时先跑）。
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// ---------------------------------------------------------------------------
// ⑤ 未登记 provider 护栏：形态守卫与 IngestOrchestrationReady 都必须显式拒绝
// ---------------------------------------------------------------------------

// TestIngestOrchestrationReady_RejectsUnwiredAndNonIngestShape 覆盖预检的两类负例：
//   - 未登记别名 → 「尚未接线数据源 Provider」；
//   - 已登记但 provider 形态不满足 ingestTarget → 「尚未接入全量灌数编排」。
//
// 破坏矩阵：把 IngestOrchestrationReady 里的形态检查删掉 / 把类型断言换成强制转换 →
// 第二条子断言立即变红（fakeProvider 无 ingestTarget 方法）。
func TestIngestOrchestrationReady_RejectsUnwiredAndNonIngestShape(t *testing.T) {
	t.Run("未登记别名一律显式拒绝", func(t *testing.T) {
		err := IngestOrchestrationReady("shop-totally-unregistered", Options{})
		if err == nil || !strings.Contains(err.Error(), "尚未接线数据源 Provider") {
			t.Fatalf("未登记别名必须报「尚未接线数据源 Provider」，got=%v", err)
		}
	})

	t.Run("已登记但形态不满足 ingestTarget 的别名必须拒绝", func(t *testing.T) {
		orig := providers[ProductAlias]
		providers[ProductAlias] = func(_ *gorm.DB, def *es.IndexDefinition, _ Options) DocProvider {
			return &fakeProvider{def: def}
		}
		t.Cleanup(func() { providers[ProductAlias] = orig })

		err := IngestOrchestrationReady(ProductAlias, Options{})
		if err == nil || !strings.Contains(err.Error(), "尚未接入全量灌数编排") {
			t.Fatalf("非 ingestTarget 形态必须报「尚未接入全量灌数编排」，got=%v", err)
		}
	})

	t.Run("已接入编排的商品/订单别名放行", func(t *testing.T) {
		if err := IngestOrchestrationReady(ProductAlias, Options{}); err != nil {
			t.Errorf("商品别名应放行，got=%v", err)
		}
		if err := IngestOrchestrationReady(OrderAlias, Options{}); err != nil {
			t.Errorf("订单别名应放行（任务 #42 主交付），got=%v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// ⑥ 口径描述按别名登记：dbCounterScopes 与 dbCounters 同源，破坏矩阵锁定字面量
// ---------------------------------------------------------------------------

// TestDBCounterScope_PerAliasLiteral 逐字钉死两侧口径文案。
// 破坏矩阵：
//   - 把订单那条改回硬编码 "is_del=0 AND is_show=1"（任务书里的经典回归）→ 立即变红；
//   - 把商品那条改成 "is_del=0"（丢失 is_show 维度）→ 立即变红；
//   - 未登记别名返回空串或商品兜底 → 立即变红（fallback 必须是明示的中文说明）。
func TestDBCounterScope_PerAliasLiteral(t *testing.T) {
	if got := DBCounterScope(ProductAlias); got != "is_del=0 AND is_show=1" {
		t.Errorf("商品口径必须逐字为 is_del=0 AND is_show=1（守住 es:status 现有输出），got=%q", got)
	}
	if got := DBCounterScope(OrderAlias); got != "is_del=0" {
		t.Errorf("订单口径必须为 is_del=0（无 is_show 维度），got=%q", got)
	}
	// fallback 不得静默返回商品口径 —— 未登记别名一律显式说明。
	got := DBCounterScope("shop-totally-unregistered")
	if got == "" || got == "is_del=0 AND is_show=1" || got == "is_del=0" {
		t.Errorf("未登记别名的兜底不得是任何具体口径（会误导运维），必须点明未登记，got=%q", got)
	}
	// 与 providers/dbCounters 表键集对齐：已登记 DB 计数口径的别名必须有对应描述。
	for alias := range dbCounters {
		s := DBCounterScope(alias)
		if s == "" || strings.Contains(s, "未登记") {
			t.Errorf("dbCounters 已登记的别名 %s 缺失口径描述（会导致 es:status 打印「未登记」误导）", alias)
		}
	}
}

// TestProviderRegistry_ScopeTableCoversAllWiredAliases 守护：providers 里每一条表项
// 都必须在 dbCounterScopes 里有对应描述，防止「加了新业务但忘了登记口径」的静默劣化。
func TestProviderRegistry_ScopeTableCoversAllWiredAliases(t *testing.T) {
	for alias := range providers {
		if _, ok := dbCounterScopes[alias]; !ok {
			t.Errorf("别名 %s 已在 providers 表接线灌数，但未登记 dbCounterScopes 口径描述（es:status / es:cutover 会退回「未登记」文案）", alias)
		}
	}
}

// TestCheckpointPathHonoursIsolatedBaseDir 是自检：isolateCpDir 生效。
// 若哪天 checkpointBaseDir 不再被 checkpointPath 使用，本用例会立刻红，
// 顺带把上面所有写文件的用例从「悄悄污染 runtime/」拉回 t.TempDir()。
func TestCheckpointPathHonoursIsolatedBaseDir(t *testing.T) {
	dir := isolateCpDir(t)
	p := checkpointPath("shop-order_v1")
	if filepath.Dir(p) != dir {
		t.Fatalf("checkpointPath 必须落在隔离目录里，got=%q want-dir=%q", p, dir)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("隔离目录未创建：%v", err)
	}
}
