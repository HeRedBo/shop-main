package es

// ingest.go 承载各编排共用的「灌数侧」契约与判定：灌数/计数窄接口（SyncFunc/CountFunc）、
// DB 期望数统计口径（ProductDocCount/OrderDocCount）、对账判定（ReconcileCounts）、
// 灌入期调优与恢复请求体（IngestTuneBody/IngestRestoreBody）。
// cutover / rebuild / migrate 共用这一层。
//
// 历史说明：原在 cutover.go，现按职责拆出（同包内纯搬家，零行为变化）。
// 窄接口刻意不引用 internal/es/sync 的内部类型：sync 的签名再变只冲击 CLI 装配层。

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"shop/internal/models"
	"shop/pkg/global"
)

// ---------------------------------------------------------------------------
// 窄接口：灌数回调 + DB 期望数
// ---------------------------------------------------------------------------

// SyncSummary 是灌数回调返回的最小统计集。
//
// 刻意不直接使用 sync.SyncStats：那个类型的字段是 atomic.Int64 且正被重构，
// 一旦跨层引用会把 #5 的签名变动传导进割接编排。这里只留割接决策真正关心的量。
type SyncSummary struct {
	Target  string        // 实际灌入目标（割接场景恒为新建的物理索引名）
	Scanned int64         // 从 DB 读出的行数
	Sent    int64         // 成功写入 ES 的文档数
	Failed  int64         // item 级失败文档数
	Batches int64         // 提交的 bulk 批次
	Elapsed time.Duration // 灌数耗时
}

// SyncFunc 是割接第 3 步的灌数入口，由 CLI 层装配成对「按别名分发的灌数实现」的调用。
//
// 约定：target 是**物理索引名**（不持有任何别名），实现内部必须以 op_type=index
// 幂等写入 —— create 对重复 _id 会返回 409 version_conflict_engine_exception。
//
// alias 是本次编排正在处理的别名（多索引 --alias all 时逐条不同）：灌数实现据此从
// 「别名 → 数据源」注册表里取对应的 Provider，**绝不能拿别名去猜**、也绝不能恒定取商品。
// 一期只有一个别名时传什么都落到商品实现，行为与升级前逐字一致；订单接入后无需再改本层。
type SyncFunc func(ctx context.Context, alias, target string) (SyncSummary, error)

// CountFunc 提供 DB 侧期望文档数，同样按 alias 分发到该别名登记的统计口径。
// 未登记口径的别名必须返回错误（宁可报不出来，绝不拿商品口径套别的索引）。
type CountFunc func(ctx context.Context, alias string) (int64, error)

// productCountFunc 把商品专用统计器适配成带别名的 CountFunc。
// 仅用于「调用方未注入 DBCount」的回落，保持升级前 nil → ProductDocCount 的既有语义。
func productCountFunc(ctx context.Context, _ string) (int64, error) {
	return ProductDocCount(ctx)
}

// ProductDocCount 按对账口径统计应进入索引的商品数。
//
// 口径与 BuildIndexDoc / productProvider.filter 完全一致：GORM 的
// soft_delete.DeletedAt(softDelete:flag) 会自动为查询追加 is_del=0，
// 这里再显式加一次 is_del=0 让 SQL 自解释（重复条件不影响结果集），
// is_show=1 必须显式加。任何一处口径改动都必须同步另外两处。
func ProductDocCount(ctx context.Context) (int64, error) {
	if global.Db == nil {
		return 0, fmt.Errorf("es: MySQL 客户端未初始化，无法统计商品期望数")
	}
	var n int64
	q := global.Db.WithContext(ctx).Model(&models.StoreProduct{}).
		Where("is_del = 0").Where("is_show = 1")
	if err := q.Count(&n).Error; err != nil {
		return 0, fmt.Errorf("es: 统计商品期望数失败（is_del=0 AND is_show=1）: %w", err)
	}
	return n, nil
}

// OrderDocCount 按对账口径统计应进入 shop-order 索引的订单数。
//
// 口径与 orderProvider.filter / BuildOrderDoc 完全一致：订单【没有 is_show 概念】，
// 入索引资格只有「未软删」——GORM 的 soft_delete.DeletedAt(softDelete:flag) 会自动为
// 查询追加 is_del=0，这里再显式加一次让 SQL 自解释（重复条件不影响结果集）。
// 绝不能拿商品口径（多一条 is_show=1）去套订单，那会少算、对账永远不通过。
func OrderDocCount(ctx context.Context) (int64, error) {
	if global.Db == nil {
		return 0, fmt.Errorf("es: MySQL 客户端未初始化，无法统计订单期望数")
	}
	var n int64
	q := global.Db.WithContext(ctx).Model(&models.StoreOrder{}).Where("is_del = 0")
	if err := q.Count(&n).Error; err != nil {
		return 0, fmt.Errorf("es: 统计订单期望数失败（is_del=0）: %w", err)
	}
	return n, nil
}

// ---------------------------------------------------------------------------
// 对账判定（cutover / rebuild / migrate 共用同一口径）
// ---------------------------------------------------------------------------

// ReconcileCounts 对账：diff 为 ES 相对 DB 期望数的偏差，超容差即不达标（必须中止于切别名之前）。
//
// **负容差 = 永不可达标，不是「严格相等」**：与 RebuildOptions.Tolerance 同一口径，
// 两层共用这一个判定函数。把负值静默归 0 当严格相等用，运维会以为「给了容差」而实际按 0
// 判定，并且 --force 分支会被绕开（审查必修④报出的正是两层各写一套语义）。
func ReconcileCounts(dbExpected, esCount, tolerance int64) (diff int64, ok bool) {
	diff = esCount - dbExpected
	if tolerance < 0 {
		return diff, false
	}
	abs := diff
	if abs < 0 {
		abs = -abs
	}
	return diff, abs <= tolerance
}

// ---------------------------------------------------------------------------
// 灌入期调优与恢复请求体
// ---------------------------------------------------------------------------

// DefaultRefreshInterval 是恢复灌数调优时缺失声明值的兜底，与 product.jsonc 一致。
const DefaultRefreshInterval = "1s"

// settingLookup 在「顶层简写 + settings.index 嵌套 + flat dotted」三种形态里找值。
// 声明文件是混合层级写法（refresh_interval 在顶层、sort 在 index 下），线上回读是扁平，
// 这里一次覆盖，避免各处再写第二套取值逻辑。
func settingLookup(settings map[string]any, key string) (any, bool) {
	if settings == nil {
		return nil, false
	}
	if v, ok := settings[key]; ok {
		return v, true
	}
	dotted := "index." + key
	if v, ok := settings[dotted]; ok {
		return v, true
	}
	if idx, ok := settings["index"].(map[string]any); ok {
		if v, ok := idx[key]; ok {
			return v, true
		}
	}
	return nil, false
}

// IngestTuneBody 返回灌数期提速 settings：refresh 关闭 + 副本归零。
//
// 只允许对**不持有别名**的物理索引用（割接场景新建的 v1 天然满足）。
// 注意集群健康会因此变 yellow —— 单节点本无副本可放，属预期，不当故障处理。
func IngestTuneBody() string {
	return `{"index":{"refresh_interval":"-1","number_of_replicas":0}}`
}

// IngestRestoreBody 从声明 settings 取回 refresh_interval / number_of_replicas，
// 生成灌数后的恢复请求体。缺失声明值时回落到 1s / 1（与 product.jsonc 一致）。
func IngestRestoreBody(declared map[string]any) string {
	refresh, ok := settingLookup(declared, "refresh_interval")
	if !ok {
		refresh = DefaultRefreshInterval
	}
	replicas, ok := settingLookup(declared, "number_of_replicas")
	if !ok {
		replicas = 1
	}
	b, err := json.Marshal(map[string]any{"index": map[string]any{
		"refresh_interval":   refresh,
		"number_of_replicas": replicas,
	}})
	if err != nil {
		// 值全部来自 JSON 解析结果，正常不可能失败；兜底成声明默认值而不是中止割接。
		return `{"index":{"refresh_interval":"1s","number_of_replicas":1}}`
	}
	return string(b)
}
