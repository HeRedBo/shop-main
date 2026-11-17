package sync

// index_lookup_test.go 钉住必修①：es:sync-products 显式传入的 --index 必须命中「已登记且 active」
// 的定义，否则在【任何 bulk / MySQL 触达之前】报错并列出可用名称。
//
// 缺陷事实（真机）：--index=shop-order 过去不报错——resolveProductDef 兜底到商品定义，再经写死的
// productProvider 把商品数据灌进订单目标。修法用 Options.StrictIndexLookup（CLI 由 Changed("index")
// 驱动）在 resolveProductDef 开启严格分支；编程式默认 false，保留原有友好兜底，既有测试不受影响。
//
// 纯逻辑：不连真实 MySQL/ES，用临时 registry 目录 + 包级 fake 注入点驱动。

import (
	"context"
	"os"
	"strings"
	"testing"

	"shop/internal/es"
)

// 最小声明：product 为 active（可执行），orders 为 draft（不进可执行清单）。
const (
	lookupProductJSON = `{"_meta":{"alias":"shop-product","schema_version":1,"status":"active"},"settings":{},"mappings":{"properties":{"id":{"type":"long"}}}}`
	lookupOrdersJSON  = `{"_meta":{"alias":"shop-order","schema_version":1,"status":"draft"},"settings":{},"mappings":{"properties":{"id":{"type":"long"}}}}`
)

// writeLookupRegistry 写一个只含 product(active)+orders(draft) 的临时索引目录（t.TempDir，绝不碰真实 runtime/）。
func writeLookupRegistry(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{"product.jsonc": lookupProductJSON, "orders.jsonc": lookupOrdersJSON}
	for name, body := range files {
		if err := os.WriteFile(dir+"/"+name, []byte(body), 0o644); err != nil {
			t.Fatalf("写入临时 registry %s 失败: %v", name, err)
		}
	}
	return dir
}

func lookupRegistry(t *testing.T) *es.Registry {
	t.Helper()
	reg, err := es.LoadRegistry(writeLookupRegistry(t))
	if err != nil {
		t.Fatalf("LoadRegistry 失败: %v", err)
	}
	// 前置自检：确保 fixture 里 product 可执行、orders 为 draft，否则后续断言失去意义。
	if got := len(reg.ExecutableDefinitions()); got != 1 {
		t.Fatalf("可执行清单应只有 shop-product，实际 %d（fixture 异常）", got)
	}
	if d, ok := reg.Get("shop-order"); !ok || d.Executable() {
		t.Fatalf("shop-order 必须已登记且 draft，ok=%v", ok)
	}
	return reg
}

// 破坏即红主用例①：strict=true 时，未知名与 draft 名都必须被拒；active 别名/物理名必须放行。
// 若有人把 resolveProductDef 的 strict 分支删掉（退回旧兜底），前两条断言立即变红。
func TestResolveProductDef_StrictRejectsUnknownAndDraft(t *testing.T) {
	reg := lookupRegistry(t)

	t.Run("draft 别名必须拒为未启用", func(t *testing.T) {
		_, err := resolveProductDef(reg, "shop-order", true)
		if err == nil {
			t.Fatal("显式传 draft 索引 shop-order 必须报错，却成功解析（缺陷回归：兜底吞掉未接线目标）")
		}
		if !strings.Contains(err.Error(), "未接线") && !strings.Contains(err.Error(), "未启用") {
			t.Errorf("draft 报错必须点明未启用/未接线，got=%q", err.Error())
		}
		// 必须给出可用名称清单，且绝不把 draft 的 shop-order 列为可用。
		if !strings.Contains(err.Error(), "shop-product") {
			t.Errorf("报错必须列出当前可用（active）名称 shop-product，got=%q", err.Error())
		}
	})

	t.Run("完全未知名必须拒为未知", func(t *testing.T) {
		_, err := resolveProductDef(reg, "totally-unknown", true)
		if err == nil {
			t.Fatal("显式传未知名必须报错，却成功解析（缺陷回归：兜底把它当成了商品定义）")
		}
		if !strings.Contains(err.Error(), "未知或未启用") {
			t.Errorf("未知名报错必须含「未知或未启用」，got=%q", err.Error())
		}
		if !strings.Contains(err.Error(), "可用（active）：[shop-product]") {
			t.Errorf("报错必须内联可用清单 [shop-product]，got=%q", err.Error())
		}
	})

	t.Run("active 别名放行", func(t *testing.T) {
		d, err := resolveProductDef(reg, "shop-product", true)
		if err != nil {
			t.Fatalf("active 别名 shop-product 必须放行，got=%v", err)
		}
		if d.Alias() != "shop-product" {
			t.Fatalf("解析到错误定义: %+v", d)
		}
	})

	t.Run("active 物理名放行", func(t *testing.T) {
		// 物理名 shop-product_v1 = active 定义的 Name()，严格模式也必须命中。
		d, err := resolveProductDef(reg, "shop-product_v1", true)
		if err != nil {
			t.Fatalf("active 物理名 shop-product_v1 必须放行，got=%v", err)
		}
		if d.Name() != "shop-product_v1" {
			t.Fatalf("解析到错误定义: name=%s", d.Name())
		}
	})

	t.Run("strict=false 保留原有友好兜底", func(t *testing.T) {
		// 判别力反向证明：同一个未知目标，宽松模式仍回落「按商品别名精确命中」（#37 已删「唯一可执行」兜底）。
		if _, err := resolveProductDef(reg, "zz_test_idx", false); err != nil {
			t.Errorf("strict=false 必须保留按别名回落 ProductAlias，got=%v", err)
		}
	})
}

// 破坏即红主用例②：走完整 SyncProducts 入口时，strict 目标被拒必须发生在灌数流水线之前——
// 即 boundsLookup（首个 MySQL/ES 触达点）与 reconcile 一次都没被调用。
// 若删掉 strict 校验，SyncProducts 不再早退 → err==nil 且 boundsLookup 被调用 → 两条断言皆红。
func TestSyncProducts_StrictIndexAbortsBeforeAnyWrite(t *testing.T) {
	cases := []struct {
		name    string
		target  string
		wantSub string
	}{
		{"未知名", "totally-unknown", "未知或未启用"},
		{"draft 名", "shop-order", "未接线"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var boundsCalled, recCalled bool

			origBounds, origRec, origCp := boundsLookup, reconcile, checkpointBaseDir
			origAliasIdx, origIndexAlias, origPut := aliasIndexesLookup, indexAliasesLookup, putSettings
			t.Cleanup(func() {
				boundsLookup, reconcile, checkpointBaseDir = origBounds, origRec, origCp
				aliasIndexesLookup, indexAliasesLookup, putSettings = origAliasIdx, origIndexAlias, origPut
			})
			// 全部换成不连外网的 fake：一旦被调用即记账（用于证明「早退、根本没进流水线」）。
			boundsLookup = func(context.Context, *productProvider) (Bounds, error) {
				boundsCalled = true
				return Bounds{Min: 0, Max: 0, Total: 0}, nil
			}
			reconcile = func(context.Context, *productProvider, string, *SyncStats, int64) error {
				recCalled = true
				return nil
			}
			aliasIndexesLookup = func(context.Context, string) ([]string, error) { return nil, nil }
			indexAliasesLookup = func(context.Context, string) ([]string, error) { return nil, nil }
			putSettings = func(context.Context, string, string) error { return nil }
			checkpointBaseDir = t.TempDir()

			_, err := SyncProducts(context.Background(), Options{
				IndicesDir:        writeLookupRegistry(t),
				Index:             tc.target,
				StrictIndexLookup: true,
				TuneIngest:        false,
				AssumeYes:         true,
			})

			if err == nil {
				t.Fatalf("显式传 %s 必须被拒（零写入），却返回成功（缺陷回归）", tc.target)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("报错应含 %q，got=%q", tc.wantSub, err.Error())
			}
			if boundsCalled {
				t.Errorf("被拒必须在灌数流水线之前，却触达了 boundsLookup（可能已扫库/准备写入）")
			}
			if recCalled {
				t.Errorf("被拒绝不应进入对账，却调用了 reconcile")
			}
			// 附带：目标名原样进错误信息，便于运维定位是哪条 --index 出错。
			if !strings.Contains(err.Error(), tc.target) {
				t.Errorf("报错必须点名被拒目标 %q，got=%q", tc.target, err.Error())
			}
		})
	}
}
