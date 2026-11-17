package main

// es_cutover_summary_test.go 钉死任务 #42「期望数口径按别名走」在 CLI 影响面摘要里的落地：
// printCutoverSummary 必须用与 dbCounters 同源的按别名口径描述，而不是对任意别名硬编码
// 商品的 is_del=0 AND is_show=1。订单没有 is_show 维度，把商品文案套上去就是错的。
//
// 破坏矩阵：
//   - 把 es_cutover.go 的 sync.DBCounterScope(spec.Alias) 改回硬编码 "is_del=0 AND is_show=1"
//     → shop-order 子断言立即变红（出现 is_show）；
//   - 把 shop-product 那条口径描述改坏（丢掉 is_show）→ shop-product 子断言变红。
//
// 完全离线：printCutoverSummary 只查表 + 纯字符串渲染，绝不碰 MySQL/ES，直接捕获 stdout 断言。

import (
	"os"
	"strings"
	"testing"
	"time"

	"shop/internal/es"
	"shop/internal/es/sync"
)

// captureStdout 把 f() 期间写到 os.Stdout 的内容收集成字符串。
// printCutoverSummary 用的是裸 fmt.Printf（写 os.Stdout），所以只能靠重定向文件描述符捕获。
func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, perr := os.Pipe()
	if perr != nil {
		t.Fatalf("os.Pipe 失败: %v", perr)
	}
	os.Stdout = w

	done := make(chan string, 1)
	go func() {
		b := make([]byte, 0, 4096)
		buf := make([]byte, 1024)
		for {
			n, rerr := r.Read(buf)
			if n > 0 {
				b = append(b, buf[:n]...)
			}
			if rerr != nil {
				break
			}
		}
		done <- string(b)
	}()

	fnErr := runAndCatch(&f)

	os.Stdout = orig
	_ = w.Close()
	got := <-done
	if fnErr != "" {
		t.Fatalf("printCutoverSummary panic: %s", fnErr)
	}
	return got
}

// runAndCatch 执行 fn 并把 panic 转成字符串（避免捕获流程被 panic 打断）。
func runAndCatch(fn *func()) (msg string) {
	defer func() {
		if rec := recover(); rec != nil {
			msg = "recovered panic"
		}
	}()
	(*fn)()
	return ""
}

func defByAliasForSummary(t *testing.T, alias string) *es.IndexDefinition {
	t.Helper()
	defs, err := es.LoadDefinitions("")
	if err != nil {
		t.Fatalf("装载内嵌声明失败: %v", err)
	}
	for _, d := range defs {
		if d.Alias() == alias {
			return d
		}
	}
	t.Fatalf("内嵌声明里没有别名 %s", alias)
	return nil
}

// TestPrintCutoverSummary_CaliberByAlias 是口径文案按别名走的直接证据。
func TestPrintCutoverSummary_CaliberByAlias(t *testing.T) {
	t.Run("订单摘要不得出现 is_show，必须点明 is_del=0", func(t *testing.T) {
		spec := cutoverSpec{Alias: sync.OrderAlias, Tolerance: 0, Timeout: time.Minute}
		out := captureStdout(t, func() {
			printCutoverSummary(spec, defByAliasForSummary(t, sync.OrderAlias))
		})
		if !strings.Contains(out, "is_del=0") {
			t.Fatalf("订单摘要必须含真实口径 is_del=0，got:\n%s", out)
		}
		if strings.Contains(out, "is_show") {
			t.Errorf("订单没有 is_show 维度，摘要却出现 is_show（口径硬编码回归）:\n%s", out)
		}
	})

	t.Run("商品摘要仍逐字保留 is_del=0 AND is_show=1", func(t *testing.T) {
		spec := cutoverSpec{Alias: sync.ProductAlias, Tolerance: 0, Timeout: time.Minute}
		out := captureStdout(t, func() {
			printCutoverSummary(spec, defByAliasForSummary(t, sync.ProductAlias))
		})
		if !strings.Contains(out, "is_del=0 AND is_show=1") {
			t.Fatalf("商品摘要必须逐字保留 is_del=0 AND is_show=1（改造前后一致），got:\n%s", out)
		}
	})
}
