package admin

// es_admin_integration_test.go 是「走真实全局配置」的集成验证骨架，与纯单元测试分开：
//
//   - 纯单元测试（es_admin_test.go / bulk_test.go / indices_url_test.go）：零依赖，用假 ES，随时跑。
//   - 本文件的集成验证：ES_IT=1 时由 TestMain 跑一次真实 bootstrap（读 conf/config.yml，
//     连真 Redis/MySQL/ES），和 cmd/cli 入口同一条初始化链路，用来开发时一步步确认 admin 功能。
//
// 运行：
//
//	ES_IT=1 go test -count=1 -v ./internal/es/admin/ -run TestIT
//
// 不带 ES_IT=1 时本文件所有用例自动 Skip，不影响包内其它测试。

import (
	"context"
	"fmt"
	"os"
	"shop/internal/bootstrap"
	"testing"
)

// TestMain 是整个 admin 包测试的入口（只允许有一个）。
// 仅当 ES_IT=1 时才做真实初始化；否则保持原样直接跑，纯单元测试无需任何外部环境。
func TestMain(m *testing.M) {
	if os.Getenv("ES_IT") != "1" {
		os.Exit(m.Run())
	}

	// go test 的工作目录默认是包目录 internal/es/admin/，先切到项目根，
	// 让 conf/config.yml、runtime/logs 等相对路径与真实 CLI 运行时完全一致。
	if err := os.Chdir("../../.."); err != nil {
		fmt.Fprintf(os.Stderr, "集成验证切到项目根失败: %v\n", err)
		os.Exit(1)
	}

	// 真实全局初始化：配置→日志→Redis→MySQL→ES（基础组件始终初始化，连不上会 panic）。
	bootstrap.BootstrapWith(bootstrap.DefaultConfigPath, bootstrap.WithElasticsearch())

	// 注意：os.Exit 不会执行 defer，所以先拿退出码、再 Shutdown、最后 Exit。
	code := m.Run()
	bootstrap.Shutdown()
	os.Exit(code)
}

// requireIntegration 集成用例的统一开关：未开启真实环境时跳过，而不是失败。
func requireIntegration(t *testing.T) {
	t.Helper()
	if os.Getenv("ES_IT") != "1" {
		t.Skip("真实环境集成验证：需 ES_IT=1 启用（会走 conf/config.yml 连真 Redis/MySQL/ES）")
	}
}

// TestITClient 第一步：验证「全局配置 → bootstrap → global.EsClient → admin 封装 → 真实 ES」整条链路。
// 全程只读，不写任何数据。
func TestITClient(t *testing.T) {
	requireIntegration(t)
	ctx := context.Background()

	c, err := Client()
	if err != nil {
		t.Fatalf("Client() 应返回已初始化客户端: %v", err)
	}
	if c == nil {
		t.Fatal("Client() 返回 nil")
	}
	raw, err := Raw()
	if err != nil {
		t.Fatalf("Raw() 应返回底层 *elastic.Client: %v", err)
	}
	if raw == nil {
		t.Fatal("Raw() 返回 nil")
	}

	// 只读探活：Exists 会真实发 HEAD 请求，err==nil 即证明 admin 封装到真 ES 的链路通。
	exists, err := Exists(ctx, "shop-product")
	if err != nil {
		t.Fatalf("Exists 只读探测失败（ES 链路不通）: %v", err)
	}
	t.Logf("真实 ES 链路正常：shop-product 存在=%v", exists)
}

// 验证别名索引的真是指向
func TestAliasIndexes(t *testing.T) {
	requireIntegration(t)
	indexs, err := AliasIndexes(context.Background(), "shop-product")
	if err != nil {
		t.Fatalf("AliasIndexes() error = %v", err)
	}
	t.Logf("AliasIndexes() indexs = %v", indexs)
}

func TestGetMapping(t *testing.T) {
	requireIntegration(t)
	mapping, err := GetMapping(context.Background(), "shop-product_v1")
	if err != nil {
		t.Fatalf("GetMapping() error = %v", err)
	}
	t.Logf("GetMapping() mapping = %v", mapping)
}
func TestIndexAlias(t *testing.T) {
	requireIntegration(t)
	aliases, err := IndexAliases(context.Background(), "shop-product_v1")
	if err != nil {
		t.Fatalf("IndexAliases() error = %v", err)
	}
	t.Logf("IndexAliases() aliases = %v", aliases)
}

// ---------------------------------------------------------------------------
// 下一步模板（写路径）：开发到哪个功能就取消注释哪一段，逐步验证。
// 铁律：用 test- 前缀隔离 + defer 清理，绝不碰线上索引/别名。
// ---------------------------------------------------------------------------
//
// func TestITIndexLifecycle(t *testing.T) {
// 	requireIntegration(t)
// 	ctx := context.Background()
// 	idx := "test-admin-it"
//
// 	// 1. 建索引
// 	if err := Create(ctx, idx, `{"settings":{"number_of_shards":1,"number_of_replicas":0}}`); err != nil {
// 		t.Fatalf("建索引失败: %v", err)
// 	}
// 	defer Delete(ctx, idx) // 无论后续成败都清理，避免污染真实集群
//
// 	// 2. 验证存在
// 	if ok, err := Exists(ctx, idx); err != nil || !ok {
// 		t.Fatalf("建完应存在: ok=%v err=%v", ok, err)
// 	}
//
// 	// 3. 灌一条文档（IndexBulk）→ 4. Refresh → 5. Count 对账 → 6. GetMapping 回读 ...
// 	// 每加一步就 go test -run TestITIndexLifecycle 跑一次，红了就停在那一步排查。
// }
