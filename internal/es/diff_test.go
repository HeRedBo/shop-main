package es

// diff.go 验收：归一化与变更分级。全部不连 ES，只用 stdlib。
//
// 最关键的一条是「声明体顶层简写 vs 回读体 index.* 扁平 -> ActionNoop」：
// 判错方向会在生产误触发 15 分片索引的全量重建，因此除了合成用例，
// 还用真实 indices/product.jsonc 声明体做一次端到端归一化验证。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 测试夹具
// ---------------------------------------------------------------------------

// declSettings 复刻声明文件的混合层级写法：refresh_interval / number_of_shards /
// number_of_replicas / analysis 在 settings 顶层，store.preload / sort.* / slowlog 在 settings.index 下。
const declSettings = `{
  "refresh_interval": "1s",
  "number_of_shards": 3,
  "number_of_replicas": 1,
  "analysis": {
    "analyzer": {
      "ik_index_analyzer": {"type": "custom", "tokenizer": "ik_max_word", "filter": ["lowercase"]}
    },
    "tokenizer": {
      "ngram_tokenizer": {"type": "ngram", "min_gram": 1, "max_gram": 1}
    }
  },
  "index": {
    "store.preload": ["nvd", "dvd"],
    "sort.field": ["price", "sales"],
    "sort.order": ["asc", "desc"],
    "search.slowlog.threshold.query.warn": "500ms",
    "indexing.slowlog.threshold.index.warn": "1s"
  }
}`

// flatSettings 是上面这份声明体「应有的」ES 回读形态：flat_settings=true 的 index.* 扁平 key，
// 标量全部是字符串，并带 uuid / provided_name / creation_date / version.created 这类注入项。
const flatSettings = `{
  "index.number_of_shards": "3",
  "index.number_of_replicas": "1",
  "index.refresh_interval": "1s",
  "index.analysis.analyzer.ik_index_analyzer.type": "custom",
  "index.analysis.analyzer.ik_index_analyzer.tokenizer": "ik_max_word",
  "index.analysis.analyzer.ik_index_analyzer.filter": ["lowercase"],
  "index.analysis.tokenizer.ngram_tokenizer.type": "ngram",
  "index.analysis.tokenizer.ngram_tokenizer.min_gram": "1",
  "index.analysis.tokenizer.ngram_tokenizer.max_gram": "1",
  "index.store.preload": ["nvd", "dvd"],
  "index.sort.field": ["price", "sales"],
  "index.sort.order": ["asc", "desc"],
  "index.search.slowlog.threshold.query.warn": "500ms",
  "index.indexing.slowlog.threshold.index.warn": "1s",
  "index.provided_name": "shop-product_v1",
  "index.uuid": "9p2QeKdRT32abCDefGHIjK",
  "index.creation_date": "1730000000000",
  "index.version": {"created": "7100099"}
}`

const declMappings = `{
  "dynamic": "strict",
  "_source": {"excludes": ["description", "keyword"]},
  "properties": {
    "id": {"type": "keyword", "doc_values": false},
    "cate_id": {"type": "keyword", "doc_values": false},
    "price": {"type": "double"},
    "sales": {"type": "long"},
    "store_name": {
      "type": "text",
      "index_options": "positions",
      "analyzer": "ik_index_analyzer",
      "search_analyzer": "ik_search_analyzer",
      "fields": {
        "pinyin": {"type": "text", "store": false, "analyzer": "pinyin_analyzer"},
        "single": {"type": "text", "store": false, "index_options": "positions", "analyzer": "ngram_analyzer"}
      }
    },
    "desc": {"type": "text", "analyzer": "ik_index_analyzer"},
    "store_info": {"type": "text", "copy_to": "desc", "analyzer": "ik_index_analyzer"},
    "description": {"type": "text", "copy_to": "desc", "analyzer": "ik_index_analyzer"},
    "keyword": {"type": "text", "copy_to": "desc"},
    "is_hot": {"type": "keyword"},
    "update_time": {"type": "date"}
  }
}`

// liveMappings 是 declMappings 的回读形态：copy_to 被 ES 回成数组、_source 带 enabled、
// 字段上多出 norms / meta 这类白名单外的默认项。与 declMappings 结构等价。
const liveMappings = `{
  "dynamic": "strict",
  "_source": {"enabled": true, "excludes": ["description", "keyword"]},
  "properties": {
    "id": {"type": "keyword", "doc_values": false},
    "cate_id": {"type": "keyword", "doc_values": false},
    "price": {"type": "double"},
    "sales": {"type": "long"},
    "store_name": {
      "type": "text",
      "norms": true,
      "index_options": "positions",
      "analyzer": "ik_index_analyzer",
      "search_analyzer": "ik_search_analyzer",
      "fields": {
        "pinyin": {"type": "text", "store": false, "analyzer": "pinyin_analyzer"},
        "single": {"type": "text", "store": false, "index_options": "positions", "analyzer": "ngram_analyzer"}
      }
    },
    "desc": {"type": "text", "analyzer": "ik_index_analyzer", "meta": {}},
    "store_info": {"type": "text", "copy_to": ["desc"], "analyzer": "ik_index_analyzer"},
    "description": {"type": "text", "copy_to": ["desc"], "analyzer": "ik_index_analyzer"},
    "keyword": {"type": "text", "copy_to": ["desc"]},
    "is_hot": {"type": "keyword"},
    "update_time": {"type": "date"}
  }
}`

func bodyOf(t *testing.T, settings, mappings string) string {
	t.Helper()
	return `{"_meta": {"alias": "shop-product", "schema_version": 1}, "settings": ` +
		settings + `, "mappings": ` + mappings + `}`
}

// defOf 走真实的 ParseDefinitionJSONC，数字保持 json.Number，与生产装载路径一致。
func defOf(t *testing.T, settings, mappings string) *IndexDefinition {
	t.Helper()
	def, err := ParseDefinitionJSONC("testdata/product.jsonc", []byte(bodyOf(t, settings, mappings)), false)
	if err != nil {
		t.Fatalf("构造声明体失败: %v", err)
	}
	return def
}

func objOf(t *testing.T, s string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("解析测试 JSON 失败: %v", err)
	}
	return m
}

func splice(t *testing.T, s, old, new string) string {
	t.Helper()
	if !strings.Contains(s, old) {
		t.Fatalf("夹具里找不到待替换片段: %q", old)
	}
	return strings.Replace(s, old, new, 1)
}

// baseline 返回「声明体与回读体完全等价」的一对，用例各自在此基础上改一侧。
func baseline(t *testing.T) (*IndexDefinition, map[string]any, map[string]any) {
	t.Helper()
	return defOf(t, declSettings, declMappings), objOf(t, liveMappings), objOf(t, flatSettings)
}

func findSetting(diff *Diff, key string) (SettingChange, bool) {
	for _, c := range diff.SettingsDiff {
		if c.Key == key {
			return c, true
		}
	}
	return SettingChange{}, false
}

func findField(diff *Diff, path string) (FieldChange, bool) {
	for _, c := range diff.MappingDiff {
		if c.Path == path {
			return c, true
		}
	}
	return FieldChange{}, false
}

func mustInplaceJSON(t *testing.T, diff *Diff) (mappingJSON, settingsJSON string) {
	t.Helper()
	if diff.Action != ActionInplace {
		t.Fatalf("期望 ActionInplace，实际 %s / %s", diff.Action, diff.Reason)
	}
	m, _ := diff.InplacePutMappingJSON()
	s, _ := diff.InplacePutSettingsJSON()
	return m, s
}

// ---------------------------------------------------------------------------
// 1. 完全一致 -> Noop
// ---------------------------------------------------------------------------

func TestDecideIdenticalIsNoop(t *testing.T) {
	// 1a. 两侧字节级相同：走指纹快路径。
	live := objOf(t, declMappings)
	ls := objOf(t, declSettings)
	d := Decide(defOf(t, declSettings, declMappings), live, ls)
	if d.Action != ActionNoop {
		t.Fatalf("完全一致必须 Noop，实际 %s: %s", d.Action, d.Reason)
	}
	if !strings.Contains(d.Reason, "指纹") {
		t.Errorf("应走指纹快路径，Reason=%q", d.Reason)
	}
	if len(d.SettingsDiff)+len(d.MappingDiff)+len(d.NewProperties)+len(d.Warnings) != 0 {
		t.Errorf("快路径不应产生任何明细: %+v", d)
	}

	// 1b. 回读体与声明体字节级一致但带 ES 注入项：剔除只读 key 后仍应 Noop。
	lsWithInjected := objOf(t, splice(t, declSettings, `"refresh_interval": "1s",`,
		`"refresh_interval": "1s",
    "uuid": "9p2QeKdRT32abCDefGHIjK",`))
	d2 := Decide(defOf(t, declSettings, declMappings), objOf(t, declMappings), lsWithInjected)
	if d2.Action != ActionNoop {
		t.Fatalf("ES 注入的 uuid 不得判为漂移，实际 %s: %s", d2.Action, d2.Reason)
	}
}

// ---------------------------------------------------------------------------
// 2. 仅动态 settings 变更 -> Inplace
// ---------------------------------------------------------------------------

func TestDecideDynamicSettingsOnlyIsInplace(t *testing.T) {
	def, lm, _ := baseline(t)
	ls := objOf(t, splice(t, flatSettings, `"index.refresh_interval": "1s"`, `"index.refresh_interval": "30s"`))

	d := Decide(def, lm, ls)
	if d.Action != ActionInplace {
		t.Fatalf("仅动态项变更必须 Inplace，实际 %s: %s", d.Action, d.Reason)
	}
	if len(d.SettingsDiff) != 1 {
		t.Fatalf("期望 1 项 settings 漂移，实际 %+v", d.SettingsDiff)
	}
	c, ok := findSetting(d, "index.refresh_interval")
	if !ok {
		t.Fatalf("漂移项应是 index.refresh_interval，实际 %+v", d.SettingsDiff)
	}
	if !c.Inplace || c.Rebuild || c.Unknown {
		t.Errorf("refresh_interval 必须判为可原地，实际 %+v", c)
	}
	if c.Kind != KindChanged {
		t.Errorf("Kind 应为 %s，实际 %s", KindChanged, c.Kind)
	}

	_, sj := mustInplaceJSON(t, d)
	if !strings.Contains(sj, "refresh_interval") || !strings.Contains(sj, `"1s"`) {
		t.Errorf("put_settings 体应下发声明值 1s，实际 %s", sj)
	}
	if strings.Contains(sj, "30s") {
		t.Errorf("put_settings 体不应带上线上旧值，实际 %s", sj)
	}
	if strings.Contains(sj, "number_of_shards") {
		t.Errorf("只允许下发变更项，实际 %s", sj)
	}
	if mj, ok := d.InplacePutMappingJSON(); ok || mj != "" {
		t.Errorf("没有 mapping 变更时不应生成 put_mapping 体，实际 %s / %v", mj, ok)
	}

	// number_of_replicas 同样属于动态白名单。
	def2, lm2, _ := baseline(t)
	lsFlipped := objOf(t, splice(t, flatSettings, `"index.number_of_replicas": "1"`, `"index.number_of_replicas": "0"`))
	d2 := Decide(def2, lm2, lsFlipped)
	if d2.Action != ActionInplace {
		t.Fatalf("replicas 变更必须 Inplace，实际 %s: %s", d2.Action, d2.Reason)
	}
	if c, ok := findSetting(d2, "index.number_of_replicas"); !ok || !c.Inplace {
		t.Errorf("replicas 应判为可原地，实际 %+v / %v", c, ok)
	}
}

// ---------------------------------------------------------------------------
// 3. 仅新增字段 -> Inplace，且 NewProperties 恰为新增子集
// ---------------------------------------------------------------------------

func TestDecideNewFieldsOnlyIsInplaceWithSubset(t *testing.T) {
	_, lm, ls := baseline(t)
	newMappings := splice(t, declMappings, `"is_hot": {"type": "keyword"},`,
		`"is_hot": {"type": "keyword"},
    "sku": {"type": "keyword", "doc_values": false},
    "brand": {"type": "keyword", "fields": {"pinyin": {"type": "text", "analyzer": "pinyin_analyzer"}}},`)
	def := defOf(t, declSettings, newMappings)

	d := Decide(def, lm, ls)
	if d.Action != ActionInplace {
		t.Fatalf("仅新增字段必须 Inplace，实际 %s: %s", d.Action, d.Reason)
	}
	if len(d.NewProperties) != 2 {
		t.Fatalf("NewProperties 必须恰为新增子集，实际 %d 个: %v", len(d.NewProperties), keysOf(d.NewProperties))
	}
	for _, want := range []string{"sku", "brand"} {
		if _, ok := d.NewProperties[want]; !ok {
			t.Errorf("NewProperties 缺少 %s: %v", want, keysOf(d.NewProperties))
		}
	}
	for _, avoid := range []string{"price", "store_name", "id"} {
		if _, ok := d.NewProperties[avoid]; ok {
			t.Errorf("NewProperties 混入了已有字段 %s（put_mapping 会重发全量）", avoid)
		}
	}
	mj, ok := d.InplacePutMappingJSON()
	if !ok {
		t.Fatal("ActionInplace + 新增字段必须能生成 put_mapping 体")
	}
	var parsed struct {
		Dynamic    any            `json:"dynamic"`
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal([]byte(mj), &parsed); err != nil {
		t.Fatalf("put_mapping 体非法 JSON: %v / %s", err, mj)
	}
	if parsed.Dynamic != nil {
		t.Errorf("put_mapping 增量体不该带 dynamic: %s", mj)
	}
	if len(parsed.Properties) != 2 {
		t.Errorf("put_mapping 体应只含 2 个新增字段，实际: %s", mj)
	}
	if _, ok := findField(d, "sku"); !ok {
		t.Errorf("MappingDiff 应记录 sku added，实际 %+v", d.MappingDiff)
	}
}

// ---------------------------------------------------------------------------
// 4. 已有字段 type 变更 -> Rebuild
// ---------------------------------------------------------------------------

func TestDecideFieldTypeChangedIsRebuild(t *testing.T) {
	def, _, ls := baseline(t)
	lm := objOf(t, splice(t, liveMappings, `"price": {"type": "double"}`, `"price": {"type": "scaled_float", "scaling_factor": 100}`))

	d := Decide(def, lm, ls)
	if d.Action != ActionRebuild {
		t.Fatalf("字段 type 变更必须 Rebuild，实际 %s: %s", d.Action, d.Reason)
	}
	c, ok := findField(d, "price.type")
	if !ok || c.Kind != KindChanged {
		t.Fatalf("应记录 price.type changed，实际 %+v / %v", c, ok)
	}
	if _, ok := d.InplacePutMappingJSON(); ok {
		t.Error("Rebuild 路径不得生成原地 put_mapping 请求体")
	}
}

// ---------------------------------------------------------------------------
// 5. analyzer 变更 -> Rebuild
// ---------------------------------------------------------------------------

func TestDecideAnalyzerChangedIsRebuild(t *testing.T) {
	def, _, ls := baseline(t)
	lm := objOf(t, splice(t, liveMappings,
		`"analyzer": "ik_index_analyzer",
      "search_analyzer": "ik_search_analyzer",
      "fields": {`,
		`"analyzer": "standard",
      "search_analyzer": "ik_search_analyzer",
      "fields": {`))

	d := Decide(def, lm, ls)
	if d.Action != ActionRebuild {
		t.Fatalf("analyzer 变更必须 Rebuild，实际 %s: %s", d.Action, d.Reason)
	}
	if c, ok := findField(d, "store_name.analyzer"); !ok || c.Kind != KindChanged {
		t.Fatalf("应记录 store_name.analyzer changed，实际 %+v", d.MappingDiff)
	}

	// search_analyzer 同样在比对清单里。
	lm2 := objOf(t, splice(t, liveMappings, `"search_analyzer": "ik_search_analyzer"`, `"search_analyzer": "ik_max_word"`))
	if d2 := Decide(def, lm2, ls); d2.Action != ActionRebuild {
		t.Errorf("search_analyzer 变更必须 Rebuild，实际 %s", d2.Action)
	}
}

// ---------------------------------------------------------------------------
// 6. number_of_shards 变更 -> Rebuild
// ---------------------------------------------------------------------------

func TestDecideNumberOfShardsChangedIsRebuild(t *testing.T) {
	def, lm, _ := baseline(t)
	ls := objOf(t, splice(t, flatSettings, `"index.number_of_shards": "3"`, `"index.number_of_shards": "15"`))

	d := Decide(def, lm, ls)
	if d.Action != ActionRebuild {
		t.Fatalf("分片数变更必须 Rebuild，实际 %s: %s", d.Action, d.Reason)
	}
	c, ok := findSetting(d, "index.number_of_shards")
	if !ok || !c.Rebuild || c.Inplace {
		t.Fatalf("number_of_shards 必须判为静态必须重建，实际 %+v / %v", c, ok)
	}
}

// ---------------------------------------------------------------------------
// 7. index.sort.field 变更 -> Rebuild
// ---------------------------------------------------------------------------

func TestDecideSortFieldChangedIsRebuild(t *testing.T) {
	def, lm, _ := baseline(t)
	ls := objOf(t, splice(t, flatSettings, `"index.sort.field": ["price", "sales"]`,
		`"index.sort.field": ["price", "sales", "ficti"]`))

	d := Decide(def, lm, ls)
	if d.Action != ActionRebuild {
		t.Fatalf("index.sort.field 变更必须 Rebuild，实际 %s: %s", d.Action, d.Reason)
	}
	if c, ok := findSetting(d, "index.sort.field"); !ok || !c.Rebuild {
		t.Fatalf("sort.field 必须判为静态，实际 %+v / %v", c, ok)
	}

	// sort.order 与 sort.field 按位对齐，顺序变化同样是重建级别。
	ls2 := objOf(t, splice(t, flatSettings, `"index.sort.order": ["asc", "desc"]`, `"index.sort.order": ["desc", "asc"]`))
	if d2 := Decide(def, lm, ls2); d2.Action != ActionRebuild {
		t.Errorf("index.sort.order 顺序变更必须 Rebuild，实际 %s: %s", d2.Action, d2.Reason)
	}
}

// ---------------------------------------------------------------------------
// 8. 【最关键】声明体顶层简写 vs 回读体 index.* 扁平 -> Noop
// ---------------------------------------------------------------------------

func TestDecideMixedHierarchyVsFlatReadbackIsNoop(t *testing.T) {
	def, lm, ls := baseline(t)

	if def.Fingerprint() == FingerprintOf(ls, lm) {
		t.Fatal("夹具退化：两侧序列化形态必须不同，否则本用例没测到归一化")
	}
	d := Decide(def, lm, ls)
	if d.Action != ActionNoop {
		t.Fatalf("写法差异不得判为变更（会误触发 15 分片全量重建），实际 %s: %s", d.Action, d.Reason)
	}
	if len(d.SettingsDiff) != 0 {
		t.Errorf("归一化后 settings diff 必须为空，实际 %+v", d.SettingsDiff)
	}
	if len(d.MappingDiff) != 0 {
		t.Errorf("归一化后 mappings diff 必须为空，实际 %+v", d.MappingDiff)
	}
	// 指纹不等 + diff 为空 => Noop + 「仅版本号/元数据更新」告警（规则 5）。
	if len(d.Warnings) == 0 || !strings.Contains(d.Warnings[0], "元数据") {
		t.Errorf("指纹不等但 diff 为空时必须给出元数据更新告警，实际 %v", d.Warnings)
	}
	// ES 注入的只读 key 必须被剔除，不能出现在任何明细里。
	if strings.Contains(d.Reason, "uuid") || strings.Contains(d.Reason, "creation_date") ||
		strings.Contains(d.Reason, "provided_name") {
		t.Errorf("只读 key 不应进入判定明细: %s", d.Reason)
	}
}

// ---------------------------------------------------------------------------
// 9. _source.excludes 增删 -> Rebuild
// ---------------------------------------------------------------------------

func TestDecideSourceExcludesChangeIsRebuild(t *testing.T) {
	def, _, ls := baseline(t)

	// 线上少排除了 keyword（声明要求排除）
	lm := objOf(t, splice(t, liveMappings, `"excludes": ["description", "keyword"]`, `"excludes": ["description"]`))
	d := Decide(def, lm, ls)
	if d.Action != ActionRebuild {
		t.Fatalf("_source.excludes 变更必须 Rebuild，实际 %s: %s", d.Action, d.Reason)
	}
	if c, ok := findField(d, "_source.excludes"); !ok || c.Kind != KindChanged {
		t.Fatalf("应记录 _source.excludes changed，实际 %+v", d.MappingDiff)
	}

	// 线上多排除了 store_name
	lm2 := objOf(t, splice(t, liveMappings, `"excludes": ["description", "keyword"]`,
		`"excludes": ["description", "keyword", "store_name"]`))
	if d2 := Decide(def, lm2, ls); d2.Action != ActionRebuild {
		t.Errorf("excludes 增删必须 Rebuild，实际 %s", d2.Action)
	}

	// 仅顺序不同不视为变更（集合语义，防假阳性重建）
	lm3 := objOf(t, splice(t, liveMappings, `"excludes": ["description", "keyword"]`,
		`"excludes": ["keyword", "description"]`))
	if d3 := Decide(def, lm3, ls); d3.Action != ActionNoop {
		t.Errorf("excludes 顺序不同应 Noop，实际 %s: %s", d3.Action, d3.Reason)
	}
}

// ---------------------------------------------------------------------------
// 10. dynamic 从 strict 变缺省 -> Rebuild
// ---------------------------------------------------------------------------

func TestDecideDynamicSwitchChangeIsRebuild(t *testing.T) {
	// 声明体不再写 dynamic（= ES 缺省 true），线上仍是 strict。
	declNoDynamic := splice(t, declMappings, `  "dynamic": "strict",
`, ``)
	def := defOf(t, declSettings, declNoDynamic)
	_, lm, ls := baseline(t)

	d := Decide(def, lm, ls)
	if d.Action != ActionRebuild {
		t.Fatalf("dynamic 开关变化必须 Rebuild，实际 %s: %s", d.Action, d.Reason)
	}
	if c, ok := findField(d, "dynamic"); !ok || c.Kind != KindChanged {
		t.Fatalf("应记录 dynamic changed，实际 %+v", d.MappingDiff)
	}

	// 反向：声明 strict，线上没有 dynamic。
	lmNoDynamic := objOf(t, splice(t, liveMappings, `  "dynamic": "strict",
`, ``))
	defStrict, _, _ := baseline(t)
	if d2 := Decide(defStrict, lmNoDynamic, ls); d2.Action != ActionRebuild {
		t.Errorf("线上 dynamic 缺省而声明 strict 必须 Rebuild，实际 %s: %s", d2.Action, d2.Reason)
	}
}

// ---------------------------------------------------------------------------
// 11. 回读多出默认 key -> Noop
// ---------------------------------------------------------------------------

func TestDecideLiveExtraDefaultKeysAreIgnored(t *testing.T) {
	def, _, _ := baseline(t)
	lmExtra := objOf(t, splice(t, liveMappings, `"price": {"type": "double"}`,
		`"price": {"type": "double", "similarity": "BMS2", "meta": {"owner": "shop"}}`))
	lsExtra := objOf(t, splice(t, flatSettings, `"index.uuid": "9p2QeKdRT32abCDefGHIjK",`,
		`"index.uuid": "9p2QeKdRT32abCDefGHIjK",
  "index.max_docvalue_fields_search": "100",
  "index.query.default_field": ["store_name", "description"],
  "index.merge.scheduler.max_thread_count": "1",`))

	d := Decide(def, lmExtra, lsExtra)
	if d.Action != ActionNoop {
		t.Fatalf("线上回填的默认 key 必须忽略，实际 %s: %s", d.Action, d.Reason)
	}
	if len(d.SettingsDiff) != 0 || len(d.MappingDiff) != 0 {
		t.Errorf("不应产生任何漂移明细: settings=%+v mappings=%+v", d.SettingsDiff, d.MappingDiff)
	}

	// 11b. 声明显式写了 text 默认参数（index_options: positions）与继承型 analyzer
	//（pinyin 不写 search_analyzer），线上按默认值省略不回读：不得判成漂移。
	// 真实回读样本里 store_name.fields.pinyin 就是这个形态，不能假阳性触发重建。
	omitted := splice(t, liveMappings, `      "index_options": "positions",
`, ``)
	omitted = splice(t, omitted,
		`"pinyin": {"type": "text", "store": false, "analyzer": "pinyin_analyzer"}`,
		`"pinyin": {"type": "text", "analyzer": "pinyin_analyzer"}`)
	d2 := Decide(defOf(t, declSettings, declMappings), objOf(t, omitted), objOf(t, flatSettings))
	if d2.Action != ActionNoop {
		t.Fatalf("声明写了、线上省略的等价默认参数不得判为变更，实际 %s: %s", d2.Action, d2.Reason)
	}
}

// ---------------------------------------------------------------------------
// 11c. ES 8.x 自动注入的只读 / 版本元数据 -> Noop
// 仅因为客户端连的是 8.x 就误判需要蓝绿重建，是最要避免的假阳性。
// ---------------------------------------------------------------------------

func TestDecideEsInjectedSettingsAreIgnored(t *testing.T) {
	def, lm, _ := baseline(t)
	ls := objOf(t, splice(t, flatSettings, `"index.uuid": "9p2QeKdRT32abCDefGHIjK",`,
		`"index.uuid": "9p2QeKdRT32abCDefGHIjK",
  "index.routing.allocation.include._tier_preference": "data_content",
  "index.analyzer_version": "8020299",
  "index.history.uuid": "c7Q1zTaMS0-maxNjy6b1Wg",
  "index.reserved": "false",
  "index.system_uid": "abc",
  "defaults.index.number_of_shards": "1",
  "defaults.index.query.default_field": ["*"],`))

	d := Decide(def, lm, ls)
	if d.Action != ActionNoop {
		t.Fatalf("8.x 注入项与 defaults. 段不得判为漂移，实际 %s: %s", d.Action, d.Reason)
	}
	if len(d.SettingsDiff) != 0 {
		t.Errorf("settings 漂移明细应为空，实际 %+v", d.SettingsDiff)
	}
	// 反向校验：_tier_preference 不在忽略清单时会被归为「未知 key 保守 Rebuild」，
	// 而这里必须一条 Warning 都不提它。
	if strings.Contains(d.Reason, "_tier_preference") {
		t.Errorf("注入项不应出现在判定明细里: %s", d.Reason)
	}
}

// ---------------------------------------------------------------------------
// 12. 未知 settings key -> 保守 Rebuild
// ---------------------------------------------------------------------------

func TestDecideUnknownSettingKeyIsConservativeRebuild(t *testing.T) {
	// translog 在 bareTopLevelSettings 登记过（能正确归一化），但不在动态/静态任一名单里。
	declWithTranslog := splice(t, declSettings, `"store.preload": ["nvd", "dvd"],`,
		`"store.preload": ["nvd", "dvd"],
    "translog.durability": "async",
    "lifecycle.name": "warm-policy",`)
	def := defOf(t, declWithTranslog, declMappings)
	_, lm, ls := baseline(t)

	d := Decide(def, lm, ls)
	if d.Action != ActionRebuild {
		t.Fatalf("未知 settings key 必须保守 Rebuild，实际 %s: %s", d.Action, d.Reason)
	}
	c, ok := findSetting(d, "index.translog.durability")
	if !ok || !c.Rebuild || !c.Unknown {
		t.Fatalf("translog.durability 应判为未知项且必须重建，实际 %+v / %v", c, ok)
	}
	joined := strings.Join(d.Warnings, "|")
	if !strings.Contains(joined, "未知 settings key") {
		t.Errorf("未知项必须给出 Warning，实际 %v", d.Warnings)
	}
	if !strings.Contains(joined, "index.lifecycle.name") {
		t.Errorf("未登记的顶层 key 也要点名，实际 warnings=%v mapping=%+v settings=%+v", d.Warnings, d.MappingDiff, d.SettingsDiff)
	}
}

// ---------------------------------------------------------------------------
// 13. 字段删除 -> Rebuild
// ---------------------------------------------------------------------------

func TestDecideFieldRemovedIsRebuild(t *testing.T) {
	def, _, ls := baseline(t)
	lm := objOf(t, splice(t, liveMappings, `"is_hot": {"type": "keyword"},`,
		`"is_hot": {"type": "keyword"},
    "browse": {"type": "integer"},`))

	d := Decide(def, lm, ls)
	if d.Action != ActionRebuild {
		t.Fatalf("线上存在声明已删除的字段必须 Rebuild，实际 %s: %s", d.Action, d.Reason)
	}
	c, ok := findField(d, "browse")
	if !ok || c.Kind != KindRemoved {
		t.Fatalf("应记录 browse removed，实际 %+v / %v", c, ok)
	}
	if len(d.NewProperties) != 0 {
		t.Errorf("Rebuild 路径 NewProperties 应为空，实际 %v", keysOf(d.NewProperties))
	}
}

// ---------------------------------------------------------------------------
// 14. store_name.pinyin / .single 子映射内部变更 -> Rebuild
// ---------------------------------------------------------------------------

func TestDecideSubFieldsChangeIsRebuild(t *testing.T) {
	def, _, ls := baseline(t)

	lm := objOf(t, splice(t, liveMappings,
		`"pinyin": {"type": "text", "store": false, "analyzer": "pinyin_analyzer"}`,
		`"pinyin": {"type": "text", "store": false, "analyzer": "standard"}`))
	d := Decide(def, lm, ls)
	if d.Action != ActionRebuild {
		t.Fatalf("子映射 analyzer 变更必须 Rebuild，实际 %s: %s", d.Action, d.Reason)
	}
	if c, ok := findField(d, "store_name.pinyin.analyzer"); !ok || c.Kind != KindChanged {
		t.Fatalf("应记录 store_name.pinyin.analyzer changed，实际 %+v", d.MappingDiff)
	}

	// 子字段整体缺失（线上没有 .single）：属于已有字段的子结构变更，同样是重建级别。
	lm2 := objOf(t, splice(t, liveMappings, `,
        "single": {"type": "text", "store": false, "index_options": "positions", "analyzer": "ngram_analyzer"}`, ``))
	d2 := Decide(def, lm2, ls)
	if d2.Action != ActionRebuild {
		t.Fatalf("子字段缺失必须 Rebuild，实际 %s: %s", d2.Action, d2.Reason)
	}
	if c, ok := findField(d2, "store_name.single"); !ok || c.Kind != KindAdded {
		t.Errorf("应记录 store_name.single 声明有而线上无（added），实际 %+v / %v", c, ok)
	}

	// type 变更也要落到子字段路径上。
	lm3 := objOf(t, splice(t, liveMappings,
		`"single": {"type": "text", "store": false, "index_options": "positions"`,
		`"single": {"type": "keyword", "store": false, "index_options": "positions"`))
	if d3 := Decide(def, lm3, ls); d3.Action != ActionRebuild {
		t.Errorf("子字段 type 变更必须 Rebuild，实际 %s", d3.Action)
	} else if _, ok := findField(d3, "store_name.single.type"); !ok {
		t.Errorf("应记录 store_name.single.type，实际 %+v", d3.MappingDiff)
	}
}

// ---------------------------------------------------------------------------
// 15. 端到端归一化实证：真实 product.jsonc 声明体 vs 线上实测回读样本
// ---------------------------------------------------------------------------

// productDef 加载真实内嵌声明（不构造假 Definition）。
func productDef(t *testing.T) *IndexDefinition {
	t.Helper()
	defs, err := LoadDefinitions("")
	if err != nil {
		t.Fatalf("LoadDefinitions 失败: %v", err)
	}
	for _, d := range defs {
		if d.Alias() == "shop-product" {
			return d
		}
	}
	t.Fatal("未装载到 shop-product 声明")
	return nil
}

// probeRaw 读线上回读样本原文（ES 8.2.2 对 shop-product 的原样导出，只读参考、不入库）。
// 样本缺失时跳过：端到端用例依赖实测产物，不能自己造一份当作线上事实。
func probeRaw(t *testing.T, file string) string {
	t.Helper()
	path := filepath.Join("..", "..", "runtime", "es_probe", file)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("缺少线上回读样本 %s，跳过端到端归一化用例: %v", path, err)
	}
	return string(raw)
}

// unwrapProbe 从 { "<index>": { "settings"|"mappings": ... } } 里取出内层对象，
// 等价于 admin.GetSettings / admin.GetMapping 已经做过的拆封。
func unwrapProbe(t *testing.T, docText, section string) map[string]any {
	t.Helper()
	for _, v := range objOf(t, docText) {
		entry, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if inner, ok := entry[section].(map[string]any); ok {
			return inner
		}
	}
	t.Fatalf("回读样本缺少 %s 段", section)
	return nil
}

func TestDecideAgainstRealReadbackSample(t *testing.T) {
	def := productDef(t)
	mappingText := probeRaw(t, "shop-product.mapping.json")
	settingsText := probeRaw(t, "shop-product.settings.json")

	liveMappings := unwrapProbe(t, mappingText, "mappings")
	liveSettings := unwrapProbe(t, settingsText, "settings")

	d := Decide(def, liveMappings, liveSettings)

	// 1. settings：线上只多了 uuid / provided_name / creation_date / version.created 和
	// 8.x 自动写入的 routing.allocation.include._tier_preference，必须全部被忽略。
	// 否则仅因为客户端连的是 8.x 就会误判触发 15 分片全量重建。
	if len(d.SettingsDiff) != 0 {
		t.Errorf("线上 settings 与声明等价，不应有任何漂移明细，实际 %+v", d.SettingsDiff)
	}

	// 2. mappings：线上确实没有 _source.excludes（已实测确认），必须 Rebuild 且只错这一项。
	if d.Action != ActionRebuild {
		t.Fatalf("线上缺 _source.excludes 必须 Rebuild，实际 %s: %s", d.Action, d.Reason)
	}
	if len(d.MappingDiff) != 1 {
		t.Fatalf("漂移应只有 1 项（其余均为 ES 默认值省略/回填），实际 %+v", d.MappingDiff)
	}
	c, ok := findField(d, "_source.excludes")
	if !ok || c.Kind != KindChanged {
		t.Fatalf("必须精确指向 _source.excludes，实际 %+v", d.MappingDiff)
	}
	if !strings.Contains(d.Reason, "_source.excludes") {
		t.Errorf("Reason 未点名 _source.excludes: %s", d.Reason)
	}

	// 3. 把 excludes 补进线上形态后再比 -> Noop：首次 migrate 不误判重建的实证验收点。
	// 同时坐实「声明写了 index_options: positions / pinyin search_analyzer，线上按默认值省略」
	// 不会造成假阳性（真实样本里 store_name.fields.pinyin 只有 analyzer）。
	patched := splice(t, mappingText, `"dynamic":"strict",`,
		`"dynamic":"strict","_source":{"excludes":["store_name","store_info","description","keyword"]},`)
	d2 := Decide(def, unwrapProbe(t, patched, "mappings"), liveSettings)
	if d2.Action != ActionNoop {
		t.Fatalf("补齐 _source.excludes 后必须 Noop，实际 %s: %s", d2.Action, d2.Reason)
	}
	if len(d2.SettingsDiff) != 0 || len(d2.MappingDiff) != 0 {
		t.Errorf("归一化后应无任何明细: settings=%+v mappings=%+v", d2.SettingsDiff, d2.MappingDiff)
	}
	if len(d2.Warnings) == 0 || !strings.Contains(d2.Warnings[0], "元数据") {
		t.Errorf("指纹不等但 diff 为空时必须告警，实际 %v", d2.Warnings)
	}

	// 4. 夹具防退化：回读体必须真覆盖了声明里的 18 个字段，否则上面的 Noop 是假阳性。
	declProps, _ := def.MappingsMap()["properties"].(map[string]any)
	liveProps, _ := liveMappings["properties"].(map[string]any)
	if len(declProps) != 18 || len(liveProps) != len(declProps) {
		t.Fatalf("声明/回读字段数不对等，声明 %d 回读 %d", len(declProps), len(liveProps))
	}
}

// keysOf 已在 definition_test.go 提供，本文件直接复用。
