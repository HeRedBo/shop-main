package main

import (
	"fmt"
	"time"

	"github.com/schollz/progressbar/v3"
	"github.com/spf13/cobra"
)

var progressStylesCmd = &cobra.Command{
	Use:   "progress:styles",
	Short: "进度条多样式对比演示",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("╔══════════════════════════════════════╗")
		fmt.Println("║     进度条多样式对比演示              ║")
		fmt.Println("╚══════════════════════════════════════╝")

		// 样式 1：默认主题
		fmt.Println("\n[1] 默认主题（实心方块）")
		bar1 := progressbar.NewOptions(10,
			progressbar.OptionSetDescription("默认主题"),
			progressbar.OptionSetWidth(40),
			progressbar.OptionShowCount(),
			progressbar.OptionShowIts(),
			progressbar.OptionSetPredictTime(true),
		)
		for i := 0; i < 10; i++ {
			bar1.Add(1)
			time.Sleep(300 * time.Millisecond)
		}
		bar1.Finish()

		// 样式 2：ASCII 主题
		fmt.Println("\n[2] ASCII 主题（纯文本兼容）")
		bar2 := progressbar.NewOptions(10,
			progressbar.OptionSetDescription("ASCII 主题"),
			progressbar.OptionSetTheme(progressbar.ThemeASCII),
			progressbar.OptionSetWidth(40),
			progressbar.OptionShowCount(),
			progressbar.OptionShowIts(),
		)
		for i := 0; i < 10; i++ {
			bar2.Add(1)
			time.Sleep(300 * time.Millisecond)
		}
		bar2.Finish()

		// 样式 3：Unicode 主题
		fmt.Println("\n[3] Unicode 主题（需 Nerd Font）")
		bar3 := progressbar.NewOptions(10,
			progressbar.OptionSetDescription("Unicode 主题"),
			progressbar.OptionSetTheme(progressbar.ThemeUnicode),
			progressbar.OptionSetWidth(40),
			progressbar.OptionShowCount(),
			progressbar.OptionShowIts(),
		)
		for i := 0; i < 10; i++ {
			bar3.Add(1)
			time.Sleep(300 * time.Millisecond)
		}
		bar3.Finish()

		// 样式 4：自定义主题（项目风格）
		fmt.Println("\n[4] 自定义主题（项目风格 █▓▒░）")
		bar4 := progressbar.NewOptions(10,
			progressbar.OptionSetDescription("自定义主题"),
			progressbar.OptionSetTheme(progressbar.Theme{
				Saucer:        "█",
				SaucerHead:    "█",
				SaucerPadding: "░",
				BarStart:      "▒",
				BarEnd:        "▓",
			}),
			progressbar.OptionSetWidth(40),
			progressbar.OptionShowCount(),
			progressbar.OptionShowIts(),
		)
		for i := 0; i < 10; i++ {
			bar4.Add(1)
			time.Sleep(300 * time.Millisecond)
		}
		bar4.Finish()

		// 样式 5：带颜色的进度条
		fmt.Println("\n[5] 彩色进度条")
		bar5 := progressbar.NewOptions(10,
			progressbar.OptionSetDescription("[cyan]彩色进度条[reset]"),
			progressbar.OptionEnableColorCodes(true),
			progressbar.OptionSetWidth(40),
			progressbar.OptionShowCount(),
			progressbar.OptionShowIts(),
			progressbar.OptionSetTheme(progressbar.Theme{
				Saucer:        "●",
				SaucerHead:    "◉",
				SaucerPadding: "○",
				BarStart:      "┃",
				BarEnd:        "┃",
			}),
		)
		for i := 0; i < 10; i++ {
			bar5.Add(1)
			time.Sleep(300 * time.Millisecond)
		}
		bar5.Finish()

		// 样式 6：全宽进度条
		fmt.Println("\n[6] 全宽进度条")
		bar6 := progressbar.NewOptions(10,
			progressbar.OptionSetDescription("全宽进度条"),
			progressbar.OptionFullWidth(),
			progressbar.OptionShowCount(),
			progressbar.OptionShowIts(),
			progressbar.OptionSetElapsedTime(true),
			progressbar.OptionShowElapsedTimeOnFinish(),
		)
		for i := 0; i < 10; i++ {
			bar6.Add(1)
			time.Sleep(300 * time.Millisecond)
		}
		bar6.Finish()

		// 样式 7：文件传输风格
		fmt.Println("\n[7] 文件传输风格")
		bar7 := progressbar.NewOptions(1000,
			progressbar.OptionSetDescription("文件传输模拟"),
			progressbar.OptionShowBytes(true),
			progressbar.OptionSetWidth(30),
			progressbar.OptionSetPredictTime(true),
			progressbar.OptionSetElapsedTime(true),
		)
		for i := 0; i < 10; i++ {
			bar7.Add(100)
			time.Sleep(300 * time.Millisecond)
		}
		bar7.Finish()

		fmt.Println("\n✅ 所有样式演示完毕！")
	},
	PersistentPreRun:  func(cmd *cobra.Command, args []string) {},
	PersistentPostRun: func(cmd *cobra.Command, args []string) {},
}

func init() {
	rootCmd.AddCommand(progressStylesCmd)
}
