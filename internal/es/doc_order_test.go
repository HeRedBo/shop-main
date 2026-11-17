package es

// doc_order_test.go 是任务 #38「订单接入」的 dynamic:strict 结构护栏与装配正确性验收，
// 全部不连 ES / MySQL：只读内嵌 orders.jsonc + 内存构造 store_order 行。
//
// 覆盖验收清单（leader 钦定「绝对不许砍」的 6 项里落在本文件的三条）：
//   - #1 OrderIndex json tag 集合 ↔ orders.jsonc properties【双向严格互等】（strict 下单向
//     缺一即整批 400）；copy_to_only_fields 为空 => 两个方向差集都必须为空集。
//   - #2 _id=order_id（字符串订单号，不是数字主键 id）、routing=strconv.FormatInt(uid,10)。
//   - #3 cart_info 正常 / 空 / 畸形三种输入不 panic 且聚合结果正确。
//   - #4 order_status / pay_type_name 复用 order_service 导出函数【同一份实现】（值全等）。
//   - #6 时间序列化为 RFC3339、三个 date mapping 无显式 format。

import (
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"shop/internal/models"
	"shop/internal/service/order_service"
)

// orderExpectedFields 是 OrderIndex 的 13 个 json tag，顺序与 internal/es/doc_order.go
// 的字段声明、以及 orders.jsonc properties 书写顺序逐字一致。比 order-consumer 的 12 字段
// 多一个 pay_type_name（本仓库灌数现算），这是本索引的第 13 个字段。
var orderExpectedFields = []string{
	"order_id", "order_id_suffix", "names", "product_ids", "uid", "pay_time", "pay_type",
	"pay_type_name", "refund_status", "shipping_type", "order_status", "create_time", "update_time",
}

// orderMappingProperties 从内嵌声明取 shop-order 的 mappings.properties 与 _meta。
func orderMappingProperties(t *testing.T) (map[string]any, DefinitionMeta) {
	t.Helper()
	defs, err := LoadDefinitions("")
	if err != nil {
		t.Fatalf("装载索引声明失败: %v", err)
	}
	var order *IndexDefinition
	for _, d := range defs {
		if d.Alias() == "shop-order" {
			order = d
		}
	}
	if order == nil {
		t.Fatal("未装载到 shop-order 定义")
	}
	props, ok := order.MappingsMap()["properties"].(map[string]any)
	if !ok {
		t.Fatal("shop-order mappings.properties 缺失")
	}
	return props, order.Meta()
}

// orderReflectJSONTags 反射取 OrderIndex 的导出字段 json tag，并当场拒绝 omitempty。
func orderReflectJSONTags(t *testing.T) []string {
	t.Helper()
	typ := reflect.TypeOf(OrderIndex{})
	out := make([]string, 0, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.PkgPath != "" {
			continue
		}
		tag := strings.TrimSpace(f.Tag.Get("json"))
		if tag == "" || tag == "-" {
			t.Fatalf("OrderIndex.%s 必须带 json tag（字段名即协议）", f.Name)
		}
		if strings.Contains(tag, "omitempty") {
			t.Fatalf("OrderIndex.%s 不得使用 omitempty（names/product_ids 空也须产出 []）: %q", f.Name, tag)
		}
		out = append(out, strings.Split(tag, ",")[0])
	}
	return out
}

// TestOrderTagsStrictlyEqualMapping 是验收 #1 的正反双向严格互等：
// tags⊆props（多写 key 被 strict 拒）+ props⊆tags（漏写 key 静默漏灌），两个差集都必须空。
// copy_to_only_fields 为空，所以不存在「mapping 有而 struct 无」的合法虚拟字段。
func TestOrderTagsStrictlyEqualMapping(t *testing.T) {
	props, meta := orderMappingProperties(t)
	tags := orderReflectJSONTags(t)

	if len(tags) != 13 {
		t.Fatalf("OrderIndex 字段数期望 13，实际 %d: %v", len(tags), tags)
	}
	if got := append([]string(nil), tags...); !equalStrings(got, orderExpectedFields) {
		t.Fatalf("OrderIndex 的 tag 集合/顺序与协议清单不一致\n got: %v\nwant: %v", got, orderExpectedFields)
	}

	// 正向：每个 struct tag 都必须在 properties 声明，否则 strict 写入被拒。
	for _, tag := range tags {
		if _, ok := props[tag]; !ok {
			t.Fatalf("文档字段 %q 未在 shop-order mappings.properties 声明，写入会被拒为 strict_dynamic_mapping_exception", tag)
		}
	}
	// 反向：每个 properties 字段都必须由 struct 产出；copy_to_only_fields 必须为空。
	if len(meta.CopyToOnlyFields) != 0 {
		t.Fatalf("orders 的 _meta.copy_to_only_fields 必须为空（本索引无虚拟字段），实际 %v", meta.CopyToOnlyFields)
	}
	extra := make([]string, 0)
	for k := range props {
		found := false
		for _, tag := range tags {
			if tag == k {
				found = true
				break
			}
		}
		if !found {
			extra = append(extra, k)
		}
	}
	if len(extra) != 0 {
		t.Fatalf("mapping 声明但文档不产出的字段（无 copy_to 白名单兜底）必须为空集，实际 %v", extra)
	}
	if len(props) != 13 {
		t.Fatalf("shop-order properties 期望 13 个，实际 %d", len(props))
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestOrderZeroValueMarshalKeys 证明零值文档恰为 13 个 key（无字段被 omitempty 吞）。
// 注意：Names/ProductIds 在零值 struct 中是 nil（JSON null），但 BuildOrderDoc 永远保证非 nil，
// 这一契约在 TestBuildOrderDocCartVariants 里单独测。
func TestOrderZeroValueMarshalKeys(t *testing.T) {
	m := marshalToMap(t, OrderIndex{})
	got := keySet(m)
	want := append([]string(nil), orderExpectedFields...)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("零值文档 key 集合与 struct tag 不等\n got: %v\nwant: %v", got, want)
	}
	if len(got) != 13 {
		t.Fatalf("零值文档期望 13 个 key，实际 %d", len(got))
	}
}

// TestOrderNoMarshalJSON 钉死禁止项：不得实现 MarshalJSON（会破坏 time.Time 默认 RFC3339）。
func TestOrderNoMarshalJSON(t *testing.T) {
	if _, ok := reflect.TypeOf(OrderIndex{}).MethodByName("MarshalJSON"); ok {
		t.Fatal("OrderIndex 不得实现 MarshalJSON：会破坏 pay_time/create_time/update_time 的 RFC3339 默认序列化")
	}
}

// TestOrderDocIDAndRouting 是验收 #2：_id=order_id（字符串订单号）、routing=strconv.FormatInt(uid,10)。
func TestOrderDocIDAndRouting(t *testing.T) {
	d := OrderIndex{OrderId: "TN202611170001234", Uid: 42}
	if d.OrderDocID() != "TN202611170001234" {
		t.Errorf("OrderDocID 必须等于字符串订单号，got=%q", d.OrderDocID())
	}
	if d.OrderDocID() == "1234" {
		t.Error("OrderDocID 绝不能取数字主键 id 形态")
	}
	if got, want := d.OrderRouting(), "42"; got != want {
		t.Errorf("OrderRouting 必须=strconv.FormatInt(uid,10)，got=%q want=%q", got, want)
	}
	// 负 uid 也逐字对齐 FormatInt（含前导负号）。
	if (OrderIndex{Uid: -7}).OrderRouting() != "-7" {
		t.Error("OrderRouting 对负 uid 必须保留负号，与 FormatInt 逐字一致")
	}
}

// TestBuildOrderDocIDRoutingThroughBuild 证明装配产物的 _id/routing 走订单号字符串与 uid。
func TestBuildOrderDocIDRoutingThroughBuild(t *testing.T) {
	o := &models.StoreOrder{OrderId: "AB123456789", Uid: 99}
	o.Id = 555 // 数字主键绝不能成为 _id
	doc, ok := BuildOrderDoc(o, nil)
	if !ok {
		t.Fatal("活单应可装配")
	}
	if doc.OrderDocID() != "AB123456789" {
		t.Errorf("_id 应为订单号字符串，got=%q", doc.OrderDocID())
	}
	if doc.OrderRouting() != "99" {
		t.Errorf("routing 应为 uid，got=%q", doc.OrderRouting())
	}
	m := marshalToMap(t, doc)
	if v, ok := m["order_id"].(string); !ok || v != "AB123456789" {
		t.Errorf("文档 order_id 必须是字符串，实际 %T=%v", m["order_id"], m["order_id"])
	}
	// 数字主键 id 不得作为独立字段泄漏进文档（strict 下多 key 即被拒）。
	if _, exists := m["id"]; exists {
		t.Error("文档不得产出 id 字段（_id 走 order_id，数字主键不进 mappings）")
	}
}

// TestBuildOrderDocCartVariants 是验收 #3：cart_info 正常 / 空 / 畸形三种输入不 panic 且聚合正确。
func TestBuildOrderDocCartVariants(t *testing.T) {
	live := func() *models.StoreOrder {
		o := &models.StoreOrder{OrderId: "X1", Uid: 1}
		o.Id = 1
		return o
	}

	// ① 正常：两条合法 cart_info 聚合成等长 names / product_ids。
	normal := []string{
		`{"product_info":{"id":101,"store_name":"红色T恤"}}`,
		`{"product_info":{"id":202,"store_name":"蓝色裤"}}`,
	}
	doc, ok := BuildOrderDoc(live(), normal)
	if !ok {
		t.Fatal("正常输入装配应成功")
	}
	if want := []string{"红色T恤", "蓝色裤"}; !reflect.DeepEqual(doc.Names, want) {
		t.Errorf("names 聚合错误，got=%v want=%v", doc.Names, want)
	}
	if want := []int64{101, 202}; !reflect.DeepEqual(doc.ProductIds, want) {
		t.Errorf("product_ids 聚合错误，got=%v want=%v", doc.ProductIds, want)
	}

	// ② 空：nil / 空串切片 -> names/product_ids 非 nil 空数组，marshal 出 [] 而非 null。
	for _, in := range [][]string{nil, {}, {""}, {"   "}} {
		d2, ok2 := BuildOrderDoc(live(), in)
		if !ok2 {
			t.Fatalf("空 cart_info 不应导致装配失败: %#v", in)
		}
		if d2.Names == nil || d2.ProductIds == nil {
			t.Errorf("空输入下 names/product_ids 必须非 nil，got names=%v ids=%v", d2.Names, d2.ProductIds)
		}
		if len(d2.Names) != 0 || len(d2.ProductIds) != 0 {
			t.Errorf("空输入应聚合出空数组，got names=%v ids=%v", d2.Names, d2.ProductIds)
		}
	}
	emptyDoc, _ := BuildOrderDoc(live(), nil)
	if m := marshalToMap(t, emptyDoc); !isEmptyJSONArray(m["names"]) || !isEmptyJSONArray(m["product_ids"]) {
		t.Errorf("空聚合序列化必须是 [] 而非 null：names=%v product_ids=%v", m["names"], m["product_ids"])
	}

	// ③ 畸形：非法 JSON / 缺 product_info / 混入合法条目 —— 不 panic，只跳过畸形，保留合法。
	mixed := []string{
		`{not-json`,                              // 非法 JSON：跳过
		`{"product_info":{"id":0,"store_name":""}}`, // 无商品信息：跳过
		`{"unrelated":1}`,                          // 无 product_info：跳过
		`{"product_info":{"id":7,"store_name":"有效品"}}`, // 合法：保留
		``,                                         // 空串：跳过
	}
	d3, ok3 := BuildOrderDoc(live(), mixed)
	if !ok3 {
		t.Fatal("畸形输入不应使整单装配失败")
	}
	if !reflect.DeepEqual(d3.Names, []string{"有效品"}) || !reflect.DeepEqual(d3.ProductIds, []int64{7}) {
		t.Errorf("畸形输入应只保留合法条目，got names=%v ids=%v", d3.Names, d3.ProductIds)
	}
}

func isEmptyJSONArray(v any) bool {
	arr, ok := v.([]any)
	return ok && len(arr) == 0
}

// TestBuildOrderDocIneligible 覆盖资格判定：nil / is_del!=0 / 空订单号 -> false（调用方据此 delete）。
func TestBuildOrderDocIneligible(t *testing.T) {
	if _, ok := BuildOrderDoc(nil, nil); ok {
		t.Error("nil 行必须返回 false")
	}
	o := &models.StoreOrder{OrderId: "X", Uid: 1}
	o.Id = 1
	o.IsDel = 1
	if doc, ok := BuildOrderDoc(o, nil); ok {
		t.Error("is_del!=0 必须返回 false")
	} else if !reflect.DeepEqual(doc, OrderIndex{}) {
		t.Error("返回 false 时第一返回值必须是零值")
	}

	o2 := &models.StoreOrder{OrderId: "", Uid: 1}
	o2.Id = 2
	if _, ok := BuildOrderDoc(o2, nil); ok {
		t.Error("空订单号必须返回 false（无法定 _id）")
	}
}

// TestBuildOrderDocSuffix 覆盖 order_id_suffix 边界：短于 4 位留空，>=4 位取末 4 字节。
func TestBuildOrderDocSuffix(t *testing.T) {
	short := &models.StoreOrder{OrderId: "abc", Uid: 1}
	short.Id = 1
	if d, _ := BuildOrderDoc(short, nil); d.OrderIdSuffix != "" {
		t.Errorf("订单号短于 4 位时 suffix 必须为空，got=%q", d.OrderIdSuffix)
	}

	// 恰好 4 位：suffix 即整串。
	exact := &models.StoreOrder{OrderId: "abcd", Uid: 1}
	exact.Id = 2
	if d, _ := BuildOrderDoc(exact, nil); d.OrderIdSuffix != "abcd" {
		t.Errorf("订单号恰 4 位时 suffix 应为整串，got=%q", d.OrderIdSuffix)
	}

	// 长串：取末 4 字节（按字节而非 rune，与 order-consumer 逐字一致）。
	long := &models.StoreOrder{OrderId: "ORDER98765", Uid: 1}
	long.Id = 3
	d, _ := BuildOrderDoc(long, nil)
	if len(d.OrderIdSuffix) != 4 || d.OrderIdSuffix != "8765" {
		t.Errorf("订单号 >=4 位时 suffix 必须为末 4 字节，got=%q", d.OrderIdSuffix)
	}
}

// TestBuildOrderDocReusesOrderServiceFuncs 是验收 #4：order_status / pay_type_name 复用
// order_service 导出的同一份实现（不是第二份拷贝）—— 对同一批输入产出的值必须逐一相等。
func TestBuildOrderDocReusesOrderServiceFuncs(t *testing.T) {
	statusCases := [][3]int{
		{0, 0, 0}, {1, 0, 0}, {1, 1, 0}, {1, 2, 0}, {1, 3, 0}, {1, 0, 1}, {1, 0, 2}, {0, 1, 0},
	}
	for _, c := range statusCases {
		paid, status, refund := c[0], c[1], c[2]
		o := &models.StoreOrder{OrderId: "X", Uid: 1, Paid: paid, Status: status, RefundStatus: refund}
		o.Id = 1
		doc, ok := BuildOrderDoc(o, nil)
		if !ok {
			t.Fatalf("paid=%d status=%d refund=%d 活单应可装配", paid, status, refund)
		}
		want := order_service.OrderStatus(paid, status, refund)
		if doc.OrderStatus != want {
			t.Errorf("order_status 未复用 order_service.OrderStatus：paid=%d status=%d refund=%d got=%d want=%d",
				paid, status, refund, doc.OrderStatus, want)
		}
	}

	payCases := []struct {
		payType string
		paid    int
	}{
		{"weixin", 1}, {"yue", 1}, {"integral", 1}, {"alipay", 1}, {"weixin", 0}, {"", 0}, {"", 1},
	}
	for _, pc := range payCases {
		o := &models.StoreOrder{OrderId: "X", Uid: 1, PayType: pc.payType, Paid: pc.paid}
		o.Id = 1
		doc, ok := BuildOrderDoc(o, nil)
		if !ok {
			t.Fatal("活单应可装配")
		}
		want := order_service.PayTypeName(pc.payType, pc.paid)
		if doc.PayTypeName != want {
			t.Errorf("pay_type_name 未复用 order_service.PayTypeName：pay_type=%q paid=%d got=%q want=%q",
				pc.payType, pc.paid, doc.PayTypeName, want)
		}
	}
}

// TestOrderTimeSerializesAsRFC3339 是验收 #6 第一半：三个时间序列化成 RFC3339，
// 绝非空格分隔形态（会被 ES 默认 date format 拒为 date_parse_exception）。
func TestOrderTimeSerializesAsRFC3339(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	o := &models.StoreOrder{OrderId: "X", Uid: 1}
	o.Id = 1
	o.PayTime = time.Date(2026, 11, 10, 12, 34, 56, 789000000, loc)
	o.CreateTime = time.Date(2026, 11, 10, 1, 2, 3, 0, loc)
	o.UpdateTime = time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)

	doc, ok := BuildOrderDoc(o, nil)
	if !ok {
		t.Fatal("装配失败")
	}
	m := marshalToMap(t, doc)
	spaceForm := "2026-11-10 12:34:56"
	for _, f := range []string{"pay_time", "create_time", "update_time"} {
		s, ok := m[f].(string)
		if !ok {
			t.Fatalf("%s 期望字符串，实际 %T", f, m[f])
		}
		if !strings.Contains(s, "T") {
			t.Fatalf("%s=%q 缺少 'T'，不是 RFC3339", f, s)
		}
		if _, err := time.Parse(time.RFC3339Nano, s); err != nil {
			t.Fatalf("%s=%q 无法按 RFC3339Nano 解析: %v", f, s, err)
		}
		if strings.Contains(s, " ") || s == spaceForm {
			t.Fatalf("%s=%q 是空格分隔形态，会被 ES 拒为 date_parse_exception", f, s)
		}
	}

	// 零值时间也必须写出 RFC3339 形态（0001-01-01T00:00:00Z），不得因空值被省略。
	zero := marshalToMap(t, OrderIndex{})
	for _, f := range []string{"pay_time", "create_time", "update_time"} {
		s, ok := zero[f].(string)
		if !ok {
			t.Fatalf("零值文档 %s 缺失或类型错误: %T", f, zero[f])
		}
		if _, err := time.Parse(time.RFC3339Nano, s); err != nil {
			t.Fatalf("零值文档 %s=%q 不是 RFC3339: %v", f, s, err)
		}
	}
}

// TestOrderMappingDatesHaveNoFormat 是验收 #6 第二半：orders.jsonc 三个 date 字段【无显式 format】，
// 且 mappings.dynamic=strict。任何把 format 改回 yyyy-MM-dd HH:mm:ss 的改动都会让本用例变红。
func TestOrderMappingDatesHaveNoFormat(t *testing.T) {
	props, _ := orderMappingProperties(t)
	for _, f := range []string{"pay_time", "create_time", "update_time"} {
		fieldMap, ok := props[f].(map[string]any)
		if !ok {
			t.Fatalf("%s mapping 缺失或非对象", f)
		}
		if typ, _ := fieldMap["type"].(string); typ != "date" {
			t.Fatalf("%s 必须是 date，实际 %q", f, typ)
		}
		if _, exists := fieldMap["format"]; exists {
			t.Fatalf("%s 不得声明 format（阶段二定案 A：走 ES 默认 strict_date_optional_time||epoch_millis）", f)
		}
	}

	// dynamic 必须仍是 strict（结构护栏的前提）。
	defs, err := LoadDefinitions("")
	if err != nil {
		t.Fatalf("装载失败: %v", err)
	}
	for _, d := range defs {
		if d.Alias() == "shop-order" {
			if got, _ := d.MappingsMap()["dynamic"].(string); got != "strict" {
				t.Fatalf("shop-order mappings.dynamic 必须是 strict，实际 %q", got)
			}
		}
	}
}

// TestOrderMappingDatesHaveNoFormat 完成。
