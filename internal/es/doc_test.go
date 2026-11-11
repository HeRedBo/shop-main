package es

// 【本任务最重要的验收：dynamic: strict 结构护栏】
//
// strict 模式下文档 struct 与 mapping 字段集合必须严格互等：多一个 key = 整条 bulk item
// 被 ES 拒为 strict_dynamic_mapping_exception；少一个 key 而未登记 copy_to_only_fields
// = 静默漏灌数据。这两类错误都只会在灌数阶段以海量 400 暴露，所以必须在单测里拦下。
//
// 全部用例不依赖 ES / MySQL，只读内嵌声明文件 + 内存构造模型行。

import (
	"encoding/json"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"shop/internal/models"
)

// expectedDocFields 是 ProductIndex 的 17 个 json tag，顺序与
// product-consumer/internal/consumer/product_index.go 逐字对齐（三处重复定义的第 3 处）。
var expectedDocFields = []string{
	"id", "store_name", "store_info", "keyword", "cate_id", "price", "sales", "ficti",
	"is_hot", "is_benefit", "is_best", "is_new", "description", "is_postage", "is_good",
	"create_time", "update_time",
}

func productMappingProperties(t *testing.T) (map[string]any, DefinitionMeta) {
	t.Helper()

	defs, err := LoadDefinitions("")
	if err != nil {
		t.Fatalf("装载索引声明失败: %v", err)
	}
	var product *IndexDefinition
	for _, d := range defs {
		if d.Alias() == "shop-product" {
			product = d
		}
	}
	if product == nil {
		t.Fatal("未装载到 shop-product 定义")
	}
	props, ok := product.MappingsMap()["properties"].(map[string]any)
	if !ok {
		t.Fatal("mappings.properties 缺失")
	}
	return props, product.Meta()
}

func reflectJSONTags(t *testing.T) []string {
	t.Helper()

	typ := reflect.TypeOf(ProductIndex{})
	out := make([]string, 0, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.PkgPath != "" { // 非导出字段不会进 JSON
			continue
		}
		tag := strings.TrimSpace(f.Tag.Get("json"))
		if tag == "" || tag == "-" {
			t.Fatalf("ProductIndex.%s 必须带 json tag（字段名即协议）", f.Name)
		}
		name := strings.Split(tag, ",")[0]
		if name == "" {
			t.Fatalf("ProductIndex.%s 的 json tag 非法: %q", f.Name, tag)
		}
		if strings.Contains(tag, "omitempty") {
			// price/sales/ficti/is_* 参与 index.sort 与 term 查询，零值必须显式写入。
			t.Fatalf("ProductIndex.%s 不得使用 omitempty: %q", f.Name, tag)
		}
		out = append(out, name)
	}
	return out
}

func marshalToMap(t *testing.T, v any) map[string]any {
	t.Helper()

	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("反序列化失败: %v\n原始输出: %s", err, b)
	}
	return m
}

func keySet(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestDocTagsAreSubsetOfMapping 护栏 1：struct 的每个 json tag 都必须在 mapping 里声明。
// 多出一个即为硬失败（写入必被 strict 拒绝）。
func TestDocTagsAreSubsetOfMapping(t *testing.T) {
	props, _ := productMappingProperties(t)
	tags := reflectJSONTags(t)

	for _, tag := range tags {
		if _, ok := props[tag]; !ok {
			t.Fatalf("文档字段 %q 未在 mappings.properties 声明，写入会被拒为 strict_dynamic_mapping_exception；"+
				"必须先在 product.jsonc 追加字段并 es:migrate 生效", tag)
		}
	}

	if len(tags) != len(expectedDocFields) {
		t.Fatalf("ProductIndex 字段数期望 %d，实际 %d: %v", len(expectedDocFields), len(tags), tags)
	}
	if !reflect.DeepEqual(tags, expectedDocFields) {
		t.Fatalf("ProductIndex 的 tag 集合/顺序与 product-consumer 不一致\n got: %v\nwant: %v", tags, expectedDocFields)
	}

	// 反向差集护栏 2：mapping 有而 struct 没有的字段，必须恰等于 copy_to_only_fields
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
	sort.Strings(extra)

	whitelist := append([]string(nil), productMetaCopyToOnly(t)...)
	sort.Strings(whitelist)
	if !reflect.DeepEqual(extra, whitelist) {
		t.Fatalf("mapping 声明但文档不产出的字段集期望 %v（_meta.copy_to_only_fields），实际 %v；"+
			"新增真实业务字段必须同步 ProductIndex 三处定义", whitelist, extra)
	}
	if len(extra) != 1 || extra[0] != "desc" {
		t.Fatalf("本期反向差集必须恰为 {desc}，实际 %v", extra)
	}
}

func productMetaCopyToOnly(t *testing.T) []string {
	t.Helper()
	_, meta := productMappingProperties(t)
	return meta.CopyToOnlyFields
}

// TestZeroValueMarshalKeys 护栏 3：零值文档的 key 集合 == 17 个 tag，无遗漏无多余
// （直接证明没有任何字段被 omitempty 吞掉）。
func TestZeroValueMarshalKeys(t *testing.T) {
	props, _ := productMappingProperties(t)

	m := marshalToMap(t, ProductIndex{})
	got := keySet(m)
	want := append([]string(nil), expectedDocFields...)
	sort.Strings(want)

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("零值文档 key 集合与 struct tag 不等\n got: %v\nwant: %v", got, want)
	}
	if len(got) != 17 {
		t.Fatalf("零值文档期望 17 个 key，实际 %d", len(got))
	}

	// 护栏 4：零值文档不得含 desc
	if _, ok := m["desc"]; ok {
		t.Fatal("文档不得产出 desc：它是 copy_to 汇聚的虚拟字段，写入会污染倒排")
	}

	// mapping 侧多出的确实只有 desc
	if len(props) != len(got)+1 {
		t.Fatalf("mapping properties 应为 %d 个（17 文档字段 + desc），实际 %d", len(got)+1, len(props))
	}
	if _, ok := props["desc"]; !ok {
		t.Fatal("mapping 必须声明 desc")
	}
}

// TestIndexFlagFieldsMarshalAsNumbers 护栏 5：六个 is_* 输出必须是数字而非 bool
// （mapping 是 keyword，Go 侧 int8 会被 ES 强转成 "0"/"1"；改成 bool 会变成
// "true"/"false"，存量 term 查询全部不命中）。
func TestIndexFlagFieldsMarshalAsNumbers(t *testing.T) {
	props, _ := productMappingProperties(t)

	one := int8(1)
	p := &models.StoreProduct{
		IsShow: &one, IsHot: &one, IsBenefit: &one, IsBest: &one, IsNew: &one, IsPostage: &one, IsGood: &one,
	}
	p.Id = 7
	doc, ok := BuildIndexDoc(p)
	if !ok {
		t.Fatal("上架商品应可装配")
	}

	m := marshalToMap(t, doc)
	fields := []string{"is_hot", "is_benefit", "is_best", "is_new", "is_postage", "is_good"}
	for _, f := range fields {
		v, exists := m[f]
		if !exists {
			t.Fatalf("%s 未出现在序列化结果里（疑似被 omitempty 吞掉）", f)
		}
		if _, isBool := v.(bool); isBool {
			t.Fatalf("%s 序列化成 bool，ES 会写 \"true\"/\"false\"，存量 term \"1\" 查询全不命中", f)
		}
		n, isNum := v.(float64)
		if !isNum {
			t.Fatalf("%s 期望 JSON 数字，实际 %T=%v", f, v, v)
		}
		if n != 1 {
			t.Fatalf("%s 期望 1，实际 %v", f, n)
		}
		if typ, _ := props[f].(map[string]any)["type"].(string); typ != "keyword" {
			t.Fatalf("%s 在 mapping 里必须是 keyword，实际 %q", f, typ)
		}
	}

	// 零值同样必须写出 0（index.sort 与 term "0" 查询都依赖它）
	zero := marshalToMap(t, ProductIndex{})
	for _, f := range fields {
		if v, ok := zero[f].(float64); !ok || v != 0 {
			t.Fatalf("零值文档 %s 应为数字 0，实际 %T=%v", f, zero[f], zero[f])
		}
	}

	// price/sales/ficti 的 Go 类型口径必须与 Phase 0 校对结论一致
	typ := reflect.TypeOf(ProductIndex{})
	assertFieldKind(t, typ, "CateId", reflect.Int, "cate_id 是 int（不是 int64），与 consumer 逐字段一致")
	assertFieldKind(t, typ, "Sales", reflect.Int32, "")
	assertFieldKind(t, typ, "Ficti", reflect.Int32, "")
	assertFieldKind(t, typ, "Price", reflect.Float64, "")
	assertFieldKind(t, typ, "IsHot", reflect.Int8, "is_* 必须是 int8，不得改 bool")
	for _, f := range []string{"CreateTime", "UpdateTime"} {
		assertFieldKind(t, typ, f, reflect.Struct, "时间字段必须是 time.Time，靠 stdlib 默认序列化")
	}
	if _, ok := typ.MethodByName("MarshalJSON"); ok {
		t.Fatal("ProductIndex 不得实现 MarshalJSON：会破坏 time.Time 默认 RFC3339 语义")
	}
}

func assertFieldKind(t *testing.T, typ reflect.Type, name string, want reflect.Kind, why string) {
	t.Helper()
	f, ok := typ.FieldByName(name)
	if !ok {
		t.Fatalf("ProductIndex 缺少字段 %s", name)
	}
	if f.Type.Kind() != want {
		msg := f.Name + " 类型期望 " + want.String() + "，实际 " + f.Type.String()
		if why != "" {
			msg += "（" + why + "）"
		}
		t.Error(msg)
	}
}

// TestTimeSerializesAsRFC3339 护栏 7：时间产物必须能被 ES 默认 date format 接受，
// 且绝不能是 "2006-01-02 15:04:05" 这种空格分隔形态（会被拒为 date_parse_exception）。
func TestTimeSerializesAsRFC3339(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	created := time.Date(2026, 11, 10, 12, 34, 56, 789000000, loc)
	updated := time.Date(2026, 11, 10, 13, 0, 0, 0, time.UTC)

	// 时间列在嵌入的 BaseModel 里，不能用 StoreProduct 复合字面量直接赋值（Go 只允许直接字段），
	// 必须走提升字段赋值——这与 BuildIndexDoc 内部读 p.CreateTime 的写法一致。
	one := int8(1)
	p := &models.StoreProduct{IsShow: &one}
	p.Id = 1
	p.CreateTime = created
	p.UpdateTime = updated
	doc, ok := BuildIndexDoc(p)
	if !ok {
		t.Fatal("装配失败")
	}

	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	raw := string(b)
	m := marshalToMap(t, doc)

	spaceForm := regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}$`)
	for _, f := range []string{"create_time", "update_time"} {
		s, ok := m[f].(string)
		if !ok {
			t.Fatalf("%s 期望字符串，实际 %T", f, m[f])
		}
		if !strings.Contains(s, "T") {
			t.Fatalf("%s=%q 缺少 'T'，不是 RFC3339，ES 默认 date format 会拒", f, s)
		}
		if spaceForm.MatchString(s) {
			t.Fatalf("%s=%q 是空格分隔形态，会被 ES 拒为 date_parse_exception", f, s)
		}
		if _, err := time.Parse(time.RFC3339Nano, s); err != nil {
			t.Fatalf("%s=%q 无法按 RFC3339Nano 解析: %v", f, s, err)
		}
		if !strings.Contains(s, "+08:00") && f == "create_time" {
			t.Fatalf("create_time=%q 丢失时区偏移，ES 会按 UTC 解释", s)
		}
	}
	if strings.Contains(raw, " 12:34:56") {
		t.Fatalf("序列化结果含空格分隔时间: %s", raw)
	}

	// 零值时间同样必须是 RFC3339 形态（0001-01-01T00:00:00Z），不能因空值被省略
	zero := marshalToMap(t, ProductIndex{})
	for _, f := range []string{"create_time", "update_time"} {
		s, ok := zero[f].(string)
		if !ok {
			t.Fatalf("零值文档 %s 缺失或类型错误: %T", f, zero[f])
		}
		if _, err := time.Parse(time.RFC3339Nano, s); err != nil {
			t.Fatalf("零值文档 %s=%q 不是 RFC3339: %v", f, s, err)
		}
	}
}

// TestBuildIndexDocFilter 下架与已删除行必须返回 false（与 consumer「只存上架」约定一致），
// 该条件同时也是 rebuild 对账期望值的 SQL 条件。
func TestBuildIndexDocFilter(t *testing.T) {
	zero, one := int8(0), int8(1)

	base := func() *models.StoreProduct {
		p := &models.StoreProduct{IsShow: &one, StoreName: "商品"}
		p.Id = 100
		return p
	}

	if _, ok := BuildIndexDoc(nil); ok {
		t.Error("nil 行必须返回 false")
	}

	p := base()
	p.IsShow = &zero
	if _, ok := BuildIndexDoc(p); ok {
		t.Error("is_show=0 必须返回 false（下架商品不入索引）")
	}

	if _, ok := BuildIndexDoc(base()); !ok {
		t.Error("is_show=1 且未删除必须返回 true")
	}

	p = base()
	p.IsDel = 1
	if _, ok := BuildIndexDoc(p); ok {
		t.Error("is_del!=0 必须返回 false（调用方据此走 bulk delete）")
	}

	// IsShow 为 nil（列裁剪没查该列）按「不上架」处理，宁可不写
	p = base()
	p.IsShow = nil
	doc, ok := BuildIndexDoc(p)
	if ok {
		t.Error("is_show 指针为 nil 时必须返回 false")
	}
	if doc != (ProductIndex{}) {
		t.Error("返回 false 时第一返回值必须是零值")
	}
}

// TestBuildIndexDocFieldMapping 正常行的字段逐项对齐，含 int -> int32 收窄与 nil 指针兜底。
func TestBuildIndexDocFieldMapping(t *testing.T) {
	one, zero := int8(1), int8(0)
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	updated := time.Date(2026, 1, 3, 4, 5, 6, 0, time.UTC)

	p := &models.StoreProduct{
		StoreName:   "红色T恤",
		StoreInfo:   "短袖 纯棉",
		Keyword:     "夏装,男装",
		CateId:      42,
		Price:       99.5,
		Sales:       1234,
		Ficti:       5678,
		IsHot:       &one,
		IsBenefit:   &zero,
		IsBest:      &one,
		IsNew:       nil, // 列裁剪未取，按 0
		IsShow:      &one,
		Description: "<p>纯棉面料</p>",
		IsPostage:   &one,
		IsGood:      &zero,
		// 与索引无关的列必须被忽略
		Image:    "http://x/a.png",
		VipPrice: 88,
		Browse:   999,
		SpecType: 1,
	}
	p.Id = 12345
	p.CreateTime = created
	p.UpdateTime = updated

	doc, ok := BuildIndexDoc(p)
	if !ok {
		t.Fatal("期望可装配")
	}

	want := ProductIndex{
		Id:          12345,
		StoreName:   "红色T恤",
		StoreInfo:   "短袖 纯棉",
		Keyword:     "夏装,男装",
		CateId:      42,
		Price:       99.5,
		Sales:       1234,
		Ficti:       5678,
		IsHot:       1,
		IsBenefit:   0,
		IsBest:      1,
		IsNew:       0,
		Description: "纯棉面料",
		IsPostage:   1,
		IsGood:      0,
		CreateTime:  created,
		UpdateTime:  updated,
	}
	if doc != want {
		t.Fatalf("装配结果不符\n got: %+v\nwant: %+v", doc, want)
	}

	// _id 与 routing 必须与 product-consumer 逐字一致，否则同一商品落两个 shard
	if doc.BulkDocID() != "12345" {
		t.Errorf("BulkDocID=%q", doc.BulkDocID())
	}
	if doc.BulkRouting() != "42" {
		t.Errorf("BulkRouting=%q（必须与 cate_id 同形）", doc.BulkRouting())
	}

	m := marshalToMap(t, doc)
	if _, exists := m["desc"]; exists {
		t.Fatal("装配产物不得含 desc")
	}
	if _, exists := m["vip_price"]; exists {
		t.Fatal("GORM 模型字段不得平铺进文档（必须走 BuildIndexDoc，不能用 *StoreProduct 直接写）")
	}
	if len(m) != 17 {
		t.Errorf("装配产物 key 数期望 17，实际 %d: %v", len(m), keySet(m))
	}
}

// TestBuildDescriptionHTMLAndRuneLimit 覆盖描述清洗：去标签、实体、空白收敛、按 rune 截断。
func TestBuildDescriptionHTMLAndRuneLimit(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		// 去标签不插入额外空格（与 PHP strip_tags 行为一致），只依赖原有空白做收敛
		{"去块级标签", "<p>纯棉面料</p><br/>宽松版型", "纯棉面料宽松版型"},
		{"标签属性内的尖括号", `<a title="x>y">link</a>`, "link"},
		{"注释文本里的尖括号被吞", "前<!--隐藏的注-->后", "前后"},
		{"实体解码", "A&nbsp;&amp;&nbsp;B", "A & B"},
		{"普通小于号保留", "价格<100 且 >50", "价格<100 且 >50"},
		{"无标签快速路径", "纯文本描述", "纯文本描述"},
		{"空白收敛与首尾裁剪", "  <div>a</div>\n\t <div>b</div>  ", "a b"},
		{"空串", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := buildDescription(tc.in); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}

	// 中文按 rune 截断：2500 个汉字必须留 2000 个汉字（6000 字节），
	// 若按字节切会截出半个 UTF-8 序列，写进 ES 就是非法字符串。
	long := strings.Repeat("文", 2500)
	got := buildDescription(long)
	if n := utf8.RuneCountInString(got); n != MaxDescriptionRunes {
		t.Fatalf("rune 截断期望 %d，实际 %d", MaxDescriptionRunes, n)
	}
	if len(got) != MaxDescriptionRunes*3 {
		t.Fatalf("截断应按 rune 而非字节，字节数=%d", len(got))
	}
	if got != strings.Repeat("文", MaxDescriptionRunes) {
		t.Error("截断结果内容不符")
	}
	if utf8.ValidString(got) {
		// 正常路径：合法 UTF-8
	} else {
		t.Fatal("截断结果不是合法 UTF-8")
	}

	// 恰好等于上限不截断
	exact := strings.Repeat("字", MaxDescriptionRunes)
	if buildDescription(exact) != exact {
		t.Error("等于上限的描述不应被截断")
	}

	// 截断必须发生在装配链路上（BuildIndexDoc 内部调用）
	one := int8(1)
	p := &models.StoreProduct{IsShow: &one, Description: "<p>" + strings.Repeat("内", 3000) + "</p>"}
	p.Id = 1
	doc, ok := BuildIndexDoc(p)
	if !ok {
		t.Fatal("装配失败")
	}
	if n := utf8.RuneCountInString(doc.Description); n != MaxDescriptionRunes {
		t.Fatalf("BuildIndexDoc 未截断 description，rune=%d", n)
	}
	if strings.Contains(doc.Description, "<") {
		t.Error("BuildIndexDoc 未去除 HTML 标签")
	}
}

// TestTruncateRunesAndCollapse 覆盖两个清洗原语的边界。
func TestTruncateRunesAndCollapse(t *testing.T) {
	if truncateRunes("abc", 0) != "" {
		t.Error("max<=0 应返回空串")
	}
	if truncateRunes("中文abc", 3) != "中文a" {
		t.Errorf("按 rune 截断错误: %q", truncateRunes("中文abc", 3))
	}
	if truncateRunes("ab", 10) != "ab" {
		t.Error("短于上限应原样返回")
	}
	// 用字节数组书写，避免转义符与空格相邻时的书写歧义
	in := []byte{'a', ' ', ' ', 'b', '\t', '\n', '\r', ' ', 'c', ' '}
	if got := collapseSpace(string(in)); got != "a b c" {
		t.Errorf("空白收敛错误: %q", got)
	}
	if collapseSpace("中文  保留") != "中文 保留" {
		t.Error("中文字符不应被误判为空白")
	}
}

// TestBuildIndexDocNoExtraKeysOnFullRow 用一条「所有列都填上」的模型行验证：
// 无论入参多出多少字段（ProductMsg 会把 30+ 列平铺进 Kafka），产出恒为 17 key。
func TestBuildIndexDocNoExtraKeysOnFullRow(t *testing.T) {
	one := int8(1)
	p := &models.StoreProduct{
		Image: "i", SliderImage: "s", StoreName: "n", StoreInfo: "inf", Keyword: "k",
		CateId: 3, Price: 1, VipPrice: 1, OtPrice: 1, Postage: 1, UnitName: "件",
		Sort: 1, Sales: 1, Stock: 1, IsShow: &one, IsHot: &one, IsBenefit: &one,
		IsBest: &one, IsNew: &one, Description: "d", IsPostage: &one, GiveIntegral: 1,
		Cost: 1, IsGood: &one, Ficti: 1, Browse: 1, IsSub: &one, TempId: 1,
		SpecType: 1, IsIntegral: &one, Integral: 1,
	}
	p.Id = 9

	m := marshalToMap(t, mustBuild(t, p))
	if len(m) != 17 {
		t.Fatalf("期望 17 个 key，实际 %d: %v", len(m), keySet(m))
	}
	for _, noise := range []string{"image", "slider_image", "stock", "cost", "browse", "unit_name", "vip_price", "is_del", "is_show", "operation"} {
		if _, exists := m[noise]; exists {
			t.Fatalf("GORM 列 %q 泄漏进索引文档，strict 下整条 bulk item 会被拒", noise)
		}
	}
}

func mustBuild(t *testing.T, p *models.StoreProduct) ProductIndex {
	t.Helper()
	doc, ok := BuildIndexDoc(p)
	if !ok {
		t.Fatal("期望可装配")
	}
	return doc
}
