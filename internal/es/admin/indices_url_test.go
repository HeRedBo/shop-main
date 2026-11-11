package admin

// indices_url_test.go 钉死一件事：读 mapping 的请求 URL 里**绝不能出现 mapping type 段**。
//
// olivere/elastic v7.0.32 的 GetMappingService 在调用方未传 type 时兜底成 "_all"
// （indices_get_mapping.go:145-152），发出 GET /<idx>/_mapping/_all；ES 8.x 已移除
// mapping type，该路径无 handler，直接 400。TestGetMappingOlivereServiceBuildsTypedPath
// 用假服务器把这个上游行为也记下来，将来若有人把 GetMapping 换回 olivere 服务，
// 本文件会立刻失败并给出原因。全部用例跑在 httptest 上，不需要真实 ES。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	sharedes "github.com/HeRedBo/pkg/es"
	"github.com/olivere/elastic/v7"

	"shop/pkg/global"
)

// cannedMapping 是 8.2.2 回读体的最小等价形状。
const cannedMapping = `{"shop-product":{"mappings":{"dynamic":"strict","properties":{"id":{"type":"keyword"},"title":{"type":"text"}}}}}`

// newFakeESTool 起一个只回固定 mapping 的假 ES，返回客户端与被请求路径的快照函数。
// 路径收集必须加锁：handler 跑在独立 goroutine 上，-race 下裸 slice 会报数据竞争。
func newFakeESTool(t *testing.T, status int, body string) (*elastic.Client, func() []string) {
	t.Helper()
	var (
		mu    sync.Mutex
		paths []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	cli, err := elastic.NewClient(
		elastic.SetURL(srv.URL),
		elastic.SetSniff(false),
		elastic.SetHealthcheck(false),
		elastic.SetMaxRetries(0),
	)
	if err != nil {
		t.Fatalf("构造测试用 ES 客户端失败: %v", err)
	}
	return cli, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), paths...)
	}
}

func TestMappingPathHasNoTypeSegment(t *testing.T) {
	if got, want := mappingPath("shop-product"), "/shop-product/_mapping"; got != want {
		t.Errorf("mappingPath() = %q，期望 %q", got, want)
	}
	for _, idx := range []string{"shop-product", "shop-product_v1", "zz_cutover_probe_a"} {
		p := mappingPath(idx)
		for _, bad := range []string{"/_all", "/_doc", "_mapping/"} {
			if len(p) >= len(bad) && p[len(p)-len(bad):] == bad {
				t.Errorf("mappingPath(%q)=%q 末尾出现 type 段 %q", idx, p, bad)
			}
		}
	}
	// 特殊字符必须转义，否则路径注入会改变请求目标
	if got := mappingPath("a b"); got != "/a%20b/_mapping" {
		t.Errorf("索引名未转义，got=%q", got)
	}
}

func TestFetchMappingRequestsTypelessPath(t *testing.T) {
	cli, seen := newFakeESTool(t, http.StatusOK, cannedMapping)

	got, err := fetchMapping(context.Background(), cli, "shop-product")
	if err != nil {
		t.Fatalf("fetchMapping() 报错: %v", err)
	}
	paths := seen()
	if !hasExact(paths, "/shop-product/_mapping") {
		t.Errorf("实际请求路径缺少无 type 段的 /shop-product/_mapping：%v", paths)
	}
	if hasTyped(paths) {
		t.Errorf("请求路径带上了 mapping type 段（ES 8.x 必 400）：%v", paths)
	}
	props, ok := got["properties"].(map[string]any)
	if !ok || len(props) != 2 {
		t.Fatalf("未拆封到内层 mappings，got=%#v", got)
	}
	if _, ok := props["id"].(map[string]any); !ok {
		t.Errorf("properties.id 缺失，got=%#v", props)
	}
	if dyn, _ := got["dynamic"].(string); dyn != "strict" {
		t.Errorf("dynamic 应为 strict，got=%v", got["dynamic"])
	}
}

func TestFetchMappingMissingIndexIsNilNil(t *testing.T) {
	body := `{"error":{"root_cause":[{"type":"index_not_found_exception","reason":"no such index"}],` +
		`"type":"index_not_found_exception","status":404},"status":404}`
	cli, _ := newFakeESTool(t, http.StatusNotFound, body)

	got, err := fetchMapping(context.Background(), cli, "shop-product_v9")
	if err != nil {
		t.Fatalf("索引不存在必须返回 nil error（契约），got err=%v", err)
	}
	if got != nil {
		t.Errorf("索引不存在必须返回 nil map，got=%#v", got)
	}
}

// TestGetMappingOlivereServiceBuildsTypedPath 记录上游缺陷本身：一旦有人把实现换回
// olivere 的 GetMappingService，请求就会带上 _all，ES 8.2.2 实测返回 400。
func TestGetMappingOlivereServiceBuildsTypedPath(t *testing.T) {
	cli, seen := newFakeESTool(t, http.StatusOK, cannedMapping)

	if _, err := cli.GetMapping().Index("shop-product").Do(context.Background()); err != nil {
		t.Fatalf("olivere GetMappingService 调用失败: %v", err)
	}
	paths := seen()
	if !hasExact(paths, "/shop-product/_mapping/_all") {
		t.Errorf("预期 olivere 兜底出 type=\"_all\" 的路径，实际 %v —— 上游行为变了，请同步复查本文件的约束", paths)
	}
}

// hasExact / hasTyped 路径断言助手：只关心 mapping 请求本身，不管 olivere 可能附带的其它探测请求。
func hasExact(paths []string, want string) bool {
	for _, p := range paths {
		if p == want {
			return true
		}
	}
	return false
}

// hasTyped 只盯 mapping 请求本身：后缀带 type 段即 ES 8.x 必 400。
func hasTyped(paths []string) bool {
	for _, p := range paths {
		if strings.HasSuffix(p, "/_mapping/_all") || strings.HasSuffix(p, "/_mapping/_doc") {
			return true
		}
	}
	return false
}

// typedSegments 返回所有「/_mapping/<something>」形态的路径。
// 与 hasTyped 的区分很故意：hasTyped 只认识已知 type 名，而导出层用例要钉的是
// 「_mapping 后面根本不该有东西」，将来换成任何 type 名（_doc/_def/索引名）都拦得住。
func typedSegments(paths []string) []string {
	var bad []string
	for _, p := range paths {
		if i := strings.Index(p, "/_mapping/"); i >= 0 {
			bad = append(bad, p)
		}
	}
	return bad
}

// injectSharedESClient 把 httptest 假 ES 客户端当作全局客户端，让**导出**的 admin API
// 走真实生产路径：GetMapping 进 Raw() 读的就是 global.EsClient。
//
// 必须用 t.Cleanup 归还：EsClient 是进程级变量，不还回去的话同包后续用例
// （以及 -count=2 的第 2 遍）会拿到一台已 close 的服务器。
func injectSharedESClient(t *testing.T, cli *elastic.Client) {
	t.Helper()
	prev := global.EsClient
	global.EsClient = &sharedes.Client{Client: cli}
	t.Cleanup(func() { global.EsClient = prev })
}

// TestGetMappingExportedRequestsTypelessPath 钉的是**导出层**：admin.GetMapping 本身
// 必须发出不带 mapping type 段的请求，并保持契约（内层 mappings / 404 返回 (nil,nil)）。
//
// 为什么私有用例不够：把 GetMapping 函数体换成 cli.GetMapping().Index(idx).Do(ctx) 后，
// fetchMapping / mappingPath 的用例依旧全绿（它们测的是另一个函数），而真机 es:status
// 立刻 elastic: Error 400 —— 即「换回 olivere 就发 _all」这条防护实际上并不存在。
func TestGetMappingExportedRequestsTypelessPath(t *testing.T) {
	t.Run("导出函数不能发 type 段", func(t *testing.T) {
		cli, seen := newFakeESTool(t, http.StatusOK, cannedMapping)
		injectSharedESClient(t, cli)

		got, err := GetMapping(context.Background(), "shop-product")
		if err != nil {
			t.Fatalf("GetMapping() 报错: %v", err)
		}
		paths := seen()
		if !hasExact(paths, "/shop-product/_mapping") {
			t.Errorf("导出层实际请求路径里没有 /shop-product/_mapping：%v", paths)
		}
		if bad := typedSegments(paths); len(bad) > 0 {
			t.Errorf("GetMapping 发出了带 mapping type 段的路径（ES 8.x 必 400）：%v", bad)
		}

		// 契约②：返回内层 mappings，不是 {"<idx>":{...}} 外层，也不是 {"mappings":{...}} 包裹层
		if _, ok := got["shop-product"]; ok {
			t.Errorf("返回的是外层索引名包裹，未拆封：%#v", got)
		}
		if _, ok := got["mappings"]; ok {
			t.Errorf("返回的仍是 mappings 包裹层，未拆到内层：%#v", got)
		}
		props, ok := got["properties"].(map[string]any)
		if !ok || len(props) != 2 {
			t.Fatalf("properties 期待 2 个字段，got=%#v", got)
		}
		if dyn, _ := got["dynamic"].(string); dyn != "strict" {
			t.Errorf("dynamic 应为 strict，got=%v", got["dynamic"])
		}
	})

	t.Run("索引不存在返回 (nil, nil)", func(t *testing.T) {
		body := `{"error":{"root_cause":[{"type":"index_not_found_exception","reason":"no such index"}],` +
			`"type":"index_not_found_exception","status":404},"status":404}`
		cli, seen := newFakeESTool(t, http.StatusNotFound, body)
		injectSharedESClient(t, cli)

		got, err := GetMapping(context.Background(), "shop-product_v9")
		if err != nil {
			t.Fatalf("索引不存在必须返回 nil error（契约），got err=%v", err)
		}
		if got != nil {
			t.Errorf("索引不存在必须返回 nil map，got=%#v", got)
		}
		if bad := typedSegments(seen()); len(bad) > 0 {
			t.Errorf("404 分支也不该发 type 段：%v", bad)
		}
	})
}

func TestUnwrapMappingBody(t *testing.T) {
	parse := func(s string) map[string]any {
		t.Helper()
		var m map[string]any
		if err := json.Unmarshal([]byte(s), &m); err != nil {
			t.Fatalf("测试数据非法 JSON: %v", err)
		}
		return m
	}

	// 正常形态：拆到内层 mappings
	got, err := unwrapMappingBody("shop-product", parse(cannedMapping))
	if err != nil || got["dynamic"] != "strict" {
		t.Fatalf("正常拆封失败，err=%v got=%#v", err, got)
	}

	// 走别名时回读 key 是物理索引名 => 唯一条目兜底
	got, err = unwrapMappingBody("shop-product", parse(`{"shop-product_v1":{"mappings":{"dynamic":"strict"}}}`))
	if err != nil || got["dynamic"] != "strict" {
		t.Errorf("别名回读兜底失败，err=%v got=%#v", err, got)
	}

	// 没有 mappings 包裹层 => 原样返回条目
	got, err = unwrapMappingBody("shop-product", parse(`{"shop-product":{"properties":{"id":{"type":"keyword"}}}}`))
	if err != nil || got["properties"] == nil {
		t.Errorf("无包裹层时应原样返回条目，err=%v got=%#v", err, got)
	}

	// 空对象 => 不猜，报「非预期返回结构」
	if _, err := unwrapMappingBody("shop-product", parse(`{}`)); err == nil {
		t.Errorf("空回读体必须报错")
	}
	// 多条目且目标名不在其中 => 不猜（map 遍历顺序随机，猜一个会把别的索引当成线上结构）
	if _, err := unwrapMappingBody("shop-product", parse(`{"a":{"mappings":{"dynamic":"strict"}},"b":{"mappings":{}}}`)); err == nil {
		t.Errorf("多条目且无同名条目时必须报错而不是随便取一个")
	}
	// 条目不是对象 => 报错
	if _, err := unwrapMappingBody("shop-product", parse(`{"shop-product":"oops"}`)); err == nil {
		t.Errorf("条目非对象必须报错")
	}
}
