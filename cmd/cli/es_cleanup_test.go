package main

// es_cleanup_test.go 钉住 es:cleanup 的两道删除护栏（审查必修④）与版本尾缀正则口径
// （审查必修⑤）。全部为纯逻辑用例：不连 ES、不写真实 runtime/。

import (
	"strings"
	"testing"
	"time"
)

// heldSet 构造「别名持有的物理索引集合」。
func heldSet(idx ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(idx))
	for _, s := range idx {
		m[s] = struct{}{}
	}
	return m
}

// TestCheckSingleIndexTarget_VersionRedlineAppliesToIndexMode 是护栏一的破坏即红用例：
// --index 模式必须同样强制 *_v<N> 版本红线。未割接的 shop-product 按定义不持别名，
// 若只校验「无别名持有」，`--index shop-product --list-only=false --yes` 就会删掉线上存量索引。
//
// 变异鉴别力：删掉 checkSingleIndexTarget 里的 MatchString 分支（或把 --index 分支
// 改回只判别名持有），"shop-product" 一行立即变红。
func TestCheckSingleIndexTarget_VersionRedlineAppliesToIndexMode(t *testing.T) {
	tests := []struct {
		name      string
		index     string
		held      map[string]struct{}
		wantErr   bool
		errSubstr string
	}{
		{"无版本尾缀的线上存量索引必须拒绝", "shop-product", nil, true, "版本红线"},
		{"带别名前缀但无版本号同样拒绝", "shop_product_backup", nil, true, "版本红线"},
		{"合法 *_vN 放行", "shop-product_v2", nil, false, ""},
		{"被别名持有的 *_vN 仍拒绝", "shop-product_v2", heldSet("shop-product_v2"), true, "仍被别名持有"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkSingleIndexTarget(tt.index, tt.held)
			if tt.wantErr && err == nil {
				t.Fatalf("checkSingleIndexTarget(%q) 应拒绝删除，实际放行", tt.index)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("checkSingleIndexTarget(%q) 不应报错: %v", tt.index, err)
			}
			if err != nil && !strings.Contains(err.Error(), tt.errSubstr) {
				t.Errorf("错误应说明拒绝原因（含 %q），实际: %v", tt.errSubstr, err)
			}
		})
	}
}

// TestPassesAgeGate_ZeroCreationFailClosed 是护栏二的破坏即红用例：
// creation.date 缺失时 msToTime 返回零值，旧写法 !created.Before(cutoff) 恒 false，
// 年龄护栏 fail-open 地把「看不出年龄」的索引放进删除候选。现在必须跳过并给出告警原因。
//
// 变异鉴别力：把 created.IsZero() 分支改成放行（或删除该分支），
// 「零值时间」一行立即变红（pass=true 或 reason 为空）。
func TestPassesAgeGate_ZeroCreationFailClosed(t *testing.T) {
	olderThan := 168 * time.Hour
	cutoff := time.Now().Add(-olderThan)

	tests := []struct {
		name        string
		created     time.Time
		olderThan   time.Duration
		wantPass    bool
		wantWarning bool
	}{
		{"零值 creation.date 必须 fail-closed 跳过并告警", time.Time{}, olderThan, false, true},
		{"足够老的索引入选", cutoff.Add(-24 * time.Hour), olderThan, true, false},
		{"未老于 --older-than 静默跳过", cutoff.Add(24 * time.Hour), olderThan, false, false},
		{"--older-than=0 关闭年龄护栏时零值也不阻断（保持 flag 语义）", time.Time{}, 0, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pass, reason := passesAgeGate(tt.created, cutoff, tt.olderThan)
			if pass != tt.wantPass {
				t.Fatalf("passesAgeGate 入选判定 = %v, 期望 %v", pass, tt.wantPass)
			}
			if got := reason != ""; got != tt.wantWarning {
				t.Errorf("告警原因存在性 = %v, 期望 %v（reason=%q）", got, tt.wantWarning, reason)
			}
			if tt.wantWarning && !strings.Contains(reason, "creation.date") {
				t.Errorf("告警应说明 creation.date 缺失，实际: %q", reason)
			}
		})
	}
}

// TestCleanupVersionRe_SinglePatternAcrossLayer 钉住必修⑤ 的正则统一：
// cmd/cli 的 cleanupVersionRe 必须与 internal/es/admin/alias.go 的 versionSuffixRe
// 同口径 `_(?:v)?(\d+)$` —— 两者对同一个名字的判断必须一致。
//
// 变异鉴别力：把它改回 `_v\d+$`，"_N 简写与 alias.go 判断一致" 一行变红。
func TestCleanupVersionRe_SinglePatternAcrossLayer(t *testing.T) {
	tests := []struct {
		name  string
		index string
		want  bool
	}{
		{"标准 *_vN", "shop-product_v2", true},
		{"*_vN 多位数字", "shop-product_v12", true},
		{"简写 *_N（与 alias.go 一致，必须认）", "shop-product_3", true},
		{"无版本尾缀的存量索引（必须不认）", "shop-product", false},
		{"非结尾数字（必须不认）", "shop-product_v2_tmp", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cleanupVersionRe.MatchString(tt.index); got != tt.want {
				t.Errorf("cleanupVersionRe.MatchString(%q) = %v, 期望 %v", tt.index, got, tt.want)
			}
		})
	}
}
