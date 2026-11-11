package sync

import (
	"io"
	"sync/atomic"
	"testing"
	"time"
)

// TestProgress_NewAndShutdown 验证创建和关闭不 panic。
func TestProgress_NewAndShutdown(t *testing.T) {
	stats := &SyncStats{}
	p := NewProgress(stats, "test", 100*time.Millisecond)
	p.writer = io.Discard // 避免污染 stdout
	p.Start()
	time.Sleep(50 * time.Millisecond)
	p.Shutdown()
	// 重复关闭应幂等
	p.Shutdown()
}

// TestProgress_LazyCreation 验证 bar 首批懒创建（total=0 时不建）。
func TestProgress_LazyCreation(t *testing.T) {
	stats := &SyncStats{}
	stats.TotalDocs.Store(0)
	p := NewProgress(stats, "test", 50*time.Millisecond)
	p.writer = io.Discard
	p.Start()
	time.Sleep(100 * time.Millisecond)
	p.mu.Lock()
	barCreated := p.bar != nil
	p.mu.Unlock()
	p.Shutdown()
	if barCreated {
		t.Error("total=0 时不应创建 bar")
	}
}

// TestProgress_RenderUpdates 验证 ticker 拉取快照并更新进度。
func TestProgress_RenderUpdates(t *testing.T) {
	stats := &SyncStats{}
	stats.TotalDocs.Store(100)
	stats.ScannedDocs.Store(0)
	p := NewProgress(stats, "test", 50*time.Millisecond)
	p.writer = io.Discard
	p.Start()

	// 模拟扫描进度增长
	for i := 0; i < 5; i++ {
		stats.ScannedDocs.Store(int64((i + 1) * 20))
		time.Sleep(60 * time.Millisecond)
	}

	p.Shutdown()

	p.mu.Lock()
	lastRendered := p.last
	barCreated := p.bar != nil
	p.mu.Unlock()

	if !barCreated {
		t.Error("total>0 后应创建 bar")
	}
	if lastRendered != 100 {
		t.Errorf("最终 last 应为 100，实际 %d", lastRendered)
	}
}

// TestProgress_ScannedDocsAtomic 验证 ScannedDocs 是 atomic 操作（并发安全）。
func TestProgress_ScannedDocsAtomic(t *testing.T) {
	stats := &SyncStats{}
	stats.TotalDocs.Store(1000)
	p := NewProgress(stats, "test", 10*time.Millisecond)
	p.writer = io.Discard
	p.Start()

	// 并发更新 ScannedDocs
	var wg atomic.Int64
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			stats.ScannedDocs.Add(10)
			wg.Add(1)
		}
		close(done)
	}()

	<-done
	time.Sleep(50 * time.Millisecond) // 等 ticker 拉取
	p.Shutdown()

	final := stats.ScannedDocs.Load()
	if final != 1000 {
		t.Errorf("期望 Chunks=1000，实际 %d", final)
	}
}

// TestProgress_DefaultInterval 验证默认刷新间隔。
func TestProgress_DefaultInterval(t *testing.T) {
	stats := &SyncStats{}
	p := NewProgress(stats, "", 0) // interval=0 应回落默认
	if p.interval != defaultProgressInterval {
		t.Errorf("interval=0 应回落为 %v，实际 %v", defaultProgressInterval, p.interval)
	}
}
