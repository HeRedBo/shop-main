# 日志模块统一改造文档

## 一、改造背景

项目原存在两套日志系统：

| 日志系统 | 位置 | 底层库 | 问题 |
|---------|------|--------|------|
| Zap 日志 | `pkg/base/zap.go` | `go.uber.org/zap` + `go-file-rotatelogs` | 仅全局单 logger，无业务模块区分 |
| 标准库日志 | `pkg/logging/log.go` | Go 标准 `log` | 无日志切割、并发不安全、无级别分文件 |

两套日志混用导致排查问题困难，且无法对接 ELK 集中式日志平台。

---

## 二、改造目标

1. **统一日志库**：废弃标准库 `log`，全部切换到 Zap
2. **按业务分模块**：通过 `LoggerManager` 管理多业务模块 logger，每条日志带 `module` 字段
3. **ELK 就绪**：生产环境输出 JSON 格式，支持 Filebeat 直接采集
4. **双模式运行**：开发环境 console 可读性好，生产环境 JSON 结构化
5. **替换 rotatelogs**：将已停止维护的 `go-file-rotatelogs` 替换为 `lumberjack`

---

## 三、架构设计

### 3.1 整体架构

```
pkg/logging/
├── config.go      # 日志配置结构体，从 conf.Zap 映射
├── encoder.go     # 编码器（JSON / Console 双模式）
├── writer.go      # 基于 lumberjack 的日志文件写入器
└── manager.go     # LoggerManager 核心，管理所有模块 logger
```

### 3.2 LoggerManager 工作原理

```
LoggerManager（全局单例）
│
├── sync.Map 存储：moduleName → *zap.SugaredLogger
│
├── "app"     → logger（module="app"）     → runtime/logs/app/app.log
├── "order"   → logger（module="order"）   → runtime/logs/order/order.log
├── "product" → logger（module="product"） → runtime/logs/product/product.log
├── "auth"    → logger（module="auth"）    → runtime/logs/auth/auth.log
├── "http"    → logger（module="http"）    → runtime/logs/http/http.log
├── ...
│
└── 每个 logger 共享相同的 encoder 和 level，独立的 writer
```

### 3.3 调用链路

```
业务代码
  │
  │  global.GetLogger("order").Infof("订单创建: %s", orderId)
  │
  ▼
LoggerManager.GetLogger("order")
  │
  │  从 sync.Map 获取（或创建）module="order" 的 logger
  │
  ▼
zap.SugaredLogger
  │
  │  输出 JSON：{"level":"INFO","module":"order","msg":"订单创建: ORD001",...}
  │
  ▼
lumberjack.Writer（日志切割）
  │
  │  文件 > 100MB 自动切割，保留30天，压缩旧文件
  │
  ▼
runtime/logs/order/order.log
```

---

## 四、配置说明

### 4.1 配置结构（conf/conf.go）

```go
type Zap struct {
    LogLevel    string   `mapstructure:"log-level"`     // debug / info / warn / error
    LogMode     string   `mapstructure:"log-mode"`      // console / json
    LogOutput   string   `mapstructure:"log-output"`    // stdout / file / both
    LogFilepath string   `mapstructure:"log-filepath"`  // 日志文件根目录
    LogFileExt  string   `mapstructure:"log-file-ext"`  // 文件扩展名
    MaxSize     int      `mapstructure:"max-size"`      // 单文件最大 MB
    MaxAge      int      `mapstructure:"max-age"`       // 保留天数
    MaxBackups  int      `mapstructure:"max-backups"`   // 保留文件数
    Compress    bool     `mapstructure:"compress"`      // 压缩旧文件
    Modules     []string `mapstructure:"modules"`       // 业务模块列表
}
```

### 4.2 配置文件（conf/config.yml）

```yaml
zap:
  log-level: 'info'
  log-mode: 'console'          # 生产环境改为 'json'
  log-output: 'both'           # 生产环境改为 'stdout'（ELK）或 'file'
  log-filepath: 'runtime/logs'
  log-file-ext: 'log'
  max-size: 100                # 单文件 100MB
  max-age: 30                  # 保留 30 天
  max-backups: 30              # 最多 30 个文件
  compress: true
  modules:
    - 'app'                    # 默认全局
    - 'order'                  # 订单
    - 'product'                # 商品
    - 'user'                   # 用户
    - 'auth'                   # 鉴权/登录
    - 'http'                   # HTTP 请求/中间件
    - 'menu'                   # 菜单
    - 'role'                   # 角色
    - 'upload'                 # 文件上传
```

### 4.3 配置项说明

| 配置项 | 可选值 | 说明 |
|--------|--------|------|
| `log-level` | `debug` / `info` / `warn` / `error` | 日志输出级别，低于该级别的日志被过滤 |
| `log-mode` | `console` / `json` | `console` 人类可读格式；`json` 结构化 JSON（ELK 友好） |
| `log-output` | `stdout` / `file` / `both` | `stdout` 仅终端；`file` 仅文件；`both` 同时输出 |
| `max-size` | 整数（MB） | 单个日志文件最大体积，超过后自动切割 |
| `max-age` | 整数（天） | 旧日志保留天数 |
| `max-backups` | 整数 | 旧日志文件最大保留个数 |
| `compress` | `true` / `false` | 是否 gzip 压缩旧日志文件 |
| `modules` | 字符串数组 | 启动时预注册的业务模块列表 |

---

## 五、使用方式

### 5.1 获取模块 logger

```go
// 方式一：通过 global 快捷方法（推荐）
global.GetLogger("order").Infof("订单创建: %s", orderId)
global.GetLogger("auth").Errorf("token 验证失败: %v", err)

// 方式二：global.LOG 是 "app" 模块的别名
global.LOG.Error("基础设施初始化失败", err)
```

### 5.2 模块分配规则

| 模块名 | 适用场景 |
|--------|---------|
| `app` | 默认全局，基础设施初始化、未分类日志 |
| `order` | 订单相关业务 |
| `product` | 商品相关业务 |
| `user` | 用户注册/更新/查询 |
| `auth` | 鉴权、登录、JWT、签名验证 |
| `http` | HTTP 请求日志、中间件、参数校验 |
| `menu` | 菜单管理 |
| `role` | 角色管理 |
| `upload` | 文件上传 |

### 5.3 新增业务模块

1. 在 `conf/config.yml` 的 `zap.modules` 中添加模块名
2. 业务代码中使用 `global.GetLogger("新模块名")` 即可

无需其他改动，LoggerManager 会自动创建对应的 logger 和日志文件。

---

## 六、日志输出效果

### 6.1 开发环境（console 模式）

```
2026-08-23T10:00:00.000+0800  INFO  [order]    订单创建: ORD20260823001, 用户: 1001
2026-08-23T10:00:01.000+0800  INFO  [product]  商品库存扣减: SKU001, 数量: 2
2026-08-23T10:00:02.000+0800  ERROR [mysql]    SQL执行失败: connection refused
2026-08-23T10:00:03.000+0800  WARN  [auth]     token 过期: userId=1001
```

### 6.2 生产环境（json 模式）

```json
{"level":"INFO","ts":"2026-08-23T10:00:00.000+0800","caller":"order_service.go:42","module":"order","msg":"订单创建: ORD20260823001, 用户: 1001"}
{"level":"ERROR","ts":"2026-08-23T10:00:02.000+0800","caller":"client.go:88","module":"mysql","msg":"SQL执行失败: connection refused"}
```

Kibana 查询示例：
- 订单模块所有错误：`module:"order" AND level:"ERROR"`
- 鉴权相关所有日志：`module:"auth"`
- 全链路追踪：`traceId:"abc-123"`（后续接入 traceId 后可用）

---

## 七、ELK 对接指南

### 7.1 切换生产模式

修改 `conf/config.yml`：

```yaml
zap:
  log-mode: 'json'       # 切换为 JSON 格式
  log-output: 'stdout'   # 输出到 stdout，Filebeat 采集
```

### 7.2 Filebeat 配置示例

```yaml
filebeat.inputs:
  - type: log
    paths:
      - '/app/runtime/logs/*/*.log'    # 采集所有模块日志
    json.keys_under_root: true
    json.add_error_key: true

output.logstash:
  hosts: ["logstash:5044"]
```

### 7.3 Logstash Pipeline 示例

```ruby
input {
  beats {
    port => 5044
  }
}

filter {
  # module 字段已在日志 JSON 中，无需额外解析
}

output {
  elasticsearch {
    hosts => ["http://elasticsearch:9200"]
    index => "shop-logs-%{+YYYY.MM.dd}"
  }
}
```

---

## 八、依赖变更

| 操作 | 依赖包 | 说明 |
|------|--------|------|
| 新增 | `gopkg.in/natefinch/lumberjack.v2` | 日志文件切割（活跃维护） |
| 移除 | `github.com/lestrrat/go-file-rotatelogs` | 已停止维护 |
| 移除 | `github.com/HeRedBo/pkg/file`（logging 包引用） | 旧日志文件操作不再需要 |

---

## 九、文件变更清单

### 新建文件

| 文件 | 职责 |
|------|------|
| `pkg/logging/config.go` | 日志配置结构体 |
| `pkg/logging/encoder.go` | 编码器（JSON / Console） |
| `pkg/logging/writer.go` | lumberjack 写入器 |
| `pkg/logging/manager.go` | LoggerManager 核心 |

### 删除文件

| 文件 | 原因 |
|------|------|
| `pkg/logging/log.go` | 标准库日志，被 LoggerManager 替代 |
| `pkg/logging/file.go` | 文件路径逻辑已迁移到 writer.go |
| `pkg/base/zap.go` | 旧 Zap 初始化，功能由新 manager 接管 |

### 修改文件

| 文件 | 改动内容 |
|------|---------|
| `conf/conf.go` | 扩展 Zap 配置结构体 |
| `conf/config.yml` | 更新 zap 配置段 |
| `pkg/global/global.go` | 新增 `GetLogger(module)` 方法 |
| `main.go` | 替换初始化流程，接入 LoggerManager |
| `middleware/log.go` | `logging.*` → `global.GetLogger("http").*` |
| `middleware/auth_check.go` | `logging.*` → `global.GetLogger("auth").*` |
| `internal/controllers/admin/LoginController.go` | `logging.*` → `global.GetLogger("auth").*` |
| `internal/controllers/front/IndexController.go` | `logging.*` → `global.GetLogger("upload").*` |
| `internal/controllers/admin/MaterialController.go` | `logging.*` → `global.GetLogger("upload").*` |
| `internal/controllers/admin/UserController.go` | `logging.*` → `global.GetLogger("upload").*` |
| `internal/controllers/admin/RoleController.go` | `logging.*` → `global.GetLogger("role").*` |
| `internal/service/product_service/Product.go` | `logging.*` → `global.GetLogger("product").*` |
| `internal/models/sys_user.go` | `logging.*` → `global.GetLogger("user").*` |
| `internal/models/sys_memu.go` | `logging.*` → `global.GetLogger("menu").*` |
| `internal/models/store_product_attr_result.go` | `logging.*` → `global.GetLogger("product").*` |
| `pkg/jwt/jwt.go` | `logging.*` → `global.GetLogger("auth").*` |
| `pkg/app/form.go` | `logging.*` → `global.GetLogger("http").*` |
| `pkg/app/request.go` | `logging.*` → `global.GetLogger("http").*` |
| `pkg/upload/upload.go` | `logging.*` → `global.GetLogger("upload").*` |
| `internal/observers/order_observer.go` | 移除 `log.Printf` fallback |

---

## 十、注意事项

| 关注点 | 说明 |
|--------|------|
| **向后兼容** | `global.LOG` 保留为 `GetLogger("app")` 的别名，原有 `global.LOG.*` 调用无需改动 |
| **并发安全** | LoggerManager 用 `sync.Map` 存储 logger；Zap logger 本身线程安全 |
| **优雅关闭** | shutdown hook 中已调用 `logging.SyncAll()` 刷新所有 logger 缓冲 |
| **日志切割** | lumberjack 按文件大小切割（默认 100MB），比按时间切割更适合日志量不稳定的场景 |
| **新增模块** | 只需在 `config.yml` 的 `modules` 中添加名称，代码中使用 `global.GetLogger("名称")` 即可 |
| **goroutine 泄漏** | `sync.Map` 中 logger 只增不删（模块数有限），不会泄漏 |

---

## 十一、后续规划

1. **pkg 包日志注入**：待外部 pkg 包（db/cache/mq）支持 `SetLogger` 后，在 `main.go` 初始化时注入对应模块的 logger
2. **traceId 接入**：在 HTTP 中间件中生成 traceId，通过 `zap.Context` 传递，实现全链路日志追踪
3. **日志告警**：对接 ELK 后，可在 Kibana 中配置 ERROR 级别日志的实时告警
