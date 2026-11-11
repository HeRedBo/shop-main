package main

import (
	"fmt"
	"strings"

	"shop/internal/bootstrap"

	"github.com/spf13/cobra"
)

// esRootCmd 是 es:* 命令族的父命令。六个子命令各自在自身的 init() 里
// AddCommand 到本命令并声明自己的 flag（见 es_status / es_cutover / es_migrate /
// es_rebuild / es_cleanup / es_sync_products），因此新增子命令不需要改这个文件；
// 本文件只负责「初始化隔离」「命令族总览文案」与「父命令兼做漏 es: 前缀的拦截」三件事。
var esRootCmd = &cobra.Command{
	Use:   "es",
	Short: "Elasticsearch 索引运维工具",
	Long: "Elasticsearch 索引迁移与数据同步工具集，全部子命令共享同一套索引声明（internal/es/indices/*.jsonc）。\n" +
		"\n" +
		"推荐按以下顺序理解各命令的职责边界：\n" +
		"  es:status          只读体检（集群/别名指向/结构漂移/计数差），不改任何东西，任何时刻都可跑\n" +
		"  es:cutover         一次性同名割接：别名位仍是【物理索引】时才有用，单次 _aliases 里 remove_index + add，\n" +
		"                     旧物理索引连同数据一起消失，不可回滚，故执行前必须已完成快照备份\n" +
		"  es:migrate         变更分级入口：逐声明只读探测，分级为 首建 / Noop / Inplace / Rebuild / 引导 cutover；\n" +
		"                     需要重建时委托 es:rebuild，本身绝不做不可逆动作\n" +
		"  es:rebuild         蓝绿重建（别名已存在的结构变更正路）：新建 <alias>_vN → 灌数 → Refresh+Count 对账\n" +
		"                     （达标才允许切别名）→ 单请求原子切别名 → 差量补偿；默认保留旧索引可直接回滚，\n" +
		"                     只有 --drop-old 才物理删除旧索引\n" +
		"  es:sync-products   全量灌数；--dry-run 为只读预演：打印扫描/灌入执行计划（区间、并发、命中行数、对账序列），\n" +
		"                     仍会连接 MySQL 探测边界、只读探测 ES（如灌入调优安全判定），不打印 SQL/文档样本、不发起任何写请求\n" +
		"  es:cleanup         清理蓝绿失败留下的无别名孤儿索引 *_vN（默认 --list-only=true 只列出，删除需 --list-only=false）\n" +
		"\n" +
		"安全约定（所有写操作类子命令一致）：\n" +
		"  · --dry-run 只读预演，写路径被审计层硬拒，只发 GET/HEAD；\n" +
		"  · 破坏性动作必须 --yes 或在 TTY 下确认，非交互环境（管道 / < /dev/null）缺 --yes 一律退出码 1 且零 ES 调用；\n" +
		"  · 物理索引名与别名在 ES 中绝对互斥，任何切换都是单请求原子下发，不拆两步。\n" +
		"\n" +
		"注意：es 命令族会独立初始化 ES 连接，不影响其他 CLI 命令。",
	// Args 与 RunE 成对存在，修的是「静默 no-op」：此前二者皆空，Cobra 在 execute() 里
	// 走 !Runnable() → flag.ErrHelp 分支，`es status`（漏了 es: 前缀）只打印帮助就返回 nil error，
	// 退码 0，脚本/CI 里等同于静默成功。现在的语义：
	//   · 无参 / --help → 打印帮助，退码 0（合理，不当失败）；
	//   · 未知子命令 → Args 校验报错 + 提示 es es:<sub> 正确用法，退码非 0；
	//     且 Args 校验发生在 PersistentPreRun 之前，失败路径不会去连 Redis/MySQL/ES。
	Args: validateEsSubCommandArgs,
	RunE: runEsRootHelp,
}

// runEsRootHelp 只在「无参」时被调用：有位置参数的情况已在 validateEsSubCommandArgs 阶段被拒。
func runEsRootHelp(cmd *cobra.Command, args []string) error {
	return cmd.Help()
}

// validateEsSubCommandArgs 拒绝任何位置参数：es 命令族的子命令名一律带 "es:" 前缀，
// 能带参数走到父命令本体说明 Cobra 没匹配到任何子命令（如 `es status`），必须显式失败。
func validateEsSubCommandArgs(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}
	got := args[0]
	msg := fmt.Sprintf("es 没有子命令 %q：es 命令族的子命令名都带 \"es:\" 前缀，正确用法 es es:<sub>", got)
	if hint := esSuggestionFor(cmd, got); hint != "" {
		msg += fmt.Sprintf("（是否想用 %s？）", hint)
	}
	return fmt.Errorf("%s；可用子命令：%s", msg, strings.Join(esSubCommandNames(cmd), " / "))
}

// esSubCommandNames 返回父命令当前登记的子命令名（按 Cobra 的排序）。
func esSubCommandNames(cmd *cobra.Command) []string {
	subs := cmd.Commands()
	names := make([]string, 0, len(subs))
	for _, s := range subs {
		names = append(names, s.Name())
	}
	return names
}

// esSuggestionFor 专门接住「漏前缀」这一种高频误用：把输入补上 "es:" 再比对已登记子命令，
// 命中则返回建议名（`es status` → `es:status`），不命中返回空串。
func esSuggestionFor(cmd *cobra.Command, got string) string {
	for _, name := range esSubCommandNames(cmd) {
		if name == "es:"+got {
			return name
		}
	}
	return ""
}

func init() {
	// 初始化隔离：Cobra 只执行最近一层 PersistentPreRun，
	// es 父命令挂自己的 Pre/PostRun，es:* 子命令天然获得 ES 初始化，
	// 其他命令（product:sync、order:report、dlq 等）仍走 main.go 的全局初始化、不会连 ES。
	esRootCmd.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		// 父命令自身（无参打帮助）不跑初始化：补上 RunE 之后本命令已 Runnable，
		// 若不加这道短路，`es` 无参也会把 Redis/MySQL/ES 全量拉起（连不上直接 panic），
		// 破坏「无参打帮助」的可用性；改成 E 形态不改变 es:* 子命令的初始化时机与顺序。
		if cmd == esRootCmd {
			return nil
		}
		bootstrap.BootstrapWith(configPath, bootstrap.WithElasticsearch())
		return nil
	}
	esRootCmd.PersistentPostRun = func(cmd *cobra.Command, args []string) {
		bootstrap.Shutdown()
	}

	rootCmd.AddCommand(esRootCmd)
}
