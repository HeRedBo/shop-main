package sync

// registry_provider_test.go 钉住任务 #37 阶段一的骨架契约：灌数层「按别名分发」，
// 取不到表项就报错，绝不静默回落到商品实现。
//
// 全部用临时 registry + 包级 fake 注入点驱动，不连真实 MySQL/ES：所有拒绝都必须发生在
// boundsLookup（首个外部触达点）之前，这一点每条用例都单独记账断言 —— 否则「报错」可能
// 是「已经扫了库甚至写了 ES 之后才报错」，那正是本任务要消灭的缺陷形态。

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"shop/internal/es"

	"gorm.io/gorm"
)

// isolatePipelineSinks 把 syncIndexTarget 会碰到的全部外部触达点换成记账 fake。
// 返回的 *called 在首个 sink 被调用时置真，用于断言「拒绝发生在流水线之前」。
func isolatePipelineSinks(t *testing.T) *bool {
	t.Helper()

	called := new(bool)
	origBounds, origRec, origCp := boundsLookup, reconcile, checkpointBaseDir
	origAliasIdx, origIndexAlias, origPut := aliasIndexesLookup, indexAliasesLookup, putSettings
	t.Cleanup(func() {
		boundsLookup, reconcile, checkpointBaseDir = origBounds, origRec, origCp
		aliasIndexesLookup, indexAliasesLookup, putSettings = origAliasIdx, origIndexAlias, origPut
	})
	boundsLookup = func(context.Context, *productProvider) (Bounds, error) {
		*called = true
		return Bounds{}, nil
	}
	reconcile = func(context.Context, *productProvider, string, *SyncStats, int64) error {
		*called = true
		return nil
	}
	aliasIndexesLookup = func(context.Context, string) ([]string, error) { *called = true; return nil, nil }
	indexAliasesLookup = func(context.Context, string) ([]string, error) { *called = true; return nil, nil }
	putSettings = func(context.Context, string, string) error { *called = true; return nil }
	checkpointBaseDir = t.TempDir()
	return called
}

// TestProviderRegistry_ShopProductIsWiredByLiteralAlias 是「破坏即红」哨兵用例。
//
// 故意用**字面量** "shop-product" 而不是 ProductAlias 常量查表：把表里的键改名（或把常量
// 改掉）会让本用例立刻变红，而不是一起跟着改错。三张表键集必须一致 —— 一个业务要么
// 灌数/补偿/计数三条口同时接线，要么一条都别接（缺一条就会出现「能灌数但补偿静默失败」）。
func TestProviderRegistry_ShopProductIsWiredByLiteralAlias(t *testing.T) {
	if ProductAlias != "shop-product" {
		t.Fatalf("ProductAlias 常量被改动（%q），es:sync-products 与所有 fixture 的别名约定都会失效", ProductAlias)
	}
	for name, table := range map[string]map[string]any{
		"providers":    toAnyTable(providers),
		"compensators": toAnyTable(compensators),
		"dbCounters":   toAnyTable(dbCounters),
	} {
		if _, ok := table["shop-product"]; !ok {
			t.Errorf("注册表 %s 缺少字面量别名 shop-product 表项（改名即失效：商品灌数/补偿/计数会整体断线）", name)
		}
	}
	provKeys := registeredAliases(providers)
	compKeys := registeredAliases(compensators)
	cntKeys := registeredAliases(dbCounters)
	if strings.Join(provKeys, ",") != strings.Join(compKeys, ",") || strings.Join(provKeys, ",") != strings.Join(cntKeys, ",") {
		t.Errorf("三张注册表键集不一致：providers=%v compensators=%v dbCounters=%v（一个业务必须三口同时接线）", provKeys, compKeys, cntKeys)
	}
}

// toAnyTable 把任意 map[string]V 收敛成 map[string]any，只为让上面的键检查写一遍。
func toAnyTable[V any](in map[string]V) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// TestSyncByAlias_UnwiredAliasRejectedBeforePipeline：未接线的别名必须在任何扫库之前被拒。
// 若把 SyncByAlias 退回恒调商品实现，err==nil 且 boundsCalled==true → 两条断言皆红。
//
// 用 shop-unknown（任何注册表都不存在的别名）作「未接线」样本：#38 后 shop-order 已三口
// 接线，再拿它当未登记示例就测不到「拒绝」这条分支了（会命中订单 provider 走进编排）。
func TestSyncByAlias_UnwiredAliasRejectedBeforePipeline(t *testing.T) {
	boundsCalled := isolatePipelineSinks(t)

	_, err := SyncByAlias(context.Background(), "shop-unknown", Options{
		IndicesDir: writeLookupRegistry(t),
		Index:      "shop-unknown",
		AssumeYes:  true,
	})
	if err == nil {
		t.Fatal("未接线别名 shop-unknown 必须被拒，却返回成功（缺陷回归：恒跑商品实现）")
	}
	if !strings.Contains(err.Error(), "尚未接线数据源 Provider") {
		t.Errorf("报错必须点明「未接线数据源 Provider」，got=%q", err.Error())
	}
	if !strings.Contains(err.Error(), ProductAlias) {
		t.Errorf("报错必须列出已接线别名便于排障，got=%q", err.Error())
	}
	if *boundsCalled {
		t.Error("被拒必须发生在任何 MySQL/ES 触达之前，却调用了 boundsLookup")
	}
}

// TestSyncByAlias_RejectsCrossBusinessTarget：调用方声明的别名与解析出的定义对不上时
// （例如把商品目标挂到别的已注册别名上调）必须拒绝，这是「串数据」的最后一道闸。
// 本期只有一条表项，该分支要等 #38 才活起来，所以这里临时注入第二条表项来驱动它。
func TestSyncByAlias_RejectsCrossBusinessTarget(t *testing.T) {
	boundsCalled := isolatePipelineSinks(t)

	providers["shop-virtual"] = func(_ *gorm.DB, _ *es.IndexDefinition, _ Options) DocProvider { return &fakeProvider{} }
	t.Cleanup(func() { delete(providers, "shop-virtual") })

	_, err := SyncByAlias(context.Background(), "shop-virtual", Options{
		IndicesDir: writeLookupRegistry(t),
		Index:      "shop-product", // 解析到商品定义，与声明的别名不是同一个业务
		AssumeYes:  true,
	})
	if err == nil {
		t.Fatal("别名与解析结果不一致必须被拒，却返回成功（缺陷回归：拿别的业务数据源往商品索引灌）")
	}
	if !strings.Contains(err.Error(), "拒绝灌数") || !strings.Contains(err.Error(), "不一致") {
		t.Errorf("报错必须点明「别名不一致、拒绝灌数」，got=%q", err.Error())
	}
	if *boundsCalled {
		t.Error("串数据守卫必须发生在流水线之前，却调用了 boundsLookup")
	}
}

// TestSyncByAlias_HitsProductProviderAndPassesAliasThrough：已登记别名必须放行到编排层，
// 并且 Provider 是由表构造的**商品** provider（这是「查表命中」与「查表未命中」的分界）。
func TestSyncByAlias_HitsProductProviderAndPassesAliasThrough(t *testing.T) {
	boundsCalled := isolatePipelineSinks(t)

	// dry-run 路径：只读、零写，但会走到 Bounds（首个 sink），据此证明「已放行到编排层」。
	_, err := SyncByAlias(context.Background(), ProductAlias, Options{
		IndicesDir: writeLookupRegistry(t),
		Index:      ProductAlias,
		DryRun:     true,
		AssumeYes:  true,
	})
	if err != nil {
		t.Fatalf("已接线别名 %s 必须放行，got=%v", ProductAlias, err)
	}
	if !*boundsCalled {
		t.Fatal("已接线别名必须走到编排层（boundsLookup），却没走到＝查表没命中商品表项")
	}
}

// TestNewProvider_NonProductShapeRejectedByOrchestration：表里塞进一个非商品形态的
// provider 时，编排必须显式报错，绝不能按商品 provider 的字段（ck/stats/minExclusiveID）跑下去。
func TestNewProvider_NonProductShapeRejectedByOrchestration(t *testing.T) {
	boundsCalled := isolatePipelineSinks(t)

	orig := providers[ProductAlias]
	providers[ProductAlias] = func(_ *gorm.DB, def *es.IndexDefinition, _ Options) DocProvider {
		return &fakeProvider{def: def}
	}
	t.Cleanup(func() { providers[ProductAlias] = orig })

	_, err := syncIndexTarget(context.Background(), "", Options{
		IndicesDir: writeLookupRegistry(t),
		Index:      ProductAlias,
		AssumeYes:  true,
	})
	if err == nil {
		t.Fatal("非商品形态 provider 必须被编排层拒绝，却返回成功")
	}
	if !strings.Contains(err.Error(), "尚未接入全量灌数编排") {
		t.Errorf("报错必须点明编排未接入该形态，got=%q", err.Error())
	}
	if *boundsCalled {
		t.Error("形态守卫必须发生在流水线之前，却调用了 boundsLookup")
	}
}

// TestCompensateByAlias_DispatchesByTableAndRejectsUnwired：补偿也必须按别名查表。
// 注入一条假表项证明「传什么别名就跑什么实现」，而不是恒调商品 Compensate。
func TestCompensateByAlias_DispatchesByTableAndRejectsUnwired(t *testing.T) {
	t.Run("未登记别名必须拒", func(t *testing.T) {
		// shop-unknown 不在 compensators 表；shop-order 已于 #38 登记 CompensateOrder，不能再当未接线样本。
		_, err := CompensateByAlias(context.Background(), "shop-unknown", "shop-unknown_v1", time.Unix(0, 0), CompensateOptions{})
		if err == nil || !strings.Contains(err.Error(), "尚未接线差量补偿实现") {
			t.Fatalf("未接线别名必须报错并点明补偿未接线，got=%v", err)
		}
	})

	t.Run("登记别名走表里的实现", func(t *testing.T) {
		var gotTarget string
		var gotSince time.Time
		compensators["shop-virtual"] = func(_ context.Context, target string, since time.Time, _ CompensateOptions) (Stats, error) {
			gotTarget, gotSince = target, since
			return Stats{Scanned: 7, Indexed: 5}, nil
		}
		t.Cleanup(func() { delete(compensators, "shop-virtual") })

		since := time.Now().Add(-time.Hour)
		st, err := CompensateByAlias(context.Background(), "shop-virtual", "shop-virtual_v2", since, CompensateOptions{})
		if err != nil {
			t.Fatalf("已登记别名不应报错，got=%v", err)
		}
		if gotTarget != "shop-virtual_v2" || !gotSince.Equal(since) {
			t.Errorf("target/since 必须原样透传给表里的实现，got target=%q since=%v", gotTarget, gotSince)
		}
		if st.Scanned != 7 || st.Indexed != 5 {
			t.Errorf("Stats 必须原样带出（吞掉统计就是把失败当成功），got=%+v", st)
		}
	})
}

// TestDBCountByAlias_SharedSingleSourceOfTruth：对账口径与 es:status 共用同一张表，
// 且商品那条表项就是 es.ProductDocCount 本身（不是又抄了一份口径实现）。
func TestDBCountByAlias_SharedSingleSourceOfTruth(t *testing.T) {
	if _, ok := DBCounterFor(ProductAlias); !ok {
		t.Fatalf("别名 %s 必须登记 DB 计数口径（否则 es:status 会报 count_comparable=false）", ProductAlias)
	}
	if _, ok := DBCounterFor("shop-unknown"); ok {
		t.Error("未登记的业务不得有 DB 计数口径（口径错配比没比更有害）")
	}

	want := reflect.ValueOf(es.ProductDocCount).Pointer()
	got := reflect.ValueOf(dbCounters[ProductAlias]).Pointer()
	if want != got {
		t.Errorf("商品计数口径必须是 es.ProductDocCount 本体（单点拥有口径），got=%x want=%x", got, want)
	}

	if _, err := DBCountByAlias(context.Background(), "shop-unknown"); err == nil ||
		!strings.Contains(err.Error(), "未登记 DB 计数口径") {
		t.Errorf("未登记别名必须拒绝统计，got=%v", err)
	}

	// 表驱动证明「按别名分发」：注入一条假口径，未登记别名不受影响。
	dbCounters["shop-virtual"] = func(context.Context) (int64, error) { return 42, nil }
	t.Cleanup(func() { delete(dbCounters, "shop-virtual") })
	n, err := DBCountByAlias(context.Background(), "shop-virtual")
	if err != nil || n != 42 {
		t.Errorf("已登记别名必须落到表里的口径实现，got n=%d err=%v", n, err)
	}
}

// TestResolveProductDef_UniqueActiveFallbackRemoved：旧的「唯一可执行定义兜底」必须删干净。
// 订单转 active 后 len(ExecutableDefinitions()) 变 2，旧兜底会静默失效（同一调用在两种登记
// 状态下解析到不同定义）；现在只按别名精确命中，命不中一律报错。
func TestResolveProductDef_UniqueActiveFallbackRemoved(t *testing.T) {
	dir := t.TempDir()
	// 目录里只有一份 active 声明，且它**不是**商品：旧实现会「唯一可执行」兜底命中它。
	body := `{"_meta":{"alias":"shop-order","schema_version":1,"status":"active"},"settings":{},"mappings":{"properties":{"id":{"type":"long"}}}}`
	if err := os.WriteFile(dir+"/orders.jsonc", []byte(body), 0o644); err != nil {
		t.Fatalf("写入临时声明失败: %v", err)
	}
	reg, err := es.LoadRegistry(dir)
	if err != nil {
		t.Fatalf("LoadRegistry 失败: %v", err)
	}
	if got := len(reg.ExecutableDefinitions()); got != 1 {
		t.Fatalf("fixture 必须只有一条 active 声明（否则测不到旧兜底），实际 %d", got)
	}

	if _, err := resolveProductDef(reg, "", false); err == nil {
		t.Fatal("商品别名不存在时必须报错，却回落到「唯一可执行定义」（缺陷回归：旧兜底未删净）")
	} else if !strings.Contains(err.Error(), "无法定位产品索引声明") {
		t.Errorf("报错必须点明无法定位商品声明，got=%q", err.Error())
	}

	// 商品别名在册时按别名精确命中，与登记条数无关。
	prodDir := writeLookupRegistry(t)
	prodReg, err := es.LoadRegistry(prodDir)
	if err != nil {
		t.Fatalf("LoadRegistry 失败: %v", err)
	}
	d, err := resolveProductDef(prodReg, "", false)
	if err != nil || d.Alias() != ProductAlias {
		t.Errorf("商品在册时必须按别名命中，got alias=%v err=%v", d, err)
	}
}

// 编译期自检：fakeProvider 确实是 DocProvider（否则上面的形态注入会静默失效）。
var _ DocProvider = (*fakeProvider)(nil)
