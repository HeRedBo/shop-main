package admin

// 本文件是 Elasticsearch 索引管理能力的薄封装层，全部下钻到底层
// *elastic.Client（olivere/elastic v7），因为共享库 github.com/HeRedBo/pkg/es
// 根包缺少 DeleteIndex / PutMapping / GetMapping / Settings 等能力，
// 且其 CreateIndex 依赖无 TTL 的 sync.Map 存在性缓存，会在删索引后静默 no-op。
//
// 统一约定：
//   - 每个函数首参一律 context.Context；
//   - 入口处通过 Raw()/Client() 做 nil 防御，客户端未初始化返回带指引的 error；
//   - 失败返回包装后的 error，携带操作名与索引名上下文。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"

	"github.com/olivere/elastic/v7"
)

// Exists 判断索引是否存在。
//
// 关键：强制直连底层 elastic.Client，绕过 pkg/es 的 sync.Map 存在性缓存
// （该缓存无 TTL，删除索引后仍会命中，导致上层 es.CreateIndex 静默 no-op）。
// olivere 的 IndexExists 走 HEAD 请求，索引不存在返回 (false, nil)，不会报错。
func Exists(ctx context.Context, idx string) (bool, error) {
	cli, err := Raw()
	if err != nil {
		return false, err
	}
	exists, err := cli.IndexExists(idx).Do(ctx)
	if err != nil {
		return false, fmt.Errorf("检查索引存在性失败 [index=%s]: %w", idx, err)
	}
	return exists, nil
}

// Create 创建索引。bodyJSON 原样作为请求体提交，不加工、不注入 aliases
// （是否挂别名由上层按场景决定，Phase 5 会在 body 中拼装 aliases）。
//
// 刻意不走 pkg/es 的 CreateIndex：它在存在性缓存命中时直接返回 nil（静默 no-op）。
// 若索引已存在，ES 返回 400 resource_already_exists_exception，如实上抛。
func Create(ctx context.Context, idx string, bodyJSON string) error {
	cli, err := Raw()
	if err != nil {
		return err
	}
	res, err := cli.CreateIndex(idx).BodyString(bodyJSON).Do(ctx)
	if err != nil {
		return fmt.Errorf("创建索引失败 [index=%s]: %w", idx, err)
	}
	if res != nil && !res.Acknowledged {
		return fmt.Errorf("创建索引未被集群确认 [index=%s]", idx)
	}
	// 说明：这里不写入 pkg/es 的 exists 缓存——本层读写一律绕过该缓存，
	// 主动污染反而会让其他 pkg/es 调用方的 CreateIndex 落入 no-op 陷阱。
	return nil
}

// Delete 删除索引。成功后调用共享库 (*es.Client).DeleteIndexCache 失效存在性缓存，
// 防止后续 CreateIndex 因缓存静默 no-op。
//
// 注意：DeleteIndexCache 是 *es.Client 的方法（签名 `DeleteIndexCache(indexName ...string)`），
// 并非包级函数。删除幂等：索引不存在（404）视为成功。
func Delete(ctx context.Context, idx string) error {
	cli, err := Raw()
	if err != nil {
		return err
	}
	if _, err := cli.DeleteIndex(idx).Do(ctx); err != nil {
		if elastic.IsNotFound(err) {
			// 幂等：目标本就不存在，仍需确保缓存干净
			invalidateCache(idx)
			return nil
		}
		return fmt.Errorf("删除索引失败 [index=%s]: %w", idx, err)
	}
	invalidateCache(idx)
	return nil
}

// GetMapping 返回指定索引的 mapping（map[string]any）。
//
// ES 原始返回形如 { "<idx>": { "mappings": { ... } } }，本函数拆封索引名后返回
// 内层 mappings 内容，便于上层直接与声明文件的 mappings 做 diff。
// 索引不存在返回 (nil, nil)——上层据此判定「线上无此结构」。
//
// 不能走 olivere 的 GetMappingService：它在调用方未显式传 type 时把 type 兜底成
// "_all"（olivere/elastic v7.0.32 indices_get_mapping.go:145-152），URL 模板
// "/{index}/_mapping/{type}" 于是发出 GET /<idx>/_mapping/_all，而 ES 8.x 已移除
// mapping type，该路径无 handler。本地 8.2.2 实测：
//
//	GET /shop-product/_mapping      -> 200
//	GET /shop-product/_mapping/_all -> 400 {"error":"no handler found for uri ..."}
//	GET /shop-product/_mapping/_doc -> 400 同上
//
// 即旧实现对任何索引都必然 400。这里改成像 GetSettings 那样用 PerformRequest 显式
// 拼不带 type 段的路径，对外契约（拆封层级、(nil,nil) 语义、错误上下文）逐字不变。
func GetMapping(ctx context.Context, idx string) (map[string]any, error) {
	cli, err := Raw()
	if err != nil {
		return nil, err
	}
	return fetchMapping(ctx, cli, idx)
}

// fetchMapping 是 GetMapping 的可测内核：只依赖传入的 *elastic.Client，
// 便于用 httptest 服务器直接断言实际请求路径（见 indices_url_test.go）。
func fetchMapping(ctx context.Context, cli *elastic.Client, idx string) (map[string]any, error) {
	res, err := cli.PerformRequest(ctx, elastic.PerformRequestOptions{
		Method: "GET",
		Path:   mappingPath(idx),
	})
	if err != nil {
		if elastic.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("读取 mapping 失败 [index=%s]: %w", idx, err)
	}
	raw, err := parseMappingBody(res.Body)
	if err != nil {
		return nil, fmt.Errorf("读取 mapping 失败 [index=%s]: %w", idx, err)
	}
	return unwrapMappingBody(idx, raw)
}

// mappingPath 构造读 mapping 的请求路径：只到 /_mapping 为止，绝不带 mapping type 段。
// 单独成函数是为了让这条约束被测试钉死，避免将来又换回 olivere 的 GetMappingService。
func mappingPath(idx string) string {
	return "/" + url.PathEscape(idx) + "/_mapping"
}

// parseMappingBody 解析 GET /<idx>/_mapping 的回读体。
func parseMappingBody(body []byte) (map[string]any, error) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// unwrapMappingBody 把 { "<idx>": { "mappings": {...} } } 拆到内层 mappings。
// 索引名取不到时兜底取唯一条目（走别名时回读 key 是别名解析后的物理索引名），
// 两边都拿不到则报「非预期返回结构」。
//
// 这里顺带修一个原实现的作用域 bug：旧写法 `if m, ok := v.(map[string]any); ok` 里
// 的 ok 是 if 语句新建的变量，循环体内的 `entry, ok = m, true` 写的是这个内层 ok，
// 循环外判的仍是外层 ok —— 兜底分支从来没有真正生效过，只要 raw[idx] 缺失就一律报错。
// 割接后按别名读 mapping（回读 key = 物理索引名）正是这条路径，不修就会让 es:status
// 在割接完成后拿不到线上结构。
func unwrapMappingBody(idx string, raw map[string]any) (map[string]any, error) {
	entry, ok := raw[idx].(map[string]any)
	if !ok {
		if len(raw) == 1 {
			for _, v := range raw {
				if m, isMap := v.(map[string]any); isMap {
					entry, ok = m, true
				}
			}
		}
		if !ok {
			return nil, fmt.Errorf("mapping 拆封失败 [index=%s]: 非预期返回结构", idx)
		}
	}
	if mp, ok := entry["mappings"].(map[string]any); ok {
		return mp, nil
	}
	// 无 mappings 包裹层时原样返回该索引条目
	return entry, nil
}

// GetSettings 读取索引 settings，返回扁平化（flat_settings=true）的 map，
// 便于上层 diff 归一化为带 index. 前缀的 key 集合。
//
// withDefaults=true 时追加 include_defaults=true 返回含默认值的完整 settings；
// 该参数 olivere 服务未暴露，故走 PerformRequest 裸请求统一两种形态。
// 索引不存在返回 (nil, nil)。
func GetSettings(ctx context.Context, idx string, withDefaults bool) (map[string]any, error) {
	cli, err := Raw()
	if err != nil {
		return nil, err
	}
	params := url.Values{}
	params.Set("flat_settings", "true")
	if withDefaults {
		params.Set("include_defaults", "true")
	}
	res, err := cli.PerformRequest(ctx, elastic.PerformRequestOptions{
		Method: "GET",
		Path:   "/" + url.PathEscape(idx) + "/_settings",
		Params: params,
	})
	if err != nil {
		if elastic.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("读取 settings 失败 [index=%s]: %w", idx, err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(res.Body, &parsed); err != nil {
		return nil, fmt.Errorf("解析 settings 失败 [index=%s]: %w", idx, err)
	}
	entry := pickEntry(parsed, idx)
	if entry == nil {
		return nil, fmt.Errorf("settings 拆封失败 [index=%s]: 非预期返回结构", idx)
	}
	settings, _ := entry["settings"].(map[string]any)
	return settings, nil
}

// PutMapping 更新索引 mapping。mappingJSON 应仅包含需要提交的字段子集
// （strict 模式下 Phase 5 只发新增字段），不重发全量。
func PutMapping(ctx context.Context, idx, mappingJSON string) error {
	cli, err := Raw()
	if err != nil {
		return err
	}
	// olivere v7：PutMapping() 无参，setter .Index(indices ...string) + .BodyString
	if _, err := cli.PutMapping().Index(idx).BodyString(mappingJSON).Do(ctx); err != nil {
		return fmt.Errorf("更新 mapping 失败 [index=%s]: %w", idx, err)
	}
	return nil
}

// PutSettings 更新索引 settings（动态项无需 close）。settingsJSON 原样提交。
func PutSettings(ctx context.Context, idx, settingsJSON string) error {
	cli, err := Raw()
	if err != nil {
		return err
	}
	if _, err := cli.IndexPutSettings(idx).BodyString(settingsJSON).Do(ctx); err != nil {
		return fmt.Errorf("更新 settings 失败 [index=%s]: %w", idx, err)
	}
	return nil
}

// Refresh 显式刷新索引使写入可见。
// 注意：olivere 的 RefreshResult 仅含 _shards 计数，不能用于文档数对账；
// 对账请配合 Count 使用。
func Refresh(ctx context.Context, idx string) error {
	cli, err := Raw()
	if err != nil {
		return err
	}
	if _, err := cli.Refresh(idx).Do(ctx); err != nil {
		return fmt.Errorf("刷新索引失败 [index=%s]: %w", idx, err)
	}
	return nil
}

// Count 返回索引文档总数，供重建后对账。索引不存在时如实上抛错误
// （不吞成 0，避免把「索引缺失」误判为「数据为空」）。
func Count(ctx context.Context, idx string) (int64, error) {
	cli, err := Raw()
	if err != nil {
		return 0, err
	}
	n, err := cli.Count(idx).Do(ctx)
	if err != nil {
		return 0, fmt.Errorf("统计文档数失败 [index=%s]: %w", idx, err)
	}
	return n, nil
}

// OpenIndex 打开（解除 close）索引。
//
// 命名偏离方案第四节：方案列出的是 Open/Close，但同包 client.go 已有导出 Close() error
// （关闭 ES 客户端，属 Phase 1 已有导出不宜破坏），故索引级开关改用 OpenIndex/CloseIndex
// 以避免 Close 重名冲突，语义与 olivere 的 OpenIndex/CloseIndex 对齐。
func OpenIndex(ctx context.Context, idx string) error {
	cli, err := Raw()
	if err != nil {
		return err
	}
	if _, err := cli.OpenIndex(idx).Do(ctx); err != nil {
		return fmt.Errorf("打开索引失败 [index=%s]: %w", idx, err)
	}
	return nil
}

// CloseIndex 关闭索引（关闭后不可读写，用于安全重命名/清理场景）。
// 命名原因见 OpenIndex 注释（避开 client.go 已有的 Close() 导出）。
func CloseIndex(ctx context.Context, idx string) error {
	cli, err := Raw()
	if err != nil {
		return err
	}
	if _, err := cli.CloseIndex(idx).Do(ctx); err != nil {
		return fmt.Errorf("关闭索引失败 [index=%s]: %w", idx, err)
	}
	return nil
}

// CatIndices 返回索引的文档数与存储体积，供 es:status 展示与蓝绿磁盘预估。
//
// 预期 idx 解析为单个物理索引；若为通配/别名命中多行，docs 累加、
// storeSize 取首个非空的 store.size（含主副本的总存储）。
func CatIndices(ctx context.Context, idx string) (docs int64, storeSize string, err error) {
	cli, err := Raw()
	if err != nil {
		return 0, "", err
	}
	rows, err := cli.CatIndices().Index(idx).Do(ctx)
	if err != nil {
		return 0, "", fmt.Errorf("获取索引统计失败 [index=%s]: %w", idx, err)
	}
	for _, row := range rows {
		docs += int64(row.DocsCount)
		if storeSize == "" {
			storeSize = row.StoreSize
		}
	}
	return docs, storeSize, nil
}

// -- 内部辅助 --

// invalidateCache 尽力失效 pkg/es 的索引存在性缓存；客户端未初始化时静默跳过。
func invalidateCache(idx string) {
	if c, err := Client(); err == nil {
		c.DeleteIndexCache(idx)
	}
}

// pickEntry 从 {name: {...}} 的响应中取目标条目，命中失败时回退到唯一条目。
func pickEntry(m map[string]any, name string) map[string]any {
	if e, ok := m[name].(map[string]any); ok {
		return e
	}
	for _, v := range m {
		if e, ok := v.(map[string]any); ok {
			return e
		}
	}
	return nil
}
