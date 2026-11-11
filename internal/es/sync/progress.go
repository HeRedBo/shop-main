package sync

import (
	"io"
	golangsync "sync"
	"time"

	"github.com/schollz/progressbar/v3"
)

// defaultProgressInterval 是 ticker 默认刷新节奏（够肉眼顺滑，又不过度抢锁/IO）。
const defaultProgressInterval = 200 * time.Millisecond

// progressTheme 复用 cmd/cli/product_sync_bar.go 里已验收的方块主题，
// 保证 es:sync-products 与既有商品导出条视觉一致。
var progressTheme = progressbar.Theme{
	Saucer:        "█",
	SaucerPadding: "░",
	BarStart:      "▒",
	BarEnd:        "▓",
}

// Progress 是一个由独立 ticker 驱动的进度渲染器：它周期性拉取 SyncStats 的原子快照
// （进度分子 = ScannedDocs，分母 = TotalDocs），把增量喂给 progressbar。
//
// 设计约束（方案 7.7 第 5、6 条）：
//   - 渲染与同步彻底解耦：bar.Add 的 error 一律忽略，渲染失败绝不中止灌入主流程；
//   - ticker 必须 defer Stop，goroutine 由 shutdown 关闭 stop 通道并等待其退出，
//     不留残 goroutine（供 goleak 覆盖）；
//   - 循环内禁用 time.After（每次 tick 重新分配定时器、且未读事件会堆积），
//     统一用 *time.Ticker 的 chan。
type Progress struct {
	stats    *SyncStats
	desc     string
	interval time.Duration
	writer   io.Writer // 测试可注入 io.Discard，避免污染 stdout

	// bar 首批懒创建：total 尚未就绪（Bounds 未到）时不建，避免分母为 0 的空条。
	bar     *progressbar.ProgressBar
	last    int64
	started bool

	stop chan struct{}
	done chan struct{}
	mu   golangsync.Mutex
}

// NewProgress 构造进度渲染器，但不会立即启动 goroutine（由 Start 启动）。
// desc 为空时使用默认描述。
func NewProgress(stats *SyncStats, desc string, interval time.Duration) *Progress {
	if stats == nil {
		stats = &SyncStats{}
	}
	if desc == "" {
		desc = "ES 全量同步"
	}
	if interval <= 0 {
		interval = defaultProgressInterval
	}
	return &Progress{
		stats:    stats,
		desc:     desc,
		interval: interval,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// Start 启动 ticker goroutine。重复调用无副作用（第二次直接返回）。
func (p *Progress) Start() {
	p.mu.Lock()
	if p.started {
		p.mu.Unlock()
		return
	}
	p.started = true
	p.mu.Unlock()

	go p.loop()
}

// loop 是渲染主循环：defer Stop ticker（方案 7.7 第 3 条），退出前收尾刷新一次。
func (p *Progress) loop() {
	defer close(p.done)

	t := time.NewTicker(p.interval)
	defer t.Stop()

	for {
		select {
		case <-p.stop:
			p.render() // 关闭前最后渲染一次，保证终态可见
			p.finish()
			return
		case <-t.C:
			p.render()
		}
	}
}

// render 拉一次原子快照并把增量喂给进度条；全程吞掉渲染错误（渲染失败不影响同步）。
func (p *Progress) render() {
	cur := p.stats.ScannedDocs.Load()
	total := p.stats.TotalDocs.Load()

	p.mu.Lock()
	defer p.mu.Unlock()

	// 懒创建：分母就绪（Bounds 完成）后才建条。
	if p.bar == nil {
		if total <= 0 {
			return
		}
		opts := []progressbar.Option{
			progressbar.OptionSetDescription(p.desc),
			progressbar.OptionShowCount(),
			progressbar.OptionShowIts(),
			progressbar.OptionSetWidth(40),
			progressbar.OptionSetTheme(progressTheme),
		}
		if p.writer != nil {
			opts = append(opts, progressbar.OptionSetWriter(p.writer))
		}
		p.bar = progressbar.NewOptions64(total, opts...)
	}

	if delta := cur - p.last; delta > 0 {
		_ = p.bar.Add(int(delta)) // error 一律忽略：渲染失败绝不中止同步
		p.last = cur
	}
}

// finish 收尾：若已灌完则刷新为 100% 并结束进度条（幂等，bar 未建则跳过）。
func (p *Progress) finish() {
	if p.bar == nil {
		return
	}
	if p.stats.SentDocs.Load() > 0 || p.last > 0 {
		_ = p.bar.Finish()
	}
}

// Shutdown 停止 ticker 并等待渲染 goroutine 退出，随后可安全读取终端输出。
// 幂等：未启动或已停止时直接返回。
func (p *Progress) Shutdown() {
	p.mu.Lock()
	started := p.started
	p.mu.Unlock()
	if !started {
		return
	}

	select {
	case <-p.stop:
		// 已关闭过
	default:
		close(p.stop)
	}
	<-p.done
}
