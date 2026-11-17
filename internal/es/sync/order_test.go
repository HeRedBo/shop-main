package sync

// order_test.go 钉死订单接入（任务 #38）在灌数层与补偿层最容易被无声改坏的语义：
//   - #2：_id = order_id（字符串订单号）、routing = strconv.FormatInt(uid,10)；
//   - #4：order_status / pay_type_name 复用 order_service 导出的同一份实现；
//   - #5：SQL 主键游标 + 每批一条 WHERE oid IN (?) 批量取 cart_info，装配热路径零 DB
//         查询（无 N+1），且不误用 updated_at（订单表列名是 create_time / update_time）。
//
// 全部不连真实 MySQL/ES：SQL 形态用 delta_test.go 已证过的 dryRunDialector +
// newDeltaDryRunDB 现算；编排序列用可注入函数变量（orderCartLoader / orderCompensateScan /
// orderCompensateBulk），与商品 compensateScan / compensateBulk 同构。

import (
	"context"
	"strings"
	"testing"
	"time"

	"shop/internal/es"
	"shop/internal/es/admin"
	"shop/internal/models"
	"shop/internal/service/order_service"

	"gorm.io/gorm"
)

// ---------------------------------------------------------------------------
// dispatch hit：别名命中订单 provider（而非商品）
// ---------------------------------------------------------------------------

// TestProviderRegistry_ShopOrderIsWiredByLiteralAlias 是破坏即红哨兵：故意用字面量
// "shop-order" 而不是 OrderAlias 常量查表；改名会立刻让本用例变红。三口表键集必须一致。
func TestProviderRegistry_ShopOrderIsWiredByLiteralAlias(t *testing.T) {
	if OrderAlias != "shop-order" {
		t.Fatalf("OrderAlias 常量被改动（%q），registry 三表 / fixture / CLI --index 都会失效", OrderAlias)
	}
	for name, table := range map[string]map[string]any{
		"providers":    toAnyTable(providers),
		"compensators": toAnyTable(compensators),
		"dbCounters":   toAnyTable(dbCounters),
	} {
		if _, ok := table["shop-order"]; !ok {
			t.Errorf("注册表 %s 缺少字面量别名 shop-order 表项（订单灌数/补偿/计数会断线）", name)
		}
	}
	// ProductAlias 那条不能被改坏：三张表都仍持有 shop-product。
	for name, table := range map[string]map[string]any{
		"providers":    toAnyTable(providers),
		"compensators": toAnyTable(compensators),
		"dbCounters":   toAnyTable(dbCounters),
	} {
		if _, ok := table["shop-product"]; !ok {
			t.Errorf("注册表 %s 意外丢失 shop-product 表项（本任务禁改商品侧）", name)
		}
	}
}

// TestNewProvider_ShopOrderHitsOrderProvider 证明「按别名查表」在订单侧真命中 *orderProvider
// （而不是恒构造商品 provider）：这是订单接线的直接证据，绕开 syncIndexTarget 的商品编排守卫。
func TestNewProvider_ShopOrderHitsOrderProvider(t *testing.T) {
	orderDef := lookupOrderDefForSync(t)
	p, err := NewProvider(OrderAlias, nil, orderDef, Options{Index: OrderAlias})
	if err != nil {
		t.Fatalf("已登记别名必须构造成功，got=%v", err)
	}
	op, ok := p.(*orderProvider)
	if !ok {
		t.Fatalf("NewProvider(%q) 必须返回 *orderProvider，实际 %T", OrderAlias, p)
	}
	if _, isProd := p.(*productProvider); isProd {
		t.Fatal("NewProvider(shop-order) 绝不能命中商品 provider")
	}
	if op.Definition() != orderDef {
		t.Error("provider.Definition 必须原样携带解析到的 shop-order 声明")
	}
	if got := op.Definition().Alias(); got != "shop-order" {
		t.Errorf("Definition().Alias() 必须是 shop-order，got=%q", got)
	}
}

func lookupOrderDefForSync(t *testing.T) *es.IndexDefinition {
	t.Helper()
	defs, err := es.LoadDefinitions("")
	if err != nil {
		t.Fatalf("装载内嵌声明失败: %v", err)
	}
	for _, d := range defs {
		if d.Alias() == "shop-order" {
			return d
		}
	}
	t.Fatal("未装载到 shop-order 声明")
	return nil
}

// ---------------------------------------------------------------------------
// gate #4：order_service 导出函数的表驱动直接覆盖（同一份实现）
// ---------------------------------------------------------------------------

// TestOrderServiceOrderStatus_ExportedFunction 用 8 组覆盖 order_service.OrderStatus 全部分支
// （1/2/3/4/5/6/7 与兜底 0）。断言的是**导出函数本身**，不是 es 层的拷贝。
func TestOrderServiceOrderStatus_ExportedFunction(t *testing.T) {
	cases := []struct {
		paid, status, refund int
		want                 int
		why                  string
	}{
		{0, 0, 0, 1, "未付款"},
		{1, 0, 0, 2, "未发货"},
		{1, 0, 1, 3, "退款中"},
		{1, 1, 0, 4, "待收货"},
		{1, 2, 0, 5, "待评价"},
		{1, 3, 0, 6, "已完成"},
		{1, 0, 2, 7, "已退款"},
		{0, 1, 0, 0, "非法组合兜底 0"},
		{0, 0, 2, 0, "paid=0 refund=2 兜底 0"},
	}
	for _, c := range cases {
		if got := order_service.OrderStatus(c.paid, c.status, c.refund); got != c.want {
			t.Errorf("OrderStatus(%d,%d,%d)=%d want=%d（%s）", c.paid, c.status, c.refund, got, c.want, c.why)
		}
	}
}

// TestOrderServicePayTypeName_ExportedFunction 覆盖导出 PayTypeName 全部分支
// （weixin/yue/integral 三分支 + paid!=1 恒「未支付」 + paid==1 未知 pay_type 走默认）。
func TestOrderServicePayTypeName_ExportedFunction(t *testing.T) {
	cases := []struct {
		payType string
		paid    int
		want    string
	}{
		{"weixin", 1, "微信支付"},
		{"yue", 1, "余额支付"},
		{"integral", 1, "积分兑换"},
		{"alipay", 1, "未支付"}, // 无 default 分支，未知 pay_type 落回初值「未支付」
		{"weixin", 0, "未支付"},
		{"yue", 0, "未支付"},
		{"", 0, "未支付"},
	}
	for _, c := range cases {
		if got := order_service.PayTypeName(c.payType, c.paid); got != c.want {
			t.Errorf("PayTypeName(%q,%d)=%q want=%q", c.payType, c.paid, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// gate #5：SQL 形态 —— 主键游标 + 不误用 updated_at + 每批一条 IN，装配零 DB
// ---------------------------------------------------------------------------

// TestOrderFilterSQL_PrimaryKeyCursorAndSoftDelete 扫 store_order 的 SELECT 必须含
// create_time/update_time 列、WHERE 含 is_del=0 与主键游标 `id > ? AND id <= ?`，且
// 绝不出现 updated_at（订单表没有这列，商品侧历史误用点）。
func TestOrderFilterSQL_PrimaryKeyCursorAndSoftDelete(t *testing.T) {
	db := newDeltaDryRunDB(t)
	op := newOrderProvider(db, lookupOrderDefForSync(t), Options{})

	res := op.filter(context.Background()).
		Select(orderScanColumns).
		Where("id > ? AND id <= ?", 10, 20).
		Find(&[]models.StoreOrder{})
	if res.Error != nil {
		t.Fatalf("DryRun 生成 SQL 失败: %v", res.Error)
	}
	full := res.Statement.SQL.String()
	if full == "" {
		t.Fatal("DryRun 未生成 SQL")
	}
	norm := func(s string) string { return strings.ReplaceAll(s, "`", "") }

	// ① 时间列必须是 create_time/update_time（订单表实名），绝不能出现 updated_at。
	if strings.Contains(strings.ToLower(full), "updated_at") {
		t.Errorf("SQL 出现 updated_at（订单表无此列，商品历史误用），got=%s", full)
	}
	for _, col := range []string{"create_time", "update_time"} {
		if !strings.Contains(norm(full), col) {
			t.Errorf("SELECT 投影应包含订单表时间列 %s，got=%s", col, full)
		}
	}

	// ② 扫描口径必须显式过滤 is_del=0（活单）—— 与商品 compensateScope 相反，
	//    这里正是要过滤，Unscoped 只属于补偿分支。
	if !strings.Contains(norm(whereOf(full)), "is_del") {
		t.Errorf("扫描 WHERE 必须含 is_del=0（软删单绝不能进索引），got=%s", whereOf(full))
	}

	// ③ 主键游标上下界必须落到 WHERE（chunk.ChunkById 依赖它）。
	w := norm(whereOf(full))
	if !strings.Contains(w, "id >") || !strings.Contains(w, "id <=") {
		t.Errorf("扫描 WHERE 必须含主键游标 `id > ?` 与 `id <= ?`，got=%s", w)
	}
}

// TestOrderFilterSQL_CarriesIDsAndSinceIntoWhere 保证 filter 把 opts.IDs / opts.Since 折入 WHERE：
// Bounds 与 ScanRange 共用同一 filter，漂移就会造成 “探测总数”和“实际扫描行数”对不上。
func TestOrderFilterSQL_CarriesIDsAndSinceIntoWhere(t *testing.T) {
	db := newDeltaDryRunDB(t)
	since := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	op := newOrderProvider(db, lookupOrderDefForSync(t), Options{
		IDs: []int64{5, 6}, Since: since,
	})

	res := op.filter(context.Background()).
		Select(orderScanColumns).
		Find(&[]models.StoreOrder{})
	if res.Error != nil {
		t.Fatalf("DryRun 生成 SQL 失败: %v", res.Error)
	}
	w := strings.ToLower(strings.ReplaceAll(whereOf(res.Statement.SQL.String()), "`", ""))
	for _, must := range []string{"is_del", "id in", "update_time >="} {
		if !strings.Contains(w, must) {
			t.Errorf("filter WHERE 必须携带口径 %q，实际: %s", must, w)
		}
	}
}

func whereOf(sql string) string {
	if i := strings.Index(strings.ToUpper(sql), "WHERE"); i >= 0 {
		return sql[i:]
	}
	return sql
}

// TestDefaultOrderCartLoader_SingleInQueryNoNPlusOne 用 gorm.ToSQL 现算 defaultOrderCartLoader
// 实际发出的 SQL 形态：一条 WHERE oid IN (?)，覆盖整批；绝非循环内逐条 oid = ?。
func TestDefaultOrderCartLoader_SingleInQueryNoNPlusOne(t *testing.T) {
	db := newDeltaDryRunDB(t)
	oids := []int64{11, 22, 33}

	sqlStr := db.ToSQL(func(tx *gorm.DB) *gorm.DB {
		return tx.Model(&models.StoreOrderCartInfo{}).
			Where("oid IN (?)", oids).Find(&[]models.StoreOrderCartInfo{})
	})
	if sqlStr == "" {
		t.Fatal("ToSQL 未生成 SQL")
	}
	norm := strings.ReplaceAll(sqlStr, "`", "")
	if !strings.Contains(norm, "store_order_cart_info") {
		t.Errorf("SQL 必须打在 store_order_cart_info 表上，got=%s", sqlStr)
	}
	if !strings.Contains(norm, "oid in") && !strings.Contains(strings.ToLower(norm), "oid in (") {
		t.Errorf("SQL 必须用批量 WHERE oid IN (?)，got=%s", sqlStr)
	}
	// 「无 N+1」的负面断言：一条 SQL 里绝不能出现「oid = ?」（逐行单查的形态）。
	if strings.Contains(strings.ToLower(norm), "oid = ?") {
		t.Errorf("SQL 出现逐行单查形态 oid = ?（N+1 缺陷），got=%s", sqlStr)
	}
}

// TestLoadCartAgg_CalledExactlyOncePerBatch 证明每批主表行只触发一次 loader（一次批量 IN），
// 而不是每行一次 —— 这是「装配热路径无 N+1」最直接的行为证据。
func TestLoadCartAgg_CalledExactlyOncePerBatch(t *testing.T) {
	orig := orderCartLoader
	t.Cleanup(func() { orderCartLoader = orig })

	var calls int
	var gotIds []int64
	orderCartLoader = func(_ context.Context, _ *gorm.DB, oids []int64) (map[int64][]string, error) {
		calls++
		gotIds = append(gotIds, oids...)
		return map[int64][]string{}, nil
	}

	op := newOrderProvider(newDeltaDryRunDB(t), lookupOrderDefForSync(t), Options{})
	rows := []models.StoreOrder{{OrderId: "A", Uid: 1}, {OrderId: "B", Uid: 2}, {OrderId: "C", Uid: 3}}
	if _, err := op.loadCartAgg(context.Background(), rows); err != nil {
		t.Fatalf("loadCartAgg 报错: %v", err)
	}
	if calls != 1 {
		t.Errorf("每批必须恰好一次 loader 调用（一次批量 IN 覆盖整批），calls=%d", calls)
	}
	if len(gotIds) != 3 {
		t.Errorf("loader 必须一次收到整批 3 个 id，got=%v", gotIds)
	}
}

// TestAssemble_ZeroDBTouch 用 db=nil 直接坐实装配热路径零查询：若 Assemble 里出现任何一次
// DB 触达（就是 N+1 的形态），nil 会当场 panic —— 「不 panic」本身就是证据。
func TestAssemble_ZeroDBTouch(t *testing.T) {
	op := newOrderProvider(nil, lookupOrderDefForSync(t), Options{})
	rows := []models.StoreOrder{
		{OrderId: "TN0001", Uid: 10}, {OrderId: "TN0002", Uid: 20}, {OrderId: "TN0003", Uid: 30},
	}
	// 用行主键 id 关联 cart_info，与 provider 生产路径一致（orderCartLoader 已按 oid 分组）。
	rows[0].Id, rows[1].Id, rows[2].Id = 100, 200, 300
	batch := orderBatch{
		Rows: rows,
		CartInfos: map[int64][]string{
			100: {`{"product_info":{"id":1,"store_name":"红色T恤"}}`},
			200: {`{"product_info":{"id":2,"store_name":"蓝色裤"}}`,
				`{"product_info":{"id":3,"store_name":"白色袜"}}`},
			// 300 无 cart_info（合法：空数组）
		},
	}
	docs, err := op.Assemble(batch)
	if err != nil {
		t.Fatalf("Assemble 报错: %v", err)
	}
	if len(docs) != 3 {
		t.Fatalf("三行活单应装配出 3 个 doc，got=%d", len(docs))
	}
	// _id=order_id（字符串），routing=strconv.FormatInt(uid,10)。
	if docs[0].ID != "TN0001" || docs[0].Routing != "10" {
		t.Errorf("docs[0] _id/routing 不符: %+v", docs[0])
	}
	if docs[1].ID != "TN0002" || docs[1].Routing != "20" {
		t.Errorf("docs[1] _id/routing 不符: %+v", docs[1])
	}
	if docs[2].ID != "TN0003" || docs[2].Routing != "30" {
		t.Errorf("docs[2] _id/routing 不符: %+v", docs[2])
	}
	for _, d := range docs {
		if d.Delete || d.Src == nil {
			t.Errorf("活单必须是 index 且 Src 非空: %+v", d)
		}
	}
}

// ---------------------------------------------------------------------------
// 补偿 SQL 与编排：Unscoped / update_time >= since / 主键游标 / 不加 is_del 过滤
// ---------------------------------------------------------------------------

// TestOrderCompensateScope_SQL 与商品 TestCompensateScope_SQL 同构，破坏即红。
func TestOrderCompensateScope_SQL(t *testing.T) {
	db := newDeltaDryRunDB(t)
	since := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	res := orderCompensateScope(db.Session(&gorm.Session{}), since, 1000, 42).
		Find(&[]models.StoreOrder{})
	if res.Error != nil {
		t.Fatalf("DryRun 生成 SQL 失败: %v", res.Error)
	}
	full := res.Statement.SQL.String()
	if full == "" {
		t.Fatal("DryRun 未生成 SQL")
	}
	norm := strings.ReplaceAll(full, "`", "")
	w := whereOf(norm)

	// ① 时间下界 + 主键游标必须落在 WHERE。
	if !strings.Contains(w, "update_time >=") {
		t.Errorf("WHERE 必须含 update_time >= 下界，实际: %s", w)
	}
	if !strings.Contains(w, "id >") {
		t.Errorf("WHERE 必须含 id > 主键游标，实际: %s", w)
	}
	// ② Unscoped：WHERE 里绝不能出现 is_del（否则软删单读不到、永远删不掉）。
	if strings.Contains(w, "is_del") {
		t.Errorf("WHERE 不该出现 is_del 过滤（Unscoped 缺失）: %s", w)
	}
	// ③ 不误用 updated_at。
	if strings.Contains(strings.ToLower(norm), "updated_at") {
		t.Errorf("补偿 SQL 出现 updated_at（订单表无此列）: %s", norm)
	}
	// ④ since 原样下传（取错下界＝静默漏数据）。
	var sawSince bool
	for _, v := range res.Statement.Vars {
		if tv, ok := v.(time.Time); ok && tv.Equal(since) {
			sawSince = true
			break
		}
	}
	if !sawSince {
		t.Errorf("绑定变量里应含 since=%v，实际 Vars=%v", since, res.Statement.Vars)
	}
	// ⑤ SELECT 显式投影带 is_del（Unscoped 读回软删值供 orderCompensateDocs 分流）。
	selPart := norm
	if i := strings.Index(strings.ToUpper(norm), "WHERE"); i > 0 {
		selPart = norm[:i]
	}
	if !strings.Contains(strings.ToLower(selPart), "is_del") {
		t.Errorf("SELECT 应显式包含 is_del，实际: %s", selPart)
	}
}

// TestCompensateOrder_ForwardsSinceAndBranches 与商品 TestCompensate_ForwardsSinceAndBranches
// 同构：since/target 原样透传；每批一次 loader；index/delete 分流；_id/routing 与全量同源。
func TestCompensateOrder_ForwardsSinceAndBranches(t *testing.T) {
	origScan, origBulk, origLoader := orderCompensateScan, orderCompensateBulk, orderCartLoader
	t.Cleanup(func() {
		orderCompensateScan, orderCompensateBulk, orderCartLoader = origScan, origBulk, origLoader
	})

	since := time.Now().Add(-2 * time.Hour)
	var gotSince time.Time
	var gotTarget string
	var allDocs []admin.Doc
	loaderCalls := 0
	// 装配证据：id=1 有合法 cart_info，装配产出 names=[红色]；id=2 无（软删走 delete）。
	orderCartLoader = func(_ context.Context, _ *gorm.DB, oids []int64) (map[int64][]string, error) {
		loaderCalls++
		m := map[int64][]string{}
		for _, id := range oids {
			if id == 1 {
				m[id] = []string{`{"product_info":{"id":9,"store_name":"红色T恤"}}`}
			}
		}
		return m, nil
	}
	orderCompensateScan = func(_ context.Context, s time.Time, _ int, cb func([]models.StoreOrder) error) error {
		gotSince = s
		rows := []models.StoreOrder{
			{OrderId: "OK001", Uid: 11},  // 合格 -> index
			{OrderId: "DEL002", Uid: 22}, // 软删 -> delete
			{OrderId: "", Uid: 33},       // 空订单号 -> 跳过
		}
		rows[0].Id, rows[1].Id, rows[2].Id = 1, 2, 3
		rows[1].IsDel = 1
		return cb(rows)
	}
	orderCompensateBulk = func(_ context.Context, target string, docs []admin.Doc) (*admin.BulkResult, error) {
		gotTarget = target
		allDocs = append(allDocs, docs...)
		return &admin.BulkResult{}, nil
	}

	st, err := CompensateOrder(context.Background(), "shop-order_v2", since, CompensateOptions{})
	if err != nil {
		t.Fatalf("CompensateOrder 报错: %v", err)
	}
	if !gotSince.Equal(since) {
		t.Errorf("扫描下界必须原样透传 since（取错=静默漏数据），want=%v got=%v", since, gotSince)
	}
	if gotTarget != "shop-order_v2" {
		t.Errorf("补偿目标应为新物理索引，got=%q", gotTarget)
	}
	if st.Scanned != 3 || st.Indexed != 1 || st.Deleted != 1 {
		t.Errorf("统计应为 scanned=3 indexed=1 deleted=1，实际 %+v", st)
	}
	if loaderCalls != 1 {
		t.Errorf("每批必须恰好一次 cartLoader 调用（批量 IN，消 N+1），calls=%d", loaderCalls)
	}

	byID := map[string]admin.Doc{}
	for _, d := range allDocs {
		byID[d.ID] = d
	}
	if d, ok := byID["OK001"]; !ok || d.Delete || d.Src == nil {
		t.Errorf("OK001 应为 index（Src 非空、Delete=false），got %+v ok=%v", d, ok)
	}
	if d, ok := byID["OK001"]; ok {
		if di, isOrder := d.Src.(es.OrderIndex); !isOrder || len(di.Names) != 1 || di.Names[0] != "红色T恤" {
			t.Errorf("OK001 装配应聚合出 names=[红色T恤]，got %+v", d.Src)
		}
		if d.Routing != "11" {
			t.Errorf("OK001 routing 应为 strconv.FormatInt(uid,10)=11，got=%q", d.Routing)
		}
	}
	if d, ok := byID["DEL002"]; !ok || !d.Delete || d.Src != nil {
		t.Errorf("DEL002（软删）应为 delete（Src=nil、Delete=true），got %+v ok=%v", d, ok)
	}
	// delete 的 _id / routing 必须与全量同源，否则删不掉历史残留。
	if d, ok := byID["DEL002"]; ok {
		if d.Routing != "22" {
			t.Errorf("DEL002 routing 必须=strconv.FormatInt(uid,10)，got=%q", d.Routing)
		}
	}
	// 空订单号必须跳过。
	for _, d := range allDocs {
		if d.ID == "" {
			t.Error("空订单号绝不能出现在补偿 doc 里（无 _id 无法幂等）")
		}
	}
}
