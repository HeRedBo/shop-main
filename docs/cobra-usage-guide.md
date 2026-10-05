# Cobra 使用指南

> 基于 `github.com/spf13/cobra` 及项目 `cmd/cli/` 中的实际用法整理。

---

## 一、Cobra 核心概念

Cobra 由三个核心概念构成：

| 概念 | 说明 | 示例 |
|------|------|------|
| **Command** | 一个命令节点，包含名称、描述、执行逻辑 | `shop-cli`、`product:sync` |
| **Flag** | 命令的命名参数（`--name value`），可设默认值 | `--config conf/config.yml` |
| **Args** | 位置参数，按顺序传入的无名字值 | `product:sync arg1 arg2` |

### 命令层级关系

```
root command (shop-cli)
├── subcommand: product:sync
├── subcommand: product:export   (可扩展)
├── subcommand: order:list       (可扩展)
└── ...
```

Cobra 通过 `rootCmd.AddCommand(subCmd)` 将子命令挂载到父命令下，形成树状结构。执行时按 `shop-cli product:sync --status on_sale` 的方式逐级匹配。

---

## 二、基本使用模式

### 2.1 创建 Root Command

```go
var rootCmd = &cobra.Command{
    Use:   "shop-cli",                          // 命令名称
    Short: "Shop 管理工具命令行",                  // 简短描述（帮助列表显示）
    Long:  "Shop 电商管理后台 CLI 工具，支持...",  // 详细描述（help 时显示）
    Run: func(cmd *cobra.Command, args []string) {
        // 根命令的逻辑（无子命令匹配时执行）
    },
}
```

### 2.2 添加子命令

```go
var productSyncCmd = &cobra.Command{
    Use:   "product:sync",
    Short: "商品数据查询同步",
    Run: func(cmd *cobra.Command, args []string) {
        // 子命令逻辑
    },
}

// 注册到父命令
rootCmd.AddCommand(productSyncCmd)
```

### 2.3 Flags：PersistentFlags vs LocalFlags

| 类型 | 方法 | 作用域 | 典型场景 |
|------|------|--------|----------|
| **PersistentFlags** | `cmd.PersistentFlags()` | 当前命令 + 所有子命令 | 全局配置（`--config`） |
| **LocalFlags** | `cmd.Flags()` | 仅当前命令 | 命令专属参数（`--status`） |

```go
// 全局 flag —— 子命令自动继承
rootCmd.PersistentFlags().StringVar(&configPath, "config", "conf/config.yml", "配置文件路径")

// 本地 flag —— 仅 product:sync 可用
productSyncCmd.Flags().String("status", "", "商品状态: on_sale / off_sale")
productSyncCmd.Flags().Bool("hot", false, "是否热卖")
productSyncCmd.Flags().Int("limit", 0, "限制返回数量")
```

**继承效果**：执行 `shop-cli product:sync --config custom.yml` 时，`--config` 虽定义在 rootCmd 上，但子命令也能识别。

### 2.4 钩子函数执行顺序

Cobra 提供四个执行阶段钩子，分 **Persistent**（向下继承）和 **普通**（仅当前命令）两种：

```
PersistentPreRun   →  PreRun   →   Run   →   PostRun   →  PersistentPostRun
     ↑                    ↑                     ↑                ↑
  父命令会执行         仅当前命令           仅当前命令        父命令会执行
```

完整执行顺序（含父子命令）：

```
rootCmd.PersistentPreRun      ← 最先执行（初始化配置）
  └─ subCmd.PreRun            ← 子命令前置逻辑（如有）
      └─ subCmd.Run           ← 子命令主逻辑
  └─ subCmd.PostRun           ← 子命令后置逻辑（如有）
rootCmd.PersistentPostRun     ← 最后执行（清理资源）
```

### 2.5 参数绑定与变量引用

**方式一：通过变量绑定（推荐用于需要引用的场景）**

```go
var configPath string
rootCmd.PersistentFlags().StringVar(&configPath, "config", "conf/config.yml", "配置文件路径")
// 之后直接使用 configPath 变量
```

**方式二：通过 Get 方法读取**

```go
status, _ := cmd.Flags().GetString("status")
hot, _ := cmd.Flags().GetBool("hot")
limit, _ := cmd.Flags().GetInt("limit")
```

---

## 三、项目中的实际用法解析

### 3.1 `cmd/cli/main.go` — Root Command 定义

```go
package main

import (
    "fmt"
    "os"
    "shop/internal/bootstrap"
    "github.com/spf13/cobra"
)

// 全局变量，用于接收 --config flag 的值
var configPath string

var rootCmd = &cobra.Command{
    Use:   "shop-cli",
    Short: "Shop 管理工具命令行",
    Long:  "Shop 电商管理后台 CLI 工具，支持数据查询、同步等终端命令",

    // PersistentPreRun：在所有子命令执行前运行（包括嵌套子命令）
    // 这里调用 BootstrapWith 完成数据库连接、配置加载等初始化
    PersistentPreRun: func(cmd *cobra.Command, args []string) {
        bootstrap.BootstrapWith(configPath)
    },

    // PersistentPostRun：在所有子命令执行后运行
    // 调用 Shutdown 完成数据库连接关闭、资源释放等清理工作
    PersistentPostRun: func(cmd *cobra.Command, args []string) {
        bootstrap.Shutdown()
    },
}
```

**关键点解读**：

- **为什么用 `PersistentPreRun` 而不是 `PreRun`？**
  - `PersistentPreRun` 会**向下传递**给所有子命令，无论执行 `shop-cli product:sync` 还是未来新增的 `shop-cli order:list`，都会自动先执行 `BootstrapWith()` 初始化。
  - 如果用普通 `PreRun`，则只在 rootCmd 自身被直接执行时触发，子命令不会继承。

- **为什么用 `PersistentPostRun`？**
  - 同理，确保每个子命令执行完毕后都会调用 `Shutdown()` 释放资源，无需在每个子命令中重复编写清理逻辑。

```go
func init() {
    // PersistentFlags 定义的 flag 会被所有子命令继承
    // StringVar 将 flag 值绑定到 configPath 变量
    // 默认值来自 bootstrap.DefaultConfigPath 常量
    rootCmd.PersistentFlags().StringVar(&configPath, "config",
        bootstrap.DefaultConfigPath, "配置文件路径")
}

func main() {
    // Execute 解析命令行参数，匹配命令树，执行对应 Run 函数
    // 返回 error 时输出到 stderr 并以退出码 1 退出
    if err := rootCmd.Execute(); err != nil {
        fmt.Fprintln(os.Stderr, err)
        os.Exit(1)
    }
}
```

### 3.2 `cmd/cli/product_sync.go` — 子命令定义

```go
var productSyncCmd = &cobra.Command{
    Use:   "product:sync",
    Short: "商品数据查询同步",
    Long:  "按状态查询商品数据，组装批量数据结构，输出到日志文件",
    Run: func(cmd *cobra.Command, args []string) {
        runProductSync(cmd)
    },
}
```

**命令名 `product:sync`**：使用冒号分隔命名空间，这是 CLI 工具中常见的分组约定（类似 `git remote add`），方便后续扩展 `product:export`、`product:import` 等。

```go
func init() {
    // ---- 本地 Flags（仅 product:sync 可用）----

    // 字符串 flag：商品状态筛选
    productSyncCmd.Flags().String("status", "", "商品状态: on_sale(上架) / off_sale(下架)")

    // 布尔 flags：各种商品属性筛选条件
    productSyncCmd.Flags().Bool("hot", false, "是否热卖")
    productSyncCmd.Flags().Bool("benefit", false, "是否优惠")
    productSyncCmd.Flags().Bool("best", false, "是否精品")
    productSyncCmd.Flags().Bool("new", false, "是否新品")
    productSyncCmd.Flags().Bool("good", false, "是否良品")
    productSyncCmd.Flags().Bool("postage", false, "是否包邮")
    productSyncCmd.Flags().Bool("sub", false, "是否订阅")
    productSyncCmd.Flags().Bool("integral", false, "是否积分兑换")

    // 整数 flags：分页控制
    productSyncCmd.Flags().Int("limit", 0, "限制返回数量（默认 0 不限制）")
    productSyncCmd.Flags().Int("page", 1, "页码（默认 1）")
    productSyncCmd.Flags().Int("page-size", 100, "每页数量（默认 100）")

    // ---- 注册到父命令 ----
    // init() 在包加载时自动执行，确保子命令被注册
    rootCmd.AddCommand(productSyncCmd)
}
```

**`init()` 中的 `rootCmd.AddCommand()` 注册机制**：

- Go 的 `init()` 函数在包被导入时自动执行，**早于 `main()`**。
- 当 `main()` 调用 `rootCmd.Execute()` 时，所有子命令已通过 `init()` 注册完毕。
- 同一包内多个文件的 `init()` 都会执行，因此每个子命令文件只需在自己的 `init()` 中调用 `AddCommand` 即可。

**Run 函数中读取 Flags**：

```go
func runProductSync(cmd *cobra.Command) {
    // 通过 cmd.Flags().GetXxx() 读取本地 flag 值
    status, _ := cmd.Flags().GetString("status")
    hot, _ := cmd.Flags().GetBool("hot")
    limit, _ := cmd.Flags().GetInt("limit")
    page, _ := cmd.Flags().GetInt("page")
    pageSize, _ := cmd.Flags().GetInt("page-size")
    // ...
}
```

> 注意：这里用 `cmd.Flags()` 而非 `cmd.LocalFlags()`。`Flags()` 返回该命令所有可用 flag（包含继承的 PersistentFlags），`LocalFlags()` 仅返回本地定义的。读取时用 `Flags()` 即可。

### 3.3 `--config` 全局 Flag 继承机制

```
rootCmd.PersistentFlags() 定义 --config
         │
         ├── product:sync    ← 自动继承 --config
         ├── product:export  ← 自动继承 --config（扩展）
         └── order:list      ← 自动继承 --config（扩展）
```

执行 `shop-cli product:sync --config /etc/shop.yml`：

1. Cobra 解析 `--config`，发现它定义在 rootCmd 的 PersistentFlags 上
2. 值写入 `configPath` 变量（因为用了 `StringVar` 绑定）
3. `PersistentPreRun` 触发，`bootstrap.BootstrapWith(configPath)` 使用 `/etc/shop.yml` 初始化
4. 子命令 `product:sync` 的 `Run` 执行

### 3.4 执行流程总结

```
用户输入: shop-cli product:sync --status on_sale --config custom.yml

1. Cobra 匹配 rootCmd → 匹配子命令 productSyncCmd
2. 解析 flags: --config → configPath, --status → cmd.Flags()
3. 执行 rootCmd.PersistentPreRun → bootstrap.BootstrapWith(configPath)
4. 执行 productSyncCmd.Run → runProductSync(cmd)
5. 执行 rootCmd.PersistentPostRun → bootstrap.Shutdown()
```

---

## 四、常用 API 速查

### 4.1 cobra.Command 常用字段

```go
&cobra.Command{
    Use:                "command-name",       // 命令名称
    Short:              "简短描述",            // help 列表中的描述
    Long:               "详细描述",            // help command-name 时显示
    Example:            "command-name --flag", // 使用示例
    Args:               cobra.MinimumNArgs(1),// 参数校验器
    ValidArgs:          []string{"a", "b"},   // 有效的位置参数列表
    Aliases:            []string{"alias1"},   // 命令别名
    Run:                func(cmd, args) {},   // 核心执行逻辑
    RunE:               func(cmd, args) error,// 同 Run，但可返回 error
    PreRun:             func(cmd, args) {},   // Run 之前执行
    PostRun:            func(cmd, args) {},   // Run 之后执行
    PersistentPreRun:   func(cmd, args) {},   // 向下继承的前置钩子
    PersistentPostRun:  func(cmd, args) {},   // 向下继承的后置钩子
}
```

### 4.2 Flag 定义常用方法

| 方法 | 说明 | 签名 |
|------|------|------|
| `StringVar` | 绑定字符串到变量 | `Flags().StringVar(&var, "name", "default", "usage")` |
| `BoolVar` | 绑定布尔值到变量 | `Flags().BoolVar(&var, "name", false, "usage")` |
| `IntVar` | 绑定整数到变量 | `Flags().IntVar(&var, "name", 0, "usage")` |
| `String` | 返回字符串（不绑定变量） | `Flags().String("name", "default", "usage")` |
| `Bool` | 返回布尔值 | `Flags().Bool("name", false, "usage")` |
| `Int` | 返回整数 | `Flags().Int("name", 0, "usage")` |
| `StringSlice` | 字符串切片 | `Flags().StringSlice("name", nil, "usage")` |
| `Count` | 计数（`-vvv` → 3） | `Flags().Count("verbose")` |

**读取方法**：`GetString()`、`GetBool()`、`GetInt()`、`GetStringSlice()` 等。

### 4.3 参数校验器（Args 字段）

```go
Args: cobra.NoArgs            // 不接受参数
Args: cobra.ExactArgs(2)      // 恰好 2 个参数
Args: cobra.MinimumNArgs(1)   // 至少 1 个参数
Args: cobra.MaximumNArgs(3)   // 最多 3 个参数
Args: cobra.RangeArgs(1, 3)   // 1~3 个参数
```

### 4.4 命令执行入口

```go
// Execute 解析 os.Args，匹配命令树，执行 Run/RunE
// 遇到错误返回 error
rootCmd.Execute()

// ExecuteC 同 Execute，但额外返回匹配到的 Command
rootCmd.ExecuteC()

// 自定义输入/输出（测试场景常用）
rootCmd.SetArgs([]string{"product:sync", "--status", "on_sale"})
rootCmd.SetOut(os.Stdout)
rootCmd.SetErr(os.Stderr)
rootCmd.Execute()
```

---

## 五、扩展指南

### 5.1 新增子命令（步骤模板）

以新增 `product:export` 命令为例：

**第一步**：在 `cmd/cli/` 下新建文件 `product_export.go`

```go
package main

import (
    "fmt"
    "github.com/spf13/cobra"
)

var productExportCmd = &cobra.Command{
    Use:   "product:export",
    Short: "商品数据导出",
    Long:  "将商品数据导出为指定格式文件",
    Run: func(cmd *cobra.Command, args []string) {
        runProductExport(cmd)
    },
}

func init() {
    // 定义本地 flags
    productExportCmd.Flags().String("format", "json", "导出格式: json / csv")
    productExportCmd.Flags().String("output", "", "输出文件路径")

    // 注册到父命令
    rootCmd.AddCommand(productExportCmd)
}

func runProductExport(cmd *cobra.Command) {
    format, _ := cmd.Flags().GetString("format")
    output, _ := cmd.Flags().GetString("output")

    fmt.Printf("导出格式: %s, 输出路径: %s\n", format, output)
    // 导出逻辑...
}
```

**第二步**：无需修改其他文件。`init()` 会自动执行注册。

**使用方式**：

```bash
shop-cli product:export --format csv --output /tmp/products.csv
shop-cli product:export --format json --config custom.yml  # --config 自动继承
```

### 5.2 新增命令组

当命令数量增多，可用**父命令 + 子命令**的方式组织命令组。

**示例：`order` 命令组**

```go
// cmd/cli/order.go
package main

import "github.com/spf13/cobra"

var orderCmd = &cobra.Command{
    Use:   "order",
    Short: "订单相关命令",
}

func init() {
    rootCmd.AddCommand(orderCmd)
}
```

```go
// cmd/cli/order_list.go
package main

import "github.com/spf13/cobra"

var orderListCmd = &cobra.Command{
    Use:   "list",
    Short: "查询订单列表",
    Run: func(cmd *cobra.Command, args []string) {
        // 订单列表逻辑
    },
}

func init() {
    orderCmd.AddCommand(orderListCmd)
}
```

```go
// cmd/cli/order_export.go
package main

import "github.com/spf13/cobra"

var orderExportCmd = &cobra.Command{
    Use:   "export",
    Short: "导出订单数据",
    Run: func(cmd *cobra.Command, args []string) {
        // 订单导出逻辑
    },
}

func init() {
    orderCmd.AddCommand(orderExportCmd)
}
```

**使用方式**：

```bash
shop-cli order list --status paid
shop-cli order export --format csv
shop-cli order --help   # 显示 order 下的所有子命令
```

**最终命令树结构**：

```
shop-cli
├── product:sync          (cmd/cli/product_sync.go)
├── product:export        (cmd/cli/product_export.go)
├── order
│   ├── list              (cmd/cli/order_list.go)
│   └── export            (cmd/cli/order_export.go)
└── --config (全局 flag)
```

### 5.3 新增命令的注意事项

1. **文件命名**：每个命令一个文件，文件名与命令名对应（如 `product_sync.go` → `product:sync`）
2. **注册方式**：在 `init()` 中调用 `parentCmd.AddCommand(subCmd)`
3. **初始化复用**：数据库连接、配置加载等公共逻辑放在 rootCmd 的 `PersistentPreRun` 中，子命令无需重复
4. **Flag 作用域**：全局共享的用 `PersistentFlags`，命令专属的用 `Flags`（即 LocalFlags）
5. **错误处理**：推荐使用 `RunE` 替代 `Run`，返回的 error 会被 Cobra 自动打印到 stderr
