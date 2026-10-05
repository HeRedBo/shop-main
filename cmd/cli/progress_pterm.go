package main

import (
	"time"

	"github.com/pterm/pterm"
	"github.com/spf13/cobra"
)

var progressPtermCmd = &cobra.Command{
	Use:   "progress:pterm",
	Short: "pterm 彩色进度条演示",
	Run: func(cmd *cobra.Command, args []string) {
		// 标题
		pterm.DefaultHeader.
			WithBackgroundStyle(pterm.NewStyle(pterm.BgBlue)).
			WithTextStyle(pterm.NewStyle(pterm.FgWhite)).
			Println("pterm 彩色进度条演示")
		pterm.Println()

		// ── 样式 1：pterm 默认进度条 ──
		pterm.DefaultSection.Println("样式 1：pterm 默认进度条")
		p1, _ := pterm.DefaultProgressbar.WithTotal(10).WithTitle("默认进度条").Start()
		for i := 0; i < 10; i++ {
			time.Sleep(300 * time.Millisecond)
			p1.Increment()
		}
		pterm.Println()

		// ── 样式 2：自定义颜色的进度条 ──
		pterm.DefaultSection.Println("样式 2：自定义颜色的进度条")

		// 绿色
		p2a, _ := pterm.DefaultProgressbar.
			WithTotal(10).
			WithTitle("绿色进度条").
			WithBarStyle(pterm.NewStyle(pterm.FgGreen)).
			WithShowPercentage().
			Start()
		for i := 0; i < 10; i++ {
			time.Sleep(300 * time.Millisecond)
			p2a.Increment()
		}

		// 蓝色
		p2b, _ := pterm.DefaultProgressbar.
			WithTotal(10).
			WithTitle("蓝色进度条").
			WithBarStyle(pterm.NewStyle(pterm.FgCyan)).
			WithShowPercentage().
			Start()
		for i := 0; i < 10; i++ {
			time.Sleep(300 * time.Millisecond)
			p2b.Increment()
		}

		// 红色
		p2c, _ := pterm.DefaultProgressbar.
			WithTotal(10).
			WithTitle("红色进度条").
			WithBarStyle(pterm.NewStyle(pterm.FgRed)).
			WithShowPercentage().
			Start()
		for i := 0; i < 10; i++ {
			time.Sleep(300 * time.Millisecond)
			p2c.Increment()
		}
		pterm.Println()

		// ── 样式 3：带标题的进度条 ──
		pterm.DefaultSection.Println("样式 3：带标题的进度条")
		p3, _ := pterm.DefaultProgressbar.
			WithTotal(10).
			WithTitle("正在下载文件...").
			WithBarStyle(pterm.NewStyle(pterm.FgMagenta)).
			WithShowCount().
			WithShowElapsedTime().
			Start()
		for i := 0; i < 10; i++ {
			time.Sleep(300 * time.Millisecond)
			p3.Increment()
			if i == 4 {
				p3.UpdateTitle("下载过半，继续中...")
			}
		}
		pterm.Println()

		// ── 样式 4：多进度条并发（pterm 特色功能） ──
		pterm.DefaultSection.Println("样式 4：多进度条并发（pterm 特色功能）")
		multi := pterm.DefaultMultiPrinter

		mp1, _ := pterm.DefaultProgressbar.
			WithTotal(10).
			WithTitle("下载模块 A").
			WithBarStyle(pterm.NewStyle(pterm.FgGreen)).
			WithWriter(multi.NewWriter()).
			Start()
		mp2, _ := pterm.DefaultProgressbar.
			WithTotal(10).
			WithTitle("下载模块 B").
			WithBarStyle(pterm.NewStyle(pterm.FgCyan)).
			WithWriter(multi.NewWriter()).
			Start()
		mp3, _ := pterm.DefaultProgressbar.
			WithTotal(10).
			WithTitle("下载模块 C").
			WithBarStyle(pterm.NewStyle(pterm.FgYellow)).
			WithWriter(multi.NewWriter()).
			Start()

		multi.Start()
		for i := 0; i < 10; i++ {
			mp1.Increment()
			if i%2 == 0 {
				mp2.Increment()
			}
			if i%3 == 0 {
				mp3.Increment()
			}
			time.Sleep(300 * time.Millisecond)
		}
		// 补齐未完成的
		for mp2.Current < 10 {
			mp2.Increment()
		}
		for mp3.Current < 10 {
			mp3.Increment()
		}
		multi.Stop()
		pterm.Println()

		// ── 样式 5：自定义主题色 ──
		pterm.DefaultSection.Println("样式 5：自定义主题色")

		// 暖色系
		p5a, _ := pterm.DefaultProgressbar.
			WithTotal(10).
			WithTitle("暖色系主题").
			WithBarStyle(pterm.NewStyle(pterm.FgYellow, pterm.Bold)).
			WithBarCharacter("█").
			WithBarFiller("░").
			WithShowPercentage().
			Start()
		for i := 0; i < 10; i++ {
			time.Sleep(300 * time.Millisecond)
			p5a.Increment()
		}

		// 冷色系
		p5b, _ := pterm.DefaultProgressbar.
			WithTotal(10).
			WithTitle("冷色系主题").
			WithBarStyle(pterm.NewStyle(pterm.FgBlue, pterm.Bold)).
			WithBarCharacter("█").
			WithBarFiller("░").
			WithShowPercentage().
			Start()
		for i := 0; i < 10; i++ {
			time.Sleep(300 * time.Millisecond)
			p5b.Increment()
		}

		// 高亮系
		p5c, _ := pterm.DefaultProgressbar.
			WithTotal(10).
			WithTitle("高亮系主题").
			WithBarStyle(pterm.NewStyle(pterm.FgMagenta, pterm.Bold)).
			WithBarCharacter("●").
			WithBarFiller("○").
			WithShowPercentage().
			Start()
		for i := 0; i < 10; i++ {
			time.Sleep(300 * time.Millisecond)
			p5c.Increment()
		}

		pterm.Println()

		// 总结
		pterm.DefaultBox.
			WithBoxStyle(pterm.NewStyle(pterm.FgCyan)).
			Println("✅ pterm 彩色进度条演示完毕！\n" +
				"pterm 特色：原生彩色支持、多进度条并发、丰富主题系统")
	},
	PersistentPreRun:  func(cmd *cobra.Command, args []string) {},
	PersistentPostRun: func(cmd *cobra.Command, args []string) {},
}

func init() {
	rootCmd.AddCommand(progressPtermCmd)
}
