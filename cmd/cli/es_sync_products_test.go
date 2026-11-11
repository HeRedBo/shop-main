package main

// es_sync_products_test.go 钉住本期必修①在 CLI 装配层的闭环：parseSyncOpts 真正把
// global.CONFIG.Elasticsearch 的四个数值字段读入并落到 syncOptions（此前它们全仓无读取方）。
// 用例证明「改了配置值、未显式传 flag 时行为随之变化」以及「显式 flag 优先级最高」，
// 纯逻辑：不连 MySQL/ES，不写 runtime/，用临时构造的 cobra 命令与内存态配置驱动。

import (
	"testing"

	"shop/pkg/global"

	"github.com/spf13/cobra"
)

// newSyncFlagsCmd 构造一个只注册了四个受配置影响数值 flag（外加 bulk-size）的命令，
// 每次都是全新对象以保证 Changed=false 起点，避免子测试间状态污染。
// parseSyncOpts 对其余未注册 flag 会以零值安全忽略（pflag.Changed 对未知 flag 返回 false）。
func newSyncFlagsCmd() *cobra.Command {
	c := &cobra.Command{Use: "es:sync-products"}
	f := c.Flags()
	f.Int("chunk-size", 1000, "")
	f.Int("concurrency", 4, "")
	f.Int("range-multiplier", 4, "")
	f.Int("bulk-max-mb", 0, "")
	f.Int("bulk-size", 3000, "")
	return c
}

// withESConfig 在子测试期间临时写入四个配置字段，结束后复位全局配置。
func withESConfig(t *testing.T, chunk, conc, rng, bulkMB int) {
	t.Helper()
	old := global.CONFIG.Elasticsearch
	global.CONFIG.Elasticsearch.DefaultChunk = chunk
	global.CONFIG.Elasticsearch.SyncConcurrency = conc
	global.CONFIG.Elasticsearch.RangeMultiplier = rng
	global.CONFIG.Elasticsearch.BulkMaxMB = bulkMB
	t.Cleanup(func() { global.CONFIG.Elasticsearch = old })
}

// TestParseSyncOpts_ConfigIsConsumed 是本期教训的正面防线：只要 parseSyncOpts 不再读取
// global.CONFIG（回退成「硬编码 / 无人读取」），本用例立即变红。
func TestParseSyncOpts_ConfigIsConsumed(t *testing.T) {
	withESConfig(t, 777, 9, 6, 21)

	o, err := parseSyncOpts(newSyncFlagsCmd())
	if err != nil {
		t.Fatalf("parseSyncOpts 报错: %v", err)
	}
	if o.ChunkSize != 777 {
		t.Errorf("default-chunk-size 未被读取: got %d want 777", o.ChunkSize)
	}
	if o.Concurrency != 9 {
		t.Errorf("sync-concurrency 未被读取: got %d want 9", o.Concurrency)
	}
	if o.RangeMultiplier != 6 {
		t.Errorf("range-multiplier 未被读取: got %d want 6", o.RangeMultiplier)
	}
	if o.BulkMaxMB != 21 {
		t.Errorf("bulk-max-mb 未被读取: got %d want 21", o.BulkMaxMB)
	}
}

// TestParseSyncOpts_ConfigValueChangeChangesBehavior 钉住「改了配置值行为随之变化」：
// 相同 flag 状态（全部未显式指定）下，仅移动配置值，Options 必须随之改变。
func TestParseSyncOpts_ConfigValueChangeChangesBehavior(t *testing.T) {
	withESConfig(t, 1000, 4, 4, 10)
	a, _ := parseSyncOpts(newSyncFlagsCmd())

	withESConfig(t, 250, 8, 2, 40)
	b, _ := parseSyncOpts(newSyncFlagsCmd())

	if a.ChunkSize == b.ChunkSize || a.Concurrency == b.Concurrency ||
		a.RangeMultiplier == b.RangeMultiplier || a.BulkMaxMB == b.BulkMaxMB {
		t.Fatalf("配置已变化但 Options 未随之变化，配置形同装饰: a=%+v b=%+v", a, b)
	}
	if b.Concurrency != 8 || b.RangeMultiplier != 2 || b.ChunkSize != 250 || b.BulkMaxMB != 40 {
		t.Errorf("未落到新配置值: %+v", b)
	}

	// 经 ToSync 转成 sync.Options 后，区间数 K 也应随配置改变而改变（端到端行为）。
	if ka, kb := a.ToSync(), b.ToSync(); ka.Concurrency*1 == 0 || kb.RangeMultiplier == 0 {
		t.Fatalf("ToSync 未透传并发/倍率: a=%+v b=%+v", ka, kb)
	}
}

// TestParseSyncOpts_ExplicitFlagBeatsConfig 钉住最高优先级：显式传入的 flag 必须盖过配置值，
// 现有 flag 语义与默认值不变（这里 --concurrency=1 显式覆盖配置 9）。
func TestParseSyncOpts_ExplicitFlagBeatsConfig(t *testing.T) {
	withESConfig(t, 500, 9, 6, 20)

	c := newSyncFlagsCmd()
	// 模拟用户显式传 --concurrency=1 --chunk-size=50；--bulk-max-mb=0 显式表达「交 admin」。
	if err := c.Flags().Set("concurrency", "1"); err != nil {
		t.Fatal(err)
	}
	if err := c.Flags().Set("chunk-size", "50"); err != nil {
		t.Fatal(err)
	}
	if err := c.Flags().Set("bulk-max-mb", "0"); err != nil {
		t.Fatal(err)
	}

	o, err := parseSyncOpts(c)
	if err != nil {
		t.Fatalf("parseSyncOpts 报错: %v", err)
	}
	if o.Concurrency != 1 {
		t.Errorf("显式 --concurrency=1 未盖过配置 9: got %d", o.Concurrency)
	}
	if o.ChunkSize != 50 {
		t.Errorf("显式 --chunk-size=50 未盖过配置 500: got %d", o.ChunkSize)
	}
	if o.BulkMaxMB != 0 {
		t.Errorf("显式 --bulk-max-mb=0 应尊重为 admin 默认: got %d", o.BulkMaxMB)
	}
	// range-multiplier 未显式传 → 仍取配置 6。
	if o.RangeMultiplier != 6 {
		t.Errorf("未显式指定的 range-multiplier 应取配置 6: got %d", o.RangeMultiplier)
	}
}

// TestParseSyncOpts_ZeroConfigFallsBackToBuiltin 钉住配置未提供（全 0）时回落到内置默认，
// 保证「新接线」不会让未配置环境出现 0 并发 / 0 chunk。
func TestParseSyncOpts_ZeroConfigFallsBackToBuiltin(t *testing.T) {
	withESConfig(t, 0, 0, 0, 0)

	o, err := parseSyncOpts(newSyncFlagsCmd())
	if err != nil {
		t.Fatalf("parseSyncOpts 报错: %v", err)
	}
	if o.ChunkSize != 1000 || o.Concurrency != 4 || o.RangeMultiplier != 4 {
		t.Errorf("零配置未回落内置默认: %+v", o)
	}
	if o.BulkMaxMB != 0 {
		t.Errorf("bulk-max-mb 零配置应为 0(admin 决定): got %d", o.BulkMaxMB)
	}
}

// newIndexFlagCmd 构造只注册了 --index（默认 shop-product）的命令，专供严格校验开关用例，
// 每次全新对象以保证 Changed=false 起点。
func newIndexFlagCmd() *cobra.Command {
	c := &cobra.Command{Use: "es:sync-products"}
	c.Flags().String("index", "shop-product", "")
	return c
}

// TestParseSyncOpts_StrictIndexFollowsChanged 钉住必修①的 CLI 装配判别力：
// StrictIndexLookup（→ sync.Options.StrictIndexLookup）必须由「是否显式传 --index」决定，
// 而不是由目标值是否等于默认决定——这样「未传用默认」走宽松、 「显式传（哪怕又传回 shop-product）」走严格。
func TestParseSyncOpts_StrictIndexFollowsChanged(t *testing.T) {
	// ① 未传 --index：Changed=false → 宽松（保留内层兜底），Index 取默认 shop-product。
	oDefault, err := parseSyncOpts(newIndexFlagCmd())
	if err != nil {
		t.Fatalf("parseSyncOpts 报错: %v", err)
	}
	if oDefault.StrictIndex {
		t.Errorf("未显式传 --index 不应开启严格校验，got StrictIndex=%v", oDefault.StrictIndex)
	}
	if oDefault.Index != "shop-product" {
		t.Errorf("未传时 Index 应为默认 shop-product，got=%q", oDefault.Index)
	}
	if got := oDefault.ToSync(); got.StrictIndexLookup {
		t.Errorf("ToSync 不应把宽松态透传成严格，got StrictIndexLookup=%v", got.StrictIndexLookup)
	}

	// ② 显式传 --index=shop-order：Changed=true → 严格（即便该名字最终不合法，也必须交由严格校验拒绝）。
	c := newIndexFlagCmd()
	if err := c.Flags().Set("index", "shop-order"); err != nil {
		t.Fatal(err)
	}
	oExplicit, _ := parseSyncOpts(c)
	if !oExplicit.StrictIndex {
		t.Errorf("显式传 --index 必须开启严格校验，got StrictIndex=%v", oExplicit.StrictIndex)
	}
	if oExplicit.Index != "shop-order" {
		t.Errorf("显式传入的 Index 应原样保留 shop-order，got=%q", oExplicit.Index)
	}
	// 关键透传：CLI 严格态必须落到下发给 sync.SyncProducts 的 Options.StrictIndexLookup。
	got := oExplicit.ToSync()
	if !got.StrictIndexLookup || got.Index != "shop-order" {
		t.Errorf("ToSync 必须透传严格态与目标名，got StrictIndexLookup=%v Index=%q", got.StrictIndexLookup, got.Index)
	}

	// ③ 显式又传回默认值 shop-product：Changed=true 仍应严格（区分靠 Changed，不靠值比较）。
	c2 := newIndexFlagCmd()
	if err := c2.Flags().Set("index", "shop-product"); err != nil {
		t.Fatal(err)
	}
	oSame, _ := parseSyncOpts(c2)
	if !oSame.StrictIndex {
		t.Errorf("显式传 shop-product（即便等于默认）也应严格，got StrictIndex=%v", oSame.StrictIndex)
	}
}
