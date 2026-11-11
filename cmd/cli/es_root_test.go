package main

// es_root_test.go 钉住 es 父命令的「静默 no-op」修复：
//   · 漏 es: 前缀（`es status`）→ Args 校验报错 → main() os.Exit(1)，退码非 0；
//   · 无参（`es`）→ 打印帮助、err == nil，退码 0（合理，不当失败）；
//   · 合法子命令（`es es:status`）→ 仍由 Cobra 解析到子命令本体正常执行。
// 全部为纯 Cobra 命令树用例：不连 ES、不跑真实 bootstrap（隔离树只取父命令的 Args/RunE 接线）。

import (
	"bytes"
	"strings"
	"testing"

	"shop/pkg/global"

	"github.com/spf13/cobra"
)

// isolatedEsSubCommands 是隔离树里登记的子命令名，取真实命令族的一个子集即可。
var isolatedEsSubCommands = []string{"es:cleanup", "es:status"}

// newIsolatedEsTree 造一棵与真实命令族同层级的隔离树：顶层 root → es 父命令 → 若干桩子命令。
// 必须给 es 挂一个父命令——真实的 esRootCmd 是 rootCmd 的子命令，Cobra 的 legacyArgs 只对根命令
// 做子命令名校验；若把隔离树里的 es 直接当根跑，回退修复后也会“碰巧”报 unknown command，用例失真。
// 关键：es 的 Args / RunE 直接读 esRootCmd 上的字段本身，而不是引用同名的包级函数——
// 把修复回退（删掉这两行接线）后这里拿到的是 nil，用例立即变红。
// 全程不挂 PersistentPreRunE，所以执行它不会真的去连 Redis/MySQL/ES。
func newIsolatedEsTree(executed *string) *cobra.Command {
	es := &cobra.Command{
		Use:                   "es",
		Short:                 "Elasticsearch 索引运维工具",
		DisableFlagsInUseLine: true,
		Args:                  esRootCmd.Args,
		RunE:                  esRootCmd.RunE,
	}
	for _, name := range isolatedEsSubCommands {
		subName := name
		es.AddCommand(&cobra.Command{
			Use:  subName,
			RunE: func(cmd *cobra.Command, args []string) error { *executed = subName; return nil },
		})
	}
	top := &cobra.Command{Use: "shop-cli", RunE: func(cmd *cobra.Command, args []string) error { return nil }}
	top.AddCommand(es)
	top.CompletionOptions.DisableDefaultCmd = true // 别把内建 completion 混进命令树
	return top
}

// TestEsRootCmd_ParentIsRunnableAndValidatesArgs 是接线断言（破坏即红的第一道）：
// 修复前 Run/RunE 皆空 → Cobra 走 !Runnable() → flag.ErrHelp → 打帮助后 err == nil → 退码 0；
// 修复前 Args 皆空 → 非根命令走 legacyArgs → 任意参数一律放行。二者任一回退本用例即红。
func TestEsRootCmd_ParentIsRunnableAndValidatesArgs(t *testing.T) {
	if !esRootCmd.Runnable() {
		t.Fatal("esRootCmd 不可执行（Run/RunE 皆空）：Cobra 会打印帮助后返回 nil error，`es status` 退码 0 静默失败")
	}
	if esRootCmd.Args == nil {
		t.Fatal("esRootCmd.Args 为空：解析不到子命令时会退回 legacyArgs，而 legacyArgs 对非根命令放行任意参数")
	}
}

// TestEsRootCmd_UnknownSubCommandFailsLegalOneRuns 钉住两条对称断言：
// 未知子命令 → err（非零退码）且绝不执行任何子命令；合法子命令 → 正常执行且 err == nil。
//
// 变异鉴别力：删掉 Args 接线（或把 validateEsSubCommandArgs 的 len(args)==0 判断改成恒 nil），
// 「漏前缀」「拼错」两行立即变红（回退后 es 不是根命令，legacyArgs 不会接手，err 变回 nil）；
// 把 RunE 改回报错或让 Args 误伤合法名字，「合法子命令」「无参」两行立即变红。
func TestEsRootCmd_UnknownSubCommandFailsLegalOneRuns(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantErr    bool
		wantExec   string   // 期望被执行的子命令名，"" 表示不应有任何子命令被执行
		wantErrSub []string // 错误文案必须包含的片段
		wantHelp   bool     // 父命令本体应把帮助打到 stdout
	}{
		{
			name:       "漏 es: 前缀必须报错并提示正确用法",
			args:       []string{"es", "status"},
			wantErr:    true,
			wantExec:   "",
			wantErrSub: []string{`"status"`, `es:<sub>`, "es:status"},
		},
		{
			name:       "拼错的名字同样报错（无建议也不放行）",
			args:       []string{"es", "stauts"},
			wantErr:    true,
			wantExec:   "",
			wantErrSub: []string{`"stauts"`},
		},
		{
			name:     "无参打帮助属合理：不是失败",
			args:     []string{"es"},
			wantErr:  false,
			wantExec: "",
			wantHelp: true,
		},
		{
			name:     "合法子命令必须照常解析到子命令本体执行",
			args:     []string{"es", "es:status"},
			wantErr:  false,
			wantExec: "es:status",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var executed string
			top := newIsolatedEsTree(&executed)
			var out, errOut bytes.Buffer
			top.SetOut(&out)
			top.SetErr(&errOut)
			top.SetArgs(tt.args)

			_, err := top.ExecuteC()

			if tt.wantErr && err == nil {
				t.Fatalf("ExecuteC(%v) 应返回 error（main() 据此 os.Exit(1)），实际 nil；stdout=%q", tt.args, out.String())
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("ExecuteC(%v) 不应报错: %v", tt.args, err)
			}
			if executed != tt.wantExec {
				t.Errorf("被执行的子命令 = %q，期望 %q", executed, tt.wantExec)
			}
			for _, sub := range tt.wantErrSub {
				joined := errOut.String()
				if err != nil {
					joined = err.Error() + "\n" + joined
				}
				if !strings.Contains(joined, sub) {
					t.Errorf("错误输出应包含 %q，实际: %s", sub, joined)
				}
			}
			// 无参路径的可用性：帮助仍打出来（含可用子命令清单），且不落到报错分支。
			if tt.wantHelp && !strings.Contains(out.String(), "Available Commands") {
				t.Errorf("无参应打印帮助（含 Available Commands），实际 stdout=%q", out.String())
			}
		})
	}
}

// TestEsRootCmd_ParentPathSkipsBootstrap 钉住「加 RunE 后父命令不再免费跳过初始化」这一副作用：
// Cobra 的 PersistentPreRun 在 ValidateArgs 之后、RunE 之前执行，若无父命令短路，
// `es` 无参也会全量拉起 Redis/MySQL/ES（连不上直接 panic），把「无参打帮助」打成不可用。
//
// 变异鉴别力：删掉 PersistentPreRunE 里的 `cmd == esRootCmd` 分支 → 这里会真的 bootstrap
// 并把 global.EsClient 置为非空 → 用例变红。
func TestEsRootCmd_ParentPathSkipsBootstrap(t *testing.T) {
	if esRootCmd.PersistentPreRunE == nil {
		t.Fatal("esRootCmd.PersistentPreRunE 为空：父命令无参路径会走 rootCmd 的全局初始化，或子命令失去 ES 初始化")
	}
	if err := esRootCmd.PersistentPreRunE(esRootCmd, nil); err != nil {
		t.Fatalf("父命令自身的 PersistentPreRunE 应返回 nil, got %v", err)
	}
	if global.EsClient != nil {
		t.Fatal("父命令无参路径不应触发 ES bootstrap（连不上时 BootstrapWith 会 panic）")
	}
	if global.Db != nil {
		t.Fatal("父命令无参路径不应触发 MySQL bootstrap")
	}
}
