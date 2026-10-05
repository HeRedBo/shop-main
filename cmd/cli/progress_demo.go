package main

import (
	"fmt"
	"time"

	"github.com/schollz/progressbar/v3"
	"github.com/spf13/cobra"
)

var progressDemoCmd = &cobra.Command{
	Use:   "progress:demo",
	Short: "进度条演示示例",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("=== 控制台进度条示例（默认样式）===")
		bar := progressbar.NewOptions(10,
			progressbar.OptionSetDescription("默认样式演示"),
		)
		for i := 0; i < 10; i++ {
			bar.Add(1)
			time.Sleep(1 * time.Second)
		}
	},
	PersistentPreRun:  func(cmd *cobra.Command, args []string) {},
	PersistentPostRun: func(cmd *cobra.Command, args []string) {},
}

func init() {
	rootCmd.AddCommand(progressDemoCmd)
}
