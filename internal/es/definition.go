package es

// 本文件是「索引结构事实源」的内存模型：一个 IndexDefinition 对应一个 indices/*.jsonc。
//
// 三条硬约束（来自 dynamic: strict，方案第一节）：
//  1. 声明文件是唯一事实源，代码里的文档 struct 必须与 mappings.properties 严格互等
//     （差集只允许是 _meta.copy_to_only_fields 里的 copy_to 虚拟字段，当前仅 desc）；
//  2. 结构变更必须「先 es:migrate 改索引，再发版改代码」，反向顺序会让写入全量失败；
//  3. 变更分级（可原地 / 必须重建）由 settings key 决定，判定依据是本文件导出的白名单，
//     Phase 5 的 diff.go 必须复用 IsDynamicSetting / IsStaticSetting / NormalizeSettingKey，
//     不要另写一套判定，否则两侧判定不一致会误触发 15 分片索引的全量重建。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// _meta 内约定的 key（本方案自定义的元数据命名空间，ES 不认识，BodyJSON 会剔除）。
const (
	MetaKey                 = "_meta" // 元数据容器
	MetaKeyAlias            = "alias" // 别名，如 shop-product
	MetaKeySchemaVersion    = "schema_version"
	MetaKeyNextVersionRule  = "next_version_rule" // 物理索引命名规则说明，如 "<alias>_v<N+1>"
	MetaKeyCopyToOnlyFields = "copy_to_only_fields"
	MetaKeyDateFormat       = "date_format" // 可选；空串表示走 ES 默认 date format
	MetaKeyStatus           = "status"
	MetaKeyRequiresPlugins  = "requires_plugins" // 依赖的 ES 插件，供 migrate 前置探测
	MetaKeyNotes            = "notes"            // 自由文本说明

	AliasesKey = "aliases" // BodyJSON 剔除；由调用方按场景（首建 / 蓝绿）注入
)

// 索引定义接线状态（_meta.status）。
const (
	// StatusActive 已接线：进入 Registry.ExecutableDefinitions()，es:migrate/es:rebuild 会处理它。
	StatusActive = "active"
	// StatusDraft 仅落盘：LoadDefinitions 会解析、校验、算指纹，但不进入可执行清单。
	// 用于「声明已写但数据源 / Provider 尚未接线」的索引；接线完成后转 active（orders.jsonc 二期即已从 draft 转 active）。
	StatusDraft = "draft"
)

// DefaultStatus 是 _meta.status 缺省时的取值。
// 缺省取 active：新增一份 jsonc 即视为希望被迁移流程接管，草稿必须显式声明 draft。
const DefaultStatus = StatusActive

// VersionSeparator 物理索引名中别名与版本号的连接符，命名规则固定为 <alias>_v<N>。
const VersionSeparator = "_v"

// DefinitionMeta 是 _meta 的强类型视图。
type DefinitionMeta struct {
	Alias            string   `json:"alias"`
	SchemaVersion    int      `json:"schema_version"`
	NextVersionRule  string   `json:"next_version_rule"`
	CopyToOnlyFields []string `json:"copy_to_only_fields"`
	DateFormat       string   `json:"date_format"`
	Status           string   `json:"status"`
	RequiresPlugins  []string `json:"requires_plugins"`
	Notes            string   `json:"notes"`
}

// IndexDefinition 一份索引结构声明（settings + mappings + _meta）。
// 构造后视为只读；字段全部非导出，只通过方法访问。
type IndexDefinition struct {
	source      string // 声明文件路径（内嵌时为 indices/xxx.jsonc），仅用于报错与日志
	embedded    bool   // true 来自 go:embed，false 来自 --indices-dir / 配置外置目录
	alias       string
	version     int
	name        string // 物理索引名 <alias>_v<version>
	status      string
	meta        DefinitionMeta
	body        map[string]any // 原始文档（含 _meta）
	settings    map[string]any
	mappings    map[string]any
	fingerprint string
}

// NewIndexDefinition 从已解析的 JSONC 文档构造 Definition。
// body 必须是 Strip + json.Decoder(UseNumber) + Decode(map[string]any) 的产物，
// 这样数字字面量保持 json.Number，指纹不会因 float64 往返而漂移。
//
// 校验项（任一不满足即返回错误，错误信息带上来源）：
//   - settings / mappings 必须存在且为对象；
//   - mappings.properties 必须存在且至少一个字段；
//   - _meta.alias 必须非空，_meta.schema_version 必须为 >=1 的整数；
//   - _meta.status 只能是 active / draft。
//
// 若手上是一份「线上回读的 body」（没有 _meta），请勿硬造 Definition：
// 直接用 FingerprintOf + NormalizeSettingKey 做 diff。
func NewIndexDefinition(source string, body map[string]any, embedded bool) (*IndexDefinition, error) {
	if body == nil {
		return nil, fmt.Errorf("es: 索引声明 %s 内容为空或不是 JSON 对象", sourceLabel(source))
	}

	label := sourceLabel(source)

	settings, err := objectAt(body, "settings")
	if err != nil {
		return nil, fmt.Errorf("es: 索引声明 %s 缺少 settings 对象: %w", label, err)
	}
	mappingsRaw, err := objectAt(body, "mappings")
	if err != nil {
		return nil, fmt.Errorf("es: 索引声明 %s 缺少 mappings 对象: %w", label, err)
	}
	props, err := objectAt(mappingsRaw, "properties")
	if err != nil {
		return nil, fmt.Errorf("es: 索引声明 %s 的 mappings 缺少 properties: %w", label, err)
	}
	if len(props) == 0 {
		return nil, fmt.Errorf("es: 索引声明 %s 的 mappings.properties 为空，没有任何字段声明", label)
	}

	rawMeta, err := objectAt(body, MetaKey)
	if err != nil {
		return nil, fmt.Errorf("es: 索引声明 %s 缺少 %s（本方案的结构事实源必须自带元数据）: %w", label, MetaKey, err)
	}
	meta, err := parseMeta(rawMeta)
	if err != nil {
		return nil, fmt.Errorf("es: 索引声明 %s 的 %s 无效: %w", label, MetaKey, err)
	}

	dyn, _ := mappingsRaw["dynamic"]
	if s, ok := dyn.(string); ok && s != "" && s != "strict" {
		// 非 strict 会削弱本方案的结构护栏（未声明字段被静默 dynamic 推断），
		// 目前两份线上事实源都是 strict，出现别的值直接拒绝，避免 Phase 5 判定基于错误假设。
		return nil, fmt.Errorf("es: 索引声明 %s 的 mappings.dynamic=%q 不被支持，本方案要求 strict", label, s)
	}

	def := &IndexDefinition{
		source:   source,
		embedded: embedded,
		alias:    meta.Alias,
		version:  meta.SchemaVersion,
		name:     IndexName(meta.Alias, meta.SchemaVersion),
		status:   meta.Status,
		meta:     meta,
		body:     body,
		settings: settings,
		mappings: mappingsRaw,
	}
	def.fingerprint = FingerprintOf(settings, mappingsRaw)

	return def, nil
}

// Alias 返回逻辑别名，如 shop-product。物理索引名由 Name 提供。
func (d *IndexDefinition) Alias() string { return d.alias }

// Version 返回 schema_version，即物理索引名尾部的 <N>。
func (d *IndexDefinition) Version() int { return d.version }

// Name 返回物理索引名 <alias>_v<version>，如 shop-product_v1。
func (d *IndexDefinition) Name() string { return d.name }

// Status 返回 active / draft；只有 active 会进入可执行清单。
func (d *IndexDefinition) Status() string { return d.status }

// Executable 报告该定义是否应被 es:migrate / es:rebuild 处理。
func (d *IndexDefinition) Executable() bool { return d.status == StatusActive }

// DateFormat 返回 _meta.date_format。
// 空串 = mapping 未声明 format，文档侧必须走 time.Time 默认序列化（RFC3339）；
// 非空（如 orders 的 "yyyy-MM-dd HH:mm:ss"）= 必须按该格式单独序列化，两个索引不能共用一个函数。
// 这是方案风险表里预留的「每索引日期格式」元数据位。
func (d *IndexDefinition) DateFormat() string { return d.meta.DateFormat }

// CopyToOnlyFields 返回「mapping 声明了但文档 struct 不产出」的白名单（当前只有 desc）。
func (d *IndexDefinition) CopyToOnlyFields() []string { return d.meta.CopyToOnlyFields }

// RequiresPlugins 返回该索引依赖的 ES 插件清单，供 migrate 前置 _analyze 探测。
func (d *IndexDefinition) RequiresPlugins() []string { return d.meta.RequiresPlugins }

// Source 返回声明文件路径；Embedded 报告是否来自 go:embed。
func (d *IndexDefinition) Source() string       { return d.source }
func (d *IndexDefinition) Embedded() bool       { return d.embedded }
func (d *IndexDefinition) Meta() DefinitionMeta { return d.meta }

// SettingsMap / MappingsMap / BodyMap 返回内部视图。
// 约定调用方只读不改；Phase 5 做 diff 时如需修改请先深拷贝。
func (d *IndexDefinition) SettingsMap() map[string]any { return d.settings }
func (d *IndexDefinition) MappingsMap() map[string]any { return d.mappings }
func (d *IndexDefinition) BodyMap() map[string]any     { return d.body }

// Fingerprint 返回 sha256(canonical(settings)+canonical(mappings)) 的 hex。
// 只覆盖结构本体：_meta 变化不影响指纹（方案第五节第 3 步的「指纹不等但 diff 为空」判定依赖此语义）。
func (d *IndexDefinition) Fingerprint() string { return d.fingerprint }

// SettingsJSON 输出紧凑 settings 对象 JSON（含 analysis 等全部内容，不含 _meta）。
func (d *IndexDefinition) SettingsJSON() (string, error) {
	return marshalCompactString(d.settings)
}

// MappingsJSON 输出紧凑 mappings 对象 JSON（dynamic/_source/properties 全在，不含 _meta）。
func (d *IndexDefinition) MappingsJSON() (string, error) {
	return marshalCompactString(d.mappings)
}

// BodyJSON 输出创建索引用的完整请求体：{ "settings": ..., "mappings": ... }。
//
// 明确剔除两项：
//   - _meta：本方案的元数据命名空间，ES 会在 create_index 阶段以 unknown key 拒绝；
//   - aliases：别名注入随场景不同（首次创建带别名、蓝绿新建必须不带），由调用方自行拼装，
//     本层只负责结构，不提供任何带别名的变体。
func (d *IndexDefinition) BodyJSON() (string, error) {
	body := make(map[string]any, len(d.body))
	for k, v := range d.body {
		if k == MetaKey || k == AliasesKey {
			continue
		}
		body[k] = v
	}
	return marshalCompactString(body)
}

// IndexName 按命名规则拼出物理索引名 <alias>_v<version>。
func IndexName(alias string, version int) string {
	return alias + VersionSeparator + strconv.Itoa(version)
}

// NextVersionName 给出下一版物理索引名，实现 _meta.next_version_rule 声明的 <alias>_v<N+1> 规则。
func NextVersionName(alias string, currentVersion int) string {
	return IndexName(alias, currentVersion+1)
}

// ---------------------------------------------------------------------------
// 规范化序列化与指纹
// ---------------------------------------------------------------------------

// CanonicalJSON 把任意 JSON 值序列化为「key 递归字典序 + 无空白」的紧凑字节。
//
// 用途：指纹计算的唯一规范化入口。Go 的 map 遍历顺序随机，若直接 json.Marshal
// 同一份内容两次会得到不同字节串（顶层 map 会排序，但嵌套结构里的数字/数组顺序
// 与 json.Number 表示仍需固定），因此这里显式递归排序并固定数字写法，
// 保证「同内容 => 同字节」，指纹可跨进程、跨语言比较。
//
// 数字优先保持 json.Number 的原始字面量（15 不会变成 1.5e+01 或 15.00）。
func CanonicalJSON(v any) []byte {
	var sb strings.Builder
	writeCanonical(&sb, v)
	return []byte(sb.String())
}

// marshalCompactString 是 CanonicalJSON 的字符串化包装：把任意 JSON 值序列化为
// 「key 递归字典序 + 无空白 + 数字走 json.Number 原样字面量」的紧凑字符串。
// SettingsJSON / MappingsJSON / BodyJSON 与 diff.go 的 jsonMarshal 都复用它，
// 保证请求体字节与本包规范化（FingerprintOf 用的 CanonicalJSON）完全一致。
// CanonicalJSON 是纯函数不会失败，error 位仅为调用方签名保留，恒为 nil。
func marshalCompactString(v any) (string, error) {
	return string(CanonicalJSON(v)), nil
}

func writeCanonical(sb *strings.Builder, v any) {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		sb.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				sb.WriteByte(',')
			}
			writeStringScalar(sb, k)
			sb.WriteByte(':')
			writeCanonical(sb, t[k])
		}
		sb.WriteByte('}')

	case []any:
		sb.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				sb.WriteByte(',')
			}
			writeCanonical(sb, e)
		}
		sb.WriteByte(']')

	case json.Number:
		if t.String() == "" {
			sb.WriteString("null") // 空数字非法，落 null 而不是 panic
			return
		}
		sb.WriteString(t.String())

	case string:
		writeStringScalar(sb, t)

	case bool:
		if t {
			sb.WriteString("true")
		} else {
			sb.WriteString("false")
		}

	case nil:
		sb.WriteString("null")

	case float64:
		// 只有非 json.Number 路径（例如线上回读后又手工构造）才会走到这里。
		if t == math.Trunc(t) && !math.IsInf(t, 0) && math.Abs(t) < 1e15 {
			sb.WriteString(strconv.FormatInt(int64(t), 10))
			return
		}
		writeStringScalar(sb, strconv.FormatFloat(t, 'g', -1, 64))

	default:
		// 未知类型（不该出现在 JSON 解析结果里）退化为标准 marshal，实在不行写 null。
		b, err := json.Marshal(t)
		if err != nil {
			sb.WriteString("null")
			return
		}
		sb.Write(b)
	}
}

func writeStringScalar(sb *strings.Builder, s string) {
	b, err := json.Marshal(s)
	if err != nil {
		sb.WriteString(`""`)
		return
	}
	sb.Write(b)
}

// FingerprintOf 计算任意 settings/mappings 组合的指纹，两者可任一为 nil（视作空对象）。
// Phase 5 用它给「线上 GetMapping/GetSettings 回读体」算指纹，与 Definition.Fingerprint() 比对。
func FingerprintOf(settings, mappings map[string]any) string {
	h := sha256.New()
	h.Write(CanonicalJSON(orEmpty(settings)))
	h.Write([]byte{'\n'}) // 分隔符，避免 settings 尾部与 mappings 头部拼接歧义
	h.Write(CanonicalJSON(orEmpty(mappings)))
	return hex.EncodeToString(h.Sum(nil))
}

func orEmpty(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// ---------------------------------------------------------------------------
// settings 变更分级白名单（Phase 5 diff.go 必须复用）
// ---------------------------------------------------------------------------

// dynamicSettingKeys 可原地修改的设置项（PUT /<idx>/_settings，无需 close、无需重建）。
var dynamicSettingKeys = []string{
	"index.number_of_replicas",
	"index.refresh_interval",
	"index.max_result_window",
}

// dynamicSettingPrefixes 可原地修改的设置项前缀族。
// routing.allocation 的 total_shards_per_node、blocks.* 的读写封锁、两类 slowlog 阈值都在此列。
var dynamicSettingPrefixes = []string{
	"index.routing.allocation.",
	"index.blocks.",
	"index.search.slowlog.",
	"index.indexing.slowlog.",
}

// staticSettingKeys 创建后不可变或变更代价极高的设置项：命中即 ActionRebuild（蓝绿重建）。
var staticSettingKeys = []string{
	"index.number_of_shards",
	"index.sort.field", // 本次两份事实源都用了 index.sort，新排序字段必须建模期定完
	"index.sort.order",
	"index.store.preload",
}

// staticSettingPrefixes 静态设置项前缀族；analyzer/tokenizer 任何变更都必须重建。
var staticSettingPrefixes = []string{
	"index.analysis.",
	"analysis.", // 声明文件里 analysis 写在 settings 顶层的写法，等价 index.analysis
}

// bareTopLevelSettings 是允许直接写在 settings 顶层（不带 index. 前缀）的 key 白名单。
// 两份事实源都用了这种混合层级写法：refresh_interval / number_of_shards / number_of_replicas /
// analysis 在 settings 顶层，store.preload / sort.* / slowlog 在 settings.index 下。
// ES 两种写法都接受，但回读时统一返回带 index. 前缀的形式，故 diff 前必须先归一化。
var bareTopLevelSettings = map[string]struct{}{
	"number_of_shards":        {},
	"number_of_replicas":      {},
	"refresh_interval":        {},
	"max_result_window":       {},
	"analysis":                {},
	"sort":                    {},
	"store":                   {},
	"routing":                 {},
	"blocks":                  {},
	"search":                  {},
	"indexing":                {},
	"translog":                {},
	"codec":                   {},
	"provided_name":           {},
	"uuid":                    {},
	"version":                 {},
	"creation_date":           {},
	"hidden":                  {},
	"default_pipeline":        {},
	"optimize_auto_generated": {},
}

// NormalizeSettingKey 把设置项 key 归一为带 index. 前缀的扁平形式，供 Phase 5 比较使用。
//
// 规则（务必与方案第五节第 3 步的归一化描述保持一致）：
//   - 已带 index. 前缀：原样返回；
//   - 带 defaults. 前缀（include_defaults=true 的回读形态）：剥掉 defaults. 再归一，
//     使 defaults.index.number_of_shards 与 index.number_of_shards 等价；
//   - 命中 bareTopLevelSettings 的顶层简写：补 index. 前缀；
//   - 其他：原样返回（例如未知 key，交由 diff 判定为漂移）。
//
// 不归一化的后果：声明文件写顶层简写、ES 回读返回 index.* 形式，首次 migrate 会误判为
// 变更并触发 15 分片索引的全量重建，代价极高。这是本函数存在的唯一理由。
func NormalizeSettingKey(key string) string {
	if key == "" {
		return key
	}
	k := key
	if rest, ok := strings.CutPrefix(k, "defaults."); ok {
		// include_defaults=true 的回读形态：defaults.index.number_of_shards / defaults.analysis.*
		k = rest
	}
	if strings.HasPrefix(k, "index.") {
		return k
	}
	if _, ok := bareTopLevelSettings[k]; ok {
		return "index." + k
	}
	// 顶层简写的前缀族，例如 analysis.analyzer.x / sort.field
	for bare := range bareTopLevelSettings {
		if strings.HasPrefix(k, bare+".") {
			return "index." + k
		}
	}
	return k
}

// IsDynamicSetting 报告 key 是否属于可原地修改白名单（自动做 NormalizeSettingKey）。
// 用途：diff.go 判定 settings 漂移时，命中即 ActionInplace。
func IsDynamicSetting(key string) bool {
	k := NormalizeSettingKey(key)
	for _, e := range dynamicSettingKeys {
		if k == e {
			return true
		}
	}
	for _, p := range dynamicSettingPrefixes {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	return false
}

// IsStaticSetting 报告 key 是否属于「变更必须蓝绿重建」清单（自动做 NormalizeSettingKey）。
// 用途：diff.go 判定 settings 漂移时，命中即 ActionRebuild。
//
// 注意：既非 dynamic 也非 static 的 key 属于未知项，diff.go 必须保守按 ActionRebuild 处理，
// 不能默认原地（strict 模式下的结构漂移宁可重建不可静默错判）。
func IsStaticSetting(key string) bool {
	k := NormalizeSettingKey(key)
	for _, e := range staticSettingKeys {
		if k == e {
			return true
		}
	}
	for _, p := range staticSettingPrefixes {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	return false
}

// DynamicSettingKeys 返回可原地修改清单的副本（exact key + 前缀族，前缀以 * 结尾表示）。
// 用途：错误信息与 --dry-run 输出中向运维解释分级依据，避免各处硬编码。
func DynamicSettingKeys() []string {
	out := make([]string, 0, len(dynamicSettingKeys)+len(dynamicSettingPrefixes))
	out = append(out, dynamicSettingKeys...)
	for _, p := range dynamicSettingPrefixes {
		out = append(out, p+"*")
	}
	return out
}

// StaticSettingKeys 返回必须重建清单的副本（前缀以 * 结尾表示前缀族）。
func StaticSettingKeys() []string {
	out := make([]string, 0, len(staticSettingKeys)+len(staticSettingPrefixes))
	out = append(out, staticSettingKeys...)
	for _, p := range staticSettingPrefixes {
		out = append(out, p+"*")
	}
	return out
}

// ---------------------------------------------------------------------------
// 内部工具
// ---------------------------------------------------------------------------

func parseMeta(raw map[string]any) (DefinitionMeta, error) {
	var m DefinitionMeta

	alias, _ := raw[MetaKeyAlias].(string)
	if strings.TrimSpace(alias) == "" {
		return m, fmt.Errorf("%s.alias 不能为空（物理索引名与别名都依赖它）", MetaKey)
	}
	m.Alias = strings.TrimSpace(alias)

	v, err := metaInt(raw, MetaKeySchemaVersion)
	if err != nil {
		return m, err
	}
	if v < 1 {
		return m, fmt.Errorf("%s.%s=%d 非法，必须是从 1 开始的整数版本号", MetaKey, MetaKeySchemaVersion, v)
	}
	m.SchemaVersion = v

	m.NextVersionRule, _ = raw[MetaKeyNextVersionRule].(string)
	m.DateFormat, _ = raw[MetaKeyDateFormat].(string)
	m.Notes, _ = raw[MetaKeyNotes].(string)
	m.CopyToOnlyFields = metaStringSlice(raw[MetaKeyCopyToOnlyFields])
	m.RequiresPlugins = metaStringSlice(raw[MetaKeyRequiresPlugins])

	status, _ := raw[MetaKeyStatus].(string)
	if strings.TrimSpace(status) == "" {
		status = DefaultStatus
	}
	status = strings.TrimSpace(status)
	if status != StatusActive && status != StatusDraft {
		return m, fmt.Errorf("%s.%s=%q 非法，只能是 %s 或 %s", MetaKey, MetaKeyStatus, status, StatusActive, StatusDraft)
	}
	m.Status = status

	return m, nil
}

func metaInt(raw map[string]any, key string) (int, error) {
	switch n := raw[key].(type) {
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0, fmt.Errorf("%s.%s=%s 不是整数", MetaKey, key, n.String())
		}
		return int(i), nil
	case float64:
		if n != math.Trunc(n) {
			return 0, fmt.Errorf("%s.%s=%v 不是整数", MetaKey, key, n)
		}
		return int(n), nil
	case int:
		return n, nil
	case nil:
		return 0, fmt.Errorf("%s.%s 缺失", MetaKey, key)
	default:
		return 0, fmt.Errorf("%s.%s 必须是整数，实际类型 %T", MetaKey, key, n)
	}
}

func metaStringSlice(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		if s, isStr := v.(string); isStr && s != "" {
			return []string{s}
		}
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func objectAt(m map[string]any, key string) (map[string]any, error) {
	v, ok := m[key]
	if !ok {
		return nil, fmt.Errorf("缺少 %q 字段", key)
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%q 不是 JSON 对象（实际类型 %T）", key, v)
	}
	return obj, nil
}

func sourceLabel(source string) string {
	if source == "" {
		return "<unknown>"
	}
	return source
}
