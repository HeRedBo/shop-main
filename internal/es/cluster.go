package es

// cluster.go 承载各编排共用的「集群能力面」：Cluster 接口、生产实现 AdminCluster，
// 以及集群读结果的解释（别名行 / 插件清单 / 写索引判定 / schema_version 推导）。
// cutover / rebuild / migrate / es:status 共用这一层。
//
// 历史说明：这些类型原在 cutover.go（割接最先实现，其余编排后续复用，见 rebuild.go
// 头注释「照 cutover.go 已验证的做法」）。现按职责拆出：本文件只管「集群读写能力 +
// 读结果解释」，dry-run 审计层在 audit.go，灌数契约与对账在 ingest.go，
// cutover.go 只留割接专属判定与编排。同包内纯搬家，零行为变化。
//
// 分层边界：Cluster 接口由使用方（编排层）定义，目的是可注入 fake 测试、可套审计壳；
// 生产实现全部走 internal/es/admin 已定稿的薄原语，_cat 系列与裸请求走 admin.Raw()
// 暴露的 olivere 逃生舱。接口与审计壳都不下沉 admin 包（那里是无状态原语层）。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"shop/internal/es/admin"

	"github.com/olivere/elastic/v7"
)

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

// ---------------------------------------------------------------------------
// 集群读结果的解释（纯函数，可单测）
// ---------------------------------------------------------------------------

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
