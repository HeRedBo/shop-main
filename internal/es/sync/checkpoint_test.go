package sync

// checkpoint_test.go 钉住必修①（BLOCKER）：checkpoint 的 LastID 只允许推进到
// 「已完整覆盖的最小连续覆盖右边界」。并发 scanner 乱序完成时，高 id 区间的进度
// 绝不能把游标推到尚未扫描的低 id 区间之前——否则中断后 --resume 按 id > LastID
// 扫描会整段跳过低位区间，形成静默丢数。
//
// 修复前的 RED 复现（已确认失败：LastID=100 越过未扫描的 (0,25]）：
//   生产调用形态 product.go ScanRange → sink.observe(批次尾 id)，sink 不知道区间归属，
//   依次 observe(90) observe(60) observe(50) observe(75) observe(100) 后
//   loadCheckpoint 得到 LastID=100，而区间1 (0,25] 一个字节都没扫到。
// 修复后同一场景断言见本文件（observeRange/completeRange 按区间记账）。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	golangsync "sync"
	"testing"
	"time"

	"shop/internal/es"
	"shop/internal/es/admin"
)

// cpTestPlan 是 4 段等宽、首尾相接的区间计划 (0,25] (25,50] (50,75] (75,100]。
func cpTestPlan() []IDRange {
	return []IDRange{{Lo: 0, Hi: 25}, {Lo: 25, Hi: 50}, {Lo: 50, Hi: 75}, {Lo: 75, Hi: 100}}
}

func newCpSink(t *testing.T) (string, *checkpointSink) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cp.json")
	sink := newCheckpointSink(path, "test-idx")
	sink.setTotal(100)
	sink.planRanges(cpTestPlan())
	return path, sink
}

// cpLastID 读回落盘游标（断言持久化状态，而非内存字段）。
// 文件不存在 = 游标从未前进（sink 只在连续覆盖前进时落盘），返回 0。
func cpLastID(t *testing.T, path string) int64 {
	t.Helper()
	cp, err := loadCheckpoint(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatalf("读取 checkpoint 失败: %v", err)
	}
	return cp.LastID
}

// TestCheckpoint_RangesOutOfOrder_DoesNotAdvancePastContiguousBoundary 复现并钉住 BLOCKER。
// 高位区间先完成、低位区间 (0,25] 尚未开始时，游标必须保持 0（宁可重扫不可漏扫）。
func TestCheckpoint_RangesOutOfOrder_DoesNotAdvancePastContiguousBoundary(t *testing.T) {
	path, sink := newCpSink(t)
	plan := cpTestPlan()

	// 乱序完成：区间4 完整、区间3 进行中、区间2 完整；区间1 从未开始。
	sink.observeRange(plan[3], 90)
	sink.completeRange(plan[3])
	sink.observeRange(plan[2], 60)
	sink.observeRange(plan[1], 50)
	sink.completeRange(plan[1])

	if got := cpLastID(t, path); got != 0 {
		t.Fatalf("LastID=%d 越过了尚未扫描的区间 (0,25]：--resume 会整段跳过低位数据（静默丢数）", got)
	}
}

// TestCheckpoint_FrontierAdvancesOnlyWhenContiguousCovered 断言游标随「连续覆盖」推进的完整状态序列。
// 破坏矩阵：把 frontierLocked 改成「取所有区间进度的最大值」→ 第 1/2/3 步即变红；
// 把 done 判定丢掉 → 第 4 步（区间1 完成后应推进到 25）变红。
func TestCheckpoint_FrontierAdvancesOnlyWhenContiguousCovered(t *testing.T) {
	path, sink := newCpSink(t)
	plan := cpTestPlan()

	// 1) 只有高位区间完成 → 连续覆盖仍为 0。
	sink.completeRange(plan[3])
	sink.completeRange(plan[2])
	if got := cpLastID(t, path); got != 0 {
		t.Fatalf("低位区间未覆盖时游标应为 0，实际 %d", got)
	}

	// 2) 区间2 也完成，但区间1 未开始 → 依旧 0。
	sink.completeRange(plan[1])
	if got := cpLastID(t, path); got != 0 {
		t.Fatalf("区间1 (0,25] 未开始时游标应为 0，实际 %d", got)
	}

	// 3) 区间1 进行中：游标只能推到区间1 内部已连续读出的 10，不得借后续区间凑到 50/75/100。
	sink.observeRange(plan[0], 10)
	if got := cpLastID(t, path); got != 10 {
		t.Fatalf("区间1 进行中时游标应为区间内连续右端 10，实际 %d", got)
	}

	// 4) 区间1 完成 → 连续覆盖跨过 25；区间2/3/4 早已完成 → 一次性推进到全表右边界 100。
	sink.completeRange(plan[0])
	if got := cpLastID(t, path); got != 100 {
		t.Fatalf("区间1 完成后连续覆盖应直达 100，实际 %d", got)
	}

	// 5) 游标单调不回退：迟到的低位进度不得把游标拉回去。
	sink.observeRange(plan[0], 5)
	if got := cpLastID(t, path); got != 100 {
		t.Fatalf("游标不得回退，实际 %d", got)
	}
}

// TestCheckpoint_InProgressLeadingRangeCapsCursor 钉住「进行中区间」封顶语义：
// 只要第一个未完成区间还在 (Lo,Hi] 内部，其后所有已完整覆盖的区间都不能把游标推过它的右边界。
func TestCheckpoint_InProgressLeadingRangeCapsCursor(t *testing.T) {
	path, sink := newCpSink(t)
	plan := cpTestPlan()

	sink.completeRange(plan[1])
	sink.completeRange(plan[2])
	sink.completeRange(plan[3])
	sink.observeRange(plan[0], 18)
	if got := cpLastID(t, path); got != 18 {
		t.Fatalf("游标应被区间1 的连续右端 18 封顶，实际 %d", got)
	}

	sink.observeRange(plan[0], 24)
	if got := cpLastID(t, path); got != 24 {
		t.Fatalf("区间1 内部前进应反映到游标（期望 24），实际 %d", got)
	}
}

// TestCheckpoint_ConcurrentRangesFinalCursorIsCompleteCoverage 用 -race 验证并发按区间记账：
// 4 个 goroutine 各自扫一个区间、乱序完成，全部完成后落盘游标必须等于全表右边界，
// 且文件始终可解析（锁内落盘，后写不得被旧值覆盖）。
func TestCheckpoint_ConcurrentRangesFinalCursorIsCompleteCoverage(t *testing.T) {
	path, sink := newCpSink(t)
	plan := cpTestPlan()

	var wg golangsync.WaitGroup
	for i := len(plan) - 1; i >= 0; i-- {
		r := plan[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			// 高 id 区间先跑，制造乱序到达。
			time.Sleep(time.Duration(i) * 5 * time.Millisecond)
			sink.observeRange(r, r.Lo+3)
			sink.observeRange(r, r.Hi-1)
			sink.completeRange(r)
		}()
	}
	wg.Wait()

	if got := cpLastID(t, path); got != 100 {
		t.Fatalf("全部区间完整覆盖后游标应为 100，实际 %d", got)
	}
}

// TestPlanRanges_TruncatesAtGap 钉住区间计划的空洞截断：
// 出现 (Lo,Hi] 不相接时，只有从第一个区间开始的连续前缀参与游标推进。
func TestPlanRanges_TruncatesAtGap(t *testing.T) {
	sink := newCheckpointSink(filepath.Join(t.TempDir(), "cp.json"), "t")
	sink.planRanges([]IDRange{
		{Lo: 50, Hi: 75},
		{Lo: 0, Hi: 25},
		{Lo: 25, Hi: 50},
		{Lo: 75, Hi: 100},
	})

	got := sink.plan
	if len(got) != 4 {
		t.Fatalf("乱序输入应按 Lo 升序登记完整连续计划，实际 %v", got)
	}
	if got[0].Lo != 0 || got[3].Hi != 100 {
		t.Fatalf("区间计划排序错误：%v", got)
	}

	// 制造空洞：缺 (25,50] 的相邻关系 → 只保留 (0,25] 一段参与推进。
	sink2 := newCheckpointSink(filepath.Join(t.TempDir(), "cp2.json"), "t")
	sink2.planRanges([]IDRange{{Lo: 0, Hi: 25}, {Lo: 50, Hi: 75}, {Lo: 75, Hi: 100}})
	if len(sink2.plan) != 1 {
		t.Fatalf("空洞之后区间应被截断，只保留连续前缀，实际 plan=%v", sink2.plan)
	}
	sink2.completeRange(IDRange{Lo: 50, Hi: 75})
	if f := sink2.frontierLocked(); f != 0 {
		t.Fatalf("空洞后的完成不得推进游标，实际 frontier=%d", f)
	}
}

// ---------------------------------------------------------------------------
// provider / runner 接线钉测（破坏即红）
// ---------------------------------------------------------------------------

// TestProductProvider_PlanRangesForwardsToSink 锁定 provider → sink 的区间计划接线。
// 破坏矩阵：删掉 PlanRanges 里 pp.ck.planRanges 调用 → sink.plan 为空 → 变红。
func TestProductProvider_PlanRangesForwardsToSink(t *testing.T) {
	sink := newCheckpointSink(filepath.Join(t.TempDir(), "cp.json"), "t")
	pp := &productProvider{ck: sink}
	plan := cpTestPlan()

	pp.PlanRanges(plan)

	if len(sink.plan) != len(plan) {
		t.Fatalf("PlanRanges 必须把区间计划转交 sink，实际 sink.plan=%v", sink.plan)
	}

	// 区间计划未登记前无法判定连续覆盖 → 游标必须保守不推进（宁可重扫不可漏扫）。
	unplannedPath := filepath.Join(t.TempDir(), "unplanned.json")
	unplanned := newCheckpointSink(unplannedPath, "t")
	unplanned.observeRange(IDRange{Lo: 0, Hi: 25}, 20)
	if _, err := os.Stat(unplannedPath); !os.IsNotExist(err) {
		t.Fatalf("未登记区间计划时不得落盘游标（保守不推进），实际文件已存在: %v", err)
	}
}

// TestProductProvider_MarkRangeDone 锁定 ScanRange 的完成登记口径：
// 只有游标正常走到底（err==nil）才登记完整覆盖；取消/超时/limit 打断时不得登记。
// 破坏矩阵：把 markRangeDone 改成无条件 completeRange → 第二个子断言变红。
func TestProductProvider_MarkRangeDone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cp.json")
	sink := newCheckpointSink(path, "t")
	sink.planRanges(cpTestPlan())
	pp := &productProvider{ck: sink}

	r0, r1 := IDRange{Lo: 0, Hi: 25}, IDRange{Lo: 25, Hi: 50}

	pp.markRangeDone(r0, errors.New("context canceled"))
	if f := cpLastID(t, path); f != 0 {
		t.Fatalf("ScanRange 报错时不得登记完整覆盖，游标应为 0，实际 %d", f)
	}

	pp.ck.observeRange(r0, 12)
	if f := cpLastID(t, path); f != 12 {
		t.Fatalf("区间内进度应反映为 12，实际 %d", f)
	}

	pp.markRangeDone(r0, nil)
	if f := cpLastID(t, path); f != 25 {
		t.Fatalf("区间1 正常走完应把游标推到 25，实际 %d", f)
	}

	pp.markRangeDone(r1, nil)
	if f := cpLastID(t, path); f != 50 {
		t.Fatalf("区间1+2 完整覆盖应推到 50，实际 %d", f)
	}

	// 区间3/4 尚未覆盖 → 迟到的低位进度不得回退游标。
	sink.completeRange(IDRange{Lo: 50, Hi: 75})
	sink.completeRange(IDRange{Lo: 75, Hi: 100})
	if f := cpLastID(t, path); f != 100 {
		t.Fatalf("全区间覆盖后游标应为 100，实际 %d", f)
	}
}

// planCarrierProvider 是实现 RangePlanCarrier 的 fake provider：
// 捕获 Run 传入的区间计划，并按与 product.go 相同的口径登记进度。
type planCarrierProvider struct {
	def    *es.IndexDefinition
	bounds Bounds
	docs   []admin.Doc

	mu      golangsync.Mutex
	gotPlan []IDRange
	sink    *checkpointSink
}

func (p *planCarrierProvider) Definition() *es.IndexDefinition { return p.def }
func (p *planCarrierProvider) Bounds(context.Context) (Bounds, error) {
	return p.bounds, nil
}
func (p *planCarrierProvider) PlanRanges(ranges []IDRange) {
	p.mu.Lock()
	p.gotPlan = append([]IDRange(nil), ranges...)
	p.mu.Unlock()
	p.sink.planRanges(ranges)
}
func (p *planCarrierProvider) ScanRange(ctx context.Context, r IDRange, emit func(ProviderBatch) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.sink.observeRange(r, r.Hi-1)
	if err := emit(p.docs); err != nil {
		return err
	}
	p.sink.completeRange(r)
	return nil
}
func (p *planCarrierProvider) Assemble(batch ProviderBatch) ([]admin.Doc, error) {
	docs, ok := batch.([]admin.Doc)
	if !ok {
		return nil, fmt.Errorf("planCarrierProvider: 非预期批次类型 %T", batch)
	}
	return docs, nil
}

// TestRun_HandsRangePlanToProvider 锁定 Run → RangePlanCarrier 的接线：
// provider 必须拿到与 ComputeRanges(bounds, k) 逐字一致的区间计划（k>1 并行不退化）。
// 破坏矩阵：删掉 runner.go 的 rp.PlanRanges(ranges) → gotPlan 为空 → 变红；
// 把 k 强行降成 1（退化单区间）→ 区间数断言变红（项目硬要求保留区间并行提速）。
func TestRun_HandsRangePlanToProvider(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cp.json")
	sink := newCheckpointSink(path, "test-idx")
	p := &planCarrierProvider{
		def:    &es.IndexDefinition{},
		bounds: Bounds{Min: 1, Max: 800, Total: 800},
		docs:   []admin.Doc{{ID: "1", Src: "doc1"}},
		sink:   sink,
	}
	w := &fakeWriter{}

	if _, err := Run(context.Background(), p, Options{
		Index: "test", Concurrency: 2, RangeMultiplier: 4,
	}, w.write); err != nil {
		t.Fatalf("Run 失败: %v", err)
	}

	want := ComputeRanges(p.bounds, ComputeRangeCount(2, 4))
	p.mu.Lock()
	got := append([]IDRange(nil), p.gotPlan...)
	p.mu.Unlock()

	if len(got) != len(want) || len(want) != 8 {
		t.Fatalf("Run 必须把 k=%d 的区间计划交给 provider（区间并行不得退化），got=%d want=%d", 8, len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("区间计划第 %d 段不符：got=%v want=%v", i, got[i], want[i])
		}
	}
	// 全部区间完整覆盖 → 落盘游标等于全表右边界。
	if f := cpLastID(t, path); f != p.bounds.Max {
		t.Fatalf("全区间覆盖后落盘游标应为 Max=%d，实际 %d", p.bounds.Max, f)
	}
}
