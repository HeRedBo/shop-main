package es

// 本文件实现 Phase 6「同名别名割接」（es:cutover）的编排层，同时承载 es:status
// 需要的只读探测原语（插件清单 / 别名指向 / 索引统计 / schema_version 推导）。
//
// 为什么「割接」是与 es:rebuild 分开的独立动作：
//   - rebuild 的起点是「别名已经存在」（v0 -> v1），可以只摘别名、保留旧索引回滚；
//   - cutover 的起点是「shop-product 仍然是个物理索引」，要把它变成同名别名。
// ES 8.2.2 真机实测：物理索引名与别名**绝对互斥** —— 给物理索引 shop-product 挂
// 同名别名会得到 400 invalid_alias_name_exception，反向 PUT /shop-product 会得到
// 400 invalid_index_name_exception "already exists as alias"，_close 也解除不了冲突。
// 因此唯一可行路径是在**单次** POST /_aliases 里同时提交
//   {remove_index:{index:"shop-product"}} + {add:{index:"shop-product_v1",alias:"shop-product",is_write_index:true}}
// 原子生效（olivere v7.0.32 无论挂多少个 action 都只发一次 POST /_aliases），
// 拆成两步会留下「既没有索引也没有别名」的写入黑洞窗口 —— 本层严禁拆分。
//
// 由此推出的、必须如实告知运维的回滚语义：割接请求本身就把旧物理索引连同其数据一起
// 摘除，割接后不存在「保留旧索引直接回滚」这种选项，回滚只能重跑 MySQL 全量灌 v1。
// 本文件的帮助文案、--dry-run 输出与日志都不得承诺相反内容。
//
// 与 Phase 4 的解耦：第 3 步「灌 v1」复用全量同步流水线，但 internal/es/sync 的 API
// 仍在重构，所以本层只依赖 SyncFunc 这个窄接口，由 cmd/cli/es_cutover.go 在装配时注入
// 具体形态；cutover.go 不 import internal/es/sync，签名再变也只冲击 CLI 装配层。
// 第 6 步「差量补偿」同理，复用 rebuild.go 的窄接口 CompensateFunc（同包定义，不引入依赖）。
//
// 已定决策（不再自行探测/推翻）：不用 _clone（olivere v7.0.32 无 CloneIndex），
// 不用 snapshot/restore（本地集群无 path.repo，且 GET /_snapshot/_all 无仓库时返回
// 404 repository_missing_exception 而不是空对象）。集群健康在单节点上恒为 yellow，
// 属正常态，本层任何判定都不把 yellow 当故障。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"shop/internal/es/admin"
	"shop/internal/models"
	"shop/pkg/global"

	"github.com/olivere/elastic/v7"
)

// ---------------------------------------------------------------------------
// 窄接口：灌数回调
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
// Cluster：割接与状态检查共用的集群原语
// ---------------------------------------------------------------------------

// AliasTarget 一行 _cat/aliases 结果。
//
// WriteIndex 保留 ES 的原始字符串形态："true" / "false" / "-"（未显式设置）。
// 单索引别名不带 is_write_index 时 _cat 返回 "-"，此时该索引隐式即为写索引，
// 因此不能把 "-" 直接判成 false（详见 writeIndexEffective）。
type AliasTarget struct {
	Alias      string `json:"alias"`
	Index      string `json:"index"`
	WriteIndex string `json:"is_write_index"`
}

// IndexStat 一行 _cat/indices 结果（doc 数、分片数、健康度、存储体积）。
type IndexStat struct {
	Index     string `json:"index"`
	Health    string `json:"health"`
	Status    string `json:"status"`
	Primaries int    `json:"pri"`
	Replicas  int    `json:"rep"`
	DocsCount int64  `json:"docs.count"`
	StoreSize string `json:"store.size"`
}

// Cluster 是割接编排需要的最小集群能力面。
//
// 读/写分组是刻意的：--dry-run 必须能证明「零写操作」，靠约定做不到，
// 只能让所有写路径都经过同一层拦截（见 auditedCluster）。
type Cluster interface {
	// ---- 只读（GET / HEAD）----
	IndexExists(ctx context.Context, idx string) (bool, error)          // HEAD /<idx>
	AliasTargets(ctx context.Context, alias string) ([]string, error)   // GET /_alias/<alias>
	ListAliasRows(ctx context.Context) ([]AliasTarget, error)           // GET /_cat/aliases
	ListPlugins(ctx context.Context) ([]string, error)                  // GET /_cat/plugins
	IndexStats(ctx context.Context, idx string) ([]IndexStat, error)    // GET /_cat/indices/<idx>
	Count(ctx context.Context, idx string) (int64, error)               // GET /<idx>/_count
	GetMapping(ctx context.Context, idx string) (map[string]any, error) // GET /<idx>/_mapping
	GetSettings(ctx context.Context, idx string) (map[string]any, error)

	// ---- 写入 ----
	CreateIndex(ctx context.Context, idx, bodyJSON string) error            // PUT /<idx>
	PutSettings(ctx context.Context, idx, bodyJSON string) error            // PUT /<idx>/_settings
	Refresh(ctx context.Context, idx string) error                          // POST /<idx>/_refresh
	SwitchAlias(ctx context.Context, alias, target string) error            // 单次 POST /_aliases
	Perform(ctx context.Context, method, path, body string) (string, error) // 审计用的裸请求（dry-run 只允许 GET/HEAD）
}

// AdminCluster 是 Cluster 的生产实现：写全部走 internal/es/admin 已定稿的封装，
// _cat 系列与裸请求走 admin.Raw() 暴露的 olivere 逃生舱，不新引任何依赖。
type AdminCluster struct{}

// NewAdminCluster 返回生产实现。零值可用，构造器只为调用点可读性存在。
func NewAdminCluster() *AdminCluster { return &AdminCluster{} }

func (AdminCluster) IndexExists(ctx context.Context, idx string) (bool, error) {
	return admin.Exists(ctx, idx)
}

func (AdminCluster) AliasTargets(ctx context.Context, alias string) ([]string, error) {
	return admin.AliasIndexes(ctx, alias)
}

// ListAliasRows 一次拉全部别名映射（_cat/aliases），供状态输出与割接校验复用。
// 刻意不用 admin.AliasIndexes：它只给索引名，拿不到 is_write_index。
func (AdminCluster) ListAliasRows(ctx context.Context) ([]AliasTarget, error) {
	cli, err := admin.Raw()
	if err != nil {
		return nil, err
	}
	rows, err := cli.CatAliases().Columns("alias", "index", "is_write_index").Do(ctx)
	if err != nil {
		return nil, fmt.Errorf("es: 读取别名清单失败: %w", err)
	}
	out := make([]AliasTarget, 0, len(rows))
	for _, r := range rows {
		out = append(out, AliasTarget{Alias: r.Alias, Index: r.Index, WriteIndex: r.IsWriteIndex})
	}
	SortAliasTargets(out)
	return out, nil
}

// ListPlugins 读 _cat/plugins 的 component 列。
//
// olivere v7.0.32 没有 CatPlugins 服务（只有基于 ClusterStats 的 Plugins()，
// 且内部硬用 context.Background 无法传 ctx），故按方案指定的 _cat/plugins
// 端点用 PerformRequest 裸请求，format=json 保证解析稳定。
func (AdminCluster) ListPlugins(ctx context.Context) ([]string, error) {
	cli, err := admin.Raw()
	if err != nil {
		return nil, err
	}
	res, err := cli.PerformRequest(ctx, elastic.PerformRequestOptions{
		Method: "GET",
		Path:   "/_cat/plugins",
		Params: url.Values{"format": []string{"json"}, "h": []string{"name,component"}},
	})
	if err != nil {
		return nil, fmt.Errorf("es: 读取插件清单失败（_cat/plugins）: %w", err)
	}
	return ParsePluginComponents(res.Body)
}

func (AdminCluster) IndexStats(ctx context.Context, idx string) ([]IndexStat, error) {
	cli, err := admin.Raw()
	if err != nil {
		return nil, err
	}
	rows, err := cli.CatIndices().Index(idx).
		Columns("index", "health", "status", "pri", "rep", "docs.count", "store.size").Do(ctx)
	if err != nil {
		// _cat/indices/<缺失索引> 在 8.x 返回 404：状态检查里这属于「尚不存在」，
		// 与 admin.Exists 的判定一致，退化成空清单而不是报错。
		if elastic.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("es: 读取索引统计失败 [index=%s]: %w", idx, err)
	}
	out := make([]IndexStat, 0, len(rows))
	for _, r := range rows {
		out = append(out, IndexStat{
			Index: r.Index, Health: r.Health, Status: r.Status,
			Primaries: r.Pri, Replicas: r.Rep,
			DocsCount: int64(r.DocsCount), StoreSize: r.StoreSize,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return out, nil
}

func (AdminCluster) Count(ctx context.Context, idx string) (int64, error) {
	return admin.Count(ctx, idx)
}

// GetMapping 直接复用 admin.GetMapping（已修正为不带 mapping type 段的 PerformRequest 路径，
// 返回内层 mappings，索引不存在返回 (nil, nil)），本层不再自己拆封回读体。
func (AdminCluster) GetMapping(ctx context.Context, idx string) (map[string]any, error) {
	return admin.GetMapping(ctx, idx)
}

func (AdminCluster) GetSettings(ctx context.Context, idx string) (map[string]any, error) {
	return admin.GetSettings(ctx, idx, false)
}

func (AdminCluster) CreateIndex(ctx context.Context, idx, bodyJSON string) error {
	return admin.Create(ctx, idx, bodyJSON)
}

func (AdminCluster) PutSettings(ctx context.Context, idx, bodyJSON string) error {
	return admin.PutSettings(ctx, idx, bodyJSON)
}

func (AdminCluster) Refresh(ctx context.Context, idx string) error {
	return admin.Refresh(ctx, idx)
}

// SwitchAlias 以单次 POST /_aliases 完成同名别名割接。
//
// 拒绝使用 admin.AtomicSwitchAlias（方案原本指定它）并给出理由：该函数在提交前
// 会断言 removeIndexes 当前确实持有 alias，而割接场景里 "shop-product" 是物理索引名、
// 不是别名，AliasIndexes("shop-product") 必然返回空，断言恒定失败并误报「别名指向异常」。
// 这是互斥命名带来的不可满足前提，不是数据异常。因此这里直接使用同一批 olivere 原语
// （NewAliasRemoveIndexAction + NewAliasAddAction）拼出**一次** Do()，请求体与方案要求逐字一致，
// 且仍然只校验 res.Acknowledged（v7.0.32 的 AliasResult 没有 Errors 字段）。
//
// is_write_index 必须与 remove_index 同请求下发：两个写索引会在 8.2.2 触发
// 500 illegal_state_exception 并整体拒绝，不存在「先切再补标记」的余地。
func (AdminCluster) SwitchAlias(ctx context.Context, alias, target string) error {
	cli, err := admin.Raw()
	if err != nil {
		return err
	}
	svc := cli.Alias().
		Action(elastic.NewAliasRemoveIndexAction(alias)).
		Action(elastic.NewAliasAddAction(alias).Index(target).IsWriteIndex(true))

	res, err := svc.Do(ctx)
	if err != nil {
		return fmt.Errorf("es: 原子割接失败 [alias=%s -> %s]: %w", alias, target, err)
	}
	if res == nil || !res.Acknowledged {
		return fmt.Errorf("es: 割接请求未被集群确认 [alias=%s -> %s]", alias, target)
	}
	return nil
}

func (AdminCluster) Perform(ctx context.Context, method, path, body string) (string, error) {
	cli, err := admin.Raw()
	if err != nil {
		return "", err
	}
	res, err := cli.PerformRequest(ctx, elastic.PerformRequestOptions{
		Method: method, Path: path, Body: body,
	})
	if err != nil {
		return "", err
	}
	return string(res.Body), nil
}

// SortAliasTargets 就地按 (alias, index) 排序，保证状态输出与断言可比。
func SortAliasTargets(rows []AliasTarget) {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Alias != rows[j].Alias {
			return rows[i].Alias < rows[j].Alias
		}
		return rows[i].Index < rows[j].Index
	})
}

// ParsePluginComponents 解析 _cat/plugins 的 JSON 回读，返回去重升序的 component 清单。
// 纯函数：单测直接喂真机抓到的样本文本。
func ParsePluginComponents(raw []byte) ([]string, error) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil, nil
	}
	var rows []struct {
		Name      string `json:"name"`
		Component string `json:"component"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("es: 解析 _cat/plugins 失败: %w", err)
	}
	seen := make(map[string]struct{}, len(rows))
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		c := strings.TrimSpace(r.Component)
		if c == "" {
			continue
		}
		if _, dup := seen[c]; dup {
			continue
		}
		seen[c] = struct{}{}
		out = append(out, c)
	}
	sort.Strings(out)
	return out, nil
}

// ---------------------------------------------------------------------------
// dry-run 写操作审计层
// ---------------------------------------------------------------------------

// auditedCluster 记录每一次集群调用，并在 dry-run 下硬拒任何写操作。
//
// 这是「--dry-run 必须证明零写操作」的实现手段：不靠代码走查，而是让写路径
// 一旦被调用就立刻返回错误，同时把读调用清单原样打印出来。
type auditedCluster struct {
	inner  Cluster
	dryRun bool

	mu     sync.Mutex
	reads  []string
	writes []string
}

func newAuditedCluster(inner Cluster, dryRun bool) *auditedCluster {
	return &auditedCluster{inner: inner, dryRun: dryRun}
}

func (a *auditedCluster) note(kind, op string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if kind == "write" {
		if a.dryRun {
			return fmt.Errorf("es: dry-run 拒绝写操作 [%s]（本命令只允许 GET/HEAD）", op)
		}
		a.writes = append(a.writes, op)
		return nil
	}
	a.reads = append(a.reads, op)
	return nil
}

// ReadOps 返回只读调用清单（GET/HEAD），按发生顺序。
func (a *auditedCluster) ReadOps() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.reads...)
}

// WriteOps 返回写调用清单；dry-run 下恒为空即为「零写操作」的证据。
func (a *auditedCluster) WriteOps() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.writes...)
}

func (a *auditedCluster) IndexExists(ctx context.Context, idx string) (bool, error) {
	if err := a.note("read", "HEAD /"+idx); err != nil {
		return false, err
	}
	return a.inner.IndexExists(ctx, idx)
}

func (a *auditedCluster) AliasTargets(ctx context.Context, alias string) ([]string, error) {
	if err := a.note("read", "GET /_alias/"+alias); err != nil {
		return nil, err
	}
	return a.inner.AliasTargets(ctx, alias)
}

func (a *auditedCluster) ListAliasRows(ctx context.Context) ([]AliasTarget, error) {
	if err := a.note("read", "GET /_cat/aliases"); err != nil {
		return nil, err
	}
	return a.inner.ListAliasRows(ctx)
}

func (a *auditedCluster) ListPlugins(ctx context.Context) ([]string, error) {
	if err := a.note("read", "GET /_cat/plugins"); err != nil {
		return nil, err
	}
	return a.inner.ListPlugins(ctx)
}

func (a *auditedCluster) IndexStats(ctx context.Context, idx string) ([]IndexStat, error) {
	if err := a.note("read", "GET /_cat/indices/"+idx); err != nil {
		return nil, err
	}
	return a.inner.IndexStats(ctx, idx)
}

func (a *auditedCluster) Count(ctx context.Context, idx string) (int64, error) {
	if err := a.note("read", "GET /"+idx+"/_count"); err != nil {
		return 0, err
	}
	return a.inner.Count(ctx, idx)
}

func (a *auditedCluster) GetMapping(ctx context.Context, idx string) (map[string]any, error) {
	if err := a.note("read", "GET /"+idx+"/_mapping"); err != nil {
		return nil, err
	}
	return a.inner.GetMapping(ctx, idx)
}

func (a *auditedCluster) GetSettings(ctx context.Context, idx string) (map[string]any, error) {
	if err := a.note("read", "GET /"+idx+"/_settings"); err != nil {
		return nil, err
	}
	return a.inner.GetSettings(ctx, idx)
}

func (a *auditedCluster) CreateIndex(ctx context.Context, idx, bodyJSON string) error {
	if err := a.note("write", "PUT /"+idx); err != nil {
		return err
	}
	return a.inner.CreateIndex(ctx, idx, bodyJSON)
}

func (a *auditedCluster) PutSettings(ctx context.Context, idx, bodyJSON string) error {
	if err := a.note("write", "PUT /"+idx+"/_settings"); err != nil {
		return err
	}
	return a.inner.PutSettings(ctx, idx, bodyJSON)
}

func (a *auditedCluster) Refresh(ctx context.Context, idx string) error {
	// _refresh 是 POST，虽然不改动数据，但不属于「只读 GET 白名单」，dry-run 一律拦下。
	if err := a.note("write", "POST /"+idx+"/_refresh"); err != nil {
		return err
	}
	return a.inner.Refresh(ctx, idx)
}

func (a *auditedCluster) SwitchAlias(ctx context.Context, alias, target string) error {
	if err := a.note("write", "POST /_aliases"); err != nil {
		return err
	}
	return a.inner.SwitchAlias(ctx, alias, target)
}

func (a *auditedCluster) Perform(ctx context.Context, method, path, body string) (string, error) {
	write := !IsReadOnlyMethod(method)
	if err := a.note(opKind(write), strings.ToUpper(method)+" "+path); err != nil {
		return "", err
	}
	return a.inner.Perform(ctx, method, path, body)
}

func opKind(write bool) string {
	if write {
		return "write"
	}
	return "read"
}

// IsReadOnlyMethod 报告 HTTP 方法是否属于 dry-run 白名单（只允许 GET/HEAD）。
func IsReadOnlyMethod(method string) bool {
	switch strings.ToUpper(strings.TrimSpace(method)) {
	case "GET", "HEAD":
		return true
	default:
		return false
	}
}

// ---------------------------------------------------------------------------
// 纯判定函数（割接编排的可测内核）
// ---------------------------------------------------------------------------

// CutoverVerdict 是前置探测后得出的割接结论。
type CutoverVerdict int

const (
	// VerdictProceed 可以割接：别名位还被物理索引占着，且目标名空闲。
	VerdictProceed CutoverVerdict = iota
	// VerdictAlreadyCutover 幂等：目标名已作为别名存在，什么都不做，退出 0。
	VerdictAlreadyCutover
	// VerdictTargetOccupied 目标 v1 索引已存在：绝不覆盖，报错中止。
	VerdictTargetOccupied
	// VerdictMissingSource 别名与物理索引都不存在：无割接对象，报错中止。
	VerdictMissingSource
)

func (v CutoverVerdict) String() string {
	switch v {
	case VerdictAlreadyCutover:
		return "already_alias"
	case VerdictTargetOccupied:
		return "target_occupied"
	case VerdictMissingSource:
		return "source_missing"
	default:
		return "proceed"
	}
}

// AliasProbe 是前置只读探测的原始事实集合，判定与 IO 分离才能单测。
type AliasProbe struct {
	// NameIsPhysicalIndex：HEAD /<name> 命中，且 GET /_alias/<name> 无条目 => 名称仍被物理索引占据。
	NameIsPhysicalIndex bool
	AliasTargets        []string // GET /_alias/<alias> 指向的物理索引；非空 => 已是别名
	TargetExists        bool     // HEAD /<target> 是否已存在
}

// DecideCutover 给前置探测结论。判定顺序不能换：
// 割接成功后 target 既存在又正是别名指向，必须先认「幂等已完成」而不是「目标被占」。
func DecideCutover(p AliasProbe) (CutoverVerdict, string) {
	switch {
	case len(p.AliasTargets) > 0:
		return VerdictAlreadyCutover, fmt.Sprintf(
			"别名位已是别名，指向 %s；割接已完成，不做任何变更", strings.Join(p.AliasTargets, ", "))
	case p.TargetExists:
		return VerdictTargetOccupied, "目标索引已存在，拒绝覆盖"
	case !p.NameIsPhysicalIndex:
		return VerdictMissingSource, "别名位上既没有别名也没有物理索引，无割接对象"
	default:
		return VerdictProceed, "别名位仍是物理索引，可以执行同名割接"
	}
}

// MissingPlugins 返回声明要求但未安装的插件（大小写不敏感，容忍 analysis- 前缀写法）。
func MissingPlugins(installed, required []string) []string {
	var out []string
	for _, want := range required {
		w := strings.TrimSpace(want)
		if w == "" {
			continue
		}
		found := false
		for _, has := range installed {
			if pluginMatches(has, w) {
				found = true
				break
			}
		}
		if !found {
			out = append(out, w)
		}
	}
	return out
}

// pluginMatches 兼容三种真实写法：声明 "analysis-ik" / 组件 "analysis-ik" / 简写 "ik"。
func pluginMatches(component, want string) bool {
	c := strings.ToLower(strings.TrimSpace(component))
	w := strings.ToLower(strings.TrimSpace(want))
	if c == "" || w == "" {
		return false
	}
	if c == w {
		return true
	}
	if c == "analysis-"+w || strings.TrimPrefix(c, "analysis-") == w {
		return true
	}
	return false
}

// cutoverAliasAction / cutoverRemoveIndex / cutoverAddAlias 用结构体而非 map
// 固定 _aliases 请求体的键顺序（map 序列化会随机化 key，运维对不上方案正文的样本会误判）。
type cutoverRemoveIndex struct {
	Index string `json:"index"`
}

type cutoverAddAlias struct {
	Index        string `json:"index"`
	Alias        string `json:"alias"`
	IsWriteIndex bool   `json:"is_write_index"`
}

type cutoverAliasAction struct {
	RemoveIndex *cutoverRemoveIndex `json:"remove_index,omitempty"`
	Add         *cutoverAddAlias    `json:"add,omitempty"`
}

type cutoverAliasRequest struct {
	Actions []cutoverAliasAction `json:"actions"`
}

// CutoverAliasBody 生成割接的 POST /_aliases 请求体（也是 dry-run 要打印的内容）。
//
// 顺序固定为先 remove_index 后 add，与真机验证过的样本逐字一致。它描述的是
// AdminCluster.SwitchAlias 实际下发的同一对 action，两处必须保持同语义 ——
// 真要拆成两个请求就会留下写入黑洞，本层不提供拆分能力。
func CutoverAliasBody(alias, target string) (string, error) {
	if strings.TrimSpace(alias) == "" || strings.TrimSpace(target) == "" {
		return "", fmt.Errorf("es: 割接请求体需要非空的别名与目标索引")
	}
	if alias == target {
		// 物理索引名与别名互斥，同名 add 必然 400 invalid_alias_name_exception。
		return "", fmt.Errorf("es: 目标索引名 %s 不能与别名同名（ES 中二者绝对互斥）", alias)
	}
	req := cutoverAliasRequest{Actions: []cutoverAliasAction{
		{RemoveIndex: &cutoverRemoveIndex{Index: alias}},
		{Add: &cutoverAddAlias{Index: target, Alias: alias, IsWriteIndex: true}},
	}}
	b, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("es: 序列化割接请求体失败: %w", err)
	}
	return string(b), nil
}

// VerifyCutoverTarget 校验割接结果：别名必须只指向 target，且 target 是写索引。
// 纯函数，入参是 _cat/aliases 的解析结果。
func VerifyCutoverTarget(rows []AliasTarget, alias, target string) (bool, string) {
	var targets []AliasTarget
	for _, r := range rows {
		if r.Alias == alias {
			targets = append(targets, r)
		}
	}
	if len(targets) == 0 {
		return false, fmt.Sprintf("别名 %s 不存在，割接未生效", alias)
	}
	if len(targets) > 1 {
		names := make([]string, 0, len(targets))
		for _, t := range targets {
			names = append(names, t.Index)
		}
		return false, fmt.Sprintf("别名 %s 指向 %d 个索引 %v，割接后应只剩 %s", alias, len(targets), names, target)
	}
	got := targets[0]
	if got.Index != target {
		return false, fmt.Sprintf("别名 %s 指向 %s，与期望目标 %s 不符", alias, got.Index, target)
	}
	effective, explicit := writeIndexEffective(got.WriteIndex, []string{got.Index})
	if !effective {
		return false, fmt.Sprintf("别名 %s -> %s 已生效，但 is_write_index=%q 非写索引", alias, target, got.WriteIndex)
	}
	if !explicit {
		return true, fmt.Sprintf("别名 %s -> %s 正确（单索引隐式写索引，_cat 回读 %q）", alias, target, got.WriteIndex)
	}
	return true, fmt.Sprintf("别名 %s -> %s 正确，is_write_index=true 已生效", alias, target)
}

// writeIndexEffective 解析 _cat/aliases 的 is_write_index 三态。
//
// ES 8.2.2 实测：显式设置返回 "true"/"false"，从未设置返回 "-"。别名只挂一个索引时
// "-" 表示隐式写索引（读写都落到它），因此 "-" + 单目标算「生效但非显式」，
// 不能判成失败 —— 否则 probe 验证里不带标记的别名会被误报为割接未生效。
func writeIndexEffective(flag string, targets []string) (effective, explicit bool) {
	switch strings.ToLower(strings.TrimSpace(flag)) {
	case "true", "1":
		return true, true
	case "false", "0":
		return false, true
	default: // "-"、""：未显式设置
		return len(targets) == 1, false
	}
}

// ResolveWriteIndex 从 _cat/aliases 的行里挑出别名当前真正承接写入的物理索引。
//
// es:status 需要它来定位「该数哪个索引的 doc」：割接前别名位上是物理索引（根本不在
// _cat/aliases 里，ok=false 由调用方按「未割接」处理）；割接后单索引可能只带隐式写标记
// （"-"）；多索引别名必须看显式 is_write_index=true，猜错就会拿一个并不承接写入的索引去做对账。
func ResolveWriteIndex(rows []AliasTarget, alias string) (index, note string, ok bool) {
	var targets []AliasTarget
	for _, r := range rows {
		if r.Alias == alias {
			targets = append(targets, r)
		}
	}
	if len(targets) == 0 {
		return "", fmt.Sprintf("别名 %s 不存在", alias), false
	}
	names := make([]string, 0, len(targets))
	for _, t := range targets {
		names = append(names, t.Index)
	}
	if len(targets) == 1 {
		effective, explicit := writeIndexEffective(targets[0].WriteIndex, names)
		if !effective {
			return "", fmt.Sprintf("别名 %s -> %s 的 is_write_index=%q，不可写", alias, targets[0].Index, targets[0].WriteIndex), false
		}
		if explicit {
			return targets[0].Index, "显式写索引", true
		}
		return targets[0].Index, "单索引隐式写索引", true
	}
	for _, t := range targets {
		if strings.EqualFold(strings.TrimSpace(t.WriteIndex), "true") {
			return t.Index, fmt.Sprintf("%d 个索引中的写索引", len(targets)), true
		}
	}
	return "", fmt.Sprintf("别名 %s 指向 %v，但没有任何索引带 is_write_index=true，无法判定写索引", alias, names), false
}

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

// targetVersionRe 与 admin/alias.go 的 versionSuffixRe 同一语义（那个是包内私有，
// 不能跨包复用，这里保持字面一致：兼容 shop-product_v1 与 shop-product_1 两种写法）。
var targetVersionRe = regexp.MustCompile(`_(?:v)?(\d+)$`)

// SchemaVersionFromMappings 读线上 mappings._meta.schema_version。
//
// 8.2.2 实测 _meta 可以完整回读，是首选来源；本项目声明文件的顶层 _meta 会被
// BodyJSON 剔除，所以线上是否带 _meta 取决于灌结构时是否显式写入 —— 读不到就回退。
func SchemaVersionFromMappings(mappings map[string]any) (int, bool) {
	if len(mappings) == 0 {
		return 0, false
	}
	raw, ok := mappings[MetaKey].(map[string]any)
	if !ok {
		return 0, false
	}
	v, err := metaInt(raw, MetaKeySchemaVersion)
	if err != nil || v < 1 {
		return 0, false
	}
	return v, true
}

// SchemaVersionFromIndexName 从物理索引名尾缀推导版本号。
func SchemaVersionFromIndexName(name string) (int, bool) {
	m := targetVersionRe.FindStringSubmatch(strings.TrimSpace(name))
	if m == nil {
		return 0, false
	}
	v, err := strconv.Atoi(m[1])
	if err != nil || v < 1 {
		return 0, false
	}
	return v, true
}

// ResolveSchemaVersion 按「_meta 优先、名称尾缀回退」给出版本号与来源标记。
// 两者都拿不到时返回 (0, "unknown")，绝不猜。
func ResolveSchemaVersion(liveMappings map[string]any, indexName string) (int, string) {
	if v, ok := SchemaVersionFromMappings(liveMappings); ok {
		return v, "mappings._meta"
	}
	if v, ok := SchemaVersionFromIndexName(indexName); ok {
		return v, "index_name_suffix"
	}
	return 0, "unknown"
}

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

// FormatOrphanAdvisory 生成「已建但未切走」的孤儿索引处置建议。
//
// 刻意不自动删：误删一次 15 分片索引的代价远高于留一个待人工确认的孤儿。
// 文案必须如实 —— 割接完成后旧索引连数据一起消失，回滚只能重跑全量，
// 这里不承诺任何「保留旧索引」的退路。
func FormatOrphanAdvisory(alias string, orphans []string) string {
	if len(orphans) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("孤儿索引清单（已创建但未切走别名 %s，本命令不会自动删除）：\n", alias))
	for _, o := range orphans {
		sb.WriteString("  - " + o + "\n")
	}
	sb.WriteString("处置建议（二选一，都需人工执行）：\n")
	for _, o := range orphans {
		sb.WriteString(fmt.Sprintf("  1) 确认数据无用后手工删除：DELETE /%s\n", o))
		sb.WriteString(fmt.Sprintf("  2) 修复原因后重跑本命令：重跑会因 HEAD /%s 命中而以「目标索引已存在，拒绝覆盖」中止，\n", o))
		sb.WriteString("     必须先删除该索引（op_type=index 幂等，但结构必须由本命令按声明新建）。\n")
	}
	sb.WriteString("回滚说明：同名割接由单请求 remove_index 完成，旧物理索引连同数据一起摘除，\n")
	sb.WriteString("        不存在「保留旧索引直接切回」的选项；回滚只能重跑 MySQL 全量灌入目标索引。\n")
	return sb.String()
}

// ---------------------------------------------------------------------------
// 编排
// ---------------------------------------------------------------------------

// defaultFinalizeTimeout 收尾（恢复 settings / 校验别名）独立上下文的默认超时。
const defaultFinalizeTimeout = 60 * time.Second

// CutoverOptions 割接编排入参。
//
// Cluster 与 Sync 都由调用方注入：Cluster 让 dry-run 能拦写，Sync 让 #5 的
// API 变动只冲击 CLI 装配层。DBCount 注入是为了对账口径可单测；nil 时回落 ProductDocCount。
// Compensate 与 rebuild 第 7 步同构：割接把旧物理索引连数据一起摘走，灌数窗口内的
// 改/删只能靠这一次差量补偿收敛，不注入就是丢数据（见 RunCutover 第 6 步）。
type CutoverOptions struct {
	Alias      string // 目标别名，必须与 Definition.Alias() 一致
	Definition *IndexDefinition
	Cluster    Cluster
	Sync       SyncFunc
	DBCount    CountFunc
	Compensate CompensateFunc // 切换之后收敛窗口增量；nil 时在切换后报错中止

	Tolerance       int64 // 对账容差：|ES - DB| > 该值即在切别名之前中止；负值=永不可达标
	Force           bool  // 对账不达标仍继续割接（与 es:rebuild --force 同语义，谨慎）
	DryRun          bool  // 只读预演：任何写调用都会被审计层拒绝
	TuneIngest      bool  // 灌数期临时关 refresh/副本（仅对无别名的新建物理索引安全）
	FinalizeTimeout time.Duration

	Logf func(format string, v ...any)
}

// CutoverResult 割接一次执行的完整结论，供 CLI 打印与 --json 序列化。
type CutoverResult struct {
	Alias                 string `json:"alias"`
	Target                string `json:"target"`
	DeclaredSchemaVersion int    `json:"declared_schema_version"`
	Verdict               string `json:"verdict"`
	DryRun                bool   `json:"dry_run"`
	Idempotent            bool   `json:"idempotent"`

	PluginsInstalled []string `json:"plugins_installed"`
	PluginsRequired  []string `json:"plugins_required"`
	PluginsMissing   []string `json:"plugins_missing"`

	AliasTargetsBefore []string `json:"alias_targets_before"`

	DBExpected         int64  `json:"db_expected"`
	Tolerance          int64  `json:"tolerance"` // 本次生效的容差（负值=永不可达标，不当 0 用）
	Force              bool   `json:"force"`     // --force：对账不达标仍继续割接
	PlannedCreateBody  string `json:"planned_create_body,omitempty"`
	PlannedAliasesBody string `json:"planned_aliases_body,omitempty"`

	Sync        SyncSummary `json:"sync"`
	ESCount     int64       `json:"es_count"`
	Diff        int64       `json:"count_diff"`
	Reconciled  bool        `json:"reconciled"`
	Created     bool        `json:"created"`
	Tuned       bool        `json:"tuned"`
	Restored    bool        `json:"settings_restored"`
	Switched    bool        `json:"switched"`
	Verified    bool        `json:"verified"`
	VerifyNote  string      `json:"verify_note,omitempty"`
	Orphans     []string    `json:"orphans,omitempty"`
	AbortedStep string      `json:"aborted_step,omitempty"`

	// Compensate 是窗口增量补偿的统计；Compensated 只在补偿无 error 且 Failed==0 时置 true。
	// 旧物理索引已被 remove_index 摘除，窗口增量没收敛就是丢了，不能报成成功（与 rebuild 同口径）。
	Compensate  CompensateSummary `json:"compensate"`
	Compensated bool              `json:"compensated"`
	StartedAt   time.Time         `json:"started_at"` // 编排起点，也是差量补偿的下界

	ReadOps  []string `json:"read_ops"`
	WriteOps []string `json:"write_ops"`
}

// RunCutover 按已定稿的 7 步执行同名别名割接：前置校验 -> 建 target -> 灌数 ->
// 对账（闸门，可 --force）-> 单次 _aliases 原子割接 -> **差量补偿** -> 校验别名。
//
// 差量补偿为什么不可省略：第 5 步的 remove_index 把旧物理索引连同数据一起摘除，
// 灌数窗口（startedAt 至切换完成）内 MySQL 发生的增/改/删既没进 target，也再也回不来
// —— 只能靠这一次以 startedAt 为下界、以新 write index 为目标的扫描收敛。
//
// 收尾动作（恢复 settings、校验别名）一律用 context.WithoutCancel(ctx) 派生的独立
// context：--timeout 到期时主 ctx 已取消，若复用同一 ctx，恢复请求会直接 context canceled
// 失败，把「refresh 还关着、副本还是 0」的索引遗留在集群里。
func RunCutover(ctx context.Context, opts CutoverOptions) (*CutoverResult, error) {
	if opts.Definition == nil {
		return nil, fmt.Errorf("es: 割接需要索引声明（Definition 为 nil）")
	}
	if opts.Cluster == nil {
		return nil, fmt.Errorf("es: 割接需要集群原语（Cluster 为 nil）")
	}
	if strings.TrimSpace(opts.Alias) == "" {
		return nil, fmt.Errorf("es: 割接需要目标别名")
	}
	if opts.Alias != opts.Definition.Alias() {
		return nil, fmt.Errorf("es: 别名 %s 与声明 %s 的 alias=%s 不一致",
			opts.Alias, opts.Definition.Source(), opts.Definition.Alias())
	}
	if !opts.Definition.Executable() {
		return nil, fmt.Errorf("es: 声明 %s 的 status=%s 未接线，拒绝割接", opts.Definition.Source(), opts.Definition.Status())
	}
	logf := opts.Logf
	if logf == nil {
		logf = func(format string, v ...any) { fmt.Printf(format+"\n", v...) }
	}
	if opts.DBCount == nil {
		opts.DBCount = productCountFunc
	}

	audit := newAuditedCluster(opts.Cluster, opts.DryRun)
	c := Cluster(audit)

	alias := opts.Alias
	target := opts.Definition.Name() // <alias>_v<schema_version>
	// startedAt 在编排最开始就固化，与 rebuild 同源同一语义：本次任务首次启动时刻，
	// 也是差量补偿的唯一下界（绝不取切换时刻 —— 那会把灌数窗口内的变更静默漏掉）。
	startedAt := time.Now()
	res := &CutoverResult{
		Alias:                 alias,
		Target:                target,
		DeclaredSchemaVersion: opts.Definition.Version(),
		DryRun:                opts.DryRun,
		PlannedAliasesBody:    mustBody(CutoverAliasBody(alias, target)),
		PluginsRequired:       opts.Definition.RequiresPlugins(),
		Tolerance:             opts.Tolerance,
		Force:                 opts.Force,
		StartedAt:             startedAt,
	}
	defer func() {
		res.ReadOps = audit.ReadOps()
		res.WriteOps = audit.WriteOps()
	}()

	// ---- 1. 前置校验（全部只读）----
	logf("[1/7] 前置校验：插件、别名状态、目标名")
	plugins, err := c.ListPlugins(ctx)
	if err != nil {
		res.AbortedStep = "precheck_plugins"
		return res, err
	}
	res.PluginsInstalled = plugins
	missing := MissingPlugins(plugins, opts.Definition.RequiresPlugins())
	res.PluginsMissing = missing
	if len(missing) > 0 {
		res.AbortedStep = "precheck_plugins"
		return res, fmt.Errorf("es: 依赖插件缺失 %v（已安装：%v），割接会建出一个分词器不可用的索引，已中止",
			missing, plugins)
	}

	// HEAD 命中说明名字在用（物理索引与别名都可能命中），GET /_alias 才把它定性：
	// 只有「名字是别名」才有条目 —— 互斥命名让这两个只读请求足以判清三种状态。
	nameHit, err := c.IndexExists(ctx, alias)
	if err != nil {
		res.AbortedStep = "precheck_alias"
		return res, err
	}
	targets, err := c.AliasTargets(ctx, alias)
	if err != nil {
		res.AbortedStep = "precheck_alias"
		return res, err
	}
	targetExists, err := c.IndexExists(ctx, target)
	if err != nil {
		res.AbortedStep = "precheck_target"
		return res, err
	}
	probe := AliasProbe{
		NameIsPhysicalIndex: nameHit && len(targets) == 0,
		AliasTargets:        targets,
		TargetExists:        targetExists,
	}
	verdict, detail := DecideCutover(probe)
	res.Verdict = verdict.String()
	res.AliasTargetsBefore = probe.AliasTargets
	logf("      判定=%s（%s）", verdict, detail)

	switch verdict {
	case VerdictAlreadyCutover:
		res.Idempotent = true
		logf("      别名已就位，指向 %v；割接无需重复执行，退出码 0", probe.AliasTargets)
		if probe.AliasTargets[0] != target {
			logf("      注意：当前指向的不是本次声明的 %s，本命令不做任何变更", target)
		}
		return res, nil
	case VerdictTargetOccupied:
		res.AbortedStep = "precheck_target"
		res.Orphans = []string{target}
		return res, fmt.Errorf("es: 目标索引 %s 已存在，拒绝覆盖（确认无用后手工删除再重跑）", target)
	case VerdictMissingSource:
		res.AbortedStep = "precheck_source"
		return res, fmt.Errorf("es: 别名位 %s 上既无别名也无物理索引，没有可割接的对象", alias)
	}

	dbExpected, err := opts.DBCount(ctx, alias)
	if err != nil {
		res.AbortedStep = "precheck_db_count"
		return res, err
	}
	res.DBExpected = dbExpected
	createBody, err := opts.Definition.BodyJSON()
	if err != nil {
		res.AbortedStep = "precheck_definition"
		return res, err
	}
	res.PlannedCreateBody = createBody

	// ---- dry-run：到此为止，只打印计划 ----
	if opts.DryRun {
		logf("[dry-run] 计划执行的写操作（本次一条都不会发出）：")
		logf("      1) PUT /%s            按声明建索引（settings+mappings，不带 aliases）", target)
		logf("      2) PUT /%s/_settings  灌数提速 %s", target, IngestTuneBody())
		logf("      3) 灌数               目标 %s，op_type=index 幂等，期望 %d 条", target, dbExpected)
		logf("      4) POST /%s/_refresh  对账前刷新", target)
		logf("      5) POST /_aliases     %s", res.PlannedAliasesBody)
		logf("      6) PUT /%s/_settings  恢复 %s", target, IngestRestoreBody(opts.Definition.SettingsMap()))
		logf("      7) 差量补偿           目标 %s，下界 %s（编排起点固化，非切换时刻）——窗口内变更只能靠它收敛",
			target, startedAt.Format(time.RFC3339))
		if opts.Force {
			logf("      注：本次 --force 已开启，对账不达标也不会中止（仍会执行第 7 步补偿）")
		}
		logf("[dry-run] 只读调用清单（%d 次，全部 GET/HEAD）：%v", len(audit.ReadOps()), audit.ReadOps())
		logf("[dry-run] 写调用清单：%v（空即零写操作已证明）", audit.WriteOps())
		logf("[dry-run] 回滚语义：割接由单请求 remove_index 摘除旧物理索引，数据同步消失，" +
			"回滚只能重跑 MySQL 全量灌入目标索引；不存在「保留旧索引可回滚」。")
		return res, nil
	}

	// ---- 2. 按声明 mapping 建 v1 ----
	logf("[2/7] 创建目标索引 %s（声明：%s）", target, opts.Definition.Source())
	if err := c.CreateIndex(ctx, target, createBody); err != nil {
		res.AbortedStep = "create"
		return res, err
	}
	res.Created = true

	// ---- 3. 灌数（窄接口注入）----
	if opts.TuneIngest {
		if err := c.PutSettings(ctx, target, IngestTuneBody()); err != nil {
			res.AbortedStep = "tune_ingest"
			res.Orphans = []string{target}
			return res, err
		}
		res.Tuned = true
		logf("      灌数提速已开启：refresh=-1、replicas=0（单节点健康会呈 yellow，属预期）")
	}
	// 收尾用的独立 context：主 ctx 取消/超时后仍要把 settings 恢复回去。
	finalize := func() (context.Context, context.CancelFunc) {
		d := opts.FinalizeTimeout
		if d <= 0 {
			d = defaultFinalizeTimeout
		}
		return context.WithTimeout(context.WithoutCancel(ctx), d)
	}
	// restoreSettings 幂等且总用独立 ctx，任何失败路径都必须先跑它。
	restoreSettings := func() {
		if !res.Tuned || res.Restored {
			return
		}
		fctx, cancel := finalize()
		defer cancel()
		if err := c.PutSettings(fctx, target, IngestRestoreBody(opts.Definition.SettingsMap())); err != nil {
			logf("      ⚠️  恢复 settings 失败（收尾 ctx 也已耗尽）：%v —— 请手工对 %s 恢复 refresh_interval/replicas",
				err, target)
			return
		}
		res.Restored = true
		logf("      settings 已恢复：%s", IngestRestoreBody(opts.Definition.SettingsMap()))
	}

	if opts.Sync == nil {
		res.AbortedStep = "sync"
		restoreSettings()
		res.Orphans = []string{target}
		return res, fmt.Errorf("es: 未注入灌数回调（SyncFunc 为 nil）")
	}
	logf("[3/7] 灌入 %s（从 MySQL 全量，op_type=index 幂等）", target)
	summary, err := opts.Sync(ctx, alias, target)
	res.Sync = summary
	if err != nil {
		res.AbortedStep = "sync"
		restoreSettings()
		res.Orphans = []string{target}
		return res, fmt.Errorf("es: 灌数失败 [target=%s]: %w", target, err)
	}
	restoreSettings()
	if summary.Failed > 0 {
		res.AbortedStep = "sync_failures"
		res.Orphans = []string{target}
		return res, fmt.Errorf("es: 灌数存在 %d 条 item 级失败，切别名前中止", summary.Failed)
	}

	// ---- 4. 对账（差异超阈值必须在切别名之前中止）----
	// 口径差异如实记账（本次不改变判定）：dbExpected 取自灌数**之前**，esCount 是灌数**之后**的
	// _count，两者本就不是同一时点；窗口内新增/下架会造成假失败。因此给 --force 退路，
	// 语义与 es:rebuild 完全对齐：不达标仍继续割接，但结论里 switched/reconciled 照实记录。
	logf("[4/7] 对账：DB 期望 %d 条 vs %s/_count（容差 %d，判定在本层）", dbExpected, target, opts.Tolerance)
	if err := c.Refresh(ctx, target); err != nil {
		res.AbortedStep = "reconcile_refresh"
		res.Orphans = []string{target}
		return res, err
	}
	esCount, err := c.Count(ctx, target)
	if err != nil {
		res.AbortedStep = "reconcile_count"
		res.Orphans = []string{target}
		return res, err
	}
	res.ESCount = esCount
	diff, ok := ReconcileCounts(dbExpected, esCount, opts.Tolerance)
	res.Diff = diff
	res.Reconciled = ok
	logf("      ES=%d，差值=%+d，容差=%d，达标=%v", esCount, diff, opts.Tolerance, ok)
	if !ok {
		if !opts.Force {
			res.AbortedStep = "reconcile"
			res.Orphans = []string{target}
			return res, fmt.Errorf("es: 对账不达标（差 %+d 超容差 %d），已在切别名之前中止，别名 %s 未被改动",
				diff, opts.Tolerance, alias)
		}
		logf("      ⚠️  对账不达标但 --force 已开启，继续割接（谨慎）")
	}

	// ---- 5. 单次 _aliases 原子割接 ----
	logf("[5/7] 原子割接：POST /_aliases %s", res.PlannedAliasesBody)
	if err := c.SwitchAlias(ctx, alias, target); err != nil {
		res.AbortedStep = "switch"
		res.Orphans = []string{target}
		return res, err
	}
	res.Switched = true

	// ---- 6. 差量补偿（remove_index 之后、校验之前）----
	// 与 --force / 对账闸门的先后关系（必须如实）：对账闸门与 --force 都只作用于第 4 步
	// 「切不切」，一旦切完就已无可回滚对象，第 6 步无论对账是否达标、是否 --force 都必跑，
	// 也绝不能被 --force 跳过；它失败不回滚割接（旧索引已连数据消失），但必须如实报错。
	if opts.DryRun {
		// 写原语拦截不依赖「dry-run 在第 5 步之前就 return」这条约定：万一将来有人
		// 把补偿提到预演分支之前，这里直接硬拒，而不是静默写入。
		res.AbortedStep = "compensate"
		return res, fmt.Errorf("es: dry-run 拒绝差量补偿（写操作）")
	}
	if opts.Compensate == nil {
		res.AbortedStep = "compensate"
		return res, fmt.Errorf("es: 已割接但未注入差量补偿回调（CompensateFunc 为 nil），"+
			"旧物理索引已被 remove_index，灌数窗口内的增量无法回补，请立即手工对 %s 跑一次 update_time >= %s 的补偿",
			target, startedAt.Format(time.RFC3339))
	}
	logf("[6/7] 差量补偿：扫描 update_time >= %s（Unscoped，含删分支），目标 %s", startedAt.Format(time.RFC3339), target)
	cstats, cerr := opts.Compensate(ctx, alias, target, startedAt)
	res.Compensate = cstats
	if cerr != nil {
		res.AbortedStep = "compensate"
		return res, fmt.Errorf("es: 差量补偿失败 [target=%s]（割接已生效，补偿幂等可重跑）: %w", target, cerr)
	}
	if cstats.Failed > 0 {
		// item 级失败不能被吞成 success：这些文档在旧索引里已经连索引一起消失。
		res.AbortedStep = "compensate_failures"
		return res, fmt.Errorf("es: 差量补偿存在 %d 条 item 级失败 [target=%s，扫描 %d/写入 %d/删除 %d]"+
			"（割接已生效且旧索引已消失，不回滚；修复结构问题后可用同一 since 幂等重跑）",
			cstats.Failed, target, cstats.Scanned, cstats.Indexed, cstats.Deleted)
	}
	res.Compensated = true

	// ---- 7. 校验别名指向与写标记（收尾用独立 ctx：切完正好 --timeout 到期时，
	//        校验仍要跑完，否则会把已经成功的割接误报成失败）----
	logf("[7/7] 校验别名指向")
	fctx, cancel := finalize()
	defer cancel()
	rows, err := c.ListAliasRows(fctx)
	if err != nil {
		res.AbortedStep = "verify"
		return res, fmt.Errorf("es: 割接已提交但校验别名失败：%w", err)
	}
	verified, note := VerifyCutoverTarget(rows, alias, target)
	res.Verified = verified
	res.VerifyNote = note
	if !verified {
		res.AbortedStep = "verify"
		return res, fmt.Errorf("es: 割接校验未通过：%s", note)
	}
	logf("      %s", note)
	return res, nil
}

// mustBody 生成 dry-run 用的计划请求体；失败退化成占位说明而不是把编排打断
// （割接主流程会另行重新调用 CutoverAliasBody 并如实报错）。
func mustBody(body string, err error) string {
	if err != nil {
		return "<invalid: " + err.Error() + ">"
	}
	return body
}
