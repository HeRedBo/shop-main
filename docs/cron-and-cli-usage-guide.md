# Cron 定时任务 & CLI 终端命令 使用指南

## 一、概述

### 1.1 多入口架构

项目采用 `cmd/` 多入口架构，编译为三个独立的可执行二进制文件，运行时作为完全独立的进程：

| 入口 | 编译产物 | 进程类型 | 职责 |
|------|----------|----------|------|
| `main.go` | `shop-server` | 常驻服务 | HTTP API 服务，处理前端请求 |
| `cmd/cron/` | `shop-cron` | 常驻服务 | 定时任务调度，周期性执行业务任务 |
| `cmd/cli/` | `shop-cli` | 短生命周期 | 终端命令工具，执行数据查询/同步后退出 |

三个入口在运行时完全隔离——各自独立启动、各自管理生命周期、各自初始化连接池，但通过 `internal/` 和 `pkg/` 目录共享源代码和业务逻辑。

### 1.2 共享初始化机制（bootstrap 包）

所有入口共享 `internal/bootstrap/bootstrap.go` 中的初始化逻辑，采用 **Option 模式** 实现选择性初始化：

```
配置加载 → 日志初始化 → Redis → MySQL → [Casbin] → [Observer] → [JWT] → [Kafka]
                                              ↑ 可选组件，按需开启
```

- **`Bootstrap()`**：全量初始化，包含所有可选组件（Casbin / Observer / JWT / Kafka）
- **`BootstrapWith(path, ...Option)`**：选择性初始化，仅初始基础组件 + 指定的可选组件

`Shutdown()` 会记录已初始化的组件标记，仅关闭已初始化的资源，避免 nil 指针 panic。

---

## 二、CLI 终端命令模块

### 2.1 框架介绍

CLI 基于 [Cobra](https://github.com/spf13/cobra) 框架构建，支持子命令、参数标记、帮助信息自动生成等特性。

### 2.2 目录结构

```
cmd/cli/
├── main.go               # 入口：rootCmd 定义、全局参数、初始化/关闭钩子
├── product_sync.go       # product:sync 命令 — 商品数据查询同步
├── product_sync_bar.go   # product:sync-bar 命令 — 带进度条的分块同步
├── order_report.go       # order:report 命令 — 订单报表统计
├── progress_demo.go      # progress:demo 命令 — 进度条基础演示
├── progress_styles.go    # progress:styles 命令 — 多样式进度条对比
└── progress_pterm.go     # progress:pterm 命令 — pterm 彩色进度条
```

### 2.3 全局参数

| 参数 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `--config` | string | `conf/config.yml` | 配置文件路径 |

全局参数通过 `PersistentFlags` 注册，所有子命令均可使用。

### 2.4 初始化策略

CLI 采用 **选择性初始化**（`BootstrapWith`），仅初始基础组件：

```go
// cmd/cli/main.go
PersistentPreRun: func(cmd *cobra.Command, args []string) {
    bootstrap.BootstrapWith(configPath)  // 仅 配置/日志/Redis/MySQL
},
PersistentPostRun: func(cmd *cobra.Command, args []string) {
    bootstrap.Shutdown()
},
```

**不初始化的组件**：Casbin（权限控制）、Observer（模型观察者）、Kafka（消息队列）——CLI 是短生命周期进程，无需这些组件。

> **注意**：`progress:demo`、`progress:styles`、`progress:pterm` 三个演示命令通过覆盖 `PersistentPreRun` / `PersistentPostRun` 为空函数，跳过了 bootstrap 初始化，因为它们不需要数据库连接。

### 2.5 子命令详解

#### `product:sync` — 商品数据同步

按状态查询商品数据，组装批量数据结构，输出 JSON 到日志文件。

**参数：**

| 参数 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `--status` | string | `""` | 商品状态：`on_sale`（上架）/ `off_sale`（下架） |
| `--hot` | bool | `false` | 是否热卖 |
| `--benefit` | bool | `false` | 是否优惠 |
| `--best` | bool | `false` | 是否精品 |
| `--new` | bool | `false` | 是否新品 |
| `--good` | bool | `false` | 是否良品 |
| `--postage` | bool | `false` | 是否包邮 |
| `--sub` | bool | `false` | 是否订阅 |
| `--integral` | bool | `false` | 是否积分兑换 |
| `--limit` | int | `0` | 限制返回数量（0 不限制） |
| `--page` | int | `1` | 页码 |
| `--page-size` | int | `100` | 每页数量 |

**使用示例：**

```bash
# 查询所有上架商品
./shop-cli product:sync --status on_sale

# 查询热卖商品，限制 50 条
./shop-cli product:sync --status on_sale --hot --limit 50

# 分页查询新品
./shop-cli product:sync --new --page 1 --page-size 50

# 组合条件：上架 + 包邮 + 新品
./shop-cli product:sync --status on_sale --postage --new

# 指定配置文件
./shop-cli --config /path/to/config.yml product:sync --status on_sale
```

数据输出到 `runtime/logs/product_sync/` 日志文件。

---

#### `product:sync-bar` — 带进度条的商品同步

使用主键游标分块查询商品数据（`pkg/chunk` 包），实时显示进度条，逐批输出到日志文件。适用于大数据量同步场景。

**参数：**

| 参数 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `--status` | string | `""` | 商品状态：`on_sale` / `off_sale` |
| `--hot` | bool | `false` | 是否热卖 |
| `--benefit` | bool | `false` | 是否优惠 |
| `--best` | bool | `false` | 是否精品 |
| `--new` | bool | `false` | 是否新品 |
| `--good` | bool | `false` | 是否良品 |
| `--postage` | bool | `false` | 是否包邮 |
| `--sub` | bool | `false` | 是否订阅 |
| `--integral` | bool | `false` | 是否积分兑换 |
| `--chunk-size` | int | `100` | 每批查询数量 |

**使用示例：**

```bash
# 默认分块同步（每批 100 条）
./shop-cli product:sync-bar --status on_sale

# 自定义每批 500 条
./shop-cli product:sync-bar --status on_sale --chunk-size 500

# 同步热卖商品
./shop-cli product:sync-bar --hot --best --chunk-size 200
```

终端输出示例：

```
商品数据同步 ▒████████████████░░░░░░░░░░░░░░░░░░░░░░░░▓ 1200/3000  40.00%  120 it/s
========== 商品数据同步 ==========
处理总数:   3000
每批数量:   100
总耗时:     25.034s
平均速度:   119.8 条/秒
数据已输出到日志文件 (product_sync)
==================================
```

---

#### `order:report` — 订单报表统计

查询订单统计数据，支持按日期、日期范围或全量统计模式。调用 `order_report_service` 共享 service 层。

**参数：**

| 参数 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `--date` | string | `""` | 指定日期，格式 `YYYY-MM-DD` |
| `--start` | string | `""` | 开始日期，格式 `YYYY-MM-DD` |
| `--end` | string | `""` | 结束日期，格式 `YYYY-MM-DD` |
| `--all` | bool | `false` | 全量统计模式（按月汇总） |

**使用示例：**

```bash
# 默认：查询昨天的订单统计
./shop-cli order:report

# 查询指定日期
./shop-cli order:report --date 2025-04-20

# 查询日期范围
./shop-cli order:report --start 2025-04-01 --end 2025-04-30

# 全量统计（按月汇总，从 2020 年至今）
./shop-cli order:report --all
```

输出示例：

```
╔════════════════════════════════════════╗
║    订单统计报表  统计日期: 2025-04-20   ║
╠════════════════════════════════════════╣
║  总下单数:    128                      ║
║  已支付:      96                       ║
║  未支付:      32                       ║
║  待发货:      12                       ║
║  待收货:      45                       ║
║  已完成:      35                       ║
║  退款中:      4                        ║
║  已退款:      2                        ║
╚════════════════════════════════════════╝
```

---

#### `progress:demo` — 进度条演示

基础进度条示例，不初始化数据库连接。

```bash
./shop-cli progress:demo
```

---

#### `progress:styles` — 多样式进度条对比

展示 7 种不同的进度条样式（默认主题、ASCII、Unicode、自定义、彩色、全宽、文件传输风格），不初始化数据库连接。

```bash
./shop-cli progress:styles
```

---

#### `progress:pterm` — pterm 彩色进度条

基于 [pterm](https://github.com/pterm/pterm) 库的彩色进度条演示，支持多颜色、多进度条并发等特色功能，不初始化数据库连接。

```bash
./shop-cli progress:pterm
```

### 2.6 如何新增子命令

**步骤 1**：在 `cmd/cli/` 目录下新建文件，如 `user_export.go`：

```go
package main

import (
    "fmt"
    "shop/pkg/logging"
    "github.com/spf13/cobra"
)

var userExportCmd = &cobra.Command{
    Use:   "user:export",
    Short: "导出用户数据",
    Long:  "按条件查询用户数据并导出到文件",
    Run: func(cmd *cobra.Command, args []string) {
        runUserExport(cmd)
    },
}

func init() {
    // 注册命令参数
    userExportCmd.Flags().String("status", "", "用户状态: active/inactive")
    userExportCmd.Flags().Int("limit", 0, "限制返回数量")

    // 挂载到 rootCmd
    rootCmd.AddCommand(userExportCmd)
}

func runUserExport(cmd *cobra.Command) {
    logger := logging.GetLogger("user")  // 需在 config.yml 注册该日志模块

    status, _ := cmd.Flags().GetString("status")
    // ... 业务逻辑
    fmt.Println("导出完成")
    _ = logger.Sync()
}
```

**步骤 2**：如需独立日志模块，在 `conf/config.yml` 的 `zap.modules` 中添加模块名：

```yaml
zap:
  modules:
    - 'user'   # 已存在则无需重复添加
```

**步骤 3**：编译运行：

```bash
make cli
./build/shop-cli user:export --status active
```

---

## 三、Cron 定时任务模块

### 3.1 框架介绍

定时任务基于 [robfig/cron/v3](https://github.com/robfig/cron) 实现，使用 `WithSeconds()` 选项支持 **6 段 cron 表达式**（秒级精度）。

### 3.2 目录结构

```
cmd/cron/
└── main.go                    # 入口：初始化、注册任务、启动调度器、信号监听

internal/cron/
├── registry.go                # 注册表：管理任务的注册、启动、停止
├── order_report.go            # 旧版订单报表任务（多次查询模式，保留参考）
└── order_stats.go             # 新版订单统计任务（调用共享 service）
```

### 3.3 Registry 注册表 API

`internal/cron/registry.go` 提供了定时任务注册表，封装了 cron 调度器的生命周期管理：

```go
// 创建注册表
registry := cron.NewRegistry()

// 注册任务
registry.Register(name string, spec string, cmd func())

// 启动调度器（将所有已注册任务添加到 cron 并启动）
registry.Start()

// 停止调度器（等待正在执行的任务完成后退出）
registry.Stop()

// 查询
registry.Jobs() []JobEntry    // 返回已注册任务列表（只读副本）
registry.IsRunning() bool     // 调度器是否正在运行
registry.String() string      // 可读描述
```

### 3.4 当前已注册的定时任务

在 `cmd/cron/main.go` 中注册：

```go
registry := cron.NewRegistry()

// 每日 00:00:00 — 旧版订单报表（多次查询模式）
registry.Register("每日订单统计", "0 0 0 * * *", cron.OrderReportJob)

// 每日 01:00 — 新版订单统计（调用共享 service，单 SQL 聚合）
registry.Register("每日订单统计", "0 0 1 * * *", cron.OrderStatsJob)
```

| 任务 | Cron 表达式 | 执行时间 | 说明 |
|------|------------|----------|------|
| `OrderReportJob` | `0 0 0 * * *` | 每天 00:00 | 统计前一天订单数据（多次查询模式） |
| `OrderStatsJob` | `0 0 1 * * *` | 每天 01:00 | 统计前一天订单数据（单 SQL 聚合，推荐） |

### 3.5 初始化策略

Cron 入口采用 **全量初始化**（`Bootstrap()`），包含所有组件：

```go
// cmd/cron/main.go
func main() {
    bootstrap.Bootstrap()  // 配置→日志→Redis→MySQL→Casbin→Observer→JWT→Kafka

    registry := cron.NewRegistry()
    // ... 注册任务
    registry.Start()

    // 阻塞等待退出信号
    ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
    <-ctx.Done()

    registry.Stop()
    bootstrap.Shutdown()
}
```

Cron 是常驻服务，可能需要访问所有基础设施组件，因此使用全量初始化。

### 3.6 如何新增定时任务

**步骤 1**：在 `internal/cron/` 目录下新建任务文件，如 `product_cleanup.go`：

```go
package cron

import (
    "shop/pkg/logging"
)

// ProductCleanupJob 清理过期商品数据
func ProductCleanupJob() {
    logger := logging.GetLogger("product")

    // ... 业务逻辑

    logger.Info("商品清理任务执行完成")
}
```

**步骤 2**：在 `cmd/cron/main.go` 中注册：

```go
registry.Register("商品清理", "0 0 2 * * *", cron.ProductCleanupJob)
//                                         ↑ 每天凌晨 2 点执行
```

**步骤 3**：编译部署：

```bash
make cron
./build/shop-cron
```

### 3.7 Cron 表达式说明

项目使用 **6 段格式**（支持秒级精度），由 `robfig/cron` 的 `WithSeconds()` 选项启用：

```
┌──────────── 秒 (0-59)
│ ┌──────────── 分 (0-59)
│ │ ┌──────────── 时 (0-23)
│ │ │ ┌──────────── 日 (1-31)
│ │ │ │ ┌──────────── 月 (1-12)
│ │ │ │ │ ┌──────────── 星期 (0-6, 0=周日)
│ │ │ │ │ │
* * * * * *
```

**常用表达式示例：**

| 表达式 | 含义 |
|--------|------|
| `0 0 0 * * *` | 每天 00:00:00 |
| `0 30 * * * *` | 每小时第 30 分钟 |
| `0 0 */2 * * *` | 每 2 小时 |
| `0 0 9 * * 1-5` | 工作日每天 09:00 |
| `0 0 0 1 * *` | 每月 1 号 00:00 |
| `0 */5 * * * *` | 每 5 分钟 |

---

## 四、Service 层共享架构

### 4.1 共享模式说明

CLI 和 Cron 入口通过 `internal/service/` 共享业务逻辑，避免代码重复。以订单报表为例：

```
internal/service/order_report_service/
└── order_report.go          # 共享 service：OrderReport 结构体 + 查询方法
                                    ↑
                    ┌───────────────┼───────────────┐
                    │               │               │
              cmd/cli/         internal/cron/    controllers/
           order_report.go    order_stats.go     admin/order.go
           （CLI 调用）        （Cron 调用）     （HTTP 调用）
```

### 4.2 代码结构示例

**Service 层**（`internal/service/order_report_service/order_report.go`）：

```go
// 单条 SQL + CASE WHEN 聚合查询，一次查询获取所有状态统计
func GetOrderReport(startDate, endDate time.Time) (*OrderReport, error) { ... }

// 便捷方法：按日期查询
func GetOrderReportByDate(date time.Time) (*OrderReport, error) { ... }

// 便捷方法：查询昨天（供 Cron 使用）
func GetYesterdayReport() (*OrderReport, error) { ... }
```

**CLI 调用**（`cmd/cli/order_report.go`）：

```go
report, err := order_report_service.GetYesterdayReport()
// 格式化输出到终端...
```

**Cron 调用**（`internal/cron/order_stats.go`）：

```go
report, err := order_report_service.GetYesterdayReport()
// 写入日志...
```

### 4.3 新增业务模块推荐做法

1. **业务逻辑写在 `internal/service/{module}_service/`**，不要写在 cmd 或 cron 中
2. **cmd/cli 和 internal/cron 只做调用和输出格式化**，不包含查询逻辑
3. 如果 CLI 和 Cron 都需要某功能，提取到 service 层共享
4. 如果仅某入口使用，可以直接在对应文件中实现（如 `product_sync.go` 的查询逻辑）

---

## 五、构建与部署

### 5.1 Makefile 使用方法

#### 编译命令

```bash
# 编译所有服务（server + cli + cron）并拷贝配置文件
make build

# 仅编译单个入口
make server    # HTTP 服务
make cli       # CLI 工具
make cron      # 定时任务服务

# 交叉编译 Linux 版本
make build-linux

# 清理构建产物
make clean
```

#### 运行命令

`run-*` 命令会自动完成 **编译 → 拷贝配置 → 从 build/ 目录启动** 三个步骤，确保配置文件相对路径正确：

| 命令 | 作用 | 说明 |
|------|------|------|
| `make run-server` | 编译并启动 HTTP 服务 | 自动拷贝 `conf/config.yml` 到 `build/conf/`，从 `build/` 目录运行 `shop-server` |
| `make run-cron` | 编译并启动定时任务服务 | 同上，从 `build/` 目录运行 `shop-cron` |
| `make run-cli RUN_ARGS="..."` | 编译并执行 CLI 命令 | 通过 `RUN_ARGS` 传递子命令和参数 |

```bash
# 启动 HTTP 服务（编译 + 拷贝配置 + 运行）
make run-server

# 启动定时任务服务（编译 + 拷贝配置 + 运行）
make run-cron

# 执行 CLI 命令（通过 RUN_ARGS 传递参数）
make run-cli RUN_ARGS="order:report"
make run-cli RUN_ARGS="order:report --date 2025-04-20"
make run-cli RUN_ARGS="product:sync --status on_sale --limit 50"
make run-cli RUN_ARGS="product:sync-bar --status on_sale --chunk-size 200"
```

### 5.2 各入口的编译命令

| 入口 | 源码路径 | 编译产物 | 编译命令 |
|------|----------|----------|----------|
| HTTP 服务 | `main.go` | `build/shop-server` | `make server` |
| CLI 工具 | `cmd/cli/` | `build/shop-cli` | `make cli` |
| 定时任务 | `cmd/cron/` | `build/shop-cron` | `make cron` |

编译参数：`CGO_ENABLED=0`（纯静态编译）、`-ldflags="-s -w"`（去除调试信息，减小体积）。

### 5.3 一键编译启动所有服务

使用 `make build` 可一键编译所有入口（server + cli + cron）并自动拷贝配置文件到 `build/` 目录：

```bash
# 一键编译所有服务 + 拷贝配置
make build
```

执行后 `build/` 目录结构：

```
build/
├── conf/
│   └── config.yml          # 自动拷贝的配置文件
├── shop-server              # HTTP 服务二进制
├── shop-cli                 # CLI 工具二进制
└── shop-cron                # 定时任务服务二进制
```

然后分别在独立终端窗口中启动各服务：

```bash
# 终端 1：启动 HTTP 服务
cd build && ./shop-server

# 终端 2：启动定时任务服务
cd build && ./shop-cron

# 终端 3：执行 CLI 命令（按需执行，执行完自动退出）
cd build && ./shop-cli order:report
cd build && ./shop-cli product:sync --status on_sale
```

> **提示**：开发阶段也可直接 `make run-server` / `make run-cron` 自动完成编译 + 配置 + 启动的完整流程。

### 5.4 Docker 部署建议

**Server + Cron 容器化，CLI 直接二进制**：

```dockerfile
# Dockerfile.server
FROM alpine:latest
WORKDIR /app
COPY build/shop-server .
COPY build/conf/ ./conf/
COPY build/runtime/ ./runtime/
EXPOSE 8000
CMD ["./shop-server"]
```

```dockerfile
# Dockerfile.cron
FROM alpine:latest
WORKDIR /app
COPY build/shop-cron .
COPY build/conf/ ./conf/
COPY build/runtime/ ./runtime/
CMD ["./shop-cron"]
```

CLI 工具不需要容器化，直接在宿主机使用二进制执行：

```bash
./build/shop-cli order:report --date 2025-04-20
```

---

## 六、注意事项

### 6.1 进程生命周期

- **CLI 是短生命周期进程**：命令执行完毕后自动退出，无需手动停止
- **Cron 是常驻服务**：需要保持运行，建议使用 systemd / supervisor / Docker 管理进程保活
- **Server 是常驻服务**：同上，需保证进程持续运行

### 6.2 配置路径问题

配置文件路径默认为 `conf/config.yml`（相对路径），**执行时需要确保当前目录下存在该文件**：

- **从项目根目录执行**：`go run ./cmd/cli/ order:report` — 直接使用根目录的 `conf/config.yml`
- **从 build 目录执行**：`make run-cli` — Makefile 会自动拷贝 `config.yml` 到 `build/conf/`
- **手动指定路径**：`./shop-cli --config /absolute/path/config.yml order:report`

### 6.3 日志文件位置

所有日志输出到 `runtime/logs/{module}/` 目录，按模块分目录存储：

| 模块 | 日志路径 | 使用场景 |
|------|----------|----------|
| `app` | `runtime/logs/app/` | 全局默认日志 |
| `product_sync` | `runtime/logs/product_sync/` | 商品同步命令 |
| `order_report` | `runtime/logs/order_report/` | 订单统计报表 |
| `order` | `runtime/logs/order/` | 订单业务 |
| `mysql` | `runtime/logs/mysql/` | SQL 业务日志 |
| `mysql_query` | `runtime/logs/mysql_query/` | SQL 查询日志 |
| `mq` | `runtime/logs/mq/` | Kafka 消息日志 |

### 6.4 新增日志模块

使用新的日志模块前，**必须在 `conf/config.yml` 的 `zap.modules` 中注册**：

```yaml
zap:
  modules:
    - 'app'
    - 'order'
    - 'product_sync'
    - 'order_report'
    - 'your_new_module'    # ← 新增模块
```

然后在代码中获取 logger：

```go
logger := logging.GetLogger("your_new_module")
logger.Info("日志内容")
```

未注册的模块名不会创建独立的日志文件。

---

## 七、进阶用法

### 7.1 泛型分块查询包（pkg/chunk）

`pkg/chunk` 提供了基于 **主键游标**（`id > last_id ORDER BY id ASC`）的泛型分块查询工具，适用于大数据量的批量处理场景。每批查询使用独立 `Session`，避免 GORM 链式调用条件累积。

#### API 一览

| 函数 | 签名 | 适用场景 |
|------|------|----------|
| `ChunkById` | `func ChunkById[T any](db *gorm.DB, chunkSize int, callback func(batch []T, batchNum int) error) error` | 不需要知道总数，逐批处理 |
| `ChunkByIdWithTotal` | `func ChunkByIdWithTotal[T any](db *gorm.DB, model *T, chunkSize int, callback func(batch []T, batchNum int, total int64) error) error` | 需要总数（如进度条计算） |

#### 使用要求

- 模型必须嵌入 `BaseModel`（含 `Id int64` 主键字段），否则 panic
- `db` 参数应已包含 WHERE 条件（如 `Where("is_del = ?", 0)`），chunk 包在此基础上追加游标条件

#### 示例：ChunkById（基础分块）

```go
import "shop/pkg/chunk"

// 分块处理所有未删除商品，每批 200 条
err := chunk.ChunkById(
    global.Db.Model(&models.StoreProduct{}).Where("is_del = ?", 0),
    200,
    func(batch []models.StoreProduct, batchNum int) error {
        fmt.Printf("第 %d 批，%d 条数据\n", batchNum, len(batch))
        // 处理 batch...
        return nil
    },
)
```

#### 示例：ChunkByIdWithTotal（带进度条）

```go
import (
    "shop/pkg/chunk"
    "github.com/schollz/progressbar/v3"
)

var bar *progressbar.ProgressBar

err := chunk.ChunkByIdWithTotal(
    global.Db.Model(&models.StoreProduct{}).Where("is_del = ?", 0),
    &models.StoreProduct{},
    100,
    func(batch []models.StoreProduct, batchNum int, total int64) error {
        // 第一批时创建进度条
        if bar == nil {
            bar = progressbar.NewOptions64(total,
                progressbar.OptionSetDescription("处理中"),
                progressbar.OptionShowCount(),
            )
        }
        // 处理 batch...
        _ = bar.Add(len(batch))
        return nil
    },
)
if bar != nil {
    _ = bar.Finish()
}
```

> **实际项目参考**：`cmd/cli/product_sync_bar.go` 是 `ChunkByIdWithTotal` 的完整使用示例。

### 7.2 进度条库选型

项目中使用两种进度条库，各有适用场景：

| 库 | 包路径 | 特点 | 适用场景 |
|----|--------|------|----------|
| **progressbar** | `github.com/schollz/progressbar/v3` | 轻量、自定义主题、支持 bytes 显示 | 数据同步、批量处理（推荐默认选择） |
| **pterm** | `github.com/pterm/pterm` | 原生彩色、多进度条并发、丰富 UI 组件 | 需要炫酷终端效果、多任务并行展示 |

- **progressbar** 适合绝大多数 CLI 场景，API 简洁，主题定制灵活
- **pterm** 适合需要多进度条并发显示（如同时下载多个模块）或更丰富的终端 UI（标题、色块、表格等）

### 7.3 定时任务错误处理与日志规范

定时任务运行在后台，无法通过终端直接观察，因此 **日志记录是唯一的调试手段**，必须遵循以下规范：

#### 错误必须捕获并记录

```go
func YourJob() {
    logger := logging.GetLogger("your_module")

    result, err := someService.DoSomething()
    if err != nil {
        // ❌ 不要静默忽略
        // ✅ 必须记录错误
        logger.Errorw("任务执行失败", "error", err)
        return
    }

    logger.Infow("任务执行完成",
        "processed_count", result.Count,
        "elapsed", result.Elapsed,
    )
}
```

#### 日志字段规范

| 字段 | 用途 | 示例 |
|------|------|------|
| `error` | 错误信息 | `"error", err` |
| `date` / `month` | 统计时间范围 | `"date", "2025-04-20"` |
| `total_orders` 等 | 业务统计指标 | `"total_orders", report.TotalOrders` |
| `elapsed` | 执行耗时 | `"elapsed", elapsed.String()` |
| `batch` / `batch_size` | 分批处理信息 | `"batch", batchNum` |

#### 任务函数末尾确保日志刷新

```go
func YourJob() {
    logger := logging.GetLogger("your_module")
    // ... 业务逻辑
    _ = logger.Sync()  // 确保缓冲日志写入磁盘
}
```

### 7.4 CLI 命令参数校验与默认值

CLI 命令应做好参数校验和默认值处理，避免运行时 panic：

```go
func runYourCommand(cmd *cobra.Command) {
    // 1. 读取参数
    limit, _ := cmd.Flags().GetInt("limit")
    status, _ := cmd.Flags().GetString("status")

    // 2. 参数校验
    if limit < 0 {
        fmt.Println("--limit 不能为负数")
        return
    }
    if status != "" && status != "on_sale" && status != "off_sale" {
        fmt.Println("--status 仅支持 on_sale / off_sale")
        return
    }

    // 3. 默认值兜底
    if limit == 0 {
        limit = 100  // 合理默认值
    }

    // 4. 执行业务逻辑...
}
```

### 7.5 开发期热重载（air）

项目集成了 [air](https://github.com/air-verse/air) 开发工具，配置文件为 `.air.toml`。在开发阶段，`air` 会监听 `.go` 文件变化，自动重新编译并重启 HTTP 服务，实现代码修改后无需手动重启。

启动开发模式：

```bash
# 项目根目录执行，监听代码变更自动重启 server
air
```

`air` 仅作用于 HTTP 服务入口（`main.go`），Cron 和 CLI 入口不受影响。

**配置说明**：配置文件（`conf/config.yml`）在 `Bootstrap` 阶段一次性读入 `global.CONFIG`，修改配置后需要重启对应进程才能生效。
