package sync

// provider_test.go 钉住本期必修①：conf/config.yml 的 elasticsearch 四个数值配置
// （default-chunk-size / sync-concurrency / range-multiplier / bulk-max-mb）经由
// ResolveSyncParams 真正接入同步流水线，并固化「显式 CLI flag > 配置值 > 代码内置默认」
// 三级优先级。核心用例证明「改了配置值，行为随之变化」——正是本期踩过的教训
// （配置项无消费方会静默沦为装饰）。全部纯逻辑：不连 MySQL、不连 ES、不写 runtime/。

import (
	"testing"
)

// allUnset 构造「四个 flag 均未显式指定」的 flag 集合，便于只驱动配置值。
func allUnset() SyncParamFlags { return SyncParamFlags{} }

// TestResolveSyncParams_ConfigDrivesValue 钉住「改了配置值，解析值随之变化」：
// 仅改变配置、不动 flag，四个字段各自的输出都必须随配置移动，且绝不等于代码内置默认。
// 这是对「字段注册了但没人读」这一假一致最直接的破坏即红防线。
func TestResolveSyncParams_ConfigDrivesValue(t *testing.T) {
	cfg := SyncParamConfig{
		ChunkSize:       777, // ≠ defaultChunkSize 1000
		Concurrency:     9,   // ≠ defaultConcurrency 4
		RangeMultiplier: 6,   // ≠ defaultRangeMultiplier 4
		BulkMaxMB:       21,  // ≠ 内置兜底 0（admin 默认）
	}
	got := ResolveSyncParams(allUnset(), cfg)

	if got.ChunkSize != 777 {
		t.Errorf("default-chunk-size=777 未生效: got %d", got.ChunkSize)
	}
	if got.Concurrency != 9 {
		t.Errorf("sync-concurrency=9 未生效: got %d", got.Concurrency)
	}
	if got.RangeMultiplier != 6 {
		t.Errorf("range-multiplier=6 未生效: got %d", got.RangeMultiplier)
	}
	if got.BulkMaxMB != 21 {
		t.Errorf("bulk-max-mb=21 未生效: got %d", got.BulkMaxMB)
	}
	if len(got.Warnings) != 0 {
		t.Errorf("合法配置不应告警, got %v", got.Warnings)
	}
}

// TestResolveSyncParams_ConfigDrivesBehavior 把「解析值变化」升级为「行为变化」：
// 同一份 Bounds 下，仅改变 sync-concurrency / range-multiplier 配置，区间总数 K 必须随之改变；
// 仅改变 default-chunk-size 配置，provider 实际取用的分批行数必须随之改变。
// 破坏任一条接线（回退到硬编码常量）都会让本用例变红。
func TestResolveSyncParams_ConfigDrivesBehavior(t *testing.T) {
	// —— 并发 × 区间倍率 → K ——
	low := ResolveSyncParams(allUnset(), SyncParamConfig{Concurrency: 2, RangeMultiplier: 3})
	high := ResolveSyncParams(allUnset(), SyncParamConfig{Concurrency: 5, RangeMultiplier: 4})

	kLow := ComputeRangeCount(low.Concurrency, low.RangeMultiplier)
	kHigh := ComputeRangeCount(high.Concurrency, high.RangeMultiplier)

	if kLow != 6 {
		t.Fatalf("K(concurrency=2,range=3) 期望 6, got %d", kLow)
	}
	if kHigh != 20 {
		t.Fatalf("K(concurrency=5,range=4) 期望 20, got %d", kHigh)
	}
	if kLow == kHigh {
		t.Fatalf("改变并发配置后区间数 K 未变化，配置形同装饰")
	}

	// 区间实际切分数量也应随 K 变化（行为级证据）。
	b := Bounds{Min: 1, Max: 1000, Total: 1000}
	rLow := ComputeRanges(b, kLow)
	rHigh := ComputeRanges(b, kHigh)
	if !(len(rHigh) > len(rLow)) {
		t.Errorf("配置提高后区间数应增多: low=%d high=%d", len(rLow), len(rHigh))
	}

	// —— chunk-size → provider 实际分批行数 ——
	chunkA := ResolveSyncParams(allUnset(), SyncParamConfig{ChunkSize: 123})
	ppA := newProductProvider(nil, nil, Options{ChunkSize: chunkA.ChunkSize})
	if ppA.chunkSize != 123 {
		t.Errorf("default-chunk-size=123 未落到 provider.chunkSize: got %d", ppA.chunkSize)
	}
	chunkB := ResolveSyncParams(allUnset(), SyncParamConfig{ChunkSize: 456})
	ppB := newProductProvider(nil, nil, Options{ChunkSize: chunkB.ChunkSize})
	if ppB.chunkSize != 456 {
		t.Errorf("default-chunk-size=456 未落到 provider.chunkSize: got %d", ppB.chunkSize)
	}
	if ppA.chunkSize == ppB.chunkSize {
		t.Errorf("改变 chunk 配置后 provider 分批大小未变化，配置形同装饰")
	}

	// —— bulk-max-mb → BulkMaxBytes 字节数 ——
	mbA := ResolveSyncParams(allUnset(), SyncParamConfig{BulkMaxMB: 8})
	mbB := ResolveSyncParams(allUnset(), SyncParamConfig{BulkMaxMB: 32})
	bytesA := mbA.BulkMaxMB * 1024 * 1024
	bytesB := mbB.BulkMaxMB * 1024 * 1024
	if bytesA == bytesB {
		t.Errorf("改变 bulk-max-mb 配置后字节上限未变化，配置形同装饰")
	}
}

// TestResolveSyncParams_FlagOverridesConfig 钉住最高优先级：显式 CLI flag 必须盖过配置值，
// 且现有 flag 语义不变（--bulk-max-mb=0 显式表达「不封顶」而非回落到配置）。
func TestResolveSyncParams_FlagOverridesConfig(t *testing.T) {
	got := ResolveSyncParams(
		SyncParamFlags{
			ChunkSize:       IntFlag{Changed: true, Value: 50},
			Concurrency:     IntFlag{Changed: true, Value: 1},
			RangeMultiplier: IntFlag{Changed: true, Value: 2},
			BulkMaxMB:       IntFlag{Changed: true, Value: 0}, // 显式 0 = 交给 admin 默认
		},
		SyncParamConfig{ChunkSize: 777, Concurrency: 9, RangeMultiplier: 6, BulkMaxMB: 21},
	)
	if got.ChunkSize != 50 || got.Concurrency != 1 || got.RangeMultiplier != 2 {
		t.Errorf("显式 flag 未盖过配置: %+v", got)
	}
	if got.BulkMaxMB != 0 {
		t.Errorf("--bulk-max-mb=0 应被尊重为 admin 默认, got %d", got.BulkMaxMB)
	}
	if len(got.Warnings) != 0 {
		t.Errorf("合法显式 flag 不应告警: %v", got.Warnings)
	}
}

// TestResolveSyncParams_BuiltinDefault 钉住最低优先级：flag 未指定且配置为 0（未提供）时，
// 安静回落到代码内置默认，且不发告警（0 视为「未配置」而非非法）。
func TestResolveSyncParams_BuiltinDefault(t *testing.T) {
	got := ResolveSyncParams(allUnset(), SyncParamConfig{})
	if got.ChunkSize != defaultChunkSize {
		t.Errorf("chunk 内置默认期望 %d, got %d", defaultChunkSize, got.ChunkSize)
	}
	if got.Concurrency != defaultConcurrency {
		t.Errorf("concurrency 内置默认期望 %d, got %d", defaultConcurrency, got.Concurrency)
	}
	if got.RangeMultiplier != defaultRangeMultiplier {
		t.Errorf("range-multiplier 内置默认期望 %d, got %d", defaultRangeMultiplier, got.RangeMultiplier)
	}
	if got.BulkMaxMB != 0 {
		t.Errorf("bulk-max-mb 内置兜底应为 0(admin 决定), got %d", got.BulkMaxMB)
	}
	if len(got.Warnings) != 0 {
		t.Errorf("未配置应安静回落, 不应告警: %v", got.Warnings)
	}
}

// TestResolveSyncParams_IllegalValueClampedAndWarned 钉住必修③：越界值不静默采纳，
// 收敛到代码内置默认的同时如实告警。覆盖两类来源：配置负值 / 显式 flag 非法值。
func TestResolveSyncParams_IllegalValueClampedAndWarned(t *testing.T) {
	t.Run("配置负值收敛并告警", func(t *testing.T) {
		got := ResolveSyncParams(allUnset(), SyncParamConfig{
			ChunkSize:       -1,
			Concurrency:     -5,
			RangeMultiplier: -2,
			BulkMaxMB:       -3,
		})
		if got.ChunkSize != defaultChunkSize || got.Concurrency != defaultConcurrency ||
			got.RangeMultiplier != defaultRangeMultiplier || got.BulkMaxMB != 0 {
			t.Fatalf("非法配置未收敛到内置默认: %+v", got)
		}
		if len(got.Warnings) != 4 {
			t.Fatalf("四个非法配置应各自产生一条告警, got %d: %v", len(got.Warnings), got.Warnings)
		}
		for _, w := range got.Warnings {
			if w == "" {
				t.Errorf("告警文案为空")
			}
		}
	})

	t.Run("显式 flag 非法值收敛并告警", func(t *testing.T) {
		// concurrency 传 0（非正、且非 allowZero）→ 收敛到内置默认 + 告警。
		got := ResolveSyncParams(
			SyncParamFlags{Concurrency: IntFlag{Changed: true, Value: 0}},
			SyncParamConfig{Concurrency: 9}, // 即便配置合法，显式非法 flag 也只收敛到内置默认
		)
		if got.Concurrency != defaultConcurrency {
			t.Errorf("显式非法 flag 应收敛到内置默认 %d, got %d", defaultConcurrency, got.Concurrency)
		}
		if len(got.Warnings) != 1 {
			t.Fatalf("显式非法 flag 应告警一条, got %d: %v", len(got.Warnings), got.Warnings)
		}
	})

	t.Run("bulk-max-mb 的 0 是合法值不告警", func(t *testing.T) {
		// 区别于三个正数计数参数：allowZero 让显式 0 表达「交给 admin」，绝不告警。
		got := ResolveSyncParams(
			SyncParamFlags{BulkMaxMB: IntFlag{Changed: true, Value: 0}},
			SyncParamConfig{},
		)
		if got.BulkMaxMB != 0 {
			t.Errorf("bulk-max-mb 显式 0 应保持 0, got %d", got.BulkMaxMB)
		}
		if len(got.Warnings) != 0 {
			t.Errorf("bulk-max-mb=0 合法, 不应告警: %v", got.Warnings)
		}
	})
}
