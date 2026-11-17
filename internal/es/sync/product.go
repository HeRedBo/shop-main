package sync

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	golangsync "sync"
	"sync/atomic"
	"time"

	"shop/internal/es"
	"shop/internal/es/admin"
	"shop/internal/models"
	"shop/pkg/chunk"
	"shop/pkg/global"

	"gorm.io/gorm"
)

// ProductAlias 是本期唯一接线索引的别名（与 indices/product.jsonc 的 _meta.alias 一致）。
// SyncProducts 据此从 Registry 定位产品索引声明；二期 orders 走各自的 Provider/别名。
const ProductAlias = "shop-product"

// productScanColumns 是全量扫描的列裁剪清单。
//
// 前 17 列与 ProductIndex 的 17 个 json tag 一一对应；额外补 is_show 一列：
// BuildIndexDoc 的资格判定依赖 derefInt8(p.IsShow)==1，若不 SELECT 该列，指针恒为 nil、
// derefInt8 归零，会把每一行都判为「不应入索引」而全量丢弃。is_del 无需补：GORM 软删
// 已自动过滤（且未查询时其零值 0 恰好通过 IsDel!=0 判定）。故实为 17 索引列 + is_show。
const productScanColumns = "id, store_name, store_info, keyword, cate_id, price, sales, " +
	"ficti, is_hot, is_benefit, is_best, is_new, is_postage, is_good, description, " +
	"create_time, update_time, is_show"

// checkpointBaseDir 是 checkpoint 落盘目录 的基目录（可被单测注入为临时目录）。
// 生产默认值与 admin 层失败明细落盘目录保持一致。
var checkpointBaseDir = "runtime/es_sync"

// productBatch 是 scanner 侧产出、经 docsCh 投递的原始批次：主表行 + 可选的关联预取。
// 保持“原始”是为了把 BuildIndexDoc（HTML 清洗）与序列化下沉到 writer 侧执行（方案 7.1）。
type productBatch struct {
	Rows  []models.StoreProduct
	Attrs map[int64][]models.StoreProductAttr
}

// productProvider 把 store_product 适配到通用流水线。
type productProvider struct {
	db   *gorm.DB
	def  *es.IndexDefinition
	opts Options

	chunkSize int
	limit     int64
	remaining atomic.Int64

	// prefetchAttrs：本期 ProductIndex 不需要关联列，默认关闭以避免每批一条无人消费的
	// IN 查询（那与列裁剪省 IO 的初衷相悖）；loadAttrs 骨架已实现并由单测覆盖 SQL 形态，
	// 二期 nested 字段落地时把它置 true 即可启用批量预取。
	prefetchAttrs bool

	// minExclusiveID：--resume 断点续传的游标下界（id > 该值），0 表示不限制。
	minExclusiveID int64

	// ck 非空时（resume 模式）逐批回写 checkpoint。
	ck *checkpointSink

	// stats 由 Run 经 StatsCarrier 注入；非空时扫描侧回写进度分子 ScannedDocs。
	stats *SyncStats
}

// AttachStats 实现 StatsCarrier：Run 在启动流水线前注入本次运行的统计容器，
// 使扫描侧读出的行数能实时反映到进度条分子（不阻塞、不加锁，仅 atomic 自增）。
func (pp *productProvider) AttachStats(stats *SyncStats) { pp.stats = stats }

// newProductProvider 构造产品 Provider。opts.Index 应已解析为最终目标（别名或物理名）。
func newProductProvider(db *gorm.DB, def *es.IndexDefinition, opts Options) *productProvider {
	pp := &productProvider{
		db:        db,
		def:       def,
		opts:      opts,
		chunkSize: clampInt(opts.ChunkSize, defaultChunkSize),
		limit:     opts.Limit,
	}
	if opts.Limit > 0 {
		pp.remaining.Store(opts.Limit)
	}
	return pp
}

// Definition 实现 DocProvider。
func (pp *productProvider) Definition() *es.IndexDefinition { return pp.def }

// filter 构造带完整过滤条件的查询（is_show=1 + 软删自动 + ids/since/resume 下界）。
// Bounds 与 ScanRange 都基于它，确保区间探测与扫描口径完全一致（方案 7.2）。
func (pp *productProvider) filter(ctx context.Context) *gorm.DB {
	q := pp.db.WithContext(ctx).Model(&models.StoreProduct{}).Where("is_show = ?", 1)
	if len(pp.opts.IDs) > 0 {
		q = q.Where("id IN (?)", pp.opts.IDs)
	}
	if !pp.opts.Since.IsZero() {
		q = q.Where("update_time >= ?", pp.opts.Since)
	}
	if pp.minExclusiveID > 0 {
		q = q.Where("id > ?", pp.minExclusiveID)
	}
	return q
}

// Bounds 用一条聚合查询拿到 MIN/MAX/COUNT，实现 DocProvider。
// 缺陷 #5 修复：空表时 MIN/MAX 返回 NULL，用 sql.NullInt64 接收避免 GORM 报错。
// 委托给 boundsLookup 变量，以便单测在 SyncProducts 端到端流程中替换。
func (pp *productProvider) Bounds(ctx context.Context) (Bounds, error) {
	return boundsLookup(ctx, pp)
}

// ScanRange 扫描一个 (lo,hi] 区间：复用 pkg/chunk 的主键游标（不改其一行代码），
// 在其之上预置 id 上界即得分片游标。每批装配成 productBatch 经 emit 投递。
func (pp *productProvider) ScanRange(ctx context.Context, r IDRange, emit func(ProviderBatch) error) error {
	db := pp.filter(ctx).Select(productScanColumns).Where("id > ? AND id <= ?", r.Lo, r.Hi)

	err := chunk.ChunkById(db, pp.chunkSize, func(batch []models.StoreProduct, _ int) error {
		// 方案 7.7：chunk 回调首行检查 ctx，取消借道 error 中断游标循环（chunk 无 ctx 参数）。
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(batch) == 0 {
			return nil
		}

		pb := productBatch{Rows: batch}
		if pp.prefetchAttrs {
			attrs, err := pp.loadAttrs(ctx, productIDs(batch))
			if err != nil {
				return err
			}
			pb.Attrs = attrs
		}

		// resume 游标必须在 emit 之前记录：emit 可能因 writer 失败而永远不返回，
		// 但本批数据已从 DB 读出，不记录 checkpoint 会导致 resume 重灌整批。
		// 必修①：按区间记账——同一个 id 值在并发下并不代表同一段覆盖范围，
		// 只有「本区间已连续读到 lastID」才是可比较的进度。
		if pp.ck != nil {
			pp.ck.observeRange(r, batch[len(batch)-1].Id)
		}

		if err := emit(pb); err != nil {
			return err
		}

		// 进度分子：本批已从 DB 读出的行数（emit 成功后才计，避免把被反压打断的批次计入）。
		if pp.stats != nil {
			pp.stats.ScannedDocs.Add(int64(len(batch)))
		}

		if pp.limit > 0 {
			if left := pp.remaining.Add(-int64(len(batch))); left <= 0 {
				return ErrLimitReached
			}
		}
		return nil
	})
	pp.markRangeDone(r, err)
	return err
}

// markRangeDone 只在区间游标正常走到底时登记「该区间完整覆盖」。
// ScanRange 返回 error（取消 / 超时 / 反压打断 / 达到 --limit）时不调用，
// 游标因此停在最小连续覆盖右边界——宁可重扫不可漏扫。
func (pp *productProvider) markRangeDone(r IDRange, err error) {
	if err != nil || pp.ck == nil {
		return
	}
	pp.ck.completeRange(r)
}

// PlanRanges 实现 RangePlanCarrier：把本次运行的区间计划转交 checkpoint sink。
// sink 据此把并发乱序的区间进度收敛成单调游标（只推进到最小连续覆盖右边界）。
func (pp *productProvider) PlanRanges(ranges []IDRange) {
	if pp.ck != nil {
		pp.ck.planRanges(ranges)
	}
}

// Assemble 在 writer 侧把原始行装配为 []admin.Doc：BuildIndexDoc（HTML 清洗）+ _id/routing。
func (pp *productProvider) Assemble(batch ProviderBatch) ([]admin.Doc, error) {
	pb, ok := batch.(productBatch)
	if !ok {
		return nil, fmt.Errorf("sync: product provider 收到非预期批次类型 %T", batch)
	}
	docs := make([]admin.Doc, 0, len(pb.Rows))
	for i := range pb.Rows {
		di, eligible := es.BuildIndexDoc(&pb.Rows[i])
		if !eligible {
			continue
		}
		// _id / routing 必须走 BulkDocID/BulkRouting，与 product-consumer 逐字一致，
		// 否则同一商品在全量与增量灌入时落到不同 shard 造成重复。
		docs = append(docs, admin.Doc{ID: di.BulkDocID(), Routing: di.BulkRouting(), Src: di})
	}
	return docs, nil
}

// loadAttrs 用单条 WHERE product_id IN (?) 批量预取关联属性（消除 N+1 的装配骨架）。
// 本期 ProductIndex 未写 nested 字段，默认不调用；二期直接复用（单测断言其 SQL 形态）。
func (pp *productProvider) loadAttrs(ctx context.Context, ids []int64) (map[int64][]models.StoreProductAttr, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var rows []models.StoreProductAttr
	if err := pp.db.WithContext(ctx).Where("product_id IN (?)", ids).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("sync: 批量预取商品属性失败: %w", err)
	}
	out := make(map[int64][]models.StoreProductAttr, len(rows))
	for _, r := range rows {
		out[r.ProductId] = append(out[r.ProductId], r)
	}
	return out, nil
}

// productIDs 抽取一批主表行的 id（预分配，热路径不用 append 逐步扩容）。
func productIDs(rows []models.StoreProduct) []int64 {
	ids := make([]int64, 0, len(rows))
	for i := range rows {
		ids = append(ids, rows[i].Id)
	}
	return ids
}

// ---------------------------------------------------------------------------
// 编程式入口：SyncProducts
// ---------------------------------------------------------------------------

// SyncProducts 是products全量同步的编程式入口，供 es:sync-products CLI 以及
// Phase 5 rebuild（灌 next 物理索引）、Phase 6 cutover（复用全量灌 v1）直接调用。
//
// 它是 syncIndexTarget 的兼容别名（不施加别名约束）：目标定义仍由 opts.Index 解析，
// 数据源仍按解析出的 def.Alias() 查表构造。migrate/rebuild/cutover 等按别名循环的
// 编排请改用 SyncByAlias，以便「解析结果与调用方声明的别名不一致」时当场拒绝。
func SyncProducts(ctx context.Context, opts Options) (*SyncStats, error) {
	return syncIndexTarget(ctx, "", opts)
}

// syncIndexTarget 是全量灌数的真正实现：它负责 provider 之外的“编排”——定位 Definition、
// 解析目标、resume 续传、dry-run 预演、retry-failed 复跑、灌入调优（临时关 refresh/副本）、
// 对账收敛。并发流水线本身委托 Run。
//
// alias 非空时是调用方对「本次在处理哪个别名」的自证：解析出的声明必须正好属于该别名，否则在任何
// bulk/MySQL 触达之前报错。数据源 Provider 一律经 registry_provider.go 的注册表按别名
// 构造，不再写死 newProductProvider（那正是「恒跑商品逻辑」的根因）。
//
// 返回 error 时调用方应视为失败（非零退出）；此时别名不切换，checkpoint 保留以便 --resume。
func syncIndexTarget(ctx context.Context, alias string, opts Options) (*SyncStats, error) {
	reg, err := es.LoadRegistry(opts.IndicesDir)
	if err != nil {
		return nil, err
	}

	target := strings.TrimSpace(opts.Index)
	def, err := resolveProductDef(reg, target, opts.StrictIndexLookup)
	if err != nil {
		return nil, err
	}
	if target == "" {
		target = def.Alias()
	}

	// 按别名分发的前置守卫：调用方声称在处理 alias，解析结果却不是它 → 立即中止。
	// 不拦住就会拿一个业务的数据源往另一个业务的索引里灌（一期已验证过的缺陷形态）。
	if alias != "" && def.Alias() != alias {
		return nil, fmt.Errorf("sync: 目标 %q 解析到别名 %s，与调用方声明的别名 %s 不一致，拒绝灌数（防止跨业务串数据）", target, def.Alias(), alias)
	}

	runOpts := opts
	runOpts.Index = target
	// 数据源由「别名 → Provider」注册表构造；本层编排（checkpoint / 对账 / 调优）目前只
	// 支持商品 provider 形态，别的形态必须显式报错，绝不能悄悄按商品跑。
	p, err := NewProvider(def.Alias(), global.Db, def, runOpts)
	if err != nil {
		return nil, err
	}
	pp, ok := p.(*productProvider)
	if !ok {
		return nil, fmt.Errorf("sync: 别名 %s 的数据源 %T 尚未接入全量灌数编排（checkpoint/对账/调优仅支持商品 provider 形态）", def.Alias(), p)
	}

	// retry-failed：跳过区间扫描，直接复跑失败明细（runtime 才连 ES）。
	if strings.TrimSpace(opts.RetryFailed) != "" {
		return runRetryFailed(ctx, target, runOpts)
	}

	// dry-run：只发读请求并打印请求序列，不写 ES、不改 settings。
	if opts.DryRun {
		return runDryRun(ctx, pp, reg, target, runOpts)
	}

	// resume：读取 checkpoint，把游标下界推进到 lastId 之后，并强制单区间单游标使 id 单调。
	// 必修①后：checkpoint 游标本身已保证「只推进到最小连续覆盖右边界」（sink 按区间记账），
	// 这里额外保留 k=1 作为 resume 路径的保守选择（本轮最小改动，不放开续传并发）。
	// checkpoint sink 始终创建（无论是否 --resume），使首次全量也落盘，中断后 --resume 可达。
	cpPath := checkpointPath(target)
	if opts.Resume {
		cp, lerr := loadCheckpoint(cpPath)
		if lerr != nil {
			if !os.IsNotExist(lerr) {
				return nil, fmt.Errorf("sync: 读取 checkpoint 失败 [%s]: %w", cpPath, lerr)
			}
			fmt.Printf("[resume] 未找到 checkpoint，将从头全量执行\n")
			pp.ck = newCheckpointSink(cpPath, target)
		} else {
			// resume：保留原始 StartedAt 与 Total，只推进 LastID。
			pp.ck = restoreCheckpointSink(cpPath, cp)
			applyResumeState(cp, pp, &runOpts)
		}
	} else {
		pp.ck = newCheckpointSink(cpPath, target)
	}
	// 生产路径设置 Total：Bounds 取得后写入 checkpoint，使落盘文件 total > 0。
	// resume 路径不覆盖：restoreCheckpointSink 已保留原始 Total，重复 setTotal 会用
	// 带游标下界的 Bounds（偏小）覆盖首次全量口径，破坏 resume 的 StartedAt/Total 保留语义。
	if !opts.Resume {
		setTotalFromBounds(ctx, pp)
	}

	// 灌入调优（方案 7.3）：仅当目标是未持有别名的物理索引时才临时关 refresh/副本。
	var restore func(context.Context, *SyncStats) error
	if opts.TuneIngest {
		tunable, terr := isIngestTargetTunable(ctx, reg, target)
		if terr != nil {
			// 必修③：安全判定自身失败（登记别名缺失、别名查询异常等）不等于 target 不可写，
			// 不能把「可以跳过调优」升级成「整个灌数失败」。降级为跳过调优 + 显著告警：
			// fail-closed 的方向是「绝不 PUT /_settings」，安全边界一点没削弱（不调优最坏只是慢）。
			fmt.Printf("[tune-ingest] !! 告警：灌入调优安全判定失败，本次跳过灌入期调优并照常灌数（零 PUT /_settings）: %v\n", terr)
			tunable = false
		}
		if tunable {
			// apply 用独立 ctx（WithoutCancel 派生 + 显式超时）：--timeout 打断也必须先把 settings
			// 落定并注册 restore，否则 target 停在 refresh_interval=-1 调优态无人恢复。
			applyCtx, applyCancel := tuningContext(ctx)
			defer applyCancel()
			rf, aerr := applyIngestTuning(applyCtx, target, def)
			if aerr != nil {
				return nil, aerr
			}
			restore = rf
		}
	}

	if opts.GoGC > 0 {
		prev := debug.SetGCPercent(opts.GoGC)
		defer debug.SetGCPercent(prev)
	}

	stats, runErr := Run(ctx, pp, runOpts, nil)

	// 缺陷 #4 修复：恢复 settings 必须用独立 context（不受 run ctx 取消/超时影响），
	// 否则超时时 PutSettings 必然失败，索引留在调优态无人知晓。
	// 必修③：由 context.Background() 改为 WithoutCancel 派生，既断开取消传播又保留 ctx 值。
	var restoreErr error
	if restore != nil {
		restoreCtx, restoreCancel := tuningContext(ctx)
		defer restoreCancel()
		restoreErr = restore(restoreCtx, stats)
	}

	if runErr != nil {
		if restoreErr != nil {
			// 缺陷 #3 修复：runErr 与 restoreErr 必须同时返回，
			// 否则 Phase 5 rebuild 程序化调用拿不到恢复失败信息，索引留在调优态无人知晓。
			return stats, errors.Join(runErr, fmt.Errorf("sync: 恢复灌入期临时 settings 也失败（索引可能仍处于调优态）: %w", restoreErr))
		}
		return stats, runErr // 部分写入保留、别名不切；checkpoint 不删，供 --resume 续
	}
	if restoreErr != nil {
		return stats, fmt.Errorf("sync: 恢复灌入期临时 settings 失败（索引可能仍处于调优态，请手工核对）: %w", restoreErr)
	}

	// checkpoint 不在这里删——对账可能失败，失败时需要 checkpoint 供 --resume 续传。

	// 对账决策：仅 --ids/--since 局部补数跳过；resume 不跳过（全量口径对账能暴露丢数）。
	if shouldSkipReconcile(opts) {
		fmt.Printf("[对账] 跳过：本次为局部补数（ids/since），不做全量对账\n")
		_ = removeCheckpoint(cpPath) // 局部补数无需 checkpoint，清理
		return stats, nil
	}

	// resume 对账：临时去掉 minExclusiveID 过滤，取全量口径期望值。
	// execResumeReconcile 封装 save/restore 模式，单测直接保护该调用点。
	if opts.Resume {
		err = execResumeReconcile(ctx, pp, target, stats, opts.Tolerance)
	} else {
		err = reconcile(ctx, pp, target, stats, opts.Tolerance)
	}
	if err != nil {
		return stats, err // checkpoint 保留，供 --resume 续传
	}
	// 全部成功（含对账）才删 checkpoint。
	_ = removeCheckpoint(cpPath)
	return stats, nil
}

// runRetryFailed 复跑 admin.ReadFailures 读回的失败项，按 bulk 体积切块重投。
func runRetryFailed(ctx context.Context, target string, opts Options) (*SyncStats, error) {
	docs, err := admin.ReadFailures(opts.RetryFailed)
	if err != nil {
		return nil, err
	}
	// 修复：使用外部注入的 Stats 容器，使 CLI 报表能反映真实计数。
	stats := opts.Stats
	if stats == nil {
		stats = &SyncStats{}
	}
	stats.MarkStart()
	stats.TotalDocs.Store(int64(len(docs)))
	if len(docs) == 0 {
		stats.MarkEnd()
		return stats, nil
	}

	w := defaultWriter(opts)
	bulkSize := clampInt(opts.BulkSize, defaultBulkSize)
	maxFailures := clampInt(opts.MaxFailures, defaultMaxFailures)

	for start := 0; start < len(docs); start += bulkSize {
		end := start + bulkSize
		if end > len(docs) {
			end = len(docs)
		}
		res, err := w(ctx, target, docs[start:end])
		if err != nil {
			stats.MarkEnd()
			return stats, err
		}
		if res != nil {
			stats.SentDocs.Add(int64(res.Sent))
			stats.FailedDocs.Add(int64(res.Failed))
			stats.Batches.Add(1)
			if maxFailures > 0 && stats.FailedDocs.Load() > int64(maxFailures) {
				stats.MarkEnd()
				return stats, fmt.Errorf("sync: 复跑失败项累计失败超过 --max-failures=%d", maxFailures)
			}
		}
		if err := ctx.Err(); err != nil {
			stats.MarkEnd()
			return stats, err
		}
	}
	stats.MarkEnd()
	return stats, nil
}

// runDryRun 只读预演：探测边界后打印将执行的请求序列，不发起任何写请求。
func runDryRun(ctx context.Context, pp *productProvider, reg *es.Registry, target string, opts Options) (*SyncStats, error) {
	bounds, err := pp.Bounds(ctx)
	if err != nil {
		return nil, err
	}
	// 修复：使用外部注入的 Stats 容器，使 CLI 报表能反映真实计数。
	stats := opts.Stats
	if stats == nil {
		stats = &SyncStats{}
	}
	planned := bounds.Total
	if opts.Limit > 0 && opts.Limit < planned {
		planned = opts.Limit
	}
	stats.TotalDocs.Store(planned)

	k := ComputeRangeCount(opts.Concurrency, opts.RangeMultiplier)
	ranges := ComputeRanges(bounds, k)

	fmt.Printf("[dry-run] 目标索引:        %s\n", target)
	fmt.Printf("[dry-run] DB 命中行数:     %d (min=%d, max=%d)\n", bounds.Total, bounds.Min, bounds.Max)
	fmt.Printf("[dry-run] 计划灌入:        %d (--limit 收敛后)\n", planned)
	fmt.Printf("[dry-run] 区间/并发:       %d 区间 / scanner=%d, writer=%d\n", len(ranges), clampInt(opts.Concurrency, defaultConcurrency), clampInt(opts.Concurrency, defaultConcurrency))
	fmt.Printf("[dry-run] chunk/bulk:      chunk=%d, bulk<= %d 条 / %d MB\n", clampInt(opts.ChunkSize, defaultChunkSize), clampInt(opts.BulkSize, defaultBulkSize), bytesToMB(opts.BulkMaxBytes))
	if opts.TuneIngest {
		tunable, terr := isIngestTargetTunable(ctx, reg, target)
		if terr != nil {
			// 必修③：判定失败不阻断预演，降级为「跳过调优」+ 告警（dry-run 本身零写操作）。
			fmt.Printf("[tune-ingest] !! 告警：灌入调优安全判定失败，预演按跳过调优展示: %v\n", terr)
			tunable = false
		}
		if tunable {
			fmt.Printf("[dry-run] 灌入调优:        临时 PUT settings {refresh_interval:-1, replicas:0} -> 结束后恢复声明值\n")
		} else {
			fmt.Printf("[dry-run] 灌入调优:        跳过（目标是别名或已持有别名的索引）\n")
		}
	}
	fmt.Printf("[dry-run] 收尾:            PUT settings(恢复) -> POST _refresh -> GET _count 对账 (tolerance=%d)\n", opts.Tolerance)
	fmt.Printf("[dry-run] 未发起任何写请求。\n")
	return stats, nil
}

// reconcile 对账：admin.Refresh + admin.Count 与 provider 过滤口径下的期望行数比较。
// 期望值 = SELECT COUNT(*) WHERE is_del=0 AND is_show=1（provider.filter 口径），
// 差异 > tolerance 即返回 error（由上层得到非零退出码）。
// 转为 var 以便单测在 SyncProducts 端到端流程中替换（避免依赖真实 ES 连接）。
var reconcile = func(ctx context.Context, pp *productProvider, target string, stats *SyncStats, tolerance int64) error {
	if err := admin.Refresh(ctx, target); err != nil {
		return err
	}
	actual, err := admin.Count(ctx, target)
	if err != nil {
		return err
	}
	bounds, err := pp.Bounds(ctx)
	if err != nil {
		return err
	}
	expected := bounds.Total
	if pp.limit > 0 && pp.limit < expected {
		expected = pp.limit
	}
	stats.TotalDocs.Store(expected)

	diff := actual - expected
	if diff < 0 {
		diff = -diff
	}
	if diff > tolerance {
		return fmt.Errorf("sync: 对账不通过 [target=%s] ES=%d，DB 期望=%d，差异=%d 超出 --tolerance=%d",
			target, actual, expected, diff, tolerance)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 灌入侧调优（方案 7.3）
// ---------------------------------------------------------------------------

// reconcileDecision 是对账是否跳过的纯逻辑输入（提取出来以便单测防护编排路径）。
type reconcileDecision struct {
	IsPartialSync bool
	Resume        bool
}

// buildReconcileDecision 从 Options 构造对账决策（纯函数，可单测防护 isPartialSync 计算逻辑）。
func buildReconcileDecision(opts Options) reconcileDecision {
	return reconcileDecision{
		IsPartialSync: len(opts.IDs) > 0 || !opts.Since.IsZero(),
		Resume:        opts.Resume,
	}
}

// shouldReconcile：仅 --ids/--since 局部定点补数才跳过对账；resume 不跳过（全量口径对账能暴露丢数）。
func shouldReconcile(d reconcileDecision) bool {
	return !d.IsPartialSync
}

// shouldSkipReconcile 封装生产代码的对账跳过条件（供测试直接保护，防止编排路径绕过 shouldReconcile）。
func shouldSkipReconcile(opts Options) bool {
	d := buildReconcileDecision(opts)
	return !shouldReconcile(d)
}

// applyResumeState 把已加载的 checkpoint 应用到 provider 与 runOpts（纯函数，可单测）。
// LastID>0 时设游标下界并强制 k=1，保证 resume 单区间单游标、id 单调不跳段。
func applyResumeState(cp checkpoint, pp *productProvider, runOpts *Options) {
	if cp.LastID > 0 {
		pp.minExclusiveID = cp.LastID
		runOpts.Concurrency = 1
		runOpts.RangeMultiplier = 1
		fmt.Printf("[resume] 从 id=%d 续传（单区间模式）\n", cp.LastID)
	}
}

// execResumeReconcile 执行 resume 对账：临时清零 minExclusiveID 取全量口径，完成后恢复。
// SyncProducts 的 resume 对账路径必须经由此函数（单测直接保护该调用点）。
var execResumeReconcile = func(ctx context.Context, pp *productProvider, target string, stats *SyncStats, tolerance int64) error {
	saved := pp.minExclusiveID
	pp.minExclusiveID = 0
	err := reconcileForTest(ctx, pp, target, stats, tolerance)
	pp.minExclusiveID = saved
	return err
}

// reconcileForTest 是可被单测替换的 reconcile 引用。
// 生产代码始终指向 reconcile；测试可替换为 fake 以捕获调用时的 provider 状态。
var reconcileForTest = reconcile

// setTotalFromBounds 从 provider Bounds 获取 Total 并写入 checkpoint sink。
// SyncProducts 的非 resume 路径必须经由此函数设置 Total（单测直接保护该调用点）。
// boundsLookup 是可被单测替换的 Bounds 查询引用（生产始终指向 pp.Bounds）。
var setTotalFromBounds = func(ctx context.Context, pp *productProvider) {
	b, err := boundsLookup(ctx, pp)
	if err == nil {
		pp.ck.setTotal(b.Total)
	}
}

var boundsLookup = func(ctx context.Context, pp *productProvider) (Bounds, error) {
	var row struct {
		MinID sql.NullInt64 `gorm:"column:min_id"`
		MaxID sql.NullInt64 `gorm:"column:max_id"`
		Cnt   int64         `gorm:"column:cnt"`
	}
	err := pp.filter(ctx).
		Select("MIN(id) AS min_id, MAX(id) AS max_id, COUNT(*) AS cnt").
		Scan(&row).Error
	if err != nil {
		return Bounds{}, fmt.Errorf("sync: 探测 store_product 边界失败: %w", err)
	}
	if row.Cnt == 0 || !row.MinID.Valid || !row.MaxID.Valid {
		return Bounds{Min: 0, Max: 0, Total: 0}, nil
	}
	return Bounds{Min: row.MinID.Int64, Max: row.MaxID.Int64, Total: row.Cnt}, nil
}

// isIngestTargetTunable 判断目标是否可安全临时调优（方案 7.3）。
//
// 权威判定走 admin.IndexAliases（直接查 ES 侧真实别名），Registry 比对作为第二道。
// 任何查询失败 → fail-closed 返回 false + error 上抛（调用点必修③：降级为跳过调优）。
func isIngestTargetTunable(ctx context.Context, reg *es.Registry, target string) (bool, error) {
	// 修复：第一道用 AliasIndexes（别名→物理索引集合），不是 IndexAliases（物理索引→别名）。
	return isIngestTargetTunableWithLookup(ctx, reg, target, aliasIndexesLookup)
}

// isIngestTargetTunableWithLookup 是可注入别名查询的 tunable 判定（测试可注入 fake lookup）。
//
// 判定逻辑（双层覆盖）：
//  1. Registry 登记别名 → AliasIndexes(alias) 得物理索引集合 → 构建 aliasMap → checkIngestTunable 判定；
//  2. 权威兜底：IndexAliases(target) 直接查 ES 侧 target 持有的别名（覆盖第三方未登记别名）。
//
// 任何一次查询返回错误必须 fail-closed（不调优），避免误把生产别名索引当独立索引调优。
func isIngestTargetTunableWithLookup(ctx context.Context, reg *es.Registry, target string,
	lookup func(ctx context.Context, index string) ([]string, error)) (bool, error) {
	aliasNames := reg.Aliases()
	aliasMap := make(map[string][]string)

	// 第一道：Registry 别名 → 物理索引集合（用 AliasIndexes 语义：别名 → 索引列表）。
	for _, alias := range aliasNames {
		indexes, err := lookup(ctx, alias)
		if err != nil {
			fmt.Printf("[tune-ingest] 跳过调优：查询别名 %q 指向失败: %v（fail-closed）\n", alias, err)
			return false, fmt.Errorf("sync: 灌入调优安全判定失败 [alias=%s]: %w", alias, err)
		}
		m := make([]string, len(indexes))
		copy(m, indexes)
		aliasMap[alias] = m
	}

	// 第二道：权威兜底 —— 直接查 ES 侧 target 持有哪些别名。
	// 修复：原实现把 IndexAliases（物理索引→别名）当 AliasIndexes（别名→物理索引）用，
	// 导致传物理索引名时 ES 把它当别名解析 → 404 → (nil,nil) → 误判「无别名，安全调优」。
	// 现在用 IndexAliases 只查 target 本身，覆盖第三方未登记别名指向 target 的场景。
	esAliases, err := indexAliasesLookup(ctx, target)
	if err != nil {
		fmt.Printf("[tune-ingest] 跳过调优：查询 target %q 的 ES 别名失败: %v（fail-closed）\n", target, err)
		return false, fmt.Errorf("sync: 灌入调优安全判定失败 [target=%s]: %w", target, err)
	}
	if len(esAliases) > 0 {
		reason := fmt.Sprintf("target=%s 在 ES 侧持有别名 %v，不可调优", target, esAliases)
		fmt.Printf("[tune-ingest] 跳过调优：%s\n", reason)
		printAliasMap(aliasMap)
		return false, nil
	}

	ok, reason := checkIngestTunable(aliasNames, aliasMap, target)
	if !ok {
		return false, fmt.Errorf("sync: 灌入调优安全判定未通过: %s", reason)
	}
	return true, nil
}

// checkIngestTunable 是 isIngestTargetTunable 的纯逻辑核心（可单测，不依赖 admin 包）。
func checkIngestTunable(aliasNames []string, aliasMap map[string][]string, target string) (bool, string) {
	// 条件 a：target 本身是否是某个别名名
	for _, alias := range aliasNames {
		if alias == target {
			reason := fmt.Sprintf("target=%q 本身就是已注册别名，不可对生产别名 PUT settings", target)
			fmt.Printf("[tune-ingest] 跳过调优：%s\n", reason)
			printAliasMap(aliasMap)
			return false, reason
		}
	}
	// 条件 b：target 是否被任何别名指向
	for alias, indexes := range aliasMap {
		for _, idx := range indexes {
			if idx == target {
				reason := fmt.Sprintf("target=%s 被别名 %s 指向（物理索引集合=%v）", target, alias, indexes)
				fmt.Printf("[tune-ingest] 跳过调优：%s\n", reason)
				printAliasMap(aliasMap)
				return false, reason
			}
		}
	}
	fmt.Printf("[tune-ingest] target=%s 未挂载任何别名，安全调优\n", target)
	return true, ""
}

// printAliasMap 打印当前已知的 alias→物理索引映射（辅助诊断）。
func printAliasMap(m map[string][]string) {
	if len(m) == 0 {
		fmt.Printf("[tune-ingest] 已知别名映射：（空）\n")
		return
	}
	for alias, idxs := range m {
		fmt.Printf("[tune-ingest]   %s → %v\n", alias, idxs)
	}
}

// putSettings 是可被单测替换的 admin.PutSettings 引用（生产始终指向真实实现）。
var putSettings = admin.PutSettings

// indexAliasesLookup 是可被单测替换的 admin.IndexAliases 引用（物理索引 → 别名列表）。
// 生产始终指向 admin.IndexAliases；测试可注入 fake 以模拟 ES 查询失败等场景。
var indexAliasesLookup = admin.IndexAliases

// aliasIndexesLookup 是可被单测替换的 admin.AliasIndexes 引用（别名 → 物理索引列表）。
// 登记别名在 ES 侧不存在（如 orders 草稿索引未建）时它会报错，调用链必须能降级。
var aliasIndexesLookup = admin.AliasIndexes

// ingestTuningTimeout 是灌入期调优/回滚/恢复单次 PUT settings 的硬超时。
const ingestTuningTimeout = 30 * time.Second

// tuningContext 从上游 ctx 派生一个「只继承值、不继承取消」的独立 settings 操作 ctx。
// 与 rebuild.go 长生命周期收尾动作的已有做法对齐：必修③——--timeout / cancel 打断灌数时，
// apply 与 restore 仍必须能把 settings 落回声明值，绝不能把 target 留在调优态。
func tuningContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(context.WithoutCancel(ctx), ingestTuningTimeout)
}

// applyIngestTuning 临时关闭 refresh/副本，返回恢复函数（把 settings 恢复到与声明文件一致）。
//
// 缺陷 #7（调优泄漏）修复：
//  1. 每次 PutSettings 使用独立 context（tuningContext：WithoutCancel 派生 + timeout），
//     不随上游 ctx 取消而中断，但保留上游 ctx 的值（必修③）；
//  2. defer-before-mutate：每项变更之前先登记回滚，已生效的项在后续项失败/被中断时逐一回滚；
//  3. refresh_interval 与 number_of_replicas 分开两次 PutSettings，使部分失败可逐粒度回滚。
//
// 恢复函数接受独立 context，以便在 run ctx 已取消时仍能执行恢复（缺陷 #4）。
func applyIngestTuning(ctx context.Context, target string, def *es.IndexDefinition) (func(context.Context, *SyncStats) error, error) {
	declRefresh := lookupSettingString(def.SettingsMap(), "refresh_interval", "1s")
	declReplicas := lookupSettingInt(def.SettingsMap(), "number_of_replicas", 1)

	type settingItem struct {
		name        string
		applyJSON   string
		restoreJSON string
	}
	items := []settingItem{
		{
			name:        "refresh_interval",
			applyJSON:   `{"index":{"refresh_interval":"-1"}}`,
			restoreJSON: fmt.Sprintf(`{"index":{"refresh_interval":%q}}`, declRefresh),
		},
		{
			name:        "number_of_replicas",
			applyJSON:   `{"index":{"number_of_replicas":0}}`,
			restoreJSON: fmt.Sprintf(`{"index":{"number_of_replicas":%d}}`, declReplicas),
		},
	}

	// 纯 defer-before-mutate：每项变更前先登记 defer 回滚。
	// 若后续项 apply 失败或函数 panic，Go defer 机制自动回滚所有已生效项。
	// success 标记区分「正常返回由 restore 函数负责回滚」与「异常返回由 defer 负责回滚」。
	var appliedCount int
	success := false

	for i, item := range items {
		// 先登记 defer 回滚，再执行变更（defer-before-mutate）。
		// defer 闭包捕获 idx（i 的快照）与 restoreJSON，确保每项回滚正确的配置。
		// 回滚条件 idx < appliedCount：只回滚真正已生效的项，未生效项不发起无意义 PUT。
		idx := i
		restoreJSON := item.restoreJSON
		defer func() {
			if idx < appliedCount && !success {
				rctx, rcancel := tuningContext(ctx)
				defer rcancel()
				_ = putSettings(rctx, target, restoreJSON)
			}
		}()

		// 每次 apply 使用独立 context（继承 ctx 值、不继承取消），不受上游 ctx 取消影响。
		applyCtx, applyCancel := tuningContext(ctx)
		err := putSettings(applyCtx, target, item.applyJSON)
		applyCancel()
		if err != nil {
			// defer 自动回滚已生效项（idx < appliedCount 的项）。
			return nil, fmt.Errorf("sync: 应用灌入期临时 settings [%s] 失败: %w", item.name, err)
		}
		appliedCount = i + 1
	}

	success = true // 全部 apply 成功 → defer 不再回滚，由返回的 restore 函数负责。
	return func(restoreCtx context.Context, stats *SyncStats) error {
		t0 := time.Now()
		var firstErr error
		for i := len(items) - 1; i >= 0; i-- {
			rctx, rcancel := context.WithTimeout(restoreCtx, ingestTuningTimeout)
			err := putSettings(rctx, target, items[i].restoreJSON)
			rcancel()
			if err != nil && firstErr == nil {
				firstErr = err
			}
		}
		if stats != nil {
			stats.RestoreNanos.Add(int64(time.Since(t0)))
		}
		return firstErr
	}, nil
}

// lookupSettingString 递归查声明 settings 里的字符串项（兼容顶层与 index.* 两种层级写法）。
func lookupSettingString(m map[string]any, key, fallback string) string {
	if v, ok := lookupSettingAny(m, key); ok {
		if s, isStr := v.(string); isStr && s != "" {
			return s
		}
	}
	return fallback
}

// lookupSettingInt 递归查声明 settings 里的整数项。
func lookupSettingInt(m map[string]any, key string, fallback int) int {
	v, ok := lookupSettingAny(m, key)
	if !ok {
		return fallback
	}
	switch n := v.(type) {
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return int(i)
		}
	case float64:
		return int(n)
	case int:
		return n
	case string:
		if i, err := strconv.Atoi(strings.TrimSpace(n)); err == nil {
			return i
		}
	}
	return fallback
}

func lookupSettingAny(m map[string]any, key string) (any, bool) {
	if v, ok := m[key]; ok {
		return v, true
	}
	if idx, ok := m["index"].(map[string]any); ok {
		if v, ok := idx[key]; ok {
			return v, true
		}
	}
	for _, v := range m {
		if sub, ok := v.(map[string]any); ok {
			if got, ok := lookupSettingAny(sub, key); ok {
				return got, true
			}
		}
	}
	return nil, false
}

func bytesToMB(b int) int {
	if b <= 0 {
		return admin.DefaultBulkMaxBytes / (1024 * 1024)
	}
	return b / (1024 * 1024)
}

// ---------------------------------------------------------------------------
// Definition 解析与 checkpoint
// ---------------------------------------------------------------------------

// resolveProductDef 从 Registry 定位产品索引声明。
//
// strict=false（编程式默认）：优先按 target（别名或物理名）匹配，回落 ProductAlias——
// 回落必须是「按别名精确命中」，rebuild/cutover/migrate 适配器与单测行为不变。
// 旧版在此额外有一条「ExecutableDefinitions() 只有一条就取它」的兼容兜底：订单转 active 后
// len 变 2 会静默失效（同一个调用在两种登记状态下解析到不同定义），已删除；命不中就报错。
//
// strict=true（CLI 显式传 --index）：非空 target 必须命中「已登记且 active」的定义，否则
// 立即报错并列出当前可用（active）名称。这是缺陷的正面防线：旧兜底会把 --index=shop-order
// 这类「已登记但 draft」或完全未知的名字静默解析到商品定义，再经写死的 productProvider 把
// 【商品】数据灌进订单目标（用户以为在同步订单）。判定发生在任何 bulk/MySQL 触达之前。
func resolveProductDef(reg *es.Registry, target string, strict bool) (*es.IndexDefinition, error) {
	if strict && target != "" {
		// 显式指定：别名命中但未接线（draft）→ 如实报「未启用」；别名与物理名都不在 active 清单→报「未知」。
		if d, ok := reg.Get(target); ok {
			if !d.Executable() {
				return nil, fmt.Errorf("sync: 索引 %q 已登记但 status=%s 未接线（不进可执行清单），拒绝灌数；当前可用（active）：%v",
					target, d.Status(), executableAliases(reg))
			}
			return d, nil
		}
		for _, d := range reg.ExecutableDefinitions() {
			if d.Name() == target {
				return d, nil
			}
		}
		return nil, fmt.Errorf("sync: 未知或未启用的索引 %q（既不是 active 别名，也不是其物理索引名）；当前可用（active）：%v",
			target, executableAliases(reg))
	}
	if target != "" {
		if d, ok := reg.Get(target); ok {
			return d, nil
		}
		for _, d := range reg.All() {
			if d.Name() == target {
				return d, nil
			}
		}
	}
	if d, ok := reg.Get(ProductAlias); ok {
		return d, nil
	}
	return nil, fmt.Errorf("sync: 无法定位产品索引声明（target=%q，已注册别名=%v）；请检查 --index 或 indices-dir", target, reg.Aliases())
}

// executableAliases 返回当前 active（可执行）定义的别名升序清单，供严格校验报错时列出可用名称。
// 只列 active，绝不把 draft（如 shop-order）当作「可用」误导用户。
func executableAliases(reg *es.Registry) []string {
	exec := reg.ExecutableDefinitions()
	out := make([]string, 0, len(exec))
	for _, d := range exec {
		out = append(out, d.Alias())
	}
	return out
}

// checkpoint 是 --resume 的续传状态。
type checkpoint struct {
	Target    string    `json:"target"`
	StartedAt time.Time `json:"started_at"`
	Total     int64     `json:"total"`
	LastID    int64     `json:"last_id"`
}

// checkpointSink 逐批回写 checkpoint（resume 为罕见恢复路径，直接落盘不加节流）。
type checkpointSink struct {
	mu   golangsync.Mutex
	path string
	cp   checkpoint

	// plan 是本次运行的区间计划（按 Lo 升序、无空洞的连续前缀），由 Run 经
	// RangePlanCarrier 注入。游标只沿 plan 推进。
	plan []IDRange
	// prog 按区间 Lo 记账各自进度；并发乱序完成时它把「谁跑得最靠后」
	// 收敛成「低位是否已完整覆盖」，避免 LastID 非单调推进。
	prog map[int64]rangeProgress
}

// rangeProgress 是单个 id 区间的扫描进度。
// 区间内 chunk 游标按 id 升序，所以 lastID 代该区间「已连续读出」的右端。
type rangeProgress struct {
	lastID int64
	done   bool // 该区间游标已走到底（(Lo,Hi] 内的行全部读出并 emit）
}

func newCheckpointSink(path, target string) *checkpointSink {
	return &checkpointSink{path: path, cp: checkpoint{Target: target, StartedAt: time.Now()}}
}

// setTotal 设置 checkpoint 的 Total 字段（全量口径行数）。
// 仅在新建 sink（非 resume）时调用，以便 checkpoint 文件从一开始就包含 Total。
func (s *checkpointSink) setTotal(total int64) {
	s.mu.Lock()
	s.cp.Total = total
	s.mu.Unlock()
}

// restoreCheckpointSink 用已加载的 checkpoint 值（StartedAt/Total）初始化 sink，
// 确保 resume 场景下任务首次启动时刻与总量不被覆盖。
// Phase 5 差量补偿应以 StartedAt（任务首次启动时刻）为 update_time >= 下界，
// 而不是恢复时刻——这条口径很关键，写错会让补偿漏掉中断窗口内的变更。
func restoreCheckpointSink(path string, cp checkpoint) *checkpointSink {
	return &checkpointSink{path: path, cp: cp}
}

// planRanges 登记本次运行的区间计划。
// 输入先按 Lo 升序，再只保留从第一个区间开始的「无空洞连续前缀」：
// 遇到区间间空洞即截断（保守），空洞之后的任何进度都不参与游标推进。
func (s *checkpointSink) planRanges(ranges []IDRange) {
	if s == nil || len(ranges) == 0 {
		return
	}
	sorted := make([]IDRange, len(ranges))
	copy(sorted, ranges)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Lo < sorted[j].Lo })

	plan := make([]IDRange, 0, len(sorted))
	plan = append(plan, sorted[0])
	for _, r := range sorted[1:] {
		if r.Lo != plan[len(plan)-1].Hi {
			break // 出现空洞：截断，其后区间一律不计入游标
		}
		plan = append(plan, r)
	}

	s.mu.Lock()
	s.plan = plan
	if s.prog == nil {
		s.prog = make(map[int64]rangeProgress, len(plan))
	}
	s.mu.Unlock()
}

// observeRange 登记「区间 r 已连续读到 lastID」（本批已从 DB 读出）。
func (s *checkpointSink) observeRange(r IDRange, lastID int64) {
	s.recordRange(r, lastID, false)
}

// completeRange 登记「区间 r 已完整扫描完毕」，游标可推进到其右边界 Hi。
func (s *checkpointSink) completeRange(r IDRange) {
	s.recordRange(r, 0, true)
}

// recordRange 写入单区间进度并把 LastID 推到最小连续覆盖右边界。
// 只在游标真正前进时落盘（单调不回退），避免无效 IO。
func (s *checkpointSink) recordRange(r IDRange, lastID int64, done bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.prog == nil {
		s.prog = make(map[int64]rangeProgress)
	}
	p := s.prog[r.Lo]
	if lastID > p.lastID {
		p.lastID = lastID
	}
	if done {
		p.done = true
	}
	s.prog[r.Lo] = p

	next := s.frontierLocked()
	if next <= s.cp.LastID { // 游标不前进：不落盘、不回退
		s.mu.Unlock()
		return
	}
	s.cp.LastID = next
	// 缺陷 #2 修复：写之前 MkdirAll，确保目录存在；写失败必须上报。
	// 落盘在锁内：并发 scanner 下后写必须反映更新的游标，锁外写会让旧值覆盖新值。
	b, err := json.Marshal(s.cp)
	if err != nil {
		s.mu.Unlock()
		fmt.Fprintf(os.Stderr, "[checkpoint] 序列化失败: %v\n", err)
		return
	}
	dir := filepath.Dir(s.path)
	if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
		s.mu.Unlock()
		fmt.Fprintf(os.Stderr, "[checkpoint] 创建目录失败 [%s]: %v\n", dir, mkErr)
		return
	}
	if wErr := os.WriteFile(s.path, b, 0o644); wErr != nil {
		s.mu.Unlock()
		fmt.Fprintf(os.Stderr, "[checkpoint] 写入失败 [%s]: %v\n", s.path, wErr)
		return
	}
	s.mu.Unlock()
}

// frontierLocked 计算最小连续覆盖右边界（调用方持锁）：
//   - 已完成的区间把边界推到其右边界 Hi，继续看下一个区间；
//   - 第一个「进行中」区间把边界推到其区间内已连续读出的 lastID 后停止；
//   - 尚未开始的区间直接停止——其后任何进度都不计入（宁可重扫不可漏扫）。
//
// plan 为空（未登记区间计划）时返回 0：无法判定连续覆盖 → 保守不推进游标。
func (s *checkpointSink) frontierLocked() int64 {
	var f int64
	for _, r := range s.plan {
		p, ok := s.prog[r.Lo]
		if !ok {
			return f
		}
		if p.done {
			f = r.Hi
			continue
		}
		if p.lastID > r.Lo && p.lastID <= r.Hi {
			f = p.lastID
		}
		return f
	}
	return f
}

// checkpointPath 生成 <checkpointBaseDir>/<target>.checkpoint.json。
func checkpointPath(target string) string {
	return filepath.Join(checkpointBaseDir, sanitizeForFile(target)+".checkpoint.json")
}

func loadCheckpoint(path string) (checkpoint, error) {
	var cp checkpoint
	b, err := os.ReadFile(path)
	if err != nil {
		return cp, err
	}
	if err := json.Unmarshal(b, &cp); err != nil {
		return cp, fmt.Errorf("checkpoint 内容损坏: %w", err)
	}
	return cp, nil
}

func removeCheckpoint(path string) error {
	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// sanitizeForFile 把 target 清洗为文件名安全形态（非 [A-Za-z0-9._-] 逐个替换为下划线）。
func sanitizeForFile(name string) string {
	var sb strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '.' || r == '_' || r == '-' {
			sb.WriteRune(r)
			continue
		}
		sb.WriteRune('_')
	}
	s := sb.String()
	if s == "" || s == "." || s == ".." {
		return "index"
	}
	return s
}
