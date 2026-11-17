package main

// es_status.go 是 es:* 命令族唯一的只读体检入口，Phase 6 补全为完整实现：
// 集群连通与版本、插件、集群健康、每个 Registry 登记别名的存在形态与指向、
// 各物理索引的 doc 数/分片/健康/store.size、schema_version（_meta 优先、尾缀回退）、
// 声明体指纹与线上结构的 Decide 判定，以及 ES 与 MySQL 的商品计数差异。
//
// 只读纪律：本文件只允许调用 Cluster 的 GET/HEAD 方法与 ClusterHealth/Ping，
// 不得出现任何 Create/PutSettings/Refresh/SwitchAlias 之类的写调用。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"shop/internal/es"
	"shop/internal/es/admin"
	"shop/internal/es/sync"
	"shop/pkg/global"

	"github.com/spf13/cobra"
)

var esStatusCmd = &cobra.Command{
	Use:   "es:status",
	Short: "检查 Elasticsearch 集群、别名指向与索引结构/数据一致性（只读）",
	Long: "只读体检：不改任何索引、不动任何别名、不改任何 settings。\n" +
		"\n" +
		"报告内容：\n" +
		"  1) ES 连通性与版本（ping：集群名 / 节点名 / ES 版本 / Lucene 版本）与集群健康；\n" +
		"  2) 已安装插件清单，并逐声明点名缺失的 requires_plugins；\n" +
		"  3) 每个 Registry 登记的别名：是别名 / 仍是物理索引（未割接）/ 不存在，" +
		"别名指向哪些物理索引、谁是写索引（is_write_index 显式或隐式）；\n" +
		"  4) 相关物理索引的 doc 数、主/副分片数、health、store.size；\n" +
		"  5) schema_version：优先读 mappings._meta，读不到再回退物理索引名尾缀 _v<N>；\n" +
		"  6) 声明体指纹 vs 线上结构指纹，直接给出 noop / inplace / rebuild 判定与原因；\n" +
		"  7) ES _count 与 MySQL 期望数（口径 is_del=0 AND is_show=1）的差异。\n" +
		"\n" +
		"退出码：只有取数失败（连不上 ES、读回显报错、别名未登记等）才返回 1；" +
		"结构漂移、计数差异、别名未割接都属于「如实报告」，不改退出码，便于人工与脚本共用。",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runESStatus(cmd)
	},
}

// esStatusSpec 是 es:status 的 flags 快照。
type esStatusSpec struct {
	Alias      string // 空 => 检查 Registry 里全部别名
	IndicesDir string
	AsJSON     bool
	Tolerance  int64
	Timeout    time.Duration
}

// esStatusReport 完整体检结论，--json 直接序列化它。
type esStatusReport struct {
	GeneratedAt  string            `json:"generated_at"`
	Hosts        []string          `json:"hosts"`
	Conn         esConnInfo        `json:"elasticsearch"`
	Health       esClusterHealth   `json:"cluster_health"`
	Plugins      []string          `json:"plugins"`
	PluginErrors string            `json:"plugin_error,omitempty"`
	Aliases      []esAliasStatus   `json:"aliases"`
	Extras       map[string]string `json:"extras,omitempty"`
}

type esConnInfo struct {
	Connected     bool   `json:"connected"`
	ClusterName   string `json:"cluster_name"`
	NodeName      string `json:"node_name"`
	Version       string `json:"version"`
	LuceneVersion string `json:"lucene_version"`
}

// esClusterHealth 单节点集群恒为 yellow（副本无处可放），属正常态：
// 本结构只如实记录，任何判定都不把 yellow 当故障。
type esClusterHealth struct {
	Available           bool   `json:"available"`
	Status              string `json:"status"`
	NumberOfNodes       int    `json:"number_of_nodes"`
	NumberOfDataNodes   int    `json:"number_of_data_nodes"`
	ActivePrimaryShards int    `json:"active_primary_shards"`
	ActiveShards        int    `json:"active_shards"`
	UnassignedShards    int    `json:"unassigned_shards"`
	RelocatingShards    int    `json:"relocating_shards"`
	InitializingShards  int    `json:"initializing_shards"`
	PendingTasks        int    `json:"pending_tasks"`
	TimedOut            bool   `json:"timed_out"`
	Error               string `json:"error,omitempty"`
}

// esAliasStatus 一个登记别名的完整画像。
type esAliasStatus struct {
	Alias           string `json:"alias"`
	Definition      string `json:"definition_source"`
	DeclaredVersion int    `json:"declared_schema_version"`
	DeclaredStatus  string `json:"declared_status"`

	// Kind 三态：alias（已割接/已由 migrate 建好）、physical_index（别名位仍被物理索引占据，
	// 即 es:cutover 的适用对象）、missing（两边都没有）。
	Kind        string           `json:"kind"`
	AliasExists bool             `json:"alias_exists"`
	Targets     []string         `json:"targets,omitempty"`
	WriteIndex  string           `json:"write_index,omitempty"`
	WriteNote   string           `json:"write_index_note,omitempty"`
	Rows        []es.AliasTarget `json:"alias_rows,omitempty"`

	RequiresPlugins []string `json:"requires_plugins,omitempty"`
	MissingPlugins  []string `json:"missing_plugins,omitempty"`

	Indices []esIndexDetail `json:"indices"`

	CountComparable bool   `json:"count_comparable"`
	CountScope      string `json:"count_scope,omitempty"`
	DBExpected      int64  `json:"db_expected"`
	ESCount         int64  `json:"es_count"`
	CountDiff       int64  `json:"count_diff"`
	CountOK         bool   `json:"count_ok"`
	CountError      string `json:"count_error,omitempty"`

	Errors []string `json:"errors,omitempty"`
}

// esIndexDetail 单个物理索引的运行时指标 + 结构一致性判定。
type esIndexDetail struct {
	Name                string   `json:"name"`
	Exists              bool     `json:"exists"`
	Health              string   `json:"health,omitempty"`
	Status              string   `json:"status,omitempty"`
	Primaries           int      `json:"pri"`
	Replicas            int      `json:"rep"`
	DocsCount           int64    `json:"docs_count"`
	StoreSize           string   `json:"store_size,omitempty"`
	SchemaVersion       int      `json:"schema_version"`
	SchemaVersionSource string   `json:"schema_version_source"`
	DeclaredFingerprint string   `json:"declared_fingerprint"`
	LiveFingerprint     string   `json:"live_fingerprint"`
	Action              string   `json:"decide_action"`
	Reason              string   `json:"reason,omitempty"`
	SettingsDrift       int      `json:"settings_drift_count"`
	MappingDrift        int      `json:"mapping_drift_count"`
	Warnings            []string `json:"warnings,omitempty"`
	Errors              []string `json:"errors,omitempty"`
}

// 口径表不在本文件自存一份：“别名 → DB 期望数”唯一事实源是 sync 包的 dbCounters
// （internal/es/sync/registry_provider.go），migrate/rebuild/cutover 的对账与本命令的
// ES/DB 计数比对共用同一张表。只有登记过的口径才参与比对；拿不到时如实报
// count_comparable=false，绝不拿商品口径去套别的索引（口径错比对「没比」更有害）。
// 新增索引时只需在那张表里补一条，status 层不需跟着改。

func init() {
	f := esStatusCmd.Flags()
	f.String("alias", "", "只检查指定别名；留空表示检查 Registry 里全部登记项")
	f.String("indices-dir", "", "索引声明目录（空 → 内置默认）")
	f.Bool("json", false, "以 JSON 格式输出完整体检结论")
	f.Int64("tolerance", 0, "计数差异判定容差：|ES - DB| > 该值即 count_ok=false")
	f.Duration("timeout", 30*time.Second, "只读检查的整体超时")

	esRootCmd.AddCommand(esStatusCmd)
}

func parseESStatusSpec(cmd *cobra.Command) (esStatusSpec, error) {
	f := cmd.Flags()
	s := esStatusSpec{}
	s.Alias, _ = f.GetString("alias")
	s.IndicesDir, _ = f.GetString("indices-dir")
	s.AsJSON, _ = f.GetBool("json")
	s.Tolerance, _ = f.GetInt64("tolerance")
	s.Timeout, _ = f.GetDuration("timeout")
	s.Alias = strings.TrimSpace(s.Alias)
	if s.Timeout <= 0 {
		return s, fmt.Errorf("--timeout 必须大于 0")
	}
	if s.Tolerance < 0 {
		return s, fmt.Errorf("--tolerance 不能为负")
	}
	return s, nil
}

func runESStatus(cmd *cobra.Command) error {
	// enabled=false 时 initES 会跳过初始化，此处正常退出而非报错
	if !global.CONFIG.Elasticsearch.Enabled {
		fmt.Println("Elasticsearch 未启用（elasticsearch.enabled=false），跳过状态检查")
		return nil
	}
	spec, err := parseESStatusSpec(cmd)
	if err != nil {
		return err
	}
	// 1. 确认全局客户端已初始化（未初始化时返回带指引的 error，不 panic）
	if _, err := admin.Client(); err != nil {
		return err
	}
	rawCli, err := admin.Raw()
	if err != nil {
		return err
	}
	hosts := global.CONFIG.Elasticsearch.Hosts
	if len(hosts) == 0 {
		return fmt.Errorf("ES 配置缺少节点地址（elasticsearch.hosts 为空）")
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), spec.Timeout)
	defer cancel()

	report := &esStatusReport{GeneratedAt: time.Now().Format(time.RFC3339), Hosts: hosts}

	// 2. Ping 确认集群可达，顺带取节点名与 ES 版本
	// olivere v7 的 Ping(url) 会直接用 url 构造绝对请求：传空串会被 TrimSuffix 退化成
	// "GET /"（无 scheme/host），触发 "unsupported protocol scheme"，必须传入带 scheme 的节点地址。
	ping, _, err := rawCli.Ping(hosts[0]).Do(ctx)
	if err != nil {
		return fmt.Errorf("ES 集群 ping 失败（hosts=%v）: %w", hosts, err)
	}
	if ping == nil {
		return fmt.Errorf("ES 集群不可达（%s 无响应体）", hosts[0])
	}

	report.Conn = esConnInfo{
		Connected:     true,
		ClusterName:   ping.ClusterName,
		NodeName:      ping.Name,
		Version:       ping.Version.Number,
		LuceneVersion: ping.Version.LuceneVersion,
	}

	// 3. 集群健康（整体；单节点 yellow 属正常态，只记录不判故障）
	if h, herr := rawCli.ClusterHealth().Do(ctx); herr != nil {
		report.Health = esClusterHealth{Available: false, Error: herr.Error()}
	} else if h != nil {
		report.Health = esClusterHealth{
			Available: true, Status: h.Status,
			NumberOfNodes: h.NumberOfNodes, NumberOfDataNodes: h.NumberOfDataNodes,
			ActivePrimaryShards: h.ActivePrimaryShards, ActiveShards: h.ActiveShards,
			UnassignedShards: h.UnassignedShards, RelocatingShards: h.RelocatingShards,
			InitializingShards: h.InitializingShards, PendingTasks: h.NumberOfPendingTasks,
			TimedOut: h.TimedOut,
		}
	}

	cluster := es.NewAdminCluster()

	// 4. 插件清单（_cat/plugins）。读不到不影响后面的判定，如实记 error。
	plugins, perr := cluster.ListPlugins(ctx)
	if perr != nil {
		report.PluginErrors = perr.Error()
	}
	report.Plugins = plugins

	// 5. 别名全量映射：一次 _cat/aliases，逐别名分拣
	rows, aerr := cluster.ListAliasRows(ctx)
	if aerr != nil {
		// 别名指向是本命令的主结论，拿不到就没有报告价值
		return aerr
	}

	// 6. 逐个登记别名出结论
	defs, err := selectDefinitionsForStatus(spec)
	if err != nil {
		return err
	}
	for _, def := range defs {
		report.Aliases = append(report.Aliases, buildAliasStatus(ctx, cluster, def, rows, plugins, spec))
	}

	if spec.AsJSON {
		out, merr := json.MarshalIndent(report, "", "  ")
		if merr != nil {
			return fmt.Errorf("状态结论序列化失败: %w", merr)
		}
		fmt.Println(string(out))
		return nil
	}
	printESStatus(report, spec)
	return nil
}

// selectDefinitionsForStatus 按 --alias 过滤 Registry 登记项，输出顺序固定按别名升序，
// 让人类可读输出与 --json 都可 diff。
func selectDefinitionsForStatus(spec esStatusSpec) ([]*es.IndexDefinition, error) {
	reg, err := es.LoadRegistry(spec.IndicesDir)
	if err != nil {
		return nil, fmt.Errorf("加载索引声明失败: %w", err)
	}
	if reg.Len() == 0 {
		return nil, fmt.Errorf("索引声明目录里没有任何登记项（indices-dir=%q）", spec.IndicesDir)
	}
	if spec.Alias != "" {
		def, ok := reg.Get(spec.Alias)
		if !ok {
			return nil, fmt.Errorf("别名 %s 没有登记的索引声明（已登记别名：%v）", spec.Alias, reg.Aliases())
		}
		return []*es.IndexDefinition{def}, nil
	}
	defs := reg.All()
	sort.Slice(defs, func(i, j int) bool { return defs[i].Alias() < defs[j].Alias() })
	return defs, nil
}

// buildAliasStatus 汇总单个别名的存在形态、指向、物理索引指标与 ES/DB 计数差异。
func buildAliasStatus(ctx context.Context, cluster es.Cluster, def *es.IndexDefinition,
	rows []es.AliasTarget, plugins []string, spec esStatusSpec) esAliasStatus {

	alias := def.Alias()
	st := esAliasStatus{
		Alias:           alias,
		Definition:      def.Source(),
		DeclaredVersion: def.Version(),
		DeclaredStatus:  def.Status(),
		RequiresPlugins: def.RequiresPlugins(),
		MissingPlugins:  es.MissingPlugins(plugins, def.RequiresPlugins()),
	}
	for _, r := range rows {
		if r.Alias == alias {
			st.Rows = append(st.Rows, r)
			st.Targets = append(st.Targets, r.Index)
		}
	}
	st.AliasExists = len(st.Rows) > 0

	// 形态判定：互斥命名让「别名 / 物理索引 / 什么都没有」三态可以只靠读请求分清。
	switch {
	case st.AliasExists:
		st.Kind = "alias"
		if idx, note, ok := es.ResolveWriteIndex(rows, alias); ok {
			st.WriteIndex, st.WriteNote = idx, note
		} else {
			st.WriteNote = note
			st.Errors = append(st.Errors, "写索引判定失败: "+note)
		}
	default:
		nameHit, err := cluster.IndexExists(ctx, alias)
		if err != nil {
			st.Errors = append(st.Errors, fmt.Sprintf("HEAD /%s 失败: %v", alias, err))
			st.Kind = "unknown"
		} else if nameHit {
			st.Kind = "physical_index" // 未割接：别名位还被物理索引占了
			st.WriteIndex = alias
			st.WriteNote = "物理索引直连读写（尚未割接为别名）"
		} else {
			st.Kind = "missing"
		}
	}

	for _, name := range st.Targets {
		st.Indices = append(st.Indices, describePhysicalIndex(ctx, cluster, def, name))
	}
	if st.Kind == "physical_index" {
		st.Indices = append(st.Indices, describePhysicalIndex(ctx, cluster, def, alias))
	}
	if st.Kind == "missing" {
		// 顺带把声明里应当存在的目标索引名探一下，便于区分「首次部署前」与「被误删」
		detail := describePhysicalIndex(ctx, cluster, def, def.Name())
		if detail.Exists {
			st.Indices = append(st.Indices, detail)
			st.Errors = append(st.Errors, fmt.Sprintf("别名不存在但 %s 已存在：别名映射缺失", def.Name()))
		}
	}

	// ES 与 DB 计数比对：只对登记过口径的别名做。
	counter, comparable := sync.DBCounterFor(alias)
	st.CountComparable = comparable
	if !comparable {
		st.CountScope = "该别名未登记 DB 计数口径，跳过 ES/DB 比对"
		return st
	}
	expected, cerr := counter(ctx)
	if cerr != nil {
		st.CountError = cerr.Error()
		st.Errors = append(st.Errors, "DB 期望数统计失败: "+cerr.Error())
		return st
	}
	st.DBExpected = expected
	if st.WriteIndex == "" {
		st.CountScope = "无写索引，ES 计数不可得"
		return st
	}
	esCount, cerr := cluster.Count(ctx, st.WriteIndex)
	if cerr != nil {
		st.CountError = cerr.Error()
		st.Errors = append(st.Errors, fmt.Sprintf("GET /%s/_count 失败: %v", st.WriteIndex, cerr))
		return st
	}
	st.ESCount = esCount
	diff, ok := es.ReconcileCounts(expected, esCount, spec.Tolerance)
	st.CountDiff, st.CountOK = diff, ok
	st.CountScope = fmt.Sprintf("MySQL 口径 is_del=0 AND is_show=1 vs %s/_count", st.WriteIndex)
	return st
}

// describePhysicalIndex 读单个物理索引的运行时指标与线上结构，给出 Decide 判定。
// 任何一步失败都只落到 Errors/存在性标记上，不让整条命令挂掉。
func describePhysicalIndex(ctx context.Context, cluster es.Cluster, def *es.IndexDefinition, name string) esIndexDetail {
	d := esIndexDetail{Name: name, DeclaredFingerprint: def.Fingerprint(), Action: "unknown"}

	stats, err := cluster.IndexStats(ctx, name)
	if err != nil {
		d.ErrorsAppend("GET /_cat/indices 失败: " + err.Error())
	}
	for _, s := range stats {
		if s.Index == name {
			d.Exists = true
			d.Health, d.Status = s.Health, s.Status
			d.Primaries, d.Replicas = s.Primaries, s.Replicas
			d.DocsCount, d.StoreSize = s.DocsCount, s.StoreSize
			break
		}
	}

	liveMappings, merr := cluster.GetMapping(ctx, name)
	if merr != nil {
		d.ErrorsAppend("GET /_mapping 失败: " + merr.Error())
	}
	liveSettings, serr := cluster.GetSettings(ctx, name)
	if serr != nil {
		d.ErrorsAppend("GET /_settings 失败: " + serr.Error())
	}

	// _meta 优先、尾缀回退；admin.GetMapping 在索引不存在时返回 (nil, nil)，
	// 此时版本号只能来自尾缀，两者都没有就报 unknown 而不是猜。
	d.SchemaVersion, d.SchemaVersionSource = es.ResolveSchemaVersion(liveMappings, name)

	diff := es.Decide(def, liveMappings, liveSettings)
	if len(liveMappings) == 0 || len(liveSettings) == 0 {
		d.Action = "unavailable"
		d.Reason = "线上回读体缺失（索引不存在或 mapping/settings 读取失败），未做指纹比对"
		if diff != nil {
			d.LiveFingerprint = diff.LiveFingerprint
		}
		return d
	}
	d.Action = diff.Action.String()
	d.Reason = diff.Reason
	d.LiveFingerprint = diff.LiveFingerprint
	d.SettingsDrift = len(diff.SettingsDiff)
	d.MappingDrift = len(diff.MappingDiff)
	d.Warnings = diff.Warnings
	return d
}

// ErrorsAppend 把读失败记进索引明细，供人类输出与 JSON 排查。
// （单索引的一条读失败不该让整份体检报告消失。）
func (d *esIndexDetail) ErrorsAppend(msg string) {
	if d.Errors == nil {
		d.Errors = []string{msg}
		return
	}
	d.Errors = append(d.Errors, msg)
}

// printESStatus 输出人类可读体检报告。
func printESStatus(r *esStatusReport, spec esStatusSpec) {
	w := os.Stdout
	line := func(format string, v ...any) { fmt.Fprintf(w, format+"\n", v...) }

	line("")
	line("========== Elasticsearch 状态 ==========")
	line("连接地址:     %v", r.Hosts)
	line("集群/节点:    %s / %s", r.Conn.ClusterName, r.Conn.NodeName)
	line("ES 版本:      %s（Lucene %s）", r.Conn.Version, r.Conn.LuceneVersion)
	if r.Health.Available {
		line("集群健康:     %s（节点 %d/%d，主分片 %d，活跃分片 %d，未分配 %d，迁移 %d，初始化 %d，待办 %d）",
			r.Health.Status, r.Health.NumberOfNodes, r.Health.NumberOfDataNodes,
			r.Health.ActivePrimaryShards, r.Health.ActiveShards, r.Health.UnassignedShards,
			r.Health.RelocatingShards, r.Health.InitializingShards, r.Health.PendingTasks)
		if strings.EqualFold(r.Health.Status, "yellow") {
			line("              ↑ 单节点集群副本无处可放，yellow 属正常态")
		}
	} else {
		line("集群健康:     读取失败 —— %s", r.Health.Error)
	}
	if len(r.Plugins) > 0 {
		line("已装插件:     %v", r.Plugins)
	} else {
		line("已装插件:     无（读取错误：%s）", r.PluginErrors)
	}

	for _, a := range r.Aliases {
		line("")
		line("---------- 别名 %s ----------", a.Alias)
		line("声明:         %s（schema_version=%d，status=%s）", a.Definition, a.DeclaredVersion, a.DeclaredStatus)
		switch a.Kind {
		case "alias":
			line("存在形态:     别名（已割接/已建好）")
		case "physical_index":
			line("存在形态:     物理索引 —— 别名位仍被同名索引占据，需执行 es:cutover 割接")
		case "missing":
			line("存在形态:     不存在（既无别名也无同名索引）")
		default:
			line("存在形态:     未知（读取失败，见下）")
		}
		if len(a.Rows) > 0 {
			for _, row := range a.Rows {
				line("别名指向:     %s -> %s（is_write_index=%s）", row.Alias, row.Index, displayWriteFlag(row.WriteIndex))
			}
			line("写索引:       %s（%s）", orDash(a.WriteIndex), a.WriteNote)
		} else if a.WriteIndex != "" {
			line("读写目标:     %s（%s）", a.WriteIndex, a.WriteNote)
		}
		if len(a.MissingPlugins) > 0 {
			line("缺失插件:     %v（声明要求 %v，已装 %v）", a.MissingPlugins, a.RequiresPlugins, r.Plugins)
		} else if len(a.RequiresPlugins) > 0 {
			line("插件校验:     声明要求 %v 全部在位", a.RequiresPlugins)
		}

		for _, idx := range a.Indices {
			line("  · %s", idx.Name)
			if idx.Exists {
				line("      doc/分片: %d 条，pri=%d rep=%d，health=%s，store=%s",
					idx.DocsCount, idx.Primaries, idx.Replicas, idx.Health, idx.StoreSize)
			} else {
				line("      线上不存在（_cat/indices 无此条目）")
			}
			line("      schema:   v%d（来源 %s）", idx.SchemaVersion, idx.SchemaVersionSource)
			line("      指纹:     声明 %s / 线上 %s", shortFP(idx.DeclaredFingerprint), shortFP(idx.LiveFingerprint))
			line("      判定:     %s（settings 漂移 %d 项，mapping 漂移 %d 项）", idx.Action, idx.SettingsDrift, idx.MappingDrift)
			if idx.Reason != "" {
				line("      原因:     %s", idx.Reason)
			}
			for _, warn := range idx.Warnings {
				line("      警告:     %s", warn)
			}
			for _, e := range idx.Errors {
				line("      读取异常: %s", e)
			}
		}

		if a.CountComparable {
			if a.CountError != "" {
				line("计数对账:     失败 —— %s", a.CountError)
			} else {
				line("计数对账:     DB=%d  ES=%d  差=%+d  达标=%v（容差 %d）",
					a.DBExpected, a.ESCount, a.CountDiff, a.CountOK, spec.Tolerance)
				line("              口径：%s", a.CountScope)
			}
		} else {
			line("计数对账:     跳过 —— %s", a.CountScope)
		}
		for _, e := range a.Errors {
			line("异常:         %s", e)
		}
	}
	line("")
	line("========================================")
}

func displayWriteFlag(v string) string {
	if strings.TrimSpace(v) == "" || v == "-" {
		return "-（未显式设置，单索引即隐式写索引）"
	}
	return v
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

// shortFP 指纹只展示前 12 位，完整值在 --json 里。
func shortFP(fp string) string {
	if fp == "" {
		return "-"
	}
	if len(fp) <= 12 {
		return fp
	}
	return fp[:12]
}
