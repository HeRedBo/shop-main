package sync

import (
	"context"
)

// ComputeRangeCount 决定区间总数 K = RangeMultiplier × Concurrency（均先取默认兜底）。
//
// 区间数刻意多于 scanner 数（默认 4×），交给 work-queue 自取：删除空洞与 cate_id
// 倾斜会让某些 id 区间行数暴多、另一些近乎空，多切区间 + 动态自取能把热点摊平到
// 所有 scanner 上，消除静态均分带来的长尾。
func ComputeRangeCount(concurrency, multiplier int) int {
	c := clampInt(concurrency, defaultConcurrency)
	m := clampInt(multiplier, defaultRangeMultiplier)
	return c * m
}

// ComputeRanges 把 (Min, Max] 沿 id 轴均分为至多 k 段左开右闭区间。
//
// 语义与 pkg/chunk 主键游标严格对齐：每段用 WHERE id > Lo AND id <= Hi 表达，
// 相邻区间互不相交、并集恰为全体 id，因此并发灌入天然无同文档竞争（_id 幂等不重叠）。
//
// 覆盖边界：行 id 落在闭区间 [Min, Max]，用左开表示即 (Min-1, Max]。
//   - 空结果（Empty）返回 nil，让上层短路；
//   - k<=0 兜底为 1；
//   - Min==Max（total>0）时 step 收敛为 1，只产出一段 (Min-1, Max]，不除零；
//   - step 向上取整保证覆盖到 Max，且总段数不超过 k。
func ComputeRanges(b Bounds, k int) []IDRange {
	if b.Empty() {
		return nil
	}
	if k < 1 {
		k = 1
	}

	lo0 := b.Min - 1     // 使 id==Min 落入第一段（id > Min-1）
	width := b.Max - lo0 // = Max - Min + 1，即 (Min-1, Max] 内的整数 id 个数
	if width <= 0 {      // 理论上 total>0 时 width>=1，此处仅防御异常 Bounds
		return []IDRange{{Lo: lo0, Hi: b.Max}}
	}

	step := (width + int64(k) - 1) / int64(k) // 向上取整
	if step < 1 {
		step = 1
	}

	out := make([]IDRange, 0, k)
	for lo := lo0; lo < b.Max; {
		hi := lo + step
		if hi > b.Max {
			hi = b.Max
		}
		out = append(out, IDRange{Lo: lo, Hi: hi})
		lo = hi
	}
	return out
}

// feedRanges 把区间投入 rangeCh（work-queue），投完即由调用方 close。
//
// 发送侧用 select 监听 ctx，避免 scanner 全部提前退出后 rangeCh 满、主协程卡死。
// 返回 ctx.Err()（若被取消）以便上层据此收敛。
func feedRanges(ctx context.Context, rangeCh chan<- IDRange, ranges []IDRange) error {
	for _, r := range ranges {
		select {
		case rangeCh <- r:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
