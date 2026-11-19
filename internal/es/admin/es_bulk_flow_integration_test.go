package admin_test

// es_bulk_flow_integration_test.go 用「外部测试包」验证真实数据写入全链路：
//
//	MySQL 真实商品 → es.BuildIndexDoc 真实装配 → admin.IndexBulk 写入 → Refresh + Count 对账
//
// 为什么单独放外部包（admin_test）而不是留在 es_admin_integration_test.go（package admin）：
// 内部测试包 admin 不能再 import es/sync —— 它们都反向 import 了 admin，会触发 import cycle。
// 而真实装配函数 es.BuildIndexDoc 在 es 包，只有外部测试包 admin_test 才能合法调用它。
// 外部包与内部包编译进同一个测试二进制，故仍复用 es_admin_integration_test.go 里的 TestMain
// 做真实 bootstrap（ES_IT=1 时连真 Redis/MySQL/ES），这里只需自带 ES_IT 跳过开关。
//
// 运行：
//
//	ES_IT=1 go test -count=1 -v ./internal/es/admin/ -run TestITIndexBulkFlow
//
// 铁律：用 test- 前缀临时索引隔离 + defer 清理，绝不碰线上索引/别名。

import (
	"context"
	"encoding/json"
	"os"
	"shop/internal/es"
	"shop/internal/es/admin"
	"shop/internal/models"
	"shop/pkg/global"
	"testing"
)

func TestITIndexBulkFlow(t *testing.T) {
	if os.Getenv("ES_IT") != "1" {
		t.Skip("真实环境集成验证：需 ES_IT=1 启用（会走 conf/config.yml 连真 Redis/MySQL/ES）")
	}
	ctx := context.Background()

	// 1. 从 MySQL 读真实商品（与线上同步同一张表），取前 10 条做样本。
	var data []models.StoreProduct
	if err := global.Db.WithContext(ctx).Where("id > ?", 0).Limit(2).Order("id asc").Find(&data).Error; err != nil {
		t.Fatalf("查询商品数据失败: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("数据库产品数据不存在")
	}
	// 2. 真实装配：es.BuildIndexDoc 做 HTML 清洗 + 入索引资格过滤（is_del/is_show），
	//    _id/routing 走 BulkDocID/BulkRouting，与 productProvider.Assemble 逐字一致。
	docs := make([]admin.Doc, 0, len(data))
	for i := range data {
		di, eligible := es.BuildIndexDoc(&data[i])
		if !eligible {
			continue
		}
		// 异常数据装配 验证异常模块
		src, _ := json.Marshal(di)
		var m map[string]any
		json.Unmarshal(src, &m)
		if di.BulkDocID() == "20" {
			m["price"] = "这是测试数据"
		}
		docs = append(docs, admin.Doc{ID: di.BulkDocID(), Routing: di.BulkRouting(), Src: m})
	}
	if len(docs) == 0 {
		t.Skip("样本商品均不符合入索引条件（is_del!=0 或 is_show!=1），无可写文档")
	}

	// 3. 用 registry 真实 mapping 建临时索引（结构事实源与线上完全一致）。
	//    BodyJSON 已剔除 _meta/aliases，临时索引不挂任何别名，defer 删除避免污染集群。
	reg, err := es.LoadRegistry("")
	if err != nil {
		t.Fatalf("装载索引声明失败: %v", err)
	}
	def, err := reg.MustGet("shop-product")
	if err != nil {
		t.Fatalf("定位 shop-product 索引定义失败: %v", err)
	}
	body, err := def.BodyJSON()
	if err != nil {
		t.Fatalf("生成建索引请求体失败: %v", err)
	}
	idx := "test-it-bulk-flow"
	if err := admin.Create(ctx, idx, body); err != nil {
		t.Fatalf("建临时索引失败: %v", err)
	}
	defer admin.Delete(ctx, idx) // 无论后续成败都清理

	// 4. IndexBulk 写入：请求级 err 必须为 nil，item 级 Failed 必须为 0，Sent 必须等于装配条数。
	res, err := admin.IndexBulk(ctx, idx, docs)
	if err != nil {
		t.Fatalf("IndexBulk 请求级失败: %v", err)
	}
	if res.Failed != 0 {
		t.Fatalf("存在 item 级失败 %d 条: %+v", res.Failed, res.Failures)
	}
	if res.Sent != len(docs) {
		t.Fatalf("Sent=%d，期望=%d（装配条数）", res.Sent, len(docs))
	}

	// 5. Refresh 让文档可搜，再 Count 对账：写入条数必须与索引内实际条数一致。
	if err := admin.Refresh(ctx, idx); err != nil {
		t.Fatalf("Refresh 失败: %v", err)
	}
	n, err := admin.Count(ctx, idx)
	if err != nil {
		t.Fatalf("Count 失败: %v", err)
	}
	if n != int64(len(docs)) {
		t.Fatalf("Count=%d，期望=%d（写入条数）", n, len(docs))
	}

	t.Logf("写入对账通过：MySQL %d 行 → 合格装配 %d 条 → Sent %d → Count %d", len(data), len(docs), res.Sent, n)
}
