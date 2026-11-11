package sync

import (
	"context"
	"testing"
	"time"
)

// TestComputeRangeCount 验证区间数计算。
func TestComputeRangeCount(t *testing.T) {
	tests := []struct {
		concurrency int
		multiplier  int
		expected    int
	}{
		{4, 4, 16},   // 默认配置
		{1, 1, 1},    // 最小
		{0, 0, 16},   // 零值兜底
		{-1, -1, 16}, // 负值兜底
		{8, 2, 16},   // 自定义
	}
	for _, tt := range tests {
		got := ComputeRangeCount(tt.concurrency, tt.multiplier)
		if got != tt.expected {
			t.Errorf("ComputeRangeCount(%d, %d) = %d, expected %d", tt.concurrency, tt.multiplier, got, tt.expected)
		}
	}
}

// TestComputeRanges_Empty 验证空表返回 nil。
func TestComputeRanges_Empty(t *testing.T) {
	b := Bounds{Min: 0, Max: 0, Total: 0}
	ranges := ComputeRanges(b, 4)
	if ranges != nil {
		t.Errorf("空表应返回 nil，实际 %v", ranges)
	}
}

// TestComputeRanges_SingleValue 验证 min==max 时只产出一段。
func TestComputeRanges_SingleValue(t *testing.T) {
	b := Bounds{Min: 100, Max: 100, Total: 1}
	ranges := ComputeRanges(b, 4)
	if len(ranges) != 1 {
		t.Fatalf("min==max 应产出 1 段，实际 %d", len(ranges))
	}
	if ranges[0].Lo != 99 || ranges[0].Hi != 100 {
		t.Errorf("区间应为 (99, 100]，实际 (%d, %d]", ranges[0].Lo, ranges[0].Hi)
	}
}

// TestComputeRanges_MultipleSegments 验证多区间覆盖。
func TestComputeRanges_MultipleSegments(t *testing.T) {
	b := Bounds{Min: 1, Max: 100, Total: 100}
	ranges := ComputeRanges(b, 4)
	if len(ranges) != 4 {
		t.Fatalf("期望 4 段，实际 %d", len(ranges))
	}
	// 验证覆盖完整：第一段从 0 开始，最后一段到 100 结束
	if ranges[0].Lo != 0 {
		t.Errorf("第一段 Lo 应为 0，实际 %d", ranges[0].Lo)
	}
	if ranges[len(ranges)-1].Hi != 100 {
		t.Errorf("最后一段 Hi 应为 100，实际 %d", ranges[len(ranges)-1].Hi)
	}
	// 验证区间连续且不相交
	for i := 1; i < len(ranges); i++ {
		if ranges[i].Lo != ranges[i-1].Hi {
			t.Errorf("区间不连续：ranges[%d].Lo=%d != ranges[%d].Hi=%d", i, ranges[i].Lo, i-1, ranges[i-1].Hi)
		}
	}
}

// TestComputeRanges_LessThanK 验证 total < k 时收敛为 total 段。
func TestComputeRanges_LessThanK(t *testing.T) {
	b := Bounds{Min: 1, Max: 3, Total: 3}
	ranges := ComputeRanges(b, 10) // k=10 但 total=3
	if len(ranges) > 3 {
		t.Errorf("total < k 时应收敛为至多 total 段，实际 %d", len(ranges))
	}
}

// TestFeedRanges_Normal 验证正常投递。
func TestFeedRanges_Normal(t *testing.T) {
	ranges := []IDRange{{Lo: 0, Hi: 10}, {Lo: 10, Hi: 20}}
	ch := make(chan IDRange, 2)
	err := feedRanges(context.Background(), ch, ranges)
	if err != nil {
		t.Fatalf("feedRanges 不应报错: %v", err)
	}
	close(ch)
	var got []IDRange
	for r := range ch {
		got = append(got, r)
	}
	if len(got) != 2 {
		t.Errorf("期望投递 2 个区间，实际 %d", len(got))
	}
}

// TestFeedRanges_ContextCancel 验证 ctx 取消时中断。
func TestFeedRanges_ContextCancel(t *testing.T) {
	ranges := make([]IDRange, 100)
	for i := range ranges {
		ranges[i] = IDRange{Lo: int64(i * 10), Hi: int64((i + 1) * 10)}
	}
	ch := make(chan IDRange, 1) // 容量 1，第二个就会阻塞
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := feedRanges(ctx, ch, ranges)
	if err == nil {
		t.Fatal("ctx 取消应返回错误")
	}
	if ctx.Err() == nil {
		t.Error("ctx 应已被取消")
	}
}
