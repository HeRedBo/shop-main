package sync

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	golangsync "sync"
	"sync/atomic"
	"testing"
	"time"

	"shop/internal/es"
	"shop/internal/es/admin"
)

// ---------------------------------------------------------------------------
// fake provider / writer
// ---------------------------------------------------------------------------

// fakeProvider 是测试用的 DocProvider 实现。
type fakeProvider struct {
	def       *es.IndexDefinition
	bounds    Bounds
	scanErr   error
	scanPanic bool
	scanDelay time.Duration
	docs      []admin.Doc
}

func (f *fakeProvider) Definition() *es.IndexDefinition { return f.def }
func (f *fakeProvider) Bounds(ctx context.Context) (Bounds, error) {
	return f.bounds, nil
}
func (f *fakeProvider) ScanRange(ctx context.Context, r IDRange, emit func(ProviderBatch) error) error {
	if f.scanPanic {
		panic("scanner panic simulated")
	}
	if f.scanErr != nil {
		return f.scanErr
	}
	if f.scanDelay > 0 {
		select {
		case <-time.After(f.scanDelay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return emit(f.docs)
}
func (f *fakeProvider) Assemble(batch ProviderBatch) ([]admin.Doc, error) {
	docs, ok := batch.([]admin.Doc)
	if !ok {
		return nil, fmt.Errorf("fakeProvider: 非预期批次类型 %T", batch)
	}
	return docs, nil
}

// holdWriter 模拟 writer 阻塞：收到前 holdAfter 批后永久阻塞（阻塞在 barrier 上）。
// writeCall 在每次 write 入口时原子递增，barrier 控制是否阻塞。
type holdWriter struct {
	barrier   chan struct{}
	holdAfter int64
	calls     atomic.Int64
}

func newHoldWriter(holdAfter int64) *holdWriter {
	return &holdWriter{barrier: make(chan struct{}), holdAfter: holdAfter}
}

func (h *holdWriter) write(ctx context.Context, target string, docs []admin.Doc) (*admin.BulkResult, error) {
	n := h.calls.Add(1)
	if n > h.holdAfter {
		// 超过配额 → 永久阻塞，直到 ctx 取消
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return &admin.BulkResult{Sent: len(docs), Failed: 0}, nil
}

// fakeWriter 是简单 WriteFunc。
type fakeWriter struct {
	result *admin.BulkResult
	err    error
	delay  time.Duration
}

func (f *fakeWriter) write(ctx context.Context, target string, docs []admin.Doc) (*admin.BulkResult, error) {
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.err != nil {
		return nil, f.err
	}
	if f.result != nil {
		return f.result, nil
	}
	return &admin.BulkResult{Sent: len(docs), Failed: 0}, nil
}

// multiBatchProvider 发射多个批次（反压测试用）。
type multiBatchProvider struct {
	def      *es.IndexDefinition
	bounds   Bounds
	docs     []admin.Doc
	batchCnt int
	emitted  atomic.Int64
}

func (m *multiBatchProvider) Definition() *es.IndexDefinition { return m.def }
func (m *multiBatchProvider) Bounds(ctx context.Context) (Bounds, error) {
	return m.bounds, nil
}
func (m *multiBatchProvider) ScanRange(ctx context.Context, r IDRange, emit func(ProviderBatch) error) error {
	for i := 0; i < m.batchCnt; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := emit(m.docs); err != nil {
			return err
		}
		m.emitted.Add(1)
	}
	return nil
}
func (m *multiBatchProvider) Assemble(batch ProviderBatch) ([]admin.Doc, error) {
	docs, ok := batch.([]admin.Doc)
	if !ok {
		return nil, fmt.Errorf("multiBatchProvider: 非预期批次类型 %T", batch)
	}
	return docs, nil
}

// ---------------------------------------------------------------------------
// 基本功能测试
// ---------------------------------------------------------------------------

func TestRun_EmptyTable(t *testing.T) {
	p := &fakeProvider{
		def:    &es.IndexDefinition{},
		bounds: Bounds{Min: 0, Max: 0, Total: 0},
	}
	w := &fakeWriter{}
	stats, err := Run(context.Background(), p, Options{Index: "test"}, w.write)
	if err != nil {
		t.Fatalf("空表不应报错: %v", err)
	}
	if stats.TotalDocs.Load() != 0 {
		t.Errorf("空表 TotalDocs 应为 0，实际 %d", stats.TotalDocs.Load())
	}
}

func TestRun_NormalEnd(t *testing.T) {
	docs := []admin.Doc{{ID: "1", Src: "doc1"}, {ID: "2", Src: "doc2"}}
	p := &fakeProvider{
		def:    &es.IndexDefinition{},
		bounds: Bounds{Min: 1, Max: 100, Total: 2},
		docs:   docs,
	}
	w := &fakeWriter{}
	stats, err := Run(context.Background(), p, Options{Index: "test"}, w.write)
	if err != nil {
		t.Fatalf("正常结束不应报错: %v", err)
	}
	if stats.SentDocs.Load() < 2 {
		t.Errorf("期望 SentDocs>=2，实际 %d", stats.SentDocs.Load())
	}
	if stats.Batches.Load() < 1 {
		t.Errorf("期望 Batches>=1，实际 %d", stats.Batches.Load())
	}
}

func TestRun_ScannerError(t *testing.T) {
	p := &fakeProvider{
		def:     &es.IndexDefinition{},
		bounds:  Bounds{Min: 1, Max: 100, Total: 100},
		scanErr: errors.New("scanner error simulated"),
	}
	w := &fakeWriter{}
	_, err := Run(context.Background(), p, Options{Index: "test"}, w.write)
	if err == nil {
		t.Fatal("scanner 报错应传播到 Run 返回")
	}
	if !strings.Contains(err.Error(), "scanner error") {
		t.Errorf("错误信息应包含 'scanner error'，实际: %v", err)
	}
}

func TestRun_ScannerPanic(t *testing.T) {
	p := &fakeProvider{
		def:       &es.IndexDefinition{},
		bounds:    Bounds{Min: 1, Max: 100, Total: 100},
		scanPanic: true,
	}
	w := &fakeWriter{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, err := Run(context.Background(), p, Options{Index: "test"}, w.write)
		if err == nil {
			t.Error("scanner panic应导致 Run 返回错误")
		}
		if !strings.Contains(err.Error(), "panic") {
			t.Errorf("错误信息应包含 'panic'，实际: %v", err)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("scanner panic 导致死锁（5s 超时）")
	}
}

func TestRun_WriterError(t *testing.T) {
	p := &fakeProvider{
		def:    &es.IndexDefinition{},
		bounds: Bounds{Min: 1, Max: 100, Total: 100},
		docs:   []admin.Doc{{ID: "1", Src: "doc1"}},
	}
	w := &fakeWriter{err: errors.New("writer error simulated")}
	_, err := Run(context.Background(), p, Options{Index: "test"}, w.write)
	if err == nil {
		t.Fatal("writer 报错应传播到 Run 返回")
	}
	if !strings.Contains(err.Error(), "writer") && !strings.Contains(err.Error(), "bulk") {
		t.Errorf("错误信息应包含 writer/bulk 相关，实际: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 确定性防护测试（缺陷 #4 修复）
// ---------------------------------------------------------------------------

// TestRun_Backpressure 确定性反压测试：
// writer 在处理 holdAfter 批后永久阻塞 → docsCh 满 → scanner emit 被阻塞。
// 断言 writer 实际处理数 ≤ docsCh 容量 + 1（正在处理的那批），证明有界 channel 确实反压了 scanner。
// cancel ctx 后 Run 必须返回 —— 证明 emit select 的 case <-gctx.Done() 有效。
//
// 鉴别力：若 runner.go:137 的 case <-gctx.Done() 被删除，scanner 将永久阻塞在 emit，
// cancel 后 Run 无法返回，本测试 3s 超时失败。
// 若 docsCh 改成无界，writer 处理数不受限，本测试的容量断言失败。
func TestRun_Backpressure(t *testing.T) {
	// concurrency=1 → docsCh 容量 = 1*2 = 2
	// holdAfter=2：writer 处理前 2 批后阻塞
	hw := newHoldWriter(2)

	p := &multiBatchProvider{
		def:      &es.IndexDefinition{},
		bounds:   Bounds{Min: 1, Max: 1000, Total: 1000},
		docs:     []admin.Doc{{ID: "1", Src: "doc1"}},
		batchCnt: 50,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var runErr error
	go func() {
		defer close(done)
		_, runErr = Run(ctx, p, Options{Index: "test", Concurrency: 1, RangeMultiplier: 1, BulkSize: 1}, hw.write)
	}()

	// 等 writer 达到阻塞点（前 2 批处理完，第 3 批开始阻塞）
	deadline := time.After(3 * time.Second)
	for {
		if hw.calls.Load() >= 3 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("writer 3s 内未收到第 3 批（calls=%d）", hw.calls.Load())
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}

	// 给 scanner 一点时间尝试继续 emit（docsCh 应该已满）
	time.Sleep(100 * time.Millisecond)

	// 断言：scanner 实际发射的批次数被有界 channel 反压限制。
	// docsCh 容量 = Concurrency * 2 = 1 * 2 = 2，writer 在 holdAfter=2 后阻塞，
	// emitted 上界 = cap(2) + writer处理中(1) + 竞态容差(1) ≈ 4~5。
	// 若 docsCh 改为无界，scanner 会一口气发射全部 50 批，emitted=50，该断言必红。
	emitted := p.emitted.Load()
	if emitted > int64(6) {
		t.Errorf("scanner emitted=%d 超出 docsCh 容量反压上界（期望≤6），channel 可能无界", emitted)
	}

	// cancel → Run 必须返回
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("cancel 后 Run 3s 内未返回（emit 未被 ctx 取消打断）")
	}
	if runErr == nil {
		t.Fatal("cancel 后 Run 应返回错误")
	}
}

// TestRun_ContextCancelInterruptsSend 确定性取消测试：
// writer 从第一批就阻塞，cancel() 后 Run 必须在 2s 内返回，
// 且错误是 context.Canceled。
//
// 鉴别力：若 runner.go:137 的 case <-gctx.Done() 被删除，
// scanner 永久阻塞在 emit → Run 不返回 → 测试超时失败。
func TestRun_ContextCancelInterruptsSend(t *testing.T) {
	hw := newHoldWriter(0) // 从第一批就阻塞

	p := &multiBatchProvider{
		def:      &es.IndexDefinition{},
		bounds:   Bounds{Min: 1, Max: 1000, Total: 1000},
		docs:     []admin.Doc{{ID: "1", Src: "doc1"}},
		batchCnt: 50,
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	var runErr error
	go func() {
		defer close(done)
		_, runErr = Run(ctx, p, Options{Index: "test", Concurrency: 1, RangeMultiplier: 1, BulkSize: 1}, hw.write)
	}()

	// 等 writer 进入阻塞
	time.Sleep(100 * time.Millisecond)

	// cancel → Run 必须被打断
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run 未被 ctx 取消打断（2s 超时）")
	}

	// 硬断言：错误必须是 context.Canceled
	if runErr == nil {
		t.Fatal("cancel 后 Run 应返回错误")
	}
	if !errors.Is(runErr, context.Canceled) {
		t.Errorf("期望 context.Canceled，实际: %v", runErr)
	}
}

// TestRun_GoroutineLeak 确定性泄漏检测：
// Run 返回后 runtime.GC() + 收敛窗口，断言 goroutine 数严格回到基线。
func TestRun_GoroutineLeak(t *testing.T) {
	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	p := &fakeProvider{
		def:    &es.IndexDefinition{},
		bounds: Bounds{Min: 1, Max: 10, Total: 5},
		docs:   []admin.Doc{{ID: "1", Src: "doc1"}},
	}
	w := &fakeWriter{}

	_, err := Run(context.Background(), p, Options{Index: "test", Concurrency: 2}, w.write)
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}

	for i := 0; i < 20; i++ {
		runtime.GC()
		time.Sleep(50 * time.Millisecond)
		if diff := runtime.NumGoroutine() - baseline; diff == 0 {
			return
		}
	}
	t.Errorf("goroutine 泄漏：基线 %d，结束后 %d，差异 %d（要求 diff==0）",
		baseline, runtime.NumGoroutine(), runtime.NumGoroutine()-baseline)
}

// TestRun_GoroutineLeakOnError 验证错误路径也无 goroutine 泄漏。
func TestRun_GoroutineLeakOnError(t *testing.T) {
	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	p := &fakeProvider{
		def:     &es.IndexDefinition{},
		bounds:  Bounds{Min: 1, Max: 100, Total: 100},
		scanErr: errors.New("scanner error"),
	}
	w := &fakeWriter{}
	_, _ = Run(context.Background(), p, Options{Index: "test", Concurrency: 2}, w.write)

	for i := 0; i < 20; i++ {
		runtime.GC()
		time.Sleep(50 * time.Millisecond)
		if diff := runtime.NumGoroutine() - baseline; diff == 0 {
			return
		}
	}
	t.Errorf("错误路径 goroutine 泄漏：基线 %d，结束后 %d", baseline, runtime.NumGoroutine())
}

// TestRun_GoroutineLeakOnCancel 验证取消路径也无 goroutine 泄漏。
func TestRun_GoroutineLeakOnCancel(t *testing.T) {
	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	p := &fakeProvider{
		def:       &es.IndexDefinition{},
		bounds:    Bounds{Min: 1, Max: 100, Total: 100},
		scanDelay: 10 * time.Second,
		docs:      []admin.Doc{{ID: "1", Src: "doc1"}},
	}
	w := &fakeWriter{}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, _ = Run(ctx, p, Options{Index: "test", Concurrency: 2}, w.write)

	for i := 0; i < 20; i++ {
		runtime.GC()
		time.Sleep(50 * time.Millisecond)
		if diff := runtime.NumGoroutine() - baseline; diff == 0 {
			return
		}
	}
	t.Errorf("取消路径 goroutine 泄漏：基线 %d，结束后 %d", baseline, runtime.NumGoroutine())
}

// ---------------------------------------------------------------------------
// 必修② 回归防护：cancel 分支必须用独立 ctx 排空尾部缓冲，计数如实
// ---------------------------------------------------------------------------

// tailHoldProvider 发射一批文档后阻塞到 ctx 取消 —— 使这批文档在取消瞬间
// 仍留在 writer 的尾部缓冲里（配合 BulkSize 大于文档数，热路径不会自动 flush）。
type tailHoldProvider struct {
	def       *es.IndexDefinition
	docs      []admin.Doc
	ready     chan struct{} // Assemble 被调用后关闭：表示尾部缓冲已成型
	readyOnce golangsync.Once
}

func (p *tailHoldProvider) Definition() *es.IndexDefinition { return p.def }
func (p *tailHoldProvider) Bounds(context.Context) (Bounds, error) {
	return Bounds{Min: 1, Max: int64(len(p.docs)), Total: int64(len(p.docs))}, nil
}
func (p *tailHoldProvider) ScanRange(ctx context.Context, _ IDRange, emit func(ProviderBatch) error) error {
	if err := emit(p.docs); err != nil {
		return err
	}
	<-ctx.Done() // 卡在源头，保证尾部只剩 writer 缓冲里那一批
	return ctx.Err()
}
func (p *tailHoldProvider) Assemble(batch ProviderBatch) ([]admin.Doc, error) {
	docs, ok := batch.([]admin.Doc)
	if !ok {
		return nil, fmt.Errorf("tailHoldProvider: 非预期批次类型 %T", batch)
	}
	p.readyOnce.Do(func() { close(p.ready) })
	return docs, nil
}

// TestRun_CancelFlushesTailWithIndependentCtx 锁定必修②：
//  1. ctx 已取消时，writer 仍必须提交尾部缓冲，且提交用的 ctx 不得是被取消的那个
//     （否则 bulk 必然 canceled —— 尾部要么静默丢弃，要么把真实取消原因覆盖成假「bulk 写入失败」）；
//  2. 尾部文档必须计入 SentDocs（不静默消失）。
//
// 鉴别力：把 tailFlush 退化成 flush(ctx)（已取消的 runCtx），假写出口会在入口看到
// ctx.Err()!=nil，且因 err 分支不再累加 SentDocs —— 两条断言同时变红；
// 把 ctx.Done() 分支退化成「只 mergeLocal 直接 return」，SentDocs==0 同样变红。
func TestRun_CancelFlushesTailWithIndependentCtx(t *testing.T) {
	tail := []admin.Doc{{ID: "91", Src: "d91"}, {ID: "92", Src: "d92"}}
	p := &tailHoldProvider{
		def:   &es.IndexDefinition{},
		docs:  tail,
		ready: make(chan struct{}),
	}

	var (
		mu           golangsync.Mutex
		cancelledCtx int
		written      int
	)
	w := func(ctx context.Context, _ string, docs []admin.Doc) (*admin.BulkResult, error) {
		mu.Lock()
		written += len(docs)
		if ctx.Err() != nil {
			cancelledCtx++
		}
		mu.Unlock()
		return &admin.BulkResult{Sent: len(docs)}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var stats *SyncStats
	var runErr error
	go func() {
		defer close(done)
		// BulkSize=100 > 2 条文档：热路径永不自动 flush，尾部缓冲只能靠退出路径提交。
		stats, runErr = Run(ctx, p, Options{
			Index: "test", Concurrency: 1, RangeMultiplier: 1, BulkSize: 100,
		}, w)
	}()

	select {
	case <-p.ready:
	case <-time.After(3 * time.Second):
		t.Fatal("3s 内尾部缓冲未成型（writer 未收到批次）")
	}
	time.Sleep(50 * time.Millisecond) // 让 writer 将文档落入 buf 后回到 select 等待

	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancel 后 Run 5s 未返回")
	}

	mu.Lock()
	defer mu.Unlock()
	if cancelledCtx > 0 {
		t.Errorf("尾部 bulk 有 %d 次跑在已取消的 ctx 上 —— 必修② 未生效（须用 WithoutCancel 派生）", cancelledCtx)
	}
	if written != len(tail) {
		t.Errorf("尾部缓冲提交文档数 = %d, 期望 %d（不得静默丢弃）", written, len(tail))
	}
	if got := stats.SentDocs.Load(); got != int64(len(tail)) {
		t.Errorf("SentDocs = %d, 期望 %d（尾部不计入即为静默丢弃）", got, len(tail))
	}
	if !errors.Is(runErr, context.Canceled) {
		t.Errorf("外部 cancel 应如实返回 context.Canceled，实际: %v", runErr)
	}
}

// TestRun_CancelTailDrainFailureIsCounted 锁定必修② 的「计数如实」侧：
// 排空提交真实失败时，失败必须计入 FailedDocs/Failures，且暴露的原因就是写出口
// 返回的真实错误，不能是被覆盖出来的 context.Canceled。
func TestRun_CancelTailDrainFailureIsCounted(t *testing.T) {
	cause := errors.New("es 连接被重置")
	tail := []admin.Doc{{ID: "71", Src: "d71"}, {ID: "72", Src: "d72"}}
	p := &tailHoldProvider{
		def:   &es.IndexDefinition{},
		docs:  tail,
		ready: make(chan struct{}),
	}

	var seenCtxNil atomic.Bool
	w := func(ctx context.Context, _ string, docs []admin.Doc) (*admin.BulkResult, error) {
		seenCtxNil.Store(ctx.Err() == nil)
		return nil, cause
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var stats *SyncStats
	var runErr error
	go func() {
		defer close(done)
		stats, runErr = Run(ctx, p, Options{
			Index: "test", Concurrency: 1, RangeMultiplier: 1, BulkSize: 100,
		}, w)
	}()

	select {
	case <-p.ready:
	case <-time.After(3 * time.Second):
		t.Fatal("3s 内尾部缓冲未成型")
	}
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancel 后 Run 5s 未返回")
	}

	if !seenCtxNil.Load() {
		t.Error("排空提交用的是已取消 ctx —— 错误原因会被覆盖成假 canceled")
	}
	if got := stats.FailedDocs.Load(); got != int64(len(tail)) {
		t.Errorf("FailedDocs = %d, 期望 %d（请求级失败必须逐条如实计入）", got, len(tail))
	}
	if len(stats.Failures) != len(tail) {
		t.Fatalf("Failures = %d 条, 期望 %d 条", len(stats.Failures), len(tail))
	}
	for _, f := range stats.Failures {
		if f.Type != "bulk_request_failed" {
			t.Errorf("失败明细 Type = %q, 期望 bulk_request_failed", f.Type)
		}
	}
	if !errors.Is(runErr, cause) {
		t.Errorf("Run 返回错误未包含真实原因 %v，实际: %v", cause, runErr)
	}
	if errors.Is(runErr, context.Canceled) {
		t.Errorf("真实失败原因被取消错误覆盖: %v", runErr)
	}
}

// ---------------------------------------------------------------------------
// 第六轮 P0 回归防护：writer ctx 不得被 errgroup 正常收敛取消
// ---------------------------------------------------------------------------

// TestRun_WriterCtxAliveDuringDrain 锁定 errgroup gctx 提前 cancel 回归：
// scanner 正常完成 → errgroup 取消 gctx → 如果在途 writer 仍用 gctx，
// 后续 bulk 调用会收到已 Done 的 ctx，导致「context canceled」失败。
//
// 鉴别力：若 runner.go writer 使用 gctx（而非 runCtx），scanner 快速完成后
// gctx 被取消，writer 后续 write 调用看到 ctx.Done()=true，本测试变红。
func TestRun_WriterCtxAliveDuringDrain(t *testing.T) {
	const batchCnt = 20
	p := &multiBatchProvider{
		def:      &es.IndexDefinition{},
		bounds:   Bounds{Min: 1, Max: int64(batchCnt), Total: int64(batchCnt)},
		docs:     []admin.Doc{{ID: "1", Src: "doc1"}},
		batchCnt: batchCnt,
	}

	var (
		mu        golangsync.Mutex
		cancelled int // write() 入口时 ctx 已 Done 的次数
		total     int // 总 write 调用次数
	)

	w := func(ctx context.Context, target string, docs []admin.Doc) (*admin.BulkResult, error) {
		// 模拟真实 bulk 耗时（网络往返），使 writer 在 scanner 完成后仍在处理
		time.Sleep(30 * time.Millisecond)
		mu.Lock()
		total++
		if ctx.Err() != nil {
			cancelled++
		}
		mu.Unlock()
		return &admin.BulkResult{Sent: len(docs), Failed: 0}, nil
	}

	stats, err := Run(context.Background(), p, Options{
		Index: "test", Concurrency: 1, RangeMultiplier: 1, BulkSize: 1,
	}, w)
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if stats.SentDocs.Load() != int64(batchCnt) {
		t.Errorf("SentDocs = %d, 期望 %d", stats.SentDocs.Load(), batchCnt)
	}
	if cancelled > 0 {
		t.Fatalf("writer 在 %d/%d 次调用时发现 ctx 已 Done — "+
			"scanner 正常完成后 gctx 被 errgroup 取消，杀掉了在途 bulk", cancelled, total)
	}
}
