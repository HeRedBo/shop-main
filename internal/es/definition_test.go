package es

// 定义层验收：两份事实源都能解析、指纹幂等且只覆盖结构本体、_meta 读取正确、
// BodyJSON 不带 _meta/aliases、装载来源优先级与校验错误信息。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadDefs(t *testing.T) []*IndexDefinition {
	t.Helper()

	defs, err := LoadDefinitions("")
	if err != nil {
		t.Fatalf("LoadDefinitions 失败: %v", err)
	}
	if len(defs) != 2 {
		t.Fatalf("期望装载到 2 份索引声明（product/orders），实际 %d 份: %v", len(defs), sourcesOf(defs))
	}
	return defs
}

func defByAlias(t *testing.T, defs []*IndexDefinition, alias string) *IndexDefinition {
	t.Helper()
	for _, d := range defs {
		if d.Alias() == alias {
			return d
		}
	}
	t.Fatalf("未装载到别名 %s，现有: %v", alias, aliasesOf(defs))
	return nil
}

func aliasesOf(defs []*IndexDefinition) []string {
	out := make([]string, 0, len(defs))
	for _, d := range defs {
		out = append(out, d.Alias())
	}
	return out
}

func sourcesOf(defs []*IndexDefinition) []string {
	out := make([]string, 0, len(defs))
	for _, d := range defs {
		out = append(out, d.Source())
	}
	return out
}

// TestDefinitionsLoadBothFiles 两份 jsonc 都必须解析成功，且物理索引名/版本/接线状态正确。
func TestDefinitionsLoadBothFiles(t *testing.T) {
	defs := loadDefs(t)
	product := defByAlias(t, defs, "shop-product")
	if product.Version() != 1 || product.Name() != "shop-product_v1" {
		t.Errorf("product 命名错误: version=%d name=%s", product.Version(), product.Name())
	}
	if !product.Executable() || product.Status() != StatusActive {
		t.Errorf("product 必须是 active 并进入可执行清单，实际 status=%s", product.Status())
	}
	if product.Source() != "indices/product.jsonc" || !product.Embedded() {
		t.Errorf("product 来源记录错误: source=%s embedded=%v", product.Source(), product.Embedded())
	}
	if len(product.Fingerprint()) != 64 {
		t.Errorf("product 指纹不是 sha256 hex: %q", product.Fingerprint())
	}

	orders := defByAlias(t, defs, "shop-order")
	if orders.Name() != "shop-order_v1" {
		t.Errorf("orders 命名错误: %s", orders.Name())
	}
	if orders.Executable() {
		t.Error("orders 本期是 draft，不得进入可执行清单")
	}

	// Registry 的可执行清单是 migrate/rebuild 的唯一入口
	r := NewRegistry()
	if err := r.Register(defs...); err != nil {
		t.Fatalf("Register 失败: %v", err)
	}
	executable := r.ExecutableDefinitions()
	if len(executable) != 1 || executable[0].Alias() != "shop-product" {
		t.Errorf("ExecutableDefinitions 应只含 shop-product，实际: %v", aliasesOf(executable))
	}
	if len(r.All()) != 2 {
		t.Errorf("All 应返回 2 份定义")
	}

	// 文件名按字典序，保证多索引 migrate 顺序可复现
	if defs[0].Alias() != "shop-order" {
		t.Errorf("LoadDefinitions 未按文件名字典序返回: %v", sourcesOf(defs))
	}
}

// TestDefinitionFingerprintIdempotent 指纹必须与 map 遍历顺序无关（同内容两次相等），
// 且对任一字段值敏感；_meta 单独变化不影响指纹（Phase 5「仅版本号变更」判定依赖此语义）。
func TestDefinitionFingerprintIdempotent(t *testing.T) {
	first := defByAlias(t, loadDefs(t), "shop-product")
	second := defByAlias(t, loadDefs(t), "shop-product")

	if first.Fingerprint() != second.Fingerprint() {
		t.Fatalf("同一份声明两次装载指纹不等: %s != %s", first.Fingerprint(), second.Fingerprint())
	}

	// 不同插入顺序 + 不同嵌套构造顺序，内容相同则指纹必须相同
	s1 := map[string]any{
		"number_of_shards": json.Number("15"),
		"index":            map[string]any{"sort.field": []any{"price"}, "refresh_interval": "1s"},
		"analysis":         map[string]any{"analyzer": map[string]any{"a": map[string]any{"type": "custom"}}},
	}
	m1 := map[string]any{"dynamic": "strict", "properties": map[string]any{"id": map[string]any{"type": "keyword"}}}

	s2 := map[string]any{}
	m2 := map[string]any{}
	for k, v := range s1 {
		s2[k] = v // 遍历顺序随机，插入顺序与 s1 不同
	}
	for k, v := range m1 {
		m2[k] = v
	}
	if FingerprintOf(s1, m1) != FingerprintOf(s2, m2) {
		t.Fatal("指纹受 map 遍历顺序影响，规范化失败")
	}

	// 改一个字段值必须导致指纹不等（逐字段敏感性）
	drifted := deepCopyMap(s1)
	drifted["number_of_shards"] = json.Number("16")
	if FingerprintOf(drifted, m1) == FingerprintOf(s1, m1) {
		t.Fatal("number_of_shards 变更未改变指纹")
	}

	driftedMapping := deepCopyMap(m1)
	props := driftedMapping["properties"].(map[string]any)
	props["id"] = map[string]any{"type": "long"}
	if FingerprintOf(s1, driftedMapping) == FingerprintOf(s1, m1) {
		t.Fatal("字段类型变更未改变指纹")
	}

	// 多一个字段同样要变（strict 模式下新增字段属于结构变更）
	extra := deepCopyMap(m1)
	extra["properties"].(map[string]any)["skus"] = map[string]any{"type": "nested"}
	if FingerprintOf(s1, extra) == FingerprintOf(s1, m1) {
		t.Fatal("新增字段未改变指纹")
	}

	// _meta 变化不影响指纹
	defBody := deepCopyMap(first.BodyMap())
	defBody[MetaKey].(map[string]any)[MetaKeySchemaVersion] = json.Number("2")
	metaOnly, err := NewIndexDefinition("test/_meta_bump", defBody, false)
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if metaOnly.Fingerprint() != first.Fingerprint() {
		t.Fatal("_meta 单独变化不应改变指纹")
	}
	if metaOnly.Name() == first.Name() {
		t.Fatal("schema_version 变化后物理索引名应不同")
	}
}

// TestDefinitionCanonicalJSON 规范化输出的字节序列必须固定（key 字典序 + 无空白）。
func TestDefinitionCanonicalJSON(t *testing.T) {
	got := string(CanonicalJSON(map[string]any{
		"b": []any{json.Number("2"), map[string]any{"z": true, "a": nil}},
		"a": json.Number("15"),
	}))
	want := `{"a":15,"b":[2,{"a":null,"z":true}]}`
	if got != want {
		t.Fatalf("规范化输出错误\n got: %s\nwant: %s", got, want)
	}
}

// TestDefinitionMetaFields 校验 _meta 约定 key 的读取。
func TestDefinitionMetaFields(t *testing.T) {
	defs := loadDefs(t)

	product := defByAlias(t, defs, "shop-product")
	pm := product.Meta()
	if pm.Alias != "shop-product" || pm.SchemaVersion != 1 {
		t.Fatalf("product _meta 基本字段错误: %+v", pm)
	}
	if pm.NextVersionRule != "<alias>_v<N+1>" {
		t.Errorf("next_version_rule 读取错误: %q", pm.NextVersionRule)
	}
	if len(pm.CopyToOnlyFields) != 1 || pm.CopyToOnlyFields[0] != "desc" {
		t.Errorf("copy_to_only_fields 期望 [desc]，实际: %v", pm.CopyToOnlyFields)
	}
	if pm.DateFormat != "" {
		t.Errorf("products 无显式 date format（必须走 RFC3339），实际读到: %q", pm.DateFormat)
	}
	if len(product.RequiresPlugins()) != 2 {
		t.Errorf("requires_plugins 读取错误: %v", product.RequiresPlugins())
	}
	if pm.Notes == "" {
		t.Error("notes 必须写明事实源纪律与三处 struct 同步约束")
	}

	orders := defByAlias(t, defs, "shop-order")
	if len(orders.Meta().CopyToOnlyFields) != 0 {
		t.Errorf("orders copy_to_only_fields 期望空，实际: %v", orders.Meta().CopyToOnlyFields)
	}
	if orders.Status() != StatusDraft {
		t.Errorf("orders status 期望 draft，实际: %s", orders.Status())
	}
	if orders.Meta().Notes == "" {
		t.Error("orders notes 必须写明本期不接线与日期格式互不兼容的发现")
	}

	// 命名与下一代推导
	if IndexName("shop-product", 3) != "shop-product_v3" {
		t.Errorf("IndexName 错误")
	}
	if NextVersionName("shop-product", 3) != "shop-product_v4" {
		t.Errorf("NextVersionName 错误")
	}
}

// TestDefinitionBodyJSON 建索引请求体不得含 _meta / aliases，且必须保留结构本体。
func TestDefinitionBodyJSON(t *testing.T) {
	for _, d := range loadDefs(t) {
		body, err := d.BodyJSON()
		if err != nil {
			t.Fatalf("%s BodyJSON 失败: %v", d.Source(), err)
		}
		if strings.Contains(body, MetaKey) {
			t.Fatalf("%s BodyJSON 含 _meta，ES 会以 unknown key 拒绝: %s", d.Source(), body)
		}
		if strings.Contains(body, AliasesKey) {
			t.Fatalf("%s BodyJSON 不应含 aliases（由调用方按场景注入）", d.Source())
		}

		var parsed map[string]any
		if err := json.Unmarshal([]byte(body), &parsed); err != nil {
			t.Fatalf("%s BodyJSON 不是合法 JSON: %v", d.Source(), err)
		}
		if len(parsed) != 2 {
			t.Fatalf("%s BodyJSON 顶层应只有 settings+mappings，实际 key: %v", d.Source(), keysOf(parsed))
		}
		for _, k := range []string{"settings", "mappings"} {
			if _, ok := parsed[k]; !ok {
				t.Fatalf("%s BodyJSON 缺少 %s", d.Source(), k)
			}
		}

		settings, err := d.SettingsJSON()
		if err != nil {
			t.Fatalf("%s SettingsJSON 失败: %v", d.Source(), err)
		}
		mappings, err := d.MappingsJSON()
		if err != nil {
			t.Fatalf("%s MappingsJSON 失败: %v", d.Source(), err)
		}
		if !strings.Contains(settings, "analysis") || !strings.Contains(mappings, "dynamic") {
			t.Fatalf("%s settings/mappings 内容不完整", d.Source())
		}
	}
}

// TestProductDeclaredValues 关键数值必须与线上一致（防止注释化改动时误改取值）。
func TestProductDeclaredValues(t *testing.T) {
	d := defByAlias(t, loadDefs(t), "shop-product")

	settings := d.SettingsMap()
	assertNumber(t, settings, "number_of_shards", "15")
	assertNumber(t, settings, "number_of_replicas", "1")
	if v, _ := settings["refresh_interval"].(string); v != "1s" {
		t.Errorf("refresh_interval 期望 \"1s\"，实际 %v", settings["refresh_interval"])
	}

	idx, _ := settings["index"].(map[string]any)
	if idx == nil {
		t.Fatal("settings.index 段缺失")
	}
	assertStringSlice(t, idx, "sort.field", []string{"price", "sales", "ficti"})
	assertStringSlice(t, idx, "sort.order", []string{"asc", "desc", "desc"})
	assertStringSlice(t, idx, "store.preload", []string{"nvd", "dvd"})

	props := d.MappingsMap()["properties"].(map[string]any)
	if len(props) != 18 {
		t.Errorf("mappings.properties 期望 18 个 key（17 字段 + desc），实际 %d: %v", len(props), keysOf(props))
	}
	if dyn, _ := d.MappingsMap()["dynamic"].(string); dyn != "strict" {
		t.Errorf("dynamic 必须是 strict，实际 %q", dyn)
	}
	for _, f := range []string{"is_hot", "is_benefit", "is_best", "is_new", "is_postage", "is_good"} {
		assertFieldType(t, props, f, "keyword")
	}
	assertFieldType(t, props, "price", "double")
	assertFieldType(t, props, "sales", "long")
	assertFieldType(t, props, "ficti", "long")
	assertFieldType(t, props, "cate_id", "keyword")
	for _, f := range []string{"create_time", "update_time"} {
		p := props[f].(map[string]any)
		if typ, _ := p["type"].(string); typ != "date" {
			t.Errorf("%s 期望 date，实际 %v", f, typ)
		}
		if _, exists := p["format"]; exists {
			t.Errorf("%s 不应声明 format（products 走 ES 默认 => 文档必须 RFC3339）", f)
		}
	}
	// desc 只由 copy_to 汇聚，三个来源字段必须都指向它
	for _, f := range []string{"store_info", "description", "keyword"} {
		p := props[f].(map[string]any)
		if to, _ := p["copy_to"].(string); to != "desc" {
			t.Errorf("%s 的 copy_to 期望 desc，实际 %v", f, p["copy_to"])
		}
	}
	if _, ok := props["desc"]; !ok {
		t.Error("properties 必须声明 desc")
	}
}

// TestLoadDefinitionsSourcePriority --indices-dir > 配置 elasticsearch.indices-dir > go:embed。
func TestLoadDefinitionsSourcePriority(t *testing.T) {
	dir := t.TempDir()
	candidate := map[string]any{
		"_meta":    map[string]any{"alias": "shop-candidate", "schema_version": json.Number("7")},
		"settings": map[string]any{"number_of_shards": json.Number("1")},
		"mappings": map[string]any{"dynamic": "strict", "properties": map[string]any{"id": map[string]any{"type": "keyword"}}},
	}
	raw, err := json.MarshalIndent(candidate, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "candidate.jsonc")
	if err := os.WriteFile(file, append(raw, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	// 1. 显式目录优先，且完全不再读取内嵌
	defs, err := LoadDefinitions(dir)
	if err != nil {
		t.Fatalf("显式目录装载失败: %v", err)
	}
	if len(defs) != 1 || defs[0].Alias() != "shop-candidate" || defs[0].Embedded() {
		t.Fatalf("显式目录结果错误: %v", aliasesOf(defs))
	}
	if defs[0].Name() != "shop-candidate_v7" {
		t.Errorf("物理索引名错误: %s", defs[0].Name())
	}
	if defs[0].Source() != file {
		t.Errorf("Source 应为外置绝对路径，实际 %s", defs[0].Source())
	}

	// 2. explicitDir 传空白串等同于未指定 -> 回落内嵌（测试进程里配置为零值）
	defs, err = LoadDefinitions("   ")
	if err != nil {
		t.Fatalf("空白 explicitDir 应回落内嵌: %v", err)
	}
	if len(defs) != 2 || !defs[0].Embedded() {
		t.Fatalf("未回落到内嵌: %v", aliasesOf(defs))
	}

	// 3. 不存在的目录必须报清晰错误
	if _, err := LoadDefinitions(filepath.Join(dir, "nope")); err == nil {
		t.Fatal("目录不存在时应报错")
	} else if !strings.Contains(err.Error(), "nope") {
		t.Errorf("错误信息未包含路径: %v", err)
	}

	// 4. 外置目录里没有任何声明文件必须报错，不能静默回落内嵌
	empty := t.TempDir()
	if _, err := LoadDefinitions(empty); err == nil {
		t.Fatal("空目录应报错，静默回落内嵌会让「改了没生效」无法发现")
	}
}

// TestLoadDefinitionsValidation 校验失败必须报出文件路径与原因。
func TestLoadDefinitionsValidation(t *testing.T) {
	cases := []struct {
		name    string
		content string
		wantErr string
	}{
		{
			name:    "缺少 _meta.alias",
			content: `{"_meta": {"schema_version": 1}, "settings": {}, "mappings": {"properties": {"id": {"type": "keyword"}}}}`,
			wantErr: "alias 不能为空",
		},
		{
			name:    "缺少 mappings.properties",
			content: `{"_meta": {"alias": "a", "schema_version": 1}, "settings": {}, "mappings": {"dynamic": "strict"}}`,
			wantErr: "properties",
		},
		{
			name:    "缺少 settings",
			content: `{"_meta": {"alias": "a", "schema_version": 1}, "mappings": {"properties": {"id": {"type": "keyword"}}}}`,
			wantErr: "settings",
		},
		{
			name:    "schema_version 非正整数",
			content: `{"_meta": {"alias": "a", "schema_version": 0}, "settings": {}, "mappings": {"properties": {"id": {"type": "keyword"}}}}`,
			wantErr: "schema_version",
		},
		{
			name:    "status 非法",
			content: `{"_meta": {"alias": "a", "schema_version": 1, "status": "wip"}, "settings": {}, "mappings": {"properties": {"id": {"type": "keyword"}}}}`,
			wantErr: "status",
		},
		{
			name:    "dynamic 非 strict 拒绝",
			content: `{"_meta": {"alias": "a", "schema_version": 1}, "settings": {}, "mappings": {"dynamic": "true", "properties": {"id": {"type": "keyword"}}}}`,
			wantErr: "strict",
		},
		{
			name:    "注释吞掉结构导致语法错误",
			content: "{\"a\": 1 /* 未闭合\n\n\"_meta\": {}}",
			wantErr: "解析失败",
		},
		{
			name:    "非法 JSON",
			content: `{not json}`,
			wantErr: "解析失败",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseDefinitionJSONC("indices/bad_case.jsonc", []byte(tc.content), false)
			if err == nil {
				t.Fatalf("期望报错，实际解析成功")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("错误信息应包含 %q，实际: %v", tc.wantErr, err)
			}
			if !strings.Contains(err.Error(), "bad_case.jsonc") {
				t.Errorf("错误信息应包含文件路径，实际: %v", err)
			}
		})
	}

	// 同一目录里两份文件声明同一 alias 必须拒绝
	dir := t.TempDir()
	one := `{"_meta": {"alias": "dup", "schema_version": 1}, "settings": {}, "mappings": {"properties": {"id": {"type": "keyword"}}}}`
	if err := os.WriteFile(filepath.Join(dir, "a.jsonc"), []byte(one), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.jsonc"), []byte(one), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDefinitions(dir); err == nil || !strings.Contains(err.Error(), "同一个 alias") {
		t.Fatalf("重复 alias 应报错，实际: %v", err)
	}
}

// TestRegistryAPI Registry 的注册/查找/重复注册语义，以及 Register 的原子性。
func TestRegistryAPI(t *testing.T) {
	defs := loadDefs(t)

	r := NewRegistry()
	if err := r.Register(defs...); err != nil {
		t.Fatalf("Register 全部定义失败: %v", err)
	}
	if err := r.Register(defs[0]); err == nil || !strings.Contains(err.Error(), "已注册") {
		t.Fatalf("同 alias 重复注册应报错，实际: %v", err)
	}
	if err := r.Register(defs...); err == nil || !strings.Contains(err.Error(), "已注册") {
		t.Fatalf("整批重复注册应报错，实际: %v", err)
	}

	// Register 必须原子：批内含非法项时不得产生任何副作用
	if err := r.Register(nil); err == nil {
		t.Fatal("注册 nil 定义应报错")
	}
	if r.Len() != 2 {
		t.Errorf("Register 失败后不应改变已注册数量，实际 %d", r.Len())
	}
	if _, ok := r.Get("shop-product"); !ok {
		t.Error("Get 应命中 shop-product")
	}
	if _, err := r.MustGet("shop-missing"); err == nil || !strings.Contains(err.Error(), "shop-order") {
		t.Errorf("MustGet 错误应列出已注册别名，实际: %v", err)
	}
	if d, ok := r.GetByName("shop-product_v1"); !ok || d.Alias() != "shop-product" {
		t.Error("GetByName 反查失败")
	}
	if r.Len() != 2 {
		t.Errorf("Len 期望 2，实际 %d", r.Len())
	}
	if got := r.Aliases(); len(got) != 2 || got[0] != "shop-order" {
		t.Errorf("Aliases 应升序返回，实际 %v", got)
	}

	// LoadRegistry 端到端
	full, err := LoadRegistry("")
	if err != nil {
		t.Fatalf("LoadRegistry 失败: %v", err)
	}
	if len(full.ExecutableDefinitions()) != 1 {
		t.Errorf("LoadRegistry 后期望 1 个可执行定义，实际 %d", len(full.ExecutableDefinitions()))
	}
}

// TestSettingClassification settings 变更分级白名单（Phase 5 diff 直接复用）。
func TestSettingClassification(t *testing.T) {
	dynamic := []string{
		"index.number_of_replicas",
		"index.refresh_interval",
		"index.max_result_window",
		"index.routing.allocation.total_shards_per_node",
		"index.blocks.read_only",
		"index.search.slowlog.threshold.query.warn",
		"index.indexing.slowlog.threshold.index.warn",
		// 顶层简写必须被归一后识别
		"number_of_replicas",
		"refresh_interval",
	}
	for _, k := range dynamic {
		if !IsDynamicSetting(k) {
			t.Errorf("%s 应判定为可原地修改", k)
		}
		if IsStaticSetting(k) {
			t.Errorf("%s 不应同时是静态项", k)
		}
	}

	static := []string{
		"index.number_of_shards",
		"index.sort.field",
		"index.sort.order",
		"index.store.preload",
		"index.analysis.analyzer.ik_index_analyzer.tokenizer",
		// 声明文件把 analysis 写在 settings 顶层，必须同样判定为静态
		"analysis.analyzer.ik_index_analyzer",
		"number_of_shards",
	}
	for _, k := range static {
		if !IsStaticSetting(k) {
			t.Errorf("%s 应判定为必须重建", k)
		}
		if IsDynamicSetting(k) {
			t.Errorf("%s 不应同时是动态项", k)
		}
	}

	// 未知 key 两边都不命中 -> diff.go 必须保守按重建处理
	if IsDynamicSetting("index.some.future.setting") || IsStaticSetting("index.some.future.setting") {
		t.Error("未知 key 不应命中任何白名单")
	}

	cases := map[string]string{
		"number_of_shards":                "index.number_of_shards",
		"refresh_interval":                "index.refresh_interval",
		"analysis.analyzer.x":             "index.analysis.analyzer.x",
		"index.sort.field":                "index.sort.field",
		"defaults.index.number_of_shards": "index.number_of_shards",
		"defaults":                        "defaults",
		"index.some.future.setting":       "index.some.future.setting",
		"":                                "",
		"provided_name":                   "index.provided_name",
	}
	for in, want := range cases {
		if got := NormalizeSettingKey(in); got != want {
			t.Errorf("NormalizeSettingKey(%q)=%q, 期望 %q", in, got, want)
		}
	}

	if len(DynamicSettingKeys()) == 0 || len(StaticSettingKeys()) == 0 {
		t.Error("白名单清单导出为空")
	}
	// 返回的必须是副本，外部修改不得污染判定
	keys := DynamicSettingKeys()
	keys[0] = "tampered"
	if !IsDynamicSetting("index.number_of_replicas") {
		t.Error("DynamicSettingKeys 返回的不是副本，白名单被外部修改")
	}
}

// TestDefinitionMatchesUpstreamSource 指纹与上游原始 JSON 一致 => 证明
// product.jsonc / orders.jsonc 是「全文原样」，注释与 _meta 未改动任何取值或层级。
// 上游仓库不在本机时跳过（CI 只跑本仓库）。
func TestDefinitionMatchesUpstreamSource(t *testing.T) {
	cases := []struct {
		alias     string
		upstream  string
		wantCount int
	}{
		{"shop-product", "../../../product-consumer/sql/shop-product.json", 18},
		{"shop-order", "../../../order-consumer/sql/shop-order-date-format.json", 13},
	}

	for _, tc := range cases {
		t.Run(tc.alias, func(t *testing.T) {
			path, err := filepath.Abs(tc.upstream)
			if err != nil {
				t.Skipf("路径解析失败，跳过: %v", err)
			}
			data, err := os.ReadFile(path)
			if os.IsNotExist(err) {
				t.Skipf("上游事实源 %s 不存在，跳过一致性核对", path)
			}
			if err != nil {
				t.Fatalf("读取上游失败: %v", err)
			}

			var body map[string]any
			if err := json.Unmarshal(data, &body); err != nil {
				t.Fatalf("上游 JSON 解析失败: %v", err)
			}
			settings, _ := body["settings"].(map[string]any)
			mappings, _ := body["mappings"].(map[string]any)
			if settings == nil || mappings == nil {
				t.Fatal("上游缺少 settings/mappings")
			}
			want := FingerprintOf(settings, mappings)

			got := defByAlias(t, loadDefs(t), tc.alias)
			if got.Fingerprint() != want {
				t.Fatalf("%s 与上游 %s 指纹不一致，声明文件被改动了取值或层级", tc.alias, filepath.Base(path))
			}

			props := mappings["properties"].(map[string]any)
			if len(props) != tc.wantCount {
				t.Fatalf("上游 properties 字段数期望 %d，实际 %d", tc.wantCount, len(props))
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 测试辅助
// ---------------------------------------------------------------------------

func deepCopyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		switch t := v.(type) {
		case map[string]any:
			out[k] = deepCopyMap(t)
		case []any:
			cp := make([]any, len(t))
			for i, e := range t {
				if sub, ok := e.(map[string]any); ok {
					cp[i] = deepCopyMap(sub)
				} else {
					cp[i] = e
				}
			}
			out[k] = cp
		default:
			out[k] = v
		}
	}
	return out
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func assertNumber(t *testing.T, m map[string]any, key, want string) {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Fatalf("settings 缺少 %s", key)
	}
	n, ok := v.(json.Number)
	if !ok {
		t.Fatalf("%s 不是数字字面量，实际 %T=%v", key, v, v)
	}
	if n.String() != want {
		t.Fatalf("%s 期望 %s，实际 %s", key, want, n.String())
	}
}

func assertStringSlice(t *testing.T, m map[string]any, key string, want []string) {
	t.Helper()
	v, ok := m[key].([]any)
	if !ok {
		t.Fatalf("%s 期望数组，实际 %T", key, m[key])
	}
	if len(v) != len(want) {
		t.Fatalf("%s 长度期望 %d，实际 %d", key, len(want), len(v))
	}
	for i, e := range v {
		s, ok := e.(string)
		if !ok || s != want[i] {
			t.Fatalf("%s[%d] 期望 %q，实际 %v", key, i, want[i], e)
		}
	}
}

func assertFieldType(t *testing.T, props map[string]any, field, want string) {
	t.Helper()
	p, ok := props[field].(map[string]any)
	if !ok {
		t.Fatalf("properties 缺少字段 %s", field)
	}
	if got, _ := p["type"].(string); got != want {
		t.Fatalf("%s 类型期望 %s，实际 %v", field, want, p["type"])
	}
}
