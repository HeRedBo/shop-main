package main

// es_migrate_test.go 钉住 es:migrate CLI 装配层下发给内层灌数（sync.SyncProducts）的容差：
// 必须是哨兵 es.IngestToleranceIgnore，绝不能透传真实 spec.Tolerance。
//
// 这与 es_rebuild.go:244 / es_cutover.go:272 同一口径、同型缺陷：把 --tolerance 下发给灌数层，
// 内层收尾对账会抢在 migrate 本层闸门之前失败，于是「中止步骤」错报成 sync、结论的 ES 计数
// 是从未回读的假 0。对账判定归 migrate 本层闸门（Rebuild 委托 runRebuild 第 5 步、首建在
// internal/es/migrate.go 直接 ReconcileCounts），--tolerance 的可见语义只在 ReconcileCounts 单点拥有。

import (
	"testing"

	"shop/internal/es"
	"shop/internal/es/sync"
)

// 破坏即红用例：migrateIngestOptions 下发给内层灌数的容差固定为哨兵 IngestToleranceIgnore。
// 一旦有人把它改回透传真实 spec.Tolerance（即缺陷回归），无论用户传什么 --tolerance 值，断言立即变红。
func TestMigrateIngestOptions_ToleranceIsIgnoreSentinel(t *testing.T) {
	const target = "shop-product_v2"
	for _, specTol := range []int64{0, 7, -1, 1000000} {
		spec := migrateSpec{Tolerance: specTol}
		got := migrateIngestOptions(spec, target)
		if got.Tolerance != es.IngestToleranceIgnore {
			t.Errorf("下发给内层灌数的容差必须是哨兵 IngestToleranceIgnore，got=%d（spec.Tolerance=%d 被透传即缺陷回归）",
				got.Tolerance, specTol)
		}
		// 显式对照：真实容差绝不能等于用户传入值（把哨兵改回 spec.Tolerance 时这一行同样变红）。
		if got.Tolerance == specTol {
			t.Errorf("容差 %d 被原样透传给灌数层（缺陷现场）：必须下发哨兵，让内层永不提前判定", specTol)
		}
	}
}

// 回归保护：把 sync.Options 构造提成 migrateIngestOptions 时，除容差外的其余装配必须逐字不变。
// 若提取过程中误改任一字段（Index/ChunkSize/Concurrency/BulkSize/MaxFailures/IndicesDir/Resume
// 透传，或 TuneIngest=false / DryRun=false / AssumeYes=true / Timeout=0 / Stats=nil 的固定语义），此用例即红。
func TestMigrateIngestOptions_PreservesIngestWiring(t *testing.T) {
	const target = "shop-product_v2"
	spec := migrateSpec{
		Tolerance:   42, // 故意给非哨兵值，验证只被哨兵覆盖、其余透传不受影响
		ChunkSize:   1234,
		Concurrency: 8,
		BulkSize:    5678,
		MaxFailures: 99,
		IndicesDir:  "conf/indices",
		Resume:      true,
	}
	got := migrateIngestOptions(spec, target)

	if got.Index != target {
		t.Errorf("Index 必须是本次灌数目标 %q，got=%q", target, got.Index)
	}
	if got.ChunkSize != spec.ChunkSize {
		t.Errorf("ChunkSize 必须透传 %d，got=%d", spec.ChunkSize, got.ChunkSize)
	}
	if got.Concurrency != spec.Concurrency {
		t.Errorf("Concurrency 必须透传 %d，got=%d", spec.Concurrency, got.Concurrency)
	}
	if got.BulkSize != spec.BulkSize {
		t.Errorf("BulkSize 必须透传 %d，got=%d", spec.BulkSize, got.BulkSize)
	}
	if got.MaxFailures != spec.MaxFailures {
		t.Errorf("MaxFailures 必须透传 %d，got=%d", spec.MaxFailures, got.MaxFailures)
	}
	if got.IndicesDir != spec.IndicesDir {
		t.Errorf("IndicesDir 必须透传 %q，got=%q", spec.IndicesDir, got.IndicesDir)
	}
	if got.Resume != spec.Resume {
		t.Errorf("Resume 必须透传 %v，got=%v", spec.Resume, got.Resume)
	}
	// 固定语义：settings 生命周期归 rebuild 编排独占、灌数期不再调优；非 dry-run；
	// 超时统一由外层 ctx 控制；AssumeYes 恒真（交互确认在本层入口已处理）；Stats 由调用方注入。
	if got.TuneIngest {
		t.Errorf("TuneIngest 必须为 false（归 rebuild 编排独占），got=%v", got.TuneIngest)
	}
	if got.DryRun {
		t.Errorf("下发给内层灌数的 DryRun 必须为 false，got=%v", got.DryRun)
	}
	if !got.AssumeYes {
		t.Errorf("AssumeYes 必须恒为 true，got=%v", got.AssumeYes)
	}
	if got.Timeout != 0 {
		t.Errorf("Timeout 必须为 0（统一由外层 --timeout 的 ctx 控制），got=%v", got.Timeout)
	}
	if got.Stats != nil {
		t.Errorf("Stats 应由 newMigrateSyncFunc 注入，构造期必须为 nil，got=%+v", got.Stats)
	}
	// 强制引用 sync，防止将来误删该依赖导致装配漂移无人察觉。
	var _ = sync.Options{}
}
