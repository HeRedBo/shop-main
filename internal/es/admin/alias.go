package admin

// 本文件封装 Elasticsearch 别名管理。核心原则：所有别名变更一律走
// client.Alias().Action(...).Do(ctx) 的「单请求 POST /_aliases」，天然原子，
// 绝不「先 putAlias 再 delete」两步（避免别名短暂缺失或同时指向两索引的窗口）。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"

	"github.com/olivere/elastic/v7"
)

// versionSuffixRe 匹配物理索引名尾部的版本号，用于推导下一代索引。
// 命名的实际形态是 <alias>_v<N>（方案 3.1 节），故 `v` 前缀为可选：
// 既能解析 shop-product_v1，也能兼容 shop-product_1 的简写；方案正文写的 `_(\d+)$`
// 无法匹配 `_v1`（`_` 与数字间夹了 v），此处按实际命名修正。
var versionSuffixRe = regexp.MustCompile(`_(?:v)?(\d+)$`)

// AliasExists 判断别名是否存在（至少指向一个物理索引）。
func AliasExists(ctx context.Context, alias string) (bool, error) {
	idx, err := AliasIndexes(ctx, alias)
	if err != nil {
		return false, err
	}
	return len(idx) > 0, nil
}

// AliasIndexes 返回别名指向的物理索引列表，按字典序排序以保证结果稳定、
// 便于上层比较与日志输出。别名不存在时返回 (nil, nil)。
//
// 陷阱：AliasesResult.Indices 的 value 类型 indexResult 未导出，无法显式声明其变量，
// 只能借助导出的 IndicesByAlias(alias) 取索引名列表。
func AliasIndexes(ctx context.Context, alias string) ([]string, error) {
	cli, err := Raw()
	if err != nil {
		return nil, err
	}
	res, err := cli.Aliases().Alias(alias).Do(ctx)
	if err != nil {
		if elastic.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("读取别名指向失败 [alias=%s]: %w", alias, err)
	}
	idx := res.IndicesByAlias(alias)
	sort.Strings(idx)
	return idx, nil
}

// IndexAliases 返回指向指定物理索引的全部别名（按字典序排序）。
// 走 GET /<index>/_alias，响应形如 {"<index>": {"aliases": {"<alias>": {}, ...}}}。
// 索引无别名时返回空 slice 而非 error；索引不存在返回 (nil, nil)。
//
// 修复：原实现走 GET /_alias/<index>，ES 把 <index> 当别名名解析，物理索引名查不到
// 返回 404 → (nil,nil) → 上层误判「无别名，安全调优」。改为 GET /<index>/_alias。
//
// Phase 4 tune-ingest 权威判定：直接查 ES 侧真实别名，不依赖 Registry 登记。
func IndexAliases(ctx context.Context, index string) ([]string, error) {
	cli, err := Raw()
	if err != nil {
		return nil, err
	}
	// 修复：路径从 /_alias/<index> 改为 /<index>/_alias
	path := "/" + url.PathEscape(index) + "/_alias"
	res, err := cli.PerformRequest(ctx, elastic.PerformRequestOptions{
		Method: "GET",
		Path:   path,
	})
	if err != nil {
		if elastic.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("查询索引别名失败 [index=%s]: %w", index, err)
	}
	// 解析响应：{ "<index>": { "aliases": { "<alias>": {}, ... } } }
	var parsed map[string]struct {
		Aliases map[string]struct{} `json:"aliases"`
	}
	if err := json.Unmarshal(res.Body, &parsed); err != nil {
		return nil, fmt.Errorf("解析索引别名响应失败 [index=%s]: %w", index, err)
	}
	for _, entry := range parsed {
		out := make([]string, 0, len(entry.Aliases))
		for a := range entry.Aliases {
			out = append(out, a)
		}
		sort.Strings(out)
		return out, nil
	}
	return nil, nil
}

// AtomicSwitchAlias 以单请求原子方式变更别名指向。
//   - addIndexes：需要挂到 alias 上的物理索引；
//   - removeIndexes：需要从 alias 摘除的物理索引；
//   - removeEntireAlias=true 用 NewAliasRemoveIndexAction(idx)（连索引带别名整体摘除，割接场景用），
//     false 用 NewAliasRemoveAction(alias).Index(idx)（仅解除别名关联，保留索引数据）。
//
// 切换前先用 AliasIndexes 断言 removeIndexes 确实持有该别名；不一致则报错中止并保留现场。
// 完成后校验 res.Acknowledged（olivere v7 的 AliasResult 没有 Errors 字段）。
func AtomicSwitchAlias(ctx context.Context, alias string, addIndexes, removeIndexes []string, removeEntireAlias bool) error {
	cli, err := Raw()
	if err != nil {
		return err
	}
	if len(removeIndexes) > 0 {
		cur, err := AliasIndexes(ctx, alias)
		if err != nil {
			return err
		}
		hold := make(map[string]bool, len(cur))
		for _, c := range cur {
			hold[c] = true
		}
		var missing []string
		for _, r := range removeIndexes {
			if !hold[r] {
				missing = append(missing, r)
			}
		}
		if len(missing) > 0 {
			return fmt.Errorf("别名指向异常，已中止切换 [alias=%s]：待移除索引 %v 并未持有该别名，当前实际指向 %v", alias, missing, cur)
		}
	}

	svc := cli.Alias()
	for _, idx := range addIndexes {
		svc.Action(elastic.NewAliasAddAction(alias).Index(idx))
	}
	for _, idx := range removeIndexes {
		if removeEntireAlias {
			svc.Action(elastic.NewAliasRemoveIndexAction(idx))
		} else {
			svc.Action(elastic.NewAliasRemoveAction(alias).Index(idx))
		}
	}

	res, err := svc.Do(ctx)
	if err != nil {
		return fmt.Errorf("原子切换别名失败 [alias=%s]: %w", alias, err)
	}
	if res == nil || !res.Acknowledged {
		return fmt.Errorf("别名切换未被集群确认 [alias=%s]", alias)
	}
	return nil
}

// EnsureAlias 幂等确保别名指向：挂上 addIndexes、摘掉 removeIndexes，单请求原子完成。
// 与 AtomicSwitchAlias 的差别仅在于 removeEntireAlias 恒为 false（只解除别名关联，不删索引）。
func EnsureAlias(ctx context.Context, alias string, addIndexes, removeIndexes []string) error {
	return AtomicSwitchAlias(ctx, alias, addIndexes, removeIndexes, false)
}

// WriteIndexName 依据 current 推导下一代写索引名：<alias>_v<N>。
// current 合规（尾部形如 _v<digits>）时取 N+1；current 为空或尾部不合规时
// 回落到入参 nextN。
func WriteIndexName(alias, current string, nextN int) string {
	n := nextN
	if m := versionSuffixRe.FindStringSubmatch(current); m != nil {
		if cur, err := strconv.Atoi(m[1]); err == nil {
			n = cur + 1
		}
	}
	return alias + "_v" + strconv.Itoa(n)
}
