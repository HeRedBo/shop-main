package sync

import (
	"context"
	"errors"
	"fmt"
	golangsync "sync"
	"time"

	"shop/internal/es/admin"

	"golang.org/x/sync/errgroup"
)

// drainTimeout 是取消/超时退出时提交尾部缓冲的硬上限。
// 尾部既不能静默丢弃，也不能挂在已取消的 ctx 上无限等（必修②）。
const drainTimeout = 30 * time.Second

// Run 是通用三段流水线调度核心（方案 7.1 / 7.4 / 7.7）：
//
//	区间 fan-out → scanner worker 池（IO，读 MySQL） → 有界 docsCh → bulk writer 池（CPU+IO，写 ES）
//
// 数据源通过 DocProvider 注入、灌入出口通过 WriteFunc 注入（生产为 admin.IndexBulk，
// 单测注入 fake 以便在无 ES 环境下验证结构性行为）。
//
// 关闭顺序严格遵循方案 7.7 第 2 条「先断源头 → 再关管道 → 最后等消费者」：
//  1. writer 先起（range docsCh，靠 channel 关闭自然退出）；
//  2. scanner 用 errgroup 起（任一 panic/error → gctx 取消 → 其余 scanner 与 writer 一起收敛）；
//  3. 主协程喂 rangeCh 并 close(rangeCh)；全部 scanner 返回后才 close(docsCh)（绝不在 writer 里 close）。
func Run(ctx context.Context, p DocProvider, opts Options, w WriteFunc) (*SyncStats, error) {
	if p == nil {
		return nil, fmt.Errorf("sync: DocProvider 不能为 nil")
	}
	if opts.Index == "" {
		return nil, fmt.Errorf("sync: 灌入目标 Index 为空（须是别名或物理索引名）")
	}
	stats := opts.Stats
	if stats == nil {
		stats = &SyncStats{}
	}
	stats.MarkStart()
	if w == nil {
		w = defaultWriter(opts)
	}
	// provider 若关心进度，把本次运行的统计容器交给它（扫描侧回写 ScannedDocs）。
	if sc, ok := p.(StatsCarrier); ok {
		sc.AttachStats(stats)
	}

	bounds, err := p.Bounds(ctx)
	if err != nil {
		return stats, fmt.Errorf("sync: 区间探测失败: %w", err)
	}

	concurrency := clampInt(opts.Concurrency, defaultConcurrency)
	bulkSize := clampInt(opts.BulkSize, defaultBulkSize)
	bulkMaxBytes := opts.BulkMaxBytes // <=0 由 BulkChunker 回落 DefaultBulkMaxBytes
	maxFailures := clampInt(opts.MaxFailures, defaultMaxFailures)

	planned := bounds.Total
	if opts.Limit > 0 && opts.Limit < planned {
		planned = opts.Limit
	}
	stats.TotalDocs.Store(planned)

	// 空表 / total=0 优雅短路：不起任何 goroutine，避免无谓连接与除零。
	if bounds.Empty() {
		return stats, nil
	}

	// 统一控制上下文：runCancel 供 writer 侧失败反向打断 scanner。
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()
	g, gctx := errgroup.WithContext(runCtx) // scanner 报错即取消 gctx（writer 也监听 gctx）

	rangeCh := make(chan IDRange, concurrency)
	// 反压：有界 channel，容量即在途批次上限；ES 慢时自动阻塞 MySQL 读，无需手写限速。
	docsCh := make(chan ProviderBatch, concurrency*defaultDocsChBufferMulr)

	var (
		failMu     golangsync.Mutex
		aggregated []admin.Failure
		held       errHolder
	)

	target := opts.Index

	// —— 阶段三先起：bulk writer 池 ——
	var writers golangsync.WaitGroup
	writerCfg := writerConfig{
		provider:     p,
		write:        w,
		target:       target,
		bulkSize:     bulkSize,
		bulkMaxBytes: bulkMaxBytes,
		maxFailures:  maxFailures,
		stats:        stats,
		failMu:       &failMu,
		aggregated:   &aggregated,
		held:         &held,
		cancel:       runCancel,
	}
	for i := 0; i < concurrency; i++ {
		writers.Add(1)
		go func() {
			defer writers.Done()
			defer func() {
				if r := recover(); r != nil {
					held.set(fmt.Errorf("writer panic: %v", r))
					runCancel()
				}
			}()
			// writer 使用 runCtx 而非 gctx：gctx 在所有 scanner 正常完成后会被
			// errgroup 自动 cancel，导致在途 bulk 被杀。runCtx 仅被 runCancel()
			// 显式取消（writer/scanner 报错时），writer 正常退出靠 docsCh 关闭。
			runWriter(runCtx, writerCfg, docsCh)
		}()
	}

	// —— 阶段二：scanner worker 池 ——
	k := ComputeRangeCount(opts.Concurrency, opts.RangeMultiplier)
	ranges := ComputeRanges(bounds, k)

	// 必修①：区间计划必须在 scanner 启动之前交给 provider（若它按区间记账），
	// 否则并发乱序完成时 checkpoint 游标会越过尚未扫描的低 id 区间。
	if rp, ok := p.(RangePlanCarrier); ok {
		rp.PlanRanges(ranges)
	}

	for i := 0; i < concurrency; i++ {
		g.Go(func() (err error) {
			// 方案 7.7 第 1 条：scanner panic 必须转 error，否则 goroutine 消失、
			// docsCh 永不 close、writer 永久阻塞形成死锁。
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("scanner panic: %v", r)
				}
			}()
			for {
				select {
				case <-gctx.Done():
					return gctx.Err()
				case r, ok := <-rangeCh:
					if !ok {
						return nil // rangeCh 已空且被关闭：本 scanner 正常结束
					}
					// emit 用 select 监听 gctx（方案 7.7 第 4 条）：ES 卡住导致 docsCh 满时，
					// ctx 取消能真正打断发送侧，--timeout 才生效。
					emit := func(batch ProviderBatch) error {
						select {
						case docsCh <- batch:
							return nil
						case <-gctx.Done():
							return gctx.Err()
						}
					}
					if err := p.ScanRange(gctx, r, emit); err != nil {
						return err
					}
				}
			}
		})
	}

	// 主协程喂区间并 close(rangeCh)（方案 7.7 第 3 条：rangeCh 由主协程 close）。
	feedErr := feedRanges(gctx, rangeCh, ranges)
	close(rangeCh)

	// 全部 scanner 返回后才 close(docsCh)（绝不在 writer 里 close）。
	scannerErr := g.Wait()
	close(docsCh)
	writers.Wait() // writer 靠 docsCh 关闭自然收敛

	if feedErr != nil && scannerErr == nil {
		scannerErr = feedErr
	}

	stats.MarkEnd()

	// 聚合失败明细到 stats（清理部分已无人消费的结构）。
	failMu.Lock()
	stats.Failures = aggregated
	failMu.Unlock()

	// 错误优先级：scanner/errgroup 错误 > writer 记录的首个错误（含 maxFailures 阈值中断）。
	finalErr := scannerErr
	if finalErr == nil {
		finalErr = held.get()
	}
	// --limit 正常收敛：provider 命中上限会返回哨兵并借 errgroup 取消其余 scanner，
	// 这里把它（以及无外部取消、无 writer 真实错误时的 context.Canceled）当作成功。
	if errors.Is(finalErr, ErrLimitReached) {
		finalErr = nil
	} else if errors.Is(finalErr, context.Canceled) {
		switch {
		case held.get() != nil:
			finalErr = held.get() // 让真实的 writer 失败（如 bulk 报错 / 超阈值）浮出
		case ctx.Err() == nil:
			finalErr = nil // 内部取消（limit 或正常收敛尾），父 ctx 未被取消 => 视为完成
		}
	}
	return stats, finalErr
}

// writerConfig 是单个 writer goroutine 的依赖集合（无共享可变状态，失败明细用局部 slice）。
type writerConfig struct {
	provider     DocProvider
	write        WriteFunc
	target       string
	bulkSize     int
	bulkMaxBytes int
	maxFailures  int
	stats        *SyncStats
	failMu       *golangsync.Mutex
	aggregated   *[]admin.Failure
	held         *errHolder
	cancel       context.CancelFunc
}

// runWriter 消费 docsCh：装配（CPU 下沉到此）→ 按条数/字节切块 → bulk → 统计与失败收集。
func runWriter(ctx context.Context, cfg writerConfig, docsCh <-chan ProviderBatch) {
	var (
		chunker = admin.NewBulkChunker(cfg.bulkSize, cfg.bulkMaxBytes)
		buf     []admin.Doc
		local   []admin.Failure // per-writer 局部失败集，退出时一次性合并（无争用）
	)
	buf = make([]admin.Doc, 0, cfg.bulkSize)

	// flush 提交当前缓冲；返回 false 表示已达失败阈值或被取消，调用方应停止。
	// wctx 是本次 bulk 提交用的 ctx：热路径用 runCtx，退出排空路径用独立 ctx（tailFlush）。
	flush := func(wctx context.Context) bool {
		if len(buf) == 0 {
			return true
		}
		docs := buf
		buf = make([]admin.Doc, 0, cfg.bulkSize) // 换新底层数组，避免提交后与复用竞写
		chunker.Reset()

		res, err := cfg.write(wctx, cfg.target, docs)
		if err != nil {
			// 请求级失败：ES 未确认任何一条。这批文档既没写进去、也不在 admin 落盘的
			// item 级失败文件里，不如实计入就会静默消失（必修②）。
			cfg.stats.FailedDocs.Add(int64(len(docs)))
			local = append(local, uncommittedFailures(cfg.target, docs, err)...)
			cfg.held.set(fmt.Errorf("bulk 写入失败 [target=%s, docs=%d]: %w", cfg.target, len(docs), err))
			cfg.cancel()
			return false
		}
		if res != nil {
			cfg.stats.SentDocs.Add(int64(res.Sent))
			cfg.stats.FailedDocs.Add(int64(res.Failed))
			cfg.stats.Batches.Add(1)
			if len(res.Failures) > 0 {
				local = append(local, res.Failures...)
			}
		}
		if cfg.maxFailures > 0 && cfg.stats.FailedDocs.Load() > int64(cfg.maxFailures) {
			cfg.held.set(fmt.Errorf("失败文档数已达阈值中断任务（--max-failures=%d）", cfg.maxFailures))
			cfg.cancel()
			return false
		}
		return true
	}

	// tailFlush 提交尾部缓冲：ctx 已被取消/超时时改用 context.WithoutCancel 派生的独立 ctx。
	// 用已取消的 ctx 提交最后一批，bulk 必然 canceled：既把真实取消原因覆盖成假「bulk 写入失败」，
	// 又让尾部缓冲静默丢弃、不计入 FailedDocs（必修②）。
	// 注意：这里绝不用 errgroup 的 gctx——gctx 会被 errgroup 在 scanner 正常收敛时
	// 自动取消，长生命周期消费者必须跑在 runCtx 上（第六轮 P0 钉过的教训）。
	tailFlush := func() {
		if ctx.Err() == nil {
			flush(ctx)
			return
		}
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), drainTimeout)
		defer cancel()
		flush(dctx)
	}

	for {
		var (
			batch ProviderBatch
			ok    bool
			alive bool
		)
		select {
		case <-ctx.Done():
			tailFlush()
			mergeLocal(cfg, local)
			return
		case batch, ok = <-docsCh:
			alive = ok
		}
		if !alive {
			// docsCh 已关闭且排空：提交尾部缓冲后退出。
			tailFlush()
			mergeLocal(cfg, local)
			return
		}

		docs, err := cfg.provider.Assemble(batch)
		if err != nil {
			cfg.held.set(fmt.Errorf("装配批次失败: %w", err))
			cfg.cancel()
			mergeLocal(cfg, local)
			return
		}
		for _, d := range docs {
			buf = append(buf, d)
			size := 0
			if !d.Delete {
				if n, e := admin.EstimateSize(d.Src); e == nil {
					size = n
				}
			}
			if chunker.Add(size) {
				if !flush(ctx) {
					mergeLocal(cfg, local)
					return
				}
			}
		}
	}
}

// mergeLocal 把 per-writer 局部失败集在 writer 退出时一次性合并进共享切片（仅退出瞬间持锁，热路径无争用）。
func mergeLocal(cfg writerConfig, local []admin.Failure) {
	if len(local) == 0 {
		return
	}
	cfg.failMu.Lock()
	*cfg.aggregated = append(*cfg.aggregated, local...)
	cfg.failMu.Unlock()
}

// uncommittedFailures 把「整批请求级失败」（HTTP/传输级错误，ES 未确认任何一条）
// 转成逐条失败明细，使尾部缓冲不静默消失、计数与 Failures 如实（必修②）。
func uncommittedFailures(target string, docs []admin.Doc, cause error) []admin.Failure {
	out := make([]admin.Failure, 0, len(docs))
	for _, d := range docs {
		op := admin.OpIndex
		if d.Delete {
			op = admin.OpDelete
		}
		out = append(out, admin.Failure{
			ID:     d.ID,
			Index:  target,
			Op:     op,
			Type:   "bulk_request_failed",
			Reason: cause.Error(),
			Doc:    d.Src,
		})
	}
	return out
}

// errHolder 记录流水线首个错误（writer 侧无法经 errgroup 上抛，用此共享容器汇聚）。
type errHolder struct {
	mu  golangsync.Mutex
	err error
}

// set 记录首个非 nil 错误（先到先得，后续不覆盖）。
func (h *errHolder) set(e error) {
	if e == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.err == nil {
		h.err = e
	}
}

// get 返回已记录的首个错误（可能为 nil）。
func (h *errHolder) get() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.err
}

// defaultWriter 构造生产灌入出口：委托 admin.IndexBulk（内部固定 op_type=index 幂等覆盖、
// 429/503 退避重投、结构类失败不重试直接落盘）。绝不改用 create。
func defaultWriter(opts Options) WriteFunc {
	return func(ctx context.Context, target string, docs []admin.Doc) (*admin.BulkResult, error) {
		var o []admin.BulkOption
		if opts.FailureDir != "" {
			o = append(o, admin.WithFailureDir(opts.FailureDir))
		}
		return admin.IndexBulk(ctx, target, docs, o...)
	}
}
