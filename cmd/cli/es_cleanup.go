package main

// es_cleanup.go 实现 es:cleanup —— 列出 / 删除「无别名挂载」的残留 <alias>_vN 索引（方案第六节残留清理）。
//
// 为什么需要它：蓝绿重建失败（灌数中断、对账不达标、被 --timeout 打断）会在集群里留下一个
// 还没切走别名的新物理索引；rebuild 刻意「不静默覆盖、不自动删」，把这些交给本命令人工收敛。
// 本地集群 snapshot/restore 不可用（无 path.repo），所以 es:cleanup 是唯一的手工回收手段，
// 默认 --list-only=true 只列不删，绝不误删正在服务的索引。
//
// 安全红线（写死在本命令里）：
//   - 只处理形如 *_v<digits> 的版本化物理索引；shop-product 这类无版本尾缀的存量索引一律不碰；
//     —— 该红线对 --index 单索引模式同样强制（必修④ 护栏一）：不能因为「显式指定了名字」
//     就绕过它，否则未割接的 shop-product（按定义不持别名）会被 --index 直接删掉；
//   - 年龄护栏 fail-closed（必修④ 护栏二）：creation.date 缺失（零值）时不进删除候选，
//     绝不把「无法判断年龄」当成「已经够老」；
//   - 只要索引当前仍被任何别名持有，就永不列入删除候选（防止把线上服务中的索引删掉）；
//   - 真正删除（--list-only=false）必须显式确认：非 TTY 且无 --yes 在触达 ES 之前退出码 1、零 ES 调用；
//   - 删除走 admin.Delete，成功即失效存在性缓存（DeleteIndexCache），与其他路径口径一致。

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"syscall"
	"time"

	"shop/internal/es/admin"
	"shop/pkg/global"

	"github.com/olivere/elastic/v7"
	"github.com/spf13/cobra"
)

// cleanupVersionRe 匹配版本化物理索引尾缀。<alias>_v<N>，并兼容 _<N> 简写。
// 必修⑤：口径与 internal/es/admin/alias.go 的 versionSuffixRe（以及 internal/es 侧
// targetVersionRe）逐字统一为 `_(?:v)?(\d+)$`，避免「一处认为带版本、另一处认为不带」
// 导致红线被绕过或下一代索引算错。
var cleanupVersionRe = regexp.MustCompile(`_(?:v)?(\d+)$`)

var esCleanupCmd = &cobra.Command{
	Use:   "es:cleanup",
	Short: "清理残留：列出 / 删除无别名挂载的版本化物理索引 *_vN",
	Long: "收敛蓝绿重建失败留下的孤儿索引（灌到一半、还没切走别名的 <alias>_vN）。\n" +
		"\n" +
		"默认 --list-only=true：只读列出候选，不做任何删除。候选必须同时满足：\n" +
		"  1) 索引名形如 *_v<数字>（版本化物理索引，无版本尾缀的存量索引如 shop-product 一律不碰）；\n" +
		"  2) 当前不被任何别名持有（防止误删正在服务的索引）；\n" +
		"  3) 创建时间早于 --older-than（默认 168h=7d，避免删掉刚建好、流程还在跑的索引）。\n" +
		"\n" +
		"--list-only=false 才真正删除，且必须显式确认：非 TTY 环境须带 --yes，否则退出码 1 且零 ES 调用。\n" +
		"本地集群无 path.repo、snapshot/restore 不可用，删除不可逆，请先 list 核对再删。",
	Example: "  # 只读列出 7 天以上的无别名 *_vN 残留\n" +
		"  go run ./cmd/cli es es:cleanup --older-than 168h\n" +
		"  # 指定单个残留索引精确删除（rebuild 报错提示 es:cleanup --index <next> 时用）\n" +
		"  go run ./cmd/cli es es:cleanup --index shop-product_v2 --list-only=false --yes",
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runESCleanup(cmd)
	},
}

func init() {
	f := esCleanupCmd.Flags()
	f.Bool("list-only", true, "true=只列出候选不删除；false=删除候选（须显式确认）")
	f.Duration("older-than", 168*time.Hour, "只处理创建时间早于 (now - 该值) 的索引（默认 7d）")
	f.String("index", "", "仅处理指定索引（配合 --list-only=false 精确删除某个残留）")
	f.Bool("yes", false, "跳过交互确认（非 TTY 环境执行删除必须显式带上）")
	f.Duration("timeout", 2*time.Minute, "整体 ctx 超时")

	esRootCmd.AddCommand(esCleanupCmd)
}

type cleanupSpec struct {
	ListOnly  bool
	OlderThan time.Duration
	Index     string
	AssumeYes bool
	Timeout   time.Duration
}

func parseCleanupSpec(cmd *cobra.Command) (cleanupSpec, error) {
	f := cmd.Flags()
	s := cleanupSpec{}
	s.ListOnly, _ = f.GetBool("list-only")
	s.OlderThan, _ = f.GetDuration("older-than")
	s.Index, _ = f.GetString("index")
	s.AssumeYes, _ = f.GetBool("yes")
	s.Timeout, _ = f.GetDuration("timeout")
	if s.OlderThan < 0 {
		return s, fmt.Errorf("--older-than 不能为负")
	}
	if s.Timeout < 0 {
		return s, fmt.Errorf("--timeout 不能为负")
	}
	return s, nil
}

// cleanupCandidate 一行删除候选。
type cleanupCandidate struct {
	Index     string
	Docs      int64
	CreatedAt time.Time
	Single    bool // 是否由 --index 显式指定
}

func runESCleanup(cmd *cobra.Command) error {
	if !global.CONFIG.Elasticsearch.Enabled {
		fmt.Println("Elasticsearch 未启用（elasticsearch.enabled=false），跳过清理")
		return nil
	}
	spec, err := parseCleanupSpec(cmd)
	if err != nil {
		return err
	}

	// 破坏性删除的护栏必须在任何 ES 连接/请求之前：非 TTY 且无 --yes 直接退出码 1、零 ES 调用。
	// list-only（默认）是只读，不需要确认。
	deleting := !spec.ListOnly
	if deleting && !spec.AssumeYes {
		if !cutoverStdinIsInteractive() {
			return fmt.Errorf("非 TTY 环境执行删除必须显式传 --yes（删除不可逆，本集群无 snapshot 可回滚）")
		}
	}

	sigCtx, sigCancel := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer sigCancel()
	ctx := sigCtx
	if spec.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, spec.Timeout)
		defer cancel()
	}

	cli, err := admin.Raw()
	if err != nil {
		return err
	}

	// 1) 全量别名持有集合：仍被任何别名指向的索引永不可删。
	aliasRows, err := cli.CatAliases().Columns("alias", "index").Do(ctx)
	if err != nil {
		return fmt.Errorf("读取别名清单失败: %w", err)
	}
	heldByAlias := make(map[string]struct{}, len(aliasRows))
	for _, r := range aliasRows {
		heldByAlias[r.Index] = struct{}{}
	}

	var candidates []cleanupCandidate
	cutoff := time.Now().Add(-spec.OlderThan)

	if spec.Index != "" {
		// 单索引模式：先过本命令写死的版本红线，再查别名持有（必修④ 护栏一）。
		// 红线在任何 ES 写操作之前、且优先于「无别名即安全」的直觉判定。
		if err := checkSingleIndexTarget(spec.Index, heldByAlias); err != nil {
			return err
		}
		row, ok, rerr := lookupIndex(cli, ctx, spec.Index)
		if rerr != nil {
			return rerr
		}
		if !ok {
			fmt.Printf("索引 %s 不存在，无需清理\n", spec.Index)
			return nil
		}
		candidates = append(candidates, cleanupCandidate{Index: row.Index, Docs: int64(row.DocsCount), CreatedAt: row.CreatedAt, Single: true})
	} else {
		idxRows, err := cli.CatIndices().Columns("index", "creation.date", "docs.count").Do(ctx)
		if err != nil {
			return fmt.Errorf("读取索引清单失败: %w", err)
		}
		for _, r := range idxRows {
			if !cleanupVersionRe.MatchString(r.Index) {
				continue // 无版本尾缀（如 shop-product）一律不碰
			}
			if _, held := heldByAlias[r.Index]; held {
				continue // 仍被别名持有，不可删
			}
			created := msToTime(r.CreationDate)
			// 必修④ 护栏二：creation.date 缺失时 fail-closed 跳过并告警，
			// 不能让零值时间穿过年龄护栏进入删除候选。
			if pass, why := passesAgeGate(created, cutoff, spec.OlderThan); !pass {
				if why != "" {
					fmt.Fprintf(os.Stderr, "⚠️  跳过 %s：%s\n", r.Index, why)
				}
				continue
			}
			candidates = append(candidates, cleanupCandidate{Index: r.Index, Docs: int64(r.DocsCount), CreatedAt: created})
		}
		sort.Slice(candidates, func(i, j int) bool { return candidates[i].Index < candidates[j].Index })
	}

	// 2) 打印候选。
	if len(candidates) == 0 {
		fmt.Println("没有符合条件的残留索引（无别名挂载的 *_vN，且老于 --older-than）。")
		return nil
	}
	fmt.Printf("候选残留索引 %d 个（无别名挂载%s）：\n", len(candidates), ageNote(spec, deleting))
	for _, c := range candidates {
		fmt.Printf("  - %-32s docs=%-8d created=%s\n", c.Index, c.Docs, c.CreatedAt.Format(time.RFC3339))
	}

	// 3) list-only：到此为止，零写操作。
	if !deleting {
		fmt.Println("\n（--list-only=true：以上仅为候选，未删除任何索引。确认无误后加 --list-only=false --yes 执行删除）")
		return nil
	}

	// 4) 交互确认（仅 TTY 且未带 --yes 时到达这里）。
	if !spec.AssumeYes {
		fmt.Println("\n⚠️  即将删除以上索引，操作不可逆（本集群无 snapshot 可回滚）。")
		if !confirmYes() {
			return fmt.Errorf("用户取消删除")
		}
	}

	var failed []string
	for _, c := range candidates {
		if err := admin.Delete(ctx, c.Index); err != nil {
			fmt.Fprintf(os.Stderr, "✗ 删除 %s 失败: %v\n", c.Index, err)
			failed = append(failed, c.Index)
			continue
		}
		fmt.Printf("✓ 已删除 %s（并失效存在性缓存）\n", c.Index)
	}
	if len(failed) > 0 {
		return fmt.Errorf("es:cleanup 有 %d 个索引删除失败: %v", len(failed), failed)
	}
	return nil
}

// checkSingleIndexTarget 是 --index 单索引模式的两道硬护栏（必修④ 护栏一）：
//  1. 版本红线：名字必须形如 *_v<N>（或 *_<N>）——与扫描模式同一个 cleanupVersionRe，
//     不能因为「用户显式指名」就绕过；未割接的 shop-product 按定义不持别名，
//     只靠别名判定会被直接删掉线上存量索引；
//  2. 别名持有：仍被任何别名指向的索引永不单删。
//
// 返回 error 即拒绝，调用方在触达任何写操作之前就能退出。
func checkSingleIndexTarget(idx string, heldByAlias map[string]struct{}) error {
	// 护栏一：版本红线 —— 与扫描模式同一个 cleanupVersionRe，不能因为「显式指定名字」就绕过。
	if !cleanupVersionRe.MatchString(idx) {
		return fmt.Errorf("索引 %s 不符合版本化命名 *_v<N>（版本红线）：无版本尾缀的存量索引（如 shop-product）一律不碰，拒绝删除", idx)
	}
	// 护栏二：仍被任何别名指向的索引永不单删。
	if _, held := heldByAlias[idx]; held {
		return fmt.Errorf("索引 %s 仍被别名持有，拒绝删除（先确认已无读流量）", idx)
	}
	return nil
}

// passesAgeGate 是年龄护栏的纯判定（必修④ 护栏二），返回 (是否入选, 需告警的原因)。
//
// creation.date 缺失时 msToTime 返回零值，`!created.Before(cutoff)` 恒为 false，
// 护栏会 fail-open 地把「看不出年龄」的索引放进删除候选 —— 这里改成 fail-closed 跳过。
// olderThan<=0 表示用户显式关闭年龄护栏，此时零值也不阻断（保持现有 flag 语义）。
func passesAgeGate(created, cutoff time.Time, olderThan time.Duration) (bool, string) {
	if created.IsZero() {
		if olderThan <= 0 {
			return true, ""
		}
		return false, "creation.date 缺失，无法判定年龄（fail-closed 跳过，不进删除候选）"
	}
	if olderThan > 0 && !created.Before(cutoff) {
		return false, "" // 未老于 --older-than：静默跳过（这是正常过滤，不是异常）
	}
	return true, ""
}

// catIndexRow 是 lookupIndex 需要的最小字段集。
type catIndexRow struct {
	Index     string
	DocsCount int
	CreatedAt time.Time
}

func (r catIndexRow) String() string { return r.Index }

// lookupIndex 用 _cat/indices/<idx> 精确取单个索引信息；不存在返回 (…, false, nil)。
func lookupIndex(cli *elastic.Client, ctx context.Context, idx string) (catIndexRow, bool, error) {
	rows, err := cli.CatIndices().Index(idx).Columns("index", "creation.date", "docs.count").Do(ctx)
	if err != nil {
		if elastic.IsNotFound(err) {
			return catIndexRow{}, false, nil
		}
		return catIndexRow{}, false, fmt.Errorf("读取索引 %s 失败: %w", idx, err)
	}
	for _, r := range rows {
		if r.Index == idx {
			return catIndexRow{Index: r.Index, DocsCount: r.DocsCount, CreatedAt: msToTime(r.CreationDate)}, true, nil
		}
	}
	return catIndexRow{}, false, nil
}

// msToTime 把 _cat/indices 的 creation.date（毫秒 epoch）转本地时间；异常值退化成零值。
func msToTime(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).Local()
}

func ageNote(spec cleanupSpec, deleting bool) string {
	if spec.Index != "" {
		return "（--index 指定）"
	}
	return fmt.Sprintf("，且创建早于 %s", time.Now().Add(-spec.OlderThan).Format(time.RFC3339))
}
