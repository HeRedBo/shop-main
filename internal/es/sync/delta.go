package sync

// delta.go 实现 Phase 5 蓝绿重建「切换后」的差量补偿（方案第六节第 7 步）。
//
// 为什么必须在原子切别名之后再跑一次：灌数（SyncProducts）从任务启动到写满 next 物理
// 索引要花一段时间，这期间 MySQL 里被改动过的商品不会被这次全量读到。若不补偿，切换瞬间
// 之前的增量就永久丢失 —— 这是本模块最需要防住的语义。因此补偿的下界必须取任务【首次
// 启动时刻】（不是恢复/重跑时刻）：早于该时刻的变更已被这次全量覆盖，晚于它的必须补。
//
// 与全量同步的三点口径差异（务必区分，否则补偿会漏删或对账错位）：
//  1. 必须 Unscoped()：全量走 is_del=0 自动过滤，只读到「活行」；补偿要看到软删行，
//     才能对它们发出 delete 动作。少了 Unscoped，删过的商品会永远留在索引里。
//  2. 不加 is_show 过滤：下架商品在全量里根本不会被扫到，补偿同样要靠 BuildIndexDoc
//     的资格判定把它们判成 delete，而不是靠 SQL 层直接排除 —— 排除就删不掉历史残留。
//  3. 逐行按 BuildIndexDoc 决定 index 还是 delete，走同一 admin.IndexBulk，
//     一律 op_type=index 幂等（create 对重复 _id 会 409 version_conflict_engine_exception）。
//
// 解耦：本文件只新增，不改动 sync 包任何既有文件；写出口与扫描口都做成可注入函数变量，
// 与 product.go 里 putSettings / boundsLookup / reconcileForTest 的既证做法一致，
// 单测无需 MySQL / ES 连接即可覆盖全部编排骨架。

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"shop/internal/es"
	"shop/internal/es/admin"
	"shop/internal/models"
	"shop/pkg/global"

	"gorm.io/gorm"
)

// Stats 是一次差量补偿的统计快照（全部普通整型，Run 返回后只读，无需 atomic：
// 补偿按 id 游标串行扫描，不在并发流水线里）。
type Stats struct {
	Scanned int64 // 从 DB 读出的行数（含软删 / 下架，Unscoped 口径）
	Indexed int64 // 判定为「应入索引」并成功写入的文档数
	Deleted int64 // 判定为「不应入索引」并发出 delete 的文档数
	Failed  int64 // item 级失败文档数
	Batches int64 // 提交的 bulk 批次
}

// CompensateOptions 是差量补偿的可调项，零值可用（缺省回落内置默认）。
type CompensateOptions struct {
	ChunkSize int // 每批读取行数（<=0 回落 defaultChunkSize）
	BulkSize  int // 单个 bulk 的最大文档数（<=0 回落 defaultBulkSize）
}

func (o CompensateOptions) chunkSize() int { return clampInt(o.ChunkSize, defaultChunkSize) }
func (o CompensateOptions) bulkSize() int  { return clampInt(o.BulkSize, defaultBulkSize) }

// compensateBatch 是从 DB 读出的一批原始行。
type compensateBatch = []models.StoreProduct

// compensateScanFunc 是扫描口的可注入签名：按 update_time >= since 且 id > lastID
// 游标读取一批行，交给 cb。生产实现走全局 MySQL，单测注入假数据。
//
// 关键约束（defaultCompensateScan 逐字落地）：必须 Unscoped()，必须带 update_time >=
// since 下界，绝不含 is_show / is_del 过滤条件。cb 返回 error 时立即上抛以中断游标循环。
type compensateScanFunc func(ctx context.Context, since time.Time, chunkSize int,
	cb func(batch compensateBatch) error) error

// compensateBulkFunc 是写出口的可注入签名，生产固定委托 admin.IndexBulk（内部 op_type=index）。
type compensateBulkFunc func(ctx context.Context, target string, docs []admin.Doc) (*admin.BulkResult, error)

// compensateScan 生产实现见 defaultCompensateScan；导出为包级变量仅为单测注入。
var compensateScan = defaultCompensateScan

// compensateBulk 生产实现见 defaultCompensateBulk；包级变量供单测注入假 bulk 出口。
var compensateBulk = defaultCompensateBulk

// defaultCompensateScan 用 Unscoped() + update_time >= since + 主键游标读取补偿行。
//
// 每批一个独立 Session（在 compensateScope 里），避免 GORM 条件在游标循环里累积；
// 回调首行检查 ctx 使取消/超时能真正打断游标。
func defaultCompensateScan(ctx context.Context, since time.Time, chunkSize int,
	cb func(batch compensateBatch) error) error {
	if global.Db == nil {
		return fmt.Errorf("sync: MySQL 客户端未初始化，无法执行差量补偿")
	}
	var lastID int64
	for {
		batch, err := compensateFetchPage(ctx, global.Db, since, chunkSize, lastID)
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

// compensateFetchPage 取一页补偿行，是 defaultCompensateScan 的可测内核。
func compensateFetchPage(ctx context.Context, db *gorm.DB, since time.Time, chunkSize int, lastID int64) (compensateBatch, error) {
	var batch compensateBatch
	err := compensateScope(db.Session(&gorm.Session{}), since, chunkSize, lastID).Find(&batch).Error
	if err != nil {
		return nil, fmt.Errorf("sync: 差量补偿扫描失败 [last_id=%d, since=%s]: %w",
			lastID, since.Format(time.RFC3339), err)
	}
	return batch, nil
}

// compensateScope 构造补偿查询的完整过滤口径 —— 这条 SQL 形态是差量补偿的语义心脏，
// 写错方向（漏 Unscoped / 误加 is_show / 下界取错）都会静默漏数据，必须被测试钉死：
//   - Unscoped()：连软删行一起读，才能对已删除商品发 delete；
//   - WHERE update_time >= ?：下界取任务首次启动时刻；
//   - WHERE id > ?：主键游标，ORDER BY id ASC LIMIT chunkSize 保证不重不漏；
//   - 绝不出现 is_show = ? 过滤条件（软删靠 Unscoped 读回、下架资格判定在 BuildIndexDoc，
//     都不在 SQL 层排除，否则删不掉历史残留）。
func compensateScope(db *gorm.DB, since time.Time, chunkSize int, lastID int64) *gorm.DB {
	return db.Model(&models.StoreProduct{}).
		Unscoped().
		Select(compensateColumns).
		Where("update_time >= ?", since).
		Where("id > ?", lastID).
		Order("id ASC").
		Limit(chunkSize)
}

// compensateColumns 是补偿扫描的列裁剪：BuildIndexDoc 需要的全部字段 + 资格判定字段。
// 与全量的 productScanColumns 一样带 is_show，但这里额外显式选 is_del（Unscoped 读回软删值），
// 且这些列只是 SELECT 清单，不构成过滤条件 —— 过滤口径全在 compensateScope。
const compensateColumns = "id, store_name, store_info, keyword, cate_id, price, sales, " +
	"ficti, is_hot, is_benefit, is_best, is_new, is_postage, is_good, description, " +
	"create_time, update_time, is_show, is_del"

// defaultCompensateBulk 生产写出口：admin.IndexBulk（内部固定 op_type=index，幂等覆盖，绝不 create）。
func defaultCompensateBulk(ctx context.Context, target string, docs []admin.Doc) (*admin.BulkResult, error) {
	return admin.IndexBulk(ctx, target, docs)
}

// compensateDocs 把一批原始行装配成 []admin.Doc：逐行按 BuildIndexDoc 决定 index 还是 delete。
//
// _id / routing 一律与全量同步逐字一致：合格行走 ProductIndex.BulkDocID/BulkRouting；
// 不合格行（拿不到有效 ProductIndex）按行 id / cate_id 现算同样的字符串（delete 必须用
// 同一个 _id 才能删掉历史残留）。Id==0 的行直接跳过：无主键既无法定 _id 也无法幂等回查。
func compensateDocs(rows compensateBatch) []admin.Doc {
	docs := make([]admin.Doc, 0, len(rows))
	for i := range rows {
		row := &rows[i]
		if row.Id == 0 {
			continue
		}
		if di, eligible := es.BuildIndexDoc(row); eligible {
			docs = append(docs, admin.Doc{ID: di.BulkDocID(), Routing: di.BulkRouting(), Src: di})
			continue
		}
		// 不合格（软删 / 下架 / nil）：发 delete，Src 置 nil、Delete=true。
		docs = append(docs, admin.Doc{
			ID:      strconv.FormatInt(row.Id, 10),
			Routing: strconv.Itoa(row.CateId),
			Delete:  true,
		})
	}
	return docs
}

// Compensate 对 target 执行一次差量补偿：扫描 update_time >= since 的全部行（Unscoped、
// 不加 is_show 过滤），逐行按 BuildIndexDoc 决定 index/delete，走同一 op_type=index bulk。
//
// 契约：
//   - since 必须是任务【首次启动时刻】（checkpoint.StartedAt），不是恢复时刻 —— 取错会静默漏掉
//     中断窗口内的变更，这是本函数最关键的入参语义；
//   - target 是新物理索引名（蓝绿切别名后别名也已指向它，二者等价，显式用物理名更直观）；
//   - ctx 取消/超时：扫描与 bulk 都即时收敛，返回已累计的 Stats 与 ctx.Err()（已提交批次不回滚，
//     补偿是幂等的，重跑同一 since 即可续上）。
func Compensate(ctx context.Context, target string, since time.Time, opts CompensateOptions) (Stats, error) {
	var st Stats
	if ctx == nil {
		return st, fmt.Errorf("sync: Compensate 需要非空 context")
	}
	chunkSize := opts.chunkSize()
	bulkSize := opts.bulkSize()

	buf := make([]admin.Doc, 0, bulkSize)

	// flush 提交当前缓冲；返回 error 时中断整个补偿。
	flush := func() error {
		if len(buf) == 0 {
			return nil
		}
		docs := buf
		buf = make([]admin.Doc, 0, bulkSize)
		res, err := compensateBulk(ctx, target, docs)
		if err != nil {
			return err
		}
		st.Batches++
		if res != nil {
			st.Failed += int64(res.Failed)
		}
		return nil
	}

	scanErr := compensateScan(ctx, since, chunkSize, func(batch compensateBatch) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		st.Scanned += int64(len(batch))
		for _, d := range compensateDocs(batch) {
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

	// 提交尾部缓冲：scanErr 为 ctx 取消时也尽量把已装配但未提交的动作发出去（补偿幂等，无害）。
	if ferr := flush(); ferr != nil && scanErr == nil {
		scanErr = ferr
	}

	if scanErr != nil {
		return st, scanErr
	}
	return st, nil
}
