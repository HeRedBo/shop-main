package es

// 本文件实现方案第五节的「显式 diff 分级」：把声明体（IndexDefinition）与线上回读体
// （admin.GetMapping / admin.GetSettings 的返回值）归一化到同一坐标系后比较，
// 输出 ActionNoop / ActionInplace / ActionRebuild 三级判定，取代 PHP「靠 ES 400 异常猜」。
//
// 三条硬约束：
//  1. 纯函数：不访问 ES、不读文件、不依赖 admin 包；线上结构由 Phase 5 的 migrate.go 取到后传入。
//  2. 归一化优先：声明文件（indices/*.jsonc）保留的是上游的混合层级写法——settings 顶层简写
//     （refresh_interval / number_of_shards / analysis）加 settings.index.* 下的 store.preload /
//     sort.* / slowlog；而 ES 回读（flat_settings=true）是带 index. 前缀的扁平 key。两侧必须先
//     拍平成叶子 key、再逐个过定义层 NormalizeSettingKey 才可比。判错方向的代价极高：
//     会把「写法差异」误判成变更，触发 15 分片索引的全量重建。
//  3. 判不准就重建：既不在动态白名单也不在静态清单里的未知 settings key 一律 ActionRebuild；
//     mappings 只比结构相关项，单侧缺失的参数按「ES 未回填默认值」处理，不制造假阳性。

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Action 结构变更的分级结果。常量顺序即严重级别（noop < inplace < rebuild），
// 聚合时直接取 max 即可，migrate.go 也按这个顺序打日志与选择执行路径。
type Action int

const (
	// ActionNoop 结构一致，不发任何写请求（幂等 migrate 的期望结果）。
	ActionNoop Action = iota
	// ActionInplace 可原地：仅动态 settings 变更 / 仅新增 mapping 字段。
	ActionInplace
	// ActionRebuild 必须蓝绿重建：静态 settings 变更 / 已有字段结构漂移 / 字段删除 / 未知项。
	ActionRebuild
)

// String 供日志与 --dry-run 输出使用。
func (a Action) String() string {
	switch a {
	case ActionNoop:
		return "noop"
	case ActionInplace:
		return "inplace"
	case ActionRebuild:
		return "rebuild"
	default:
		return "unknown"
	}
}

// 变更类型，FieldChange.Kind 与 SettingChange.Kind 共用。
const (
	KindAdded   = "added"   // 声明有、线上无
	KindRemoved = "removed" // 线上有、声明无
	KindChanged = "changed" // 两侧都有但值不同
)

// mappingFieldParams mappings 中参与结构比较的字段级参数白名单。
// 名单外的 key（norms / eager_global_ordinals / meta / similarity ...）一律视为 ES 回填的
// 默认值或装饰性配置，忽略；名单内但只出现在一侧的参数同样忽略（见 mappingLeaf 注释）。
// 漏比会漏判，多比会误判重建，这份名单就是折中的边界。
var mappingFieldParams = []string{
	"type",
	"analyzer",
	"search_analyzer",
	"copy_to",
	"index_options",
	"doc_values",
}

// mappingContainers 需要递归下降的子结构 key。
var mappingContainers = []string{"fields", "properties"}

// volatileSettingNames 是 ES 回读时自动注入、声明体不可能表达也不构成结构漂移的只读 / 版本元数据 key。
// 前四项（uuid / provided_name / version / creation_date）已在定义层 bareTopLevelSettings 登记，
// 这里只从「已登记」集合里取只读语义的子集，避免两处清单各写一份而漂移；
// 其余是 ES 8.x 实测回读（runtime/es_probe/shop-product.settings.json）里新增的注入项，在本层补全：
//   - analyzer_version：8.x 给每个 analyzer 记的版本号；
//   - history.*：reindex 血缘；
//   - reserved / system_*：系统索引标记；
//   - 任何以 _ 开头的末段（如 routing.allocation.include._tier_preference）：ES 内部注入，
//     8.x 建索引时会自动写入 data_content 这类 tier 偏好，仅因为客户端连的是 8.x
//     就把它当成漂移，会误判需要蓝绿重建，这是本清单存在的首要理由。
//
// 注意：这条忽略清单优先于「未知 key 保守 Rebuild」，两者不能混淆。
var volatileSettingNames = []string{
	"uuid", "provided_name", "version", "creation_date",
	"analyzer_version", "history", "reserved",
}

// volatileSettingPrefixes 归一化后 base（去 index. 前缀）的只读前缀族。
var volatileSettingPrefixes = []string{"system_"}

// defaultsRawPrefix 是 include_defaults=true 回读形态的段前缀。
// NormalizeSettingKey 会把 defaults. 剥掉使两者等价，但默认值段一律不参与漂移判定，
// 所以要在归一化之前就按原始 key 剔除。
const defaultsRawPrefix = "defaults."

// SettingChange 一个 settings key 的漂移明细。
type SettingChange struct {
	Key     string // 归一化后的扁平 key，如 index.refresh_interval
	Kind    string // added / removed / changed
	From    any    // 线上值（removed/changed 时有意义）
	To      any    // 声明值（added/changed 时有意义，原地路径就下发它）
	Inplace bool   // 命中动态白名单，可 PUT _settings
	Rebuild bool   // 命中静态清单或未知项，必须蓝绿
	Unknown bool   // 两侧名单都没登记（保守按 Rebuild），Warnings 里会点名
}

// FieldChange 一个 mapping 结构项的漂移明细。
type FieldChange struct {
	Path string // 字段路径，嵌套子字段用 . 连接，如 store_name.pinyin.analyzer
	Kind string // added / removed / changed
	From any    // 线上值
	To   any    // 声明值
}

// Diff 一次判定的完整结果，字段全部只读语义；Reason 直接进日志与 --dry-run。
type Diff struct {
	Action       Action
	SettingsDiff []SettingChange
	MappingDiff  []FieldChange
	// NewProperties 仅含「线上还没有」的顶层新增字段子集，供 put_mapping 只发增量不重发全量。
	// 只有 Action == ActionInplace 时才有意义。
	NewProperties map[string]any
	Warnings      []string
	Reason        string

	// DeclaredFingerprint / LiveFingerprint 保留两侧指纹，migrate.go 打结构化日志时直接取用。
	DeclaredFingerprint string
	LiveFingerprint     string
}

// Decide 对比声明体与线上回读体，给出变更分级。
//
// liveMappings 传 admin.GetMapping 的返回值本身（该函数已拆封到内层 mappings，
// 即 {"dynamic":..., "properties":...} 这一层；本函数额外容忍仍带类型名包裹的形态）。
// liveSettings 传 admin.GetSettings(ctx, idx, false) 的返回值，即 flat_settings=true 的
// 扁平 map；由于本函数按「任意树拍平」处理，嵌套形态同样可比。
func Decide(def *IndexDefinition, liveMappings map[string]any, liveSettings map[string]any) *Diff {
	d := &Diff{Action: ActionNoop, NewProperties: make(map[string]any)}

	if def == nil {
		d.Action = ActionRebuild
		d.Reason = "未收到索引声明（nil Definition），无法判定，保守按必须重建处理"
		d.Warnings = append(d.Warnings, "调用方必须传入 Registry 里的 Definition")
		return d
	}

	d.DeclaredFingerprint = def.Fingerprint()
	// 回读体没有 _meta，只能用 FingerprintOf 现算，不构造假的 IndexDefinition。
	d.LiveFingerprint = FingerprintOf(liveSettings, liveMappings)

	if len(liveMappings) == 0 || len(liveSettings) == 0 {
		d.Action = ActionRebuild
		d.Reason = "线上回读体缺失（mappings 或 settings 为空），无法做增量判定"
		d.Warnings = append(d.Warnings, "索引不存在时应走首次创建流程，而不是 Decide")
		return d
	}

	// 指纹快路径：完全一致直接 Noop，省掉全部归一化开销。
	if d.DeclaredFingerprint == d.LiveFingerprint {
		d.Action = ActionNoop
		d.Reason = "结构指纹一致，无任何变更"
		return d
	}

	settingsRebuild := d.compareSettings(
		NormalizeSettings(def.SettingsMap()),
		NormalizeSettings(liveSettings),
	)
	mappingRebuild, inplaceMapping := d.compareMappings(def.MappingsMap(), liveMappings)

	d.summarize(settingsRebuild, mappingRebuild, inplaceMapping)
	return d
}

// InplacePutMappingJSON 生成 put_mapping 请求体，只含新增字段子集（strict 不阻止新增字段，
// 但绝不能重发全量：全量重发会把线上实际结构与声明文件的默认值差异当成指令下发）。
// 返回 false 表示没有可原地下发的 mapping 变更（Action != ActionInplace，或只有 settings 漂移）。
func (d *Diff) InplacePutMappingJSON() (string, bool) {
	if d == nil || d.Action != ActionInplace || len(d.NewProperties) == 0 {
		return "", false
	}
	body := map[string]any{"properties": d.NewProperties}
	b, err := jsonMarshal(body)
	if err != nil {
		return "", false
	}
	return b, true
}

// InplacePutSettingsJSON 生成 PUT _settings 请求体，只含命中动态白名单的变更项。
// 输出嵌套形态 {"index":{"a":{"b":v}}}：ES 同时接受扁平 dotted key，但嵌套是绝对安全写法。
// 动态项无需 close，调用方不要照搬 PHP 的无条件 close/open。
func (d *Diff) InplacePutSettingsJSON() (string, bool) {
	if d == nil || d.Action != ActionInplace || len(d.SettingsDiff) == 0 {
		return "", false
	}
	settings := map[string]any{}
	n := 0
	for _, c := range d.SettingsDiff {
		if !c.Inplace || (c.Kind != KindAdded && c.Kind != KindChanged) {
			continue
		}
		putNestedKey(settings, NormalizeSettingKey(c.Key), c.To)
		n++
	}
	if n == 0 {
		return "", false
	}
	b, err := jsonMarshal(map[string]any{"index": settings})
	if err != nil {
		return "", false
	}
	return b, true
}

// NormalizeSettings 把任意层级的 settings 树归一到「带 index. 前缀的扁平叶子 key」视图，
// 并剔除 ES 注入的只读 / 版本元数据与 defaults. 段。声明体与回读体走的是同一个函数，
// 这是「首次 migrate 判 Noop」的唯一保证；导出它是因为 migrate.go 打日志时需要展示归一化结果。
func NormalizeSettings(tree map[string]any) map[string]any {
	raw := make(map[string]any, len(tree)*2)
	flattenLeaves(tree, "", raw)

	out := make(map[string]any, len(raw))
	for k, v := range raw {
		if strings.HasPrefix(k, defaultsRawPrefix) {
			continue // include_defaults=true 的默认值段，不是声明要求
		}
		nk := NormalizeSettingKey(k)
		if isVolatileSetting(nk) {
			continue
		}
		out[nk] = v
	}
	return out
}

// ---------------------------------------------------------------------------
// settings diff
// ---------------------------------------------------------------------------

// compareSettings 返回是否存在「必须重建」级别的 settings 漂移。
func (d *Diff) compareSettings(declared, live map[string]any) bool {
	rebuild := false
	for _, key := range unionKeys(declared, live) {
		dv, inDecl := declared[key]
		lv, inLive := live[key]

		kind := ""
		switch {
		case inDecl && inLive:
			if valuesEquivalent(dv, lv) {
				continue
			}
			kind = KindChanged
		case inDecl:
			kind = KindAdded // 声明要求但线上没有 => 需要下发
		default:
			// 线上多出的 key 一律是 ES 回填的默认值或未登记注入项，忽略。
			// 反向（多出的 key 也算漂移）会让首次 migrate 必然误判重建。
			continue
		}

		c := SettingChange{Key: key, Kind: kind, From: lv, To: dv}
		if IsDynamicSetting(key) {
			c.Inplace = true
		} else {
			c.Rebuild = true
			rebuild = true
			if !IsStaticSetting(key) {
				c.Unknown = true
				d.Warnings = append(d.Warnings, fmt.Sprintf(
					"未知 settings key %s：既不在动态白名单也不在静态清单，保守按必须重建处理", key))
			}
		}
		d.SettingsDiff = append(d.SettingsDiff, c)
	}
	return rebuild
}

// flattenLeaves 把嵌套 JSON 树拍成「点分 key -> 叶子值」。
// 数组整体视为叶子：ES 的 index.sort.field / _source.excludes 都是数组值，
// 拆成 [0]/[1] 下标 key 会破坏顺序语义（sort.field 与 sort.order 必须按位对齐）。
func flattenLeaves(tree map[string]any, prefix string, out map[string]any) {
	for k, v := range tree {
		key := k
		if prefix != "" {
			key = prefix + "." + k
		}
		if child, ok := v.(map[string]any); ok {
			if len(child) == 0 {
				out[key] = v // 空对象本身就是叶子（例如声明了 "analysis": {}）
				continue
			}
			flattenLeaves(child, key, out)
			continue
		}
		out[key] = v
	}
}

// isVolatileSetting 判定归一化后的 key 是否为 ES 注入的只读 / 版本元数据。
func isVolatileSetting(key string) bool {
	base := strings.TrimPrefix(key, "index.")
	if base == "" {
		return false
	}
	segs := strings.Split(base, ".")
	if strings.HasPrefix(segs[len(segs)-1], "_") {
		return true // ES 内部注入段，典型是 8.x 的 routing.allocation.include._tier_preference
	}
	for _, name := range volatileSettingNames {
		if _, registered := bareTopLevelSettings[name]; !registered && !isDeclaredVolatile(name) {
			continue // 已登记的沿用定义层清单，未登记的才是本层补全的 8.x 注入项
		}
		if base == name || strings.HasPrefix(base, name+".") {
			return true // index.version.created 这类子 key 一并剔除
		}
	}
	for _, p := range volatileSettingPrefixes {
		if strings.HasPrefix(base, p) {
			return true
		}
	}
	return false
}

// isDeclaredVolatile 标出「本层补全、尚未回写进定义层 bareTopLevelSettings」的注入项，
// 保持与定义层清单的边界清晰可审。
func isDeclaredVolatile(name string) bool {
	switch name {
	case "analyzer_version", "history", "reserved":
		return true
	default:
		return false
	}
}

// ---------------------------------------------------------------------------
// mappings diff
// ---------------------------------------------------------------------------

// mappingLeaf 一个 mapping 结构叶子。structural=true 表示「字段存在性」（单侧缺失即结构差异），
// false 表示字段参数（单侧缺失按 ES 未回填默认值处理，不判为变更）。
//
// 后者是真实回读样本里坐实的一条：线上 store_name 不回读 index_options（positions 就是 text 的
// 默认值）、fields.pinyin 也没有 search_analyzer（缺省继承 analyzer）。把「声明写了、线上省略」
// 判成漂移会直接误触发重建，所以参数只在两侧都存在时才比。
type mappingLeaf struct {
	token      string
	structural bool
}

const existsSuffix = "#exists"

// compareMappings 返回（是否必须重建，是否存在可原地的 mapping 变更）。
func (d *Diff) compareMappings(declaredRaw, liveRaw map[string]any) (bool, bool) {
	declared := unwrapMappingRoot(declaredRaw)
	live := unwrapMappingRoot(liveRaw)
	rebuild, inplace := false, false

	// 顶层护栏：dynamic 与 _source.excludes 的「缺失」本身就是语义，单侧存在必须算变更。
	declDyn, hasDeclDyn := scalarLookup(declared, "dynamic")
	liveDyn, hasLiveDyn := scalarLookup(live, "dynamic")
	if hasDeclDyn != hasLiveDyn || declDyn != liveDyn {
		d.MappingDiff = append(d.MappingDiff, FieldChange{
			Path: "dynamic", Kind: KindChanged, From: liveDyn, To: declDyn,
		})
		rebuild = true
	}

	declEx := excludesToken(declared)
	liveEx := excludesToken(live)
	if declEx != liveEx {
		d.MappingDiff = append(d.MappingDiff, FieldChange{
			Path: "_source.excludes", Kind: KindChanged, From: liveEx, To: declEx,
		})
		rebuild = true
	}

	declProps, _ := objectValue(declared, "properties")
	liveProps, _ := objectValue(live, "properties")

	for _, name := range unionKeys(declProps, liveProps) {
		dv, inDecl := declProps[name]
		lv, inLive := liveProps[name]
		switch {
		case inDecl && !inLive:
			// 仅新增字段：strict 只拒绝写未声明字段，不阻止新增，put_mapping 发这个子集即可。
			d.NewProperties[name] = dv
			d.MappingDiff = append(d.MappingDiff, FieldChange{Path: name, Kind: KindAdded, To: dv})
			inplace = true
		case !inDecl && inLive:
			// 字段删除：已建倒排无法就地摘除，且存量数据会残留，必须重建。
			d.MappingDiff = append(d.MappingDiff, FieldChange{Path: name, Kind: KindRemoved, From: lv})
			rebuild = true
		default:
			dm, lm := map[string]mappingLeaf{}, map[string]mappingLeaf{}
			writeMappingLeaves(name, dv, dm)
			writeMappingLeaves(name, lv, lm)
			for _, key := range unionKeys(dm, lm) {
				a, okA := dm[key]
				b, okB := lm[key]
				switch {
				case okA && okB:
					if a.token == b.token {
						continue
					}
					d.MappingDiff = append(d.MappingDiff, FieldChange{
						Path: displayPath(key), Kind: KindChanged, From: b.token, To: a.token,
					})
					rebuild = true
				case okA:
					if !a.structural {
						continue // 参数单侧缺失 = ES 默认值，忽略
					}
					// 已有字段内部的子字段新增：put_mapping 的子结构合并语义不可靠，按重建处理。
					d.MappingDiff = append(d.MappingDiff, FieldChange{
						Path: displayPath(key), Kind: KindAdded, To: a.token,
					})
					rebuild = true
				default:
					if !b.structural {
						continue
					}
					d.MappingDiff = append(d.MappingDiff, FieldChange{
						Path: displayPath(key), Kind: KindRemoved, From: b.token,
					})
					rebuild = true
				}
			}
		}
	}
	return rebuild, inplace
}

// writeMappingLeaves 递归展开一个字段规格（含 fields / properties 子结构）为可比对的叶子集合。
func writeMappingLeaves(path string, spec any, out map[string]mappingLeaf) {
	obj, ok := spec.(map[string]any)
	if !ok {
		out[path+existsSuffix] = mappingLeaf{token: valueToken(spec), structural: true}
		return
	}
	out[path+existsSuffix] = mappingLeaf{token: valueToken(obj["type"]), structural: true}

	for _, p := range mappingFieldParams {
		if v, present := obj[p]; present {
			out[path+"."+p] = mappingLeaf{token: valueToken(v)}
		}
	}
	for _, c := range mappingContainers {
		sub, ok := obj[c].(map[string]any)
		if !ok {
			continue
		}
		for name, child := range sub {
			writeMappingLeaves(path+"."+name, child, out)
		}
	}
}

// unwrapMappingRoot 容忍两种形态：已拆封的内层 mappings，以及 { "<type>": {...} } 包裹。
func unwrapMappingRoot(m map[string]any) map[string]any {
	if _, ok := m["properties"]; ok {
		return m
	}
	if _, ok := m["dynamic"]; ok {
		return m
	}
	if len(m) != 1 {
		return m
	}
	for _, v := range m {
		if inner, ok := v.(map[string]any); ok {
			if _, has := inner["properties"]; has {
				return inner
			}
		}
	}
	return m
}

// displayPath 把内部 key 转成人读路径：existence key 去掉标记，其余原样。
func displayPath(key string) string {
	return strings.TrimSuffix(key, existsSuffix)
}

// excludesToken _source.excludes 的稳定指纹串（集合语义：顺序不参与判定，
// 否则仅调整 excludes 书写顺序就会误触发重建）。缺失返回 "<none>"。
func excludesToken(mappings map[string]any) string {
	src, ok := objectValue(mappings, "_source")
	if !ok {
		return "<none>"
	}
	raw, present := src["excludes"]
	if !present {
		return "<none>"
	}
	items := sequenceTokens(raw)
	if len(items) == 0 {
		return "<none>"
	}
	sorted := append([]string(nil), items...)
	sort.Strings(sorted)
	return strings.Join(sorted, ",")
}

// ---------------------------------------------------------------------------
// 判定汇总
// ---------------------------------------------------------------------------

// summarize 定级并生成人读 Reason。
func (d *Diff) summarize(settingsRebuild, mappingRebuild, inplaceMapping bool) {
	switch {
	case settingsRebuild || mappingRebuild:
		d.Action = ActionRebuild
	case len(d.SettingsDiff) > 0 || inplaceMapping:
		d.Action = ActionInplace
	default:
		d.Action = ActionNoop
	}

	if d.Action == ActionNoop && len(d.SettingsDiff) == 0 && len(d.MappingDiff) == 0 {
		// 指纹不等但 diff 为空：定义层指纹不含 _meta，只会是版本号/元数据更新。
		d.Reason = "结构指纹不等但归一化 diff 为空：仅版本号/元数据更新，结构无需变更"
		d.Warnings = append(d.Warnings, "仅版本号/元数据更新（_meta 不计入指纹）")
		return
	}

	var sb strings.Builder
	sb.WriteString("判定 ")
	sb.WriteString(d.Action.String())
	if n := len(d.SettingsDiff); n > 0 {
		sb.WriteString("；settings 漂移 ")
		sb.WriteString(strconv.Itoa(n))
		sb.WriteString(" 项: ")
		sb.WriteString(joinCapped(d.settingsDetails(), reasonDetailLimit))
	}
	if n := len(d.MappingDiff); n > 0 {
		sb.WriteString("；mappings 漂移 ")
		sb.WriteString(strconv.Itoa(n))
		sb.WriteString(" 项: ")
		sb.WriteString(joinCapped(d.mappingDetails(), reasonDetailLimit))
	}
	if len(d.NewProperties) > 0 {
		sb.WriteString("；put_mapping 仅发新增字段子集 ")
		sb.WriteString(strconv.Itoa(len(d.NewProperties)))
		sb.WriteString(" 个")
	}
	d.Reason = sb.String()
}

// reasonDetailLimit Reason 里每类明细最多列举的条数，避免 15 分片索引全字段漂移时刷屏。
const reasonDetailLimit = 6

func (d *Diff) settingsDetails() []string {
	out := make([]string, 0, len(d.SettingsDiff))
	for _, c := range d.SettingsDiff {
		tag := "动态可原地"
		switch {
		case c.Unknown:
			tag = "未知项保守重建"
		case c.Rebuild:
			tag = "静态必须重建"
		}
		out = append(out, fmt.Sprintf("[%s] %s(%s: %s -> %s)", tag, c.Key, c.Kind,
			scalarToken(c.From), scalarToken(c.To)))
	}
	return out
}

func (d *Diff) mappingDetails() []string {
	out := make([]string, 0, len(d.MappingDiff))
	for _, c := range d.MappingDiff {
		out = append(out, fmt.Sprintf("%s(%s: %v -> %v)", c.Path, c.Kind,
			scalarToken(c.From), scalarToken(c.To)))
	}
	return out
}

func joinCapped(items []string, limit int) string {
	if len(items) == 0 {
		return "无"
	}
	if len(items) <= limit {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:limit], ", ") + fmt.Sprintf(" 等 %d 项", len(items))
}

// ---------------------------------------------------------------------------
// 值归一化与通用工具
// ---------------------------------------------------------------------------

// scalarToken 把任意 JSON 标量归一成可跨「声明体 / 回读体」比较的字符串。
// ES 回读把所有标量写成字符串（number_of_shards 是 "15"），声明体是 json.Number 15，
// 不统一就会把「同一个值」比成漂移。规则：能整体解析为数字的一律按数字规范化。
func scalarToken(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case bool:
		if t {
			return "true"
		}
		return "false"
	case string:
		return scalarTextToken(t)
	case json.Number:
		return scalarTextToken(t.String())
	case float64:
		return numberToken(t)
	case float32:
		return numberToken(float64(t))
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case int8:
		return strconv.FormatInt(int64(t), 10)
	default:
		return string(CanonicalJSON(v))
	}
}

func scalarTextToken(s string) string {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return s
	}
	// 只有「整个字符串都是一个数」才按数字归一，避免把 "1s" / "500ms" 误伤。
	if f, err := strconv.ParseFloat(trimmed, 64); err == nil && isPlainNumber(trimmed) {
		return numberToken(f)
	}
	return s
}

// isPlainNumber 排除 "1e5" 之外 ES 不会这样回读的写法以及 hex/inf 等 ParseFloat 宽松形态。
func isPlainNumber(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= '0' && c <= '9') || c == '-' || c == '+' || c == '.' {
			continue
		}
		return false
	}
	return true
}

func numberToken(f float64) string {
	if f == math.Trunc(f) && !math.IsInf(f, 0) && math.Abs(f) < 1e15 {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// sequenceTokens 数组逐元素归一；非数组视为单元素序列。
func sequenceTokens(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return []string{scalarToken(v)}
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		out = append(out, scalarToken(e))
	}
	return out
}

// valueToken mapping 参数值的归一写法：声明体写 copy_to:"desc"，ES 回读成 copy_to:["desc"]，
// 两者是同一个设置，按序列拼接后必须相等（否则每次 migrate 都会误判 copy_to 变更而重建）。
func valueToken(v any) string {
	return strings.Join(sequenceTokens(v), ",")
}

// valuesEquivalent 比较一个 settings 叶子的值。数组按位序比较
// （index.sort.field 与 sort.order 的顺序本身就是语义）。
func valuesEquivalent(a, b any) bool {
	ta, tb := sequenceTokens(a), sequenceTokens(b)
	if len(ta) != len(tb) {
		return false
	}
	for i := range ta {
		if ta[i] != tb[i] {
			return false
		}
	}
	return true
}

func scalarLookup(m map[string]any, key string) (string, bool) {
	v, ok := m[key]
	if !ok {
		return "", false
	}
	if s, isStr := v.(string); isStr {
		return s, true
	}
	return scalarToken(v), true
}

func objectValue(m map[string]any, key string) (map[string]any, bool) {
	obj, ok := m[key].(map[string]any)
	return obj, ok
}

// unionKeys 两个 map 的 key 并集，字典序（保证 Reason 与 diff 明细可预期）。
func unionKeys[V any](a, b map[string]V) []string {
	set := make(map[string]struct{}, len(a)+len(b))
	for k := range a {
		set[k] = struct{}{}
	}
	for k := range b {
		set[k] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// putNestedKey 把 dotted key 展开成嵌套对象；剥掉 index. 前缀（调用方已单独包一层 index）。
func putNestedKey(root map[string]any, key string, value any) {
	key = strings.TrimPrefix(key, "index.")
	parts := strings.Split(key, ".")
	cur := root
	for i, p := range parts {
		if i == len(parts)-1 {
			cur[p] = value
			continue
		}
		next, ok := cur[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[p] = next
		}
		cur = next
	}
}

// jsonMarshal 紧凑 JSON，数字走 json.Number 的原样字面量。
func jsonMarshal(v any) (string, error) {
	b, err := marshalCompactString(v)
	if err != nil {
		return "", err
	}
	return b, nil
}
