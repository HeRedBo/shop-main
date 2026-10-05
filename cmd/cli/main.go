package main

import (
	"fmt"
	"os"

	"shop/internal/bootstrap"

	"github.com/spf13/cobra"
)

var configPath string

var rootCmd = &cobra.Command{
	Use:   "shop-cli",
	Short: "Shop 管理工具命令行",
	Long:  "Shop 电商管理后台 CLI 工具，支持数据查询、同步等终端命令",
	PersistentPreRun: func(cmd *cobra.Command, args []string) {
		bootstrap.BootstrapWith(configPath)
	},
	PersistentPostRun: func(cmd *cobra.Command, args []string) {
		bootstrap.Shutdown()
	},
}

func init() {
	rootCmd.PersistentFlags().StringVar(&configPath, "config", bootstrap.DefaultConfigPath, "配置文件路径")
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
