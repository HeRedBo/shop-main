package sync

// order.go 把订单业务 shop-order 适配到通用同步流水线（DocProvider），并登记进
// registry_provider.go 的三张「别名 → 函数」表，作为继商品之后的第二个业务。
//
// 与商品 provider 的关键差异（照搬商品假设会当场出错）：
//   - _id = order_id（字符串订单号），不是数字主键 id；routing = uid（strconv.FormatInt(uid,10)），
//     与 order-consumer 写入、shop-search-api 读取逐字一致；
//   - 列裁剪扫 store_order，资格是 is_del=0（订单没有 is_show）；
//   - names / product_ids 来自关联表 store_order_cart_info.cart_info（一段 JSON）里的
//     product_info.store_name / product_info.id。装配阶段按【批】用 WHERE oid IN (?) 一次性
//     预取（严禁循环内单条查 = N+1），单测钉死这一 SQL 形态；
//   - order_status / pay_type_name 复用 order_service 导出的同一份实现（见 es.BuildOrderDoc），
//     不抄第二份。
//
// 编排边界：本文件只把「数据源 Provider + 差量补偿 + DB 计数口径」三口接线，
// 不改动 syncIndexTarget 里 product 专属的全量灌数编排（checkpoint/对账/调优仍只认商品形态）。
// 因此 SyncByAlias("shop-order") 会在编排守卫处如实报「尚未接入全量灌数编排」——这是本期既定
// 边界（订单全量由 order-consumer 增量写入维护），接线证据落在 NewProvider 命中 + Run 直连灌数。

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"sync/atomic"
	"time"

	"shop/internal/es"
	"shop/internal/es/admin"
	"shop/internal/models"
	"shop/pkg/chunk"
	"shop/pkg/global"

	"gorm.io/gorm"
)

// OrderAlias 是订单业务在 registry_provider.go 三张表里的键（与 indices/orders.jsonc
// 的 _meta.alias 逐字一致）。
const OrderAlias = "shop-order"

// orderScanColumns 是全量扫描 store_order 的列裁剪清单：主键 id + _id 源 order_id +
// routing 源 uid + 派生所需 paid/status/refund_status + 直接映射字段 + 三个时间。
// 全是订单表真实列名（订单表是 create_time/update_time，【没有 updated_at】）。
// 不含 is_del：GORM 软删已自动过滤 is_del=0，且活行恒 is_del=0、装配无需该列。
const orderScanColumns = "id, order_id, uid, status, refund_status, paid, pay_type, " +
	"shipping_type, pay_time, create_time, update_time"

// orderCompensateColumns 在扫描列基础上多带 is_del：补偿要 Unscoped 读回软删值，
// 靠 BuildOrderDoc 逐行判 index/delete；这些列只是 SELECT 投影，不构成过滤条件。
const orderCompensateColumns = "id, order_id, uid, status, refund_status, paid, pay_type, " +
	"shipping_type, pay_time, create_time, update_time, is_del"

// orderBatch 是 scanner 侧产出、经 docsCh 投递的原始批次：主表行 + 本批预取的 cart_info。
// cart_info 保持「原始 JSON 串」形态下沉到 Assemble（writer 侧）再解析，把 CPU 密集解析
// 从 IO 密集的 scanner 侧挪开，与商品 provider 的装配下沉策略一致。
type orderBatch struct {
	Rows      []models.StoreOrder
	CartInfos map[int64][]string // store_order.id -> 该单全部 cart_info 原始 JSON 串（1:N）
}

// orderProvider 把 store_order 适配到通用流水线。
type orderProvider struct {
	db   *gorm.DB
	def  *es.IndexDefinition
	opts Options

	chunkSize int
	limit     int64
	remaining atomic.Int64

	// stats 由 Run 经 StatsCarrier 注入；非空时扫描侧回写进度分子 ScannedDocs。
	stats *SyncStats
}

// newOrderProvider 构造订单 Provider。opts.Index 应已解析为最终目标（别名或物理名）。
func newOrderProvider(db *gorm.DB, def *es.IndexDefinition, opts Options) *orderProvider {
	op := &orderProvider{
		db:        db,
		def:       def,
		opts:      opts,
		chunkSize: clampInt(opts.ChunkSize, defaultChunkSize),
		limit:     opts.Limit,
	}
	if opts.Limit > 0 {
		op.remaining.Store(opts.Limit)
	}
	return op
}

// AttachStats 实现 StatsCarrier。
func (op *orderProvider) AttachStats(stats *SyncStats) { op.stats = stats }

// Definition 实现 DocProvider。
func (op *orderProvider) Definition() *es.IndexDefinition { return op.def }

// filter 构造带完整过滤条件的查询（is_del=0 显式 + GORM 软删自动 + ids/since）。
// Bounds 与 ScanRange 都基于它，确保区间探测与扫描口径完全一致，并与 OrderDocCount 对齐。
func (op *orderProvider) filter(ctx context.Context) *gorm.DB {
	q := op.db.WithContext(ctx).Model(&models.StoreOrder{}).Where("is_del = ?", 0)
	if len(op.opts.IDs) > 0 {
		q = q.Where("id IN (?)", op.opts.IDs)
	}
	if !op.opts.Since.IsZero() {
		q = q.Where("update_time >= ?", op.opts.Since)
	}
	return q
}

// Bounds 用一条聚合查询拿 MIN/MAX/COUNT；空表用 sql.NullInt64 兜住 NULL，避免 GORM 报错。
func (op *orderProvider) Bounds(ctx context.Context) (Bounds, error) {
	var row struct {
		MinID sql.NullInt64 `gorm:"column:min_id"`
		MaxID sql.NullInt64 `gorm:"column:max_id"`
		Cnt   int64         `gorm:"column:cnt"`
	}
	err := op.filter(ctx).
		Select("MIN(id) AS min_id, MAX(id) AS max_id, COUNT(*) AS cnt").
		Scan(&row).Error
	if err != nil {
		return Bounds{}, fmt.Errorf("sync: 探测 store_order 边界失败: %w", err)
	}
	if row.Cnt == 0 || !row.MinID.Valid || !row.MaxID.Valid {
		return Bounds{Min: 0, Max: 0, Total: 0}, nil
	}
	return Bounds{Min: row.MinID.Int64, Max: row.MaxID.Int64, Total: row.Cnt}, nil
}

// ScanRange 扫描 (Lo,Hi] 区间：复用 pkg/chunk 主键游标，之上预置 id 上下界得分片游标。
// 每批一次 loadCartAgg（批量取 cart_info，消 N+1）后 emit。
func (op *orderProvider) ScanRange(ctx context.Context, r IDRange, emit func(ProviderBatch) error) error {
	db := op.filter(ctx).Select(orderScanColumns).Where("id > ? AND id <= ?", r.Lo, r.Hi)

	return chunk.ChunkById(db, op.chunkSize, func(batch []models.StoreOrder, _ int) error {
		// chunk 回调首行检查 ctx，取消借道 error 中断游标循环（chunk 无 ctx 参数）。
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(batch) == 0 {
			return nil
		}

		cartByOID, err := op.loadCartAgg(ctx, batch)
		if err != nil {
			return err
		}
		if err := emit(orderBatch{Rows: batch, CartInfos: cartByOID}); err != nil {
			return err
		}

		if op.stats != nil {
			op.stats.ScannedDocs.Add(int64(len(batch)))
		}
		if op.limit > 0 {
			if left := op.remaining.Add(-int64(len(batch))); left <= 0 {
				return ErrLimitReached
			}
		}
		return nil
	})
}

// Assemble 在 writer 侧把原始批次装配成 []admin.Doc：逐行 BuildOrderDoc + _id/routing。
// 本方法零 DB 触达（cart_info 已在 ScanRange 批量预取进 CartInfos），这是「无 N+1」的
// 结构性保证 —— 装配热路径里没有任何查询。
func (op *orderProvider) Assemble(batch ProviderBatch) ([]admin.Doc, error) {
	ob, ok := batch.(orderBatch)
	if !ok {
		return nil, fmt.Errorf("sync: order provider 收到非预期批次类型 %T", batch)
	}
	docs := make([]admin.Doc, 0, len(ob.Rows))
	for i := range ob.Rows {
		row := &ob.Rows[i]
		di, eligible := es.BuildOrderDoc(row, ob.CartInfos[row.Id])
		if !eligible {
			continue
		}
		// _id / routing 必须走 OrderDocID/OrderRouting，与 order-consumer 逐字一致，
		// 否则同一订单在全量与增量灌入时落到不同 shard 造成重复。
		docs = append(docs, admin.Doc{ID: di.OrderDocID(), Routing: di.OrderRouting(), Src: di})
	}
	return docs, nil
}

// loadCartAgg 对一批主表行只发【一条】WHERE oid IN (?) 预取 cart_info（消除 N+1）。
// 委托给可注入的 orderCartLoader，单测据此断言「每批恰好一次调用」。
func (op *orderProvider) loadCartAgg(ctx context.Context, rows []models.StoreOrder) (map[int64][]string, error) {
	ids := orderIDs(rows)
	if len(ids) == 0 {
		return nil, nil
	}
	return orderCartLoader(ctx, op.db, ids)
}

// orderCartLoader 是批量取 cart_info 的可注入出口（生产 = defaultOrderCartLoader）。
// 包级变量只为单测注入存在，生产始终指向默认实现。
var orderCartLoader = defaultOrderCartLoader

// defaultOrderCartLoader 用单条 WHERE oid IN (?) 批量取 store_order_cart_info 并按 oid 分组。
// 注意 store_order_cart_info 无软删，不叠加 is_del；一个 oid 可对应多行（1:N），全部收进切片。
func defaultOrderCartLoader(ctx context.Context, db *gorm.DB, oids []int64) (map[int64][]string, error) {
	var infos []models.StoreOrderCartInfo
	if err := db.WithContext(ctx).Where("oid IN (?)", oids).Find(&infos).Error; err != nil {
		return nil, fmt.Errorf("sync: 批量预取 cart_info 失败: %w", err)
	}
	out := make(map[int64][]string, len(oids))
	for _, in := range infos {
		out[in.Oid] = append(out[in.Oid], in.CartInfo)
	}
	return out, nil
}

// orderIDs 抽取一批主表行的数字主键 id（预分配，热路径不用 append 逐步扩容）。
// 这是 cart_info 关联查询的 join key：store_order_cart_info.oid = store_order.id。
func orderIDs(rows []models.StoreOrder) []int64 {
	ids := make([]int64, 0, len(rows))
	for i := range rows {
		ids = append(ids, rows[i].Id)
	}
	return ids
}

// ---------------------------------------------------------------------------
// 差量补偿：CompensateOrder（对应 registry 的 compensators[OrderAlias]）
// ---------------------------------------------------------------------------

// orderCompensateScanFunc 是订单补偿扫描口的可注入签名，与商品 compensateScanFunc 同构。
type orderCompensateScanFunc func(ctx context.Context, since time.Time, chunkSize int,
	cb func(batch []models.StoreOrder) error) error

// orderCompensateScan / orderCompensateBulk 生产实现见下方 default*；包级变量供单测注入。
var (
	orderCompensateScan orderCompensateScanFunc = defaultOrderCompensateScan
	orderCompensateBulk                         = defaultCompensateBulk // 复用 delta.go 的 admin.IndexBulk 包装
)

// defaultOrderCompensateScan 用 Unscoped() + update_time >= since + 主键游标读取补偿行。
func defaultOrderCompensateScan(ctx context.Context, since time.Time, chunkSize int,
	cb func(batch []models.StoreOrder) error) error {
	if global.Db == nil {
		return fmt.Errorf("sync: MySQL 客户端未初始化，无法执行订单差量补偿")
	}
	var lastID int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		batch, err := orderCompensateFetchPage(ctx, global.Db, since, chunkSize, lastID)
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			return nil
		}
		if err := cb(batch); err != nil {
			return err
		}
		lastID = batch[len(batch)-1].Id
		if len(batch) < chunkSize {
			return nil
		}
	}
}

// orderCompensateFetchPage 取一页补偿行，是 defaultOrderCompensateScan 的可测内核。
func orderCompensateFetchPage(ctx context.Context, db *gorm.DB, since time.Time, chunkSize int, lastID int64) ([]models.StoreOrder, error) {
	var batch []models.StoreOrder
	err := orderCompensateScope(db.Session(&gorm.Session{}), since, chunkSize, lastID).Find(&batch).Error
	if err != nil {
		return nil, fmt.Errorf("sync: 订单差量补偿扫描失败 [last_id=%d, since=%s]: %w",
			lastID, since.Format(time.RFC3339), err)
	}
	return batch, nil
}

// orderCompensateScope 构造订单补偿查询的过滤口径，与商品 compensateScope 同构，破坏即红：
//   - Unscoped()：连软删单一起读，才能对已删除订单发 delete（否则删过的单永远留在索引里）；
//   - WHERE update_time >= ?：下界取任务首次启动时刻；
//   - WHERE id > ?：主键游标，ORDER BY id ASC LIMIT 保证不重不漏；
//   - WHERE 里【绝不出现 is_del 过滤】（软删靠 Unscoped 读回、资格判定在 BuildOrderDoc）。
//
// 注意：update_time >= ? 是补偿的【时间窗下界】语义（订单表列名 update_time，非 updated_at），
// 与商品 compensateScope 完全同构；订单侧没有 is_show 这一维。
func orderCompensateScope(db *gorm.DB, since time.Time, chunkSize int, lastID int64) *gorm.DB {
	return db.Model(&models.StoreOrder{}).
		Unscoped().
		Select(orderCompensateColumns).
		Where("update_time >= ?", since).
		Where("id > ?", lastID).
		Order("id ASC").
		Limit(chunkSize)
}

// orderCompensateDocs 把一批补偿行装配成 []admin.Doc：逐行按 BuildOrderDoc 决定 index/delete。
//
// _id / routing 一律与全量同步逐字一致：合格行走 OrderIndex.OrderDocID/OrderRouting；
// 不合格行（软删 / 空订单号）用行 order_id / uid 现算同样的字符串（delete 必须用同一个 _id
// 才能删掉历史残留）。OrderId 为空的行直接跳过：无订单号既无法定 _id 也无法幂等回查/删除。
func orderCompensateDocs(rows []models.StoreOrder, cartByOID map[int64][]string) []admin.Doc {
	docs := make([]admin.Doc, 0, len(rows))
	for i := range rows {
		row := &rows[i]
		if row.OrderId == "" {
			continue
		}
		if di, eligible := es.BuildOrderDoc(row, cartByOID[row.Id]); eligible {
			docs = append(docs, admin.Doc{ID: di.OrderDocID(), Routing: di.OrderRouting(), Src: di})
			continue
		}
		// 不合格（软删）：发 delete，Src 置 nil、Delete=true，_id/routing 与全量同源。
		docs = append(docs, admin.Doc{
			ID:      row.OrderId,
			Routing: strconv.FormatInt(row.Uid, 10),
			Delete:  true,
		})
	}
	return docs
}

// CompensateOrder 对 target 执行一次订单差量补偿：扫描 update_time >= since 的全部订单行
// （Unscoped），逐行按 BuildOrderDoc 决定 index/delete，走同一 op_type=index bulk；
// 每批一次 WHERE oid IN 预取 cart_info（与全量共用同一个 orderCartLoader，绝不 N+1）。
//
// 契约与商品 Compensate 一致：since 必须是任务【首次启动时刻】；target 是物理索引名；
// ctx 取消/超时即时收敛，返回已累计 Stats 与 ctx.Err()（补偿幂等，重跑同一 since 可续）。
func CompensateOrder(ctx context.Context, target string, since time.Time, opts CompensateOptions) (Stats, error) {
	var st Stats
	if ctx == nil {
		return st, fmt.Errorf("sync: CompensateOrder 需要非空 context")
	}
	chunkSize := opts.chunkSize()
	bulkSize := opts.bulkSize()

	buf := make([]admin.Doc, 0, bulkSize)

	flush := func() error {
		if len(buf) == 0 {
			return nil
		}
		docs := buf
		buf = make([]admin.Doc, 0, bulkSize)
		res, err := orderCompensateBulk(ctx, target, docs)
		if err != nil {
			return err
		}
		st.Batches++
		if res != nil {
			st.Failed += int64(res.Failed)
		}
		return nil
	}

	scanErr := orderCompensateScan(ctx, since, chunkSize, func(batch []models.StoreOrder) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		st.Scanned += int64(len(batch))
		// 批量预取本批 cart_info（一条 IN，覆盖整批），装配走与全量同源的 BuildOrderDoc。
		cartByOID, err := orderCartLoader(ctx, global.Db, orderIDs(batch))
		if err != nil {
			return err
		}
		for _, d := range orderCompensateDocs(batch, cartByOID) {
			if d.Delete {
				st.Deleted++
			} else {
				st.Indexed++
			}
			buf = append(buf, d)
			if len(buf) >= bulkSize {
				if err := flush(); err != nil {
					return err
				}
			}
		}
		return nil
	})

	if ferr := flush(); ferr != nil && scanErr == nil {
		scanErr = ferr
	}
	if scanErr != nil {
		return st, scanErr
	}
	return st, nil
}

// 编译期自检：orderProvider 确实实现 DocProvider。
var _ DocProvider = (*orderProvider)(nil)
