package sync

// registry_provider.go 是灌数层的「别名 → 数据源」注册表，也是本层唯一的按业务分发口。
//
// 为什么需要它：cmd/cli 的 es:migrate / es:rebuild / es:cutover 都是**按别名逐条循环**的
// 编排（--alias all 会遍历 Registry 里每条 active 声明），但一期每个注入点都恒调
// sync.SyncProducts / es.ProductDocCount —— 订单转 active 之后，同一次循环会拿商品
// Provider 往订单索引灌数、拿商品口径统计订单期望数，且**一声不吭**。
//
// 本文件把 cmd/cli/es_status.go 已验收的「别名 → 函数」查表范式推广到灌数层：
// 编排侧只按别名调下面三个薄封装，新增业务 = 在三张表各补一条表项，调用点一行不改。
// 本期只有 shop-product 一条 —— 行为与升级前逐字一致，差别只在「取不到就报错」而非「回落商品」。
//
// 三张表分别覆盖一次灌数的三个口径面：
//   - providers   ：全量灌数读哪个数据源（DocProvider）
//   - compensators：切换/割接之后的差量补偿扫哪张表
//   - dbCounters  ：对账时 DB 侧期望数怎么算（es:status 的 ES/DB 比对共用同一份）

import (
	"context"
	"fmt"
	"sort"
	"time"

	"shop/internal/es"

	"gorm.io/gorm"
)

// ProviderFactory 构造某个业务的数据源。def 已由 Registry 解析、opts.Index 已是最终目标
// （别名或物理索引名），与 newProductProvider 的入参形态一致。
type ProviderFactory func(db *gorm.DB, def *es.IndexDefinition, opts Options) DocProvider

// providers 是「别名 → 数据源」注册表。键必须是 indices/*.jsonc 里的 _meta.alias，
// 未登记的别名一律拒绝灌数（绝不静默回落商品实现）。
var providers = map[string]ProviderFactory{
	ProductAlias: func(db *gorm.DB, def *es.IndexDefinition, opts Options) DocProvider {
		return newProductProvider(db, def, opts)
	},
	// shop-order（阶段二接入）：_id=order_id（字符串订单号）、routing=strconv.FormatInt(uid,10)，
	// 与 order-consumer 写入 / shop-search-api 读取逐字一致；全量扫 store_order WHERE is_del=0，
	// 每批一条 WHERE oid IN 预取 cart_info 装配（详见 order.go）。登记本条后按别名分发才命中订单 provider。
	OrderAlias: func(db *gorm.DB, def *es.IndexDefinition, opts Options) DocProvider {
		return newOrderProvider(db, def, opts)
	},
}

// compensateFactory 是差量补偿的登记形态，与包内 Compensate 逐字同签名。
type compensateFactory func(ctx context.Context, target string, since time.Time, opts CompensateOptions) (Stats, error)

// compensators 是「别名 → 差量补偿实现」注册表。
var compensators = map[string]compensateFactory{
	ProductAlias: Compensate,
	OrderAlias:   CompensateOrder,
}

// DBCounter 是「别名 → DB 期望数」的口径签名（无别名参数，便于 es:status 直接调用）。
type DBCounter func(ctx context.Context) (int64, error)

// dbCounters 是「别名 → DB 统计口径」注册表，对账与 es:status 共用这一份唯一事实源。
//
// 只有登记过的别名才允许参与 ES/DB 计数比对；未登记的别名必须报错 / 报「不可比」，
// 绝不拿商品口径（is_del=0 AND is_show=1）去套别的索引 —— 口径错配比「没比」更有害。
var dbCounters = map[string]DBCounter{
	ProductAlias: es.ProductDocCount,
	OrderAlias:   es.OrderDocCount,
}

// ProviderByAlias 查表返回某个别名的数据源构造器。
// 未登记即 ok=false，由调用方决定是报错还是降级。
func ProviderByAlias(alias string) (ProviderFactory, bool) {
	f, ok := providers[alias]
	return f, ok
}

// NewProvider 按别名构造数据源；未登记别名直接报错并列出当前已接线的别名。
func NewProvider(alias string, db *gorm.DB, def *es.IndexDefinition, opts Options) (DocProvider, error) {
	f, ok := providers[alias]
	if !ok {
		return nil, fmt.Errorf("sync: 别名 %s 尚未接线数据源 Provider（已接线：%v）；请在 registry_provider.go 的 providers 表补一条，拒绝回落到其他业务实现",
			alias, registeredAliases(providers))
	}
	return f(db, def, opts), nil
}

// SyncByAlias 按别名跑一次全量灌数，是 migrate/rebuild/cutover/CLI 的统一灌数入口。
//
// alias 是对调用方的自证：解析出的索引声明必须正好属于该别名，否则拒绝在任何 ES/MySQL
// 触达之前报错（防「把商品数据灌进订单」）。opts.Index 允许是别名或物理索引名，与升级前
// SyncProducts 的解析规则完全一致；SyncProducts 本身保留为 alias 不施加约束的兼容入口。
func SyncByAlias(ctx context.Context, alias string, opts Options) (*SyncStats, error) {
	if _, ok := providers[alias]; !ok {
		return nil, fmt.Errorf("sync: 别名 %s 尚未接线数据源 Provider（已接线：%v），拒绝灌数",
			alias, registeredAliases(providers))
	}
	return syncIndexTarget(ctx, alias, opts)
}

// CompensateByAlias 按别名跑一次差量补偿（薄封装：内部仍调各别名登记的补偿实现）。
func CompensateByAlias(ctx context.Context, alias, target string, since time.Time, opts CompensateOptions) (Stats, error) {
	var st Stats
	f, ok := compensators[alias]
	if !ok {
		return st, fmt.Errorf("sync: 别名 %s 尚未接线差量补偿实现（已接线：%v），拒绝补偿（未收敛的窗口增量绝不能当成功）",
			alias, registeredAliases(compensators))
	}
	return f(ctx, target, since, opts)
}

// DBCounterFor 查表返回某别名的 DB 期望数口径（ok=false 表示该别名未登记口径）。
// es:status 用它决定 count_comparable，migrate/rebuild/cutover 经 DBCountByAlias 走同一份表。
func DBCounterFor(alias string) (DBCounter, bool) {
	f, ok := dbCounters[alias]
	return f, ok
}

// DBCountByAlias 按别名统计 DB 侧期望文档数，直接满足 internal/es 的 CountFunc 窄接口。
func DBCountByAlias(ctx context.Context, alias string) (int64, error) {
	f, ok := dbCounters[alias]
	if !ok {
		return 0, fmt.Errorf("sync: 别名 %s 未登记 DB 计数口径（已登记：%v），拒绝统计（绝不拿其他业务口径顶替）",
			alias, registeredAliases(dbCounters))
	}
	return f(ctx)
}

// registeredAliases 返回某张注册表的别名升序清单，只用于错误信息可排障。
func registeredAliases[V any](table map[string]V) []string {
	out := make([]string, 0, len(table))
	for a := range table {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}
