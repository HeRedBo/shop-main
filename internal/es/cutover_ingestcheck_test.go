package es

// cutover_ingestcheck_test.go 钉死任务 #42「割接前置预检」的行为契约：
// IngestCheck 一旦返回错误，RunCutover 必须在【任何】ES 请求（读或写）发出之前拒绝，
// 绝不建出空的 <alias>_v1 再在灌数步中止留下孤儿索引。
//
// 破坏矩阵：
//   - 把 cutover.go 里 IngestCheck 的调用点挪到 newAuditedCluster 之后 → 零请求断言变红；
//   - 把 IngestCheck 错误吞掉（比如改成只 logf 不 return） → 拒绝断言变红；
//   - 把 IngestCheck==nil 的守卫删掉 → 其余既有用例（未注入 IngestCheck）会 panic。

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestRunCutover_IngestCheckRejectsBeforeAnyESRequest 是护栏的正面负例：
// 模拟一个「已登记 Provider 但全量灌数编排不可用」的别名（IngestCheck 返回错误），
// 断言 cutover 在零 ES 请求（读 + 写全为零）的前提下拒绝，且错误文案点名「割接前置预检未通过」。
func TestRunCutover_IngestCheckRejectsBeforeAnyESRequest(t *testing.T) {
	def := mustCutoverDef(t, "shop-order", 1, "analysis-ik", "analysis-pinyin")
	f := newFakeCluster()
	opts := baseCutoverOpts(f, def)
	opts.Alias = "shop-order"
	opts.DBCount = fixedCount(9)
	// 订单别名当前不存在同名物理索引，割接前提探测需要它存在（与商品同一模式）。
	f.indexes["shop-order"] = true

	opts.IngestCheck = func(_ context.Context, alias string) error {
		return errors.New("sync: 别名 " + alias + " 的数据源尚未接入全量灌数编排")
	}

	res, err := RunCutover(context.Background(), opts)
	if err == nil {
		t.Fatalf("编排不可用时割接必须拒绝，却返回成功：%+v", res)
	}
	if !strings.Contains(err.Error(), "割接前置预检未通过") {
		t.Errorf("错误必须点明「割接前置预检未通过」，got=%q", err.Error())
	}
	if !strings.Contains(err.Error(), "尚未接入全量灌数编排") {
		t.Errorf("错误必须原样带上预检根因，便于运维下一步，got=%q", err.Error())
	}
	// 核心护栏：一条 ES 请求都不能发（读 + 写都是零），杜绝孤儿索引与半成品状态。
	if ops := f.allOps(); len(ops) != 0 {
		t.Errorf("预检失败时不得发出任何 ES 请求（含读探测），实际 ops=%v", ops)
	}
	if w := f.opsOfKind("write"); len(w) != 0 {
		t.Errorf("预检失败时绝不允许任何写请求，实际 writes=%v", w)
	}
	// 绝不能建出空的 shop-order_v1（正是上一轮交付查出的缺陷形态）。
	if f.indexes["shop-order_v1"] {
		t.Error("预检失败不得创建目标索引，shop-order_v1 却被建出来了（孤儿索引残留）")
	}
}

// TestRunCutover_IngestCheckPassesThroughWhenReady 是配套的正面用例：
// 预检放行（IngestCheck 返回 nil）时，割接必须照常往下跑（至少进到建索引步），
// 证明护栏没有把正常路径也堵死。
func TestRunCutover_IngestCheckPassesThroughWhenReady(t *testing.T) {
	def := mustCutoverDef(t, "shop-product", 1, "analysis-ik", "analysis-pinyin")
	f := newFakeCluster()
	opts := baseCutoverOpts(f, def)
	opts.IngestCheck = func(context.Context, string) error { return nil }

	res, _ := RunCutover(context.Background(), opts)
	if res == nil {
		t.Fatal("预检放行后必须进入割接编排，不能提前报 nil")
	}
	// 护栏验证重点：预检放行后必须真实进入建索引步，至少 PUT /<target> 一次；
	// 否则就是护栏把正常路径也堆死了。
	if !res.Created {
		t.Errorf("预检放行后必须进到建索引步（res.Created=true），实际 res=%+v", res)
	}
	var sawCreate bool
	for _, op := range f.opsOfKind("write") {
		if strings.HasPrefix(op, "PUT /shop-product_v1") && op != "PUT /shop-product_v1/_settings" && op != "PUT /shop-product_v1/_refresh" {
			sawCreate = true
			break
		}
	}
	if !sawCreate {
		t.Errorf("预检放行后必须实际写 PUT /shop-product_v1（建索引），writes=%v", f.opsOfKind("write"))
	}
}

// TestRunCutover_NilIngestCheckKeepsLegacyBehavior 守护向后兼容：未注入 IngestCheck
// （既有测试与任何外部调用方）时，割接路径与改造前逐字一致，护栏只是可选增强。
func TestRunCutover_NilIngestCheckKeepsLegacyBehavior(t *testing.T) {
	def := mustCutoverDef(t, "shop-product", 1, "analysis-ik", "analysis-pinyin")
	f := newFakeCluster()
	opts := baseCutoverOpts(f, def)
	// opts.IngestCheck 保持零值 nil。
	res, _ := RunCutover(context.Background(), opts)
	if res == nil {
		t.Fatal("nil IngestCheck 必须完全跳过预检、照常进入割接编排")
	}
	if !res.Created {
		t.Errorf("nil IngestCheck 却未进到建索引步，说明预检默认拦了下层：res=%+v", res)
	}
}
