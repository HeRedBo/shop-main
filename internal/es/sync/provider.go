// Package sync 实现 ES 全量同步流水线（方案第七节）。
//
// 包名与目录同名 sync：为避免与本包内使用的标准库 sync 冲突，
// 包内一律以别名 golangsync 引入标准库 sync；对外导入方仍用 shop/internal/es/sync。
//
// 设计目标（方案 7.4「数据源与机制解耦」）：把「数据源」抽象为 DocProvider，
// 把「并发扫描 + 反压 + bulk 灌入 + 统计 + 进度」的通用机制固化在 runner，
// orders 二期只需新增一个 Provider 实现即可复用整套流水线。
//
// 边界纪律：本包绝不直接 import olivere，也不做任何 ES 版本相关的响应解析；
// 所有 ES 交互只经 internal/es/admin 这一层，方便后续把 v7 客户端换成 v8 时
// 改动只局限在 admin 内（真实集群是 8.x，客户端仍是 olivere v7）。
package sync

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"shop/internal/es"
	"shop/internal/es/admin"
)

// ErrLimitReached 由 provider 在累计扫描行数达到 --limit 时返回，ScanRange 原样上抛。
// Run 识别该哨兵并视为正常收敛（不当作失败），使 --limit 小样本实测不会误报错误退出码。
var ErrLimitReached = errors.New("sync: 已达到 --limit 上限")

// IDRange 是一段左开右闭的 id 区间 (Lo, Hi]。
//
// 之所以左开：pkg/chunk 的游标语义是 WHERE id > lastId，scanner 侧预置
// WHERE id > Lo AND id <= Hi 即可与 pkg/chunk 追加的游标条件天然复合，
// 得到互不相交的分片区间，保证并发下同一文档不会被两个 worker 重复灌。
type IDRange struct {
	Lo int64
	Hi int64
}

// Bounds 是一次区间探测的结果：过滤条件下的最小 id、最大 id 与总行数。
// Total 供进度条与对账使用；Min/Max 供区间切分使用。
type Bounds struct {
	Min   int64
	Max   int64
	Total int64
}

// Empty 报告该 Bounds 是否代表「没有任何待同步行」，据此短路整个流水线。
func (b Bounds) Empty() bool { return b.Total <= 0 }

// ProviderBatch 是 scanner 产出、经有界 docsCh 投递、由 writer 消费的批次载体。
//
// 用 any 是为了让 runner 对数据源保持类型无关：products 侧装的是主表行 + 预取的
// 关联数据，orders 侧可以完全不同。writer 只负责调用 provider.Assemble 把它变成
// []admin.Doc，从不关心其内部结构。
type ProviderBatch any

// DocProvider 把一个数据源（表 + 过滤条件 + 装配规则）适配到通用同步流水线。
//
// 生命周期约定：
//   - 一个 provider 实例对应一次同步运行，内部可持有该次运行的过滤条件与限额；
//   - Bounds 只调用一次，供区间切分与进度分母；
//   - ScanRange 由 scanner goroutine 并发调用（IO 侧），每批通过 emit 投递；
//     emit 返回 error 表示下游要求中止（反压阻塞被打断 / 限额达到 / ctx 取消），
//     ScanRange 必须原样把该 error 上抛以中断游标循环；
//   - Assemble 由 writer goroutine 调用（CPU 侧）：序列化与 HTML 清洗等开销
//     在此发生，从 IO 密集的 scanner 侧下沉，让多核均摊 CPU 等待不阻塞读。
type DocProvider interface {
	// Definition 返回该数据源对应的索引声明（提供 alias / 物理名 / 依赖插件等元信息）。
	Definition() *es.IndexDefinition

	// Bounds 探测 (min,max,total)，必须带与 ScanRange 完全一致的过滤条件，
	// 否则区间覆盖不到全部数据、对账期望值也会错位。
	Bounds(ctx context.Context) (Bounds, error)

	// ScanRange 扫描单个 (r.Lo, r.High] 区间，按 chunkSize 分批调用 emit。
	ScanRange(ctx context.Context, r IDRange, emit func(ProviderBatch) error) error

	// Assemble 把一批原始数据装配成可直接投喂 admin.IndexBulk 的 []admin.Doc。
	// 不应入索引的行（如下架 / 软删）在此被丢弃，返回的 slice 可能比批次短。
	Assemble(batch ProviderBatch) ([]admin.Doc, error)
}

// StatsCarrier 是可选接口：DocProvider 若实现它，Run 会在启动流水线前把本次运行的
// SyncStats 注入进来，供 provider 在扫描侧回写进度分子（ScannedDocs）。
//
// 之所以做成可选而非并入 DocProvider：进度统计是旁路观测、不是数据源契约的一部分，
// 简单 provider（含单测 fake）可完全忽略它。进度渲染只读 atomic 快照，不加锁。
type StatsCarrier interface {
	AttachStats(stats *SyncStats)
}

// RangePlanCarrier 是可选接口：DocProvider 若实现它，Run 会在启动 scanner 之前把本次
// 运行的区间计划（ComputeRanges 的结果）交给它。
//
// 为什么需要它：区间并行扫描天然乱序完成，「最后一次读出的 id」并不代表低位数据已扫完。
// 断点续传一类需要按区间记账的 provider 只有拿到完整区间计划，才能把并发进度收敛成
// 单调游标（只推进到最小连续覆盖右边界）。简单 provider（含单测 fake）可完全忽略它。
type RangePlanCarrier interface {
	PlanRanges(ranges []IDRange)
}

// WriteFunc 是 bulk 灌入的可注入出口。
//
// 生产默认实现走 admin.IndexBulk（内部固定 op_type=index 幂等覆盖）；
// 单测注入 fake 以便在无 ES 连接的环境下验证流水线结构性行为（反压、panic、收敛）。
type WriteFunc func(ctx context.Context, target string, docs []admin.Doc) (*admin.BulkResult, error)

// SyncStats 是一次同步运行的原子统计快照。所有计数字段用 atomic.Int64，
// 供进度 ticker goroutine 无锁拉取渲染（方案 7.7 第 5 条：批量写入不设共享可变状态）。
type SyncStats struct {
	TotalDocs    atomic.Int64 // 计划灌入的总行数（含 --limit 收敛后的值）
	SentDocs     atomic.Int64 // 成功写入 ES 的文档数
	FailedDocs   atomic.Int64 // item 级失败文档数
	Batches      atomic.Int64 // 已提交的 bulk 批次计数
	ScannedDocs  atomic.Int64 // 已从 DB 读出的行数（进度分子）
	RestoreNanos atomic.Int64 // 恢复副本（number_of_replicas 置回）阶段耗时，仅 --tune-ingest 有意义

	// Failures 是 item 级失败明细（admin.BulkResult.Failures 聚合），供调用方诊断/重试。
	// 仅在 Run 返回后读取，无需 atomic。
	Failures []admin.Failure

	start time.Time
	end   time.Time
}

// MarkStart 记录起始时刻（编排层调用，流水线内部不调）。
func (s *SyncStats) MarkStart() { s.start = time.Now() }

// MarkEnd 记录结束时刻。
func (s *SyncStats) MarkEnd() { s.end = time.Now() }

// Elapsed 返回本次运行耗时；未 MarkStart 时返回 0。
func (s *SyncStats) Elapsed() time.Duration {
	if s.start.IsZero() {
		return 0
	}
	end := s.end
	if end.IsZero() {
		end = time.Now()
	}
	return end.Sub(s.start)
}

// DocsPerSec 返回平均灌入速率（成功文档数 / 秒）；耗时为 0 时返回 0。
func (s *SyncStats) DocsPerSec() float64 {
	d := s.Elapsed()
	if d <= 0 {
		return 0
	}
	return float64(s.SentDocs.Load()) / d.Seconds()
}

// RestoreReplicaElapsed 返回恢复副本阶段耗时（未开启 tune-ingest 时为 0）。
func (s *SyncStats) RestoreReplicaElapsed() time.Duration {
	return time.Duration(s.RestoreNanos.Load())
}

// Options 承载 es:sync-products 的全部可调参数，同时是编程式入口
// （Phase 5 rebuild / Phase 6 cutover 复用）的输入。
//
// 零值可用：数值为 0 的字段由 resolve() 回落内置默认，方便调用方只填关心的项。
type Options struct {
	// Index 是灌入目标：别名（日常补数）或物理索引名 shop-product_vN（重建内部调用）。
	// 空串时由 SyncProducts 依据 provider 的 Definition 回落为 alias。
	Index string

	// StrictIndexLookup 为 true 时，resolveProductDef 必须把非空 Index 解析到「已登记且 active
	// （进 ExecutableDefinitions）」的定义，否则报错并列出可用名称——绝不回落 ProductAlias / 唯一
	// 可执行定义。CLI 仅在用户显式传 --index（Changed=true）时置真，用以区分「未传用默认」与
	// 「传了但不存在/未启用（必须报错）」。编程式调用方（rebuild/cutover/migrate 适配器、单测）
	// 保持默认 false，沿用原有友好兜底，行为不变。
	StrictIndexLookup bool

	ChunkSize       int // pkg/chunk 每批读取行数
	Concurrency     int // scanner 与 writer 各自的 goroutine 数
	RangeMultiplier int // 区间数 = RangeMultiplier × Concurrency
	BulkSize        int // 单个 bulk 的最大文档数
	BulkMaxBytes    int // 单个 bulk 的最大字节数（0 → admin.DefaultBulkMaxBytes）

	Limit int64     // 仅灌入前 N 行（小样本吞吐实测）；0 表示不限
	IDs   []int64   // 仅灌入指定 id 集合；非空时忽略区间扫描的 id 范围但仍受其它过滤约束
	Since time.Time // 仅灌入 update_time >= Since 的行；零值表示不加该过滤

	Tolerance   int64 // 对账容差：|ES count - DB expected| > Tolerance 视为不达标
	MaxFailures int   // item 级失败累计超过该值即中断整任务（0 → 默认 100）

	RetryFailed string // 复跑失败明细 JSONL 的路径；非空时跳过区间扫描直接灌失败项
	Resume      bool   // 断点续传：读写 runtime/es_sync/<target>.checkpoint.json
	DryRun      bool   // 只发读请求并打印请求序列，不写 ES、不改 settings
	TuneIngest  bool   // 灌入物理索引时临时关 refresh / 副本（仅目标未持有别名才生效）
	AssumeYes   bool   // 跳过交互确认（CLI 的 --yes）

	GoGC    int           // >0 时 debug.SetGCPercent(GoGC)；0 表示不调
	Timeout time.Duration // 整体 ctx 超时（0 表示不设，交由调用方的 ctx 控制）

	FailureDir string // 失败明细落盘目录（空 → admin 默认 runtime/es_sync）
	IndicesDir string // --indices-dir 原始值；provider 加载 Definition 时透传

	// Stats 是外部注入的统计容器（CLI 进度条场景）；为 nil 时 Run 内部新建。
	Stats *SyncStats
}

// 代码内置默认值：三级优先级「显式 CLI flag > conf/config.yml elasticsearch 段 > 此处常量」
// 中的最后一级兜底。注意它们【不是】直接照搬 config.yml 的——config 段的
// default-chunk-size / sync-concurrency / range-multiplier / bulk-max-mb 由 CLI 读取后经
// ResolveSyncParams 合入 Options；本包常量只在「flag 与配置都未提供」或编程式调用方
// 传零值时，经 clampInt 生效。bulk-max-mb 无代码内置 MB 默认（0 表示交由
// admin.DefaultBulkMaxBytes），故其最后兜底为 0 而非下面的某个常量。
const (
	defaultChunkSize        = 1000
	defaultConcurrency      = 4
	defaultRangeMultiplier  = 4
	defaultBulkSize         = 3000
	defaultMaxFailures      = 100
	defaultDocsChBufferMulr = 2 // docsCh 缓冲 = Concurrency × 该值
)

// clampInt 在 v<=0 时返回 def。
func clampInt(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

// ---------------------------------------------------------------------------
// 参数解析：显式 CLI flag > 配置值 > 代码内置默认 三级优先级
// ---------------------------------------------------------------------------

// IntFlag 是单个 es:sync-products 数值 flag 的原始状态：Changed 表示用户是否显式传入
// （cobra Flag.Changed），Value 是 flag 当前值（未传时为 flag 默认值，故 !Changed 时被忽略）。
type IntFlag struct {
	Changed bool
	Value   int
}

// SyncParamFlags 汇总四个受配置影响的数值 flag。
type SyncParamFlags struct {
	ChunkSize       IntFlag // --chunk-size
	Concurrency     IntFlag // --concurrency
	RangeMultiplier IntFlag // --range-multiplier
	BulkMaxMB       IntFlag // --bulk-max-mb
}

// SyncParamConfig 汇总 conf/config.yml elasticsearch 段对应的四个配置值：
// default-chunk-size / sync-concurrency / range-multiplier / bulk-max-mb。
// 0 表示配置未提供（安静回落到代码内置默认）。
type SyncParamConfig struct {
	ChunkSize       int
	Concurrency     int
	RangeMultiplier int
	BulkMaxMB       int
}

// ResolvedSyncParams 是三级优先级合并后的最终取值，Warnings 承载越界值的如实告警。
type ResolvedSyncParams struct {
	ChunkSize       int
	Concurrency     int
	RangeMultiplier int
	BulkMaxMB       int
	Warnings        []string
}

// ResolveSyncParams 把「显式 CLI flag > 配置值 > 代码内置默认」落到四个数值参数上，
// 是 config.yml elasticsearch 段真正接入同步流水线的入口：CLI 读取配置后调用本函数，
// 结果写进 Options，于是本包常量退居最后兜底（仅 flag 与配置都未提供时生效）。
//
// 非法值口径复用 clampInt（非正值无效），但绝不静默采纳：越界值收敛到内置默认的同时
// 追加一条如实告警。bulk-max-mb 语义特殊——0 表示「交给 admin 默认」而非无效，
// 故按 allowZero=true 处理，其代码内置默认即 0。
func ResolveSyncParams(f SyncParamFlags, c SyncParamConfig) ResolvedSyncParams {
	var r ResolvedSyncParams
	r.ChunkSize, r.Warnings = resolveIntParam("default-chunk-size", f.ChunkSize, c.ChunkSize, defaultChunkSize, false, r.Warnings)
	r.Concurrency, r.Warnings = resolveIntParam("sync-concurrency", f.Concurrency, c.Concurrency, defaultConcurrency, false, r.Warnings)
	r.RangeMultiplier, r.Warnings = resolveIntParam("range-multiplier", f.RangeMultiplier, c.RangeMultiplier, defaultRangeMultiplier, false, r.Warnings)
	r.BulkMaxMB, r.Warnings = resolveIntParam("bulk-max-mb", f.BulkMaxMB, c.BulkMaxMB, 0, true, r.Warnings)
	return r
}

// resolveIntParam 对单个参数应用三级优先级：
//  1. flag.Changed → 采纳 flag.Value；非正值按 clampInt 收敛到 codeDefault 并告警。
//  2. 否则 cfgVal>0 → 采纳配置值；cfgVal==0 视为未配置 → 安静回落 codeDefault。
//  3. 否则（cfgVal<0）→ 收敛到 codeDefault 并告警。
//
// allowZero=true 时 0 是合法值（bulk-max-mb 表示「不封顶，交 admin」），负值才告警。
func resolveIntParam(name string, flag IntFlag, cfgVal, codeDefault int, allowZero bool, warns []string) (int, []string) {
	valid := func(v int) bool {
		if allowZero {
			return v >= 0
		}
		return v > 0
	}
	if flag.Changed {
		if valid(flag.Value) {
			return flag.Value, warns
		}
		fixed := clampInt(flag.Value, codeDefault)
		return fixed, append(warns, fmt.Sprintf("elasticsearch %s: 显式 flag 非法值 %d，已收敛为 %d（未采纳原值）", name, flag.Value, fixed))
	}
	if cfgVal > 0 {
		return cfgVal, warns
	}
	if cfgVal == 0 {
		return codeDefault, warns
	}
	fixed := clampInt(cfgVal, codeDefault)
	return fixed, append(warns, fmt.Sprintf("elasticsearch %s: 配置项非法值 %d，已收敛为 %d（未采纳原值）", name, cfgVal, fixed))
}
