# indices/*.jsonc —— ES 索引结构唯一事实源

本目录下的每个 `.jsonc` 文件对应一个索引的完整结构声明（`settings` + `mappings` + `_meta`），
由 `//go:embed` 编译进二进制（见 `internal/es/embed.go`），装载与优先级见 `internal/es/registry.go`。

| 文件 | 别名 | 物理索引 | status | 事实源出处 |
|---|---|---|---|---|
| `product.jsonc` | `shop-product` | `shop-product_v1` | `active` | `product-consumer/sql/shop-product.json` |
| `orders.jsonc` | `shop-order` | `shop-order_v1` | `draft`（本期不接线） | `order-consumer/sql/shop-order-date-format.json` |

## 语法约定

- 支持 `//` 行注释与 `/* */` 块注释，字符串值里的 `//`（如 `http://x`）不会被误删（`internal/es/jsonc.go` 状态机）。
- **不允许尾随逗号**：注释剥离后必须是严格合法的 JSON，ES 也拒绝尾随逗号。
- `settings` / `mappings` 的层级、数值、字段顺序必须与线上导出的原始 JSON 完全一致，只允许追加注释与 `_meta`。
- `_meta` 与 `aliases` 都不会进入建索引请求体（`IndexDefinition.BodyJSON()` 剔除）：ES 不认识 `_meta`，别名由调用方按场景注入。

## `_meta` 字段

| key | 含义 |
|---|---|
| `alias` | 逻辑别名，物理索引名与迁移流程都以它为入口（必填） |
| `schema_version` | 结构版本号 N，物理索引名 = `<alias>_v<N>`（必填，>=1） |
| `next_version_rule` | 命名规则说明：`<alias>_v<N+1>` |
| `copy_to_only_fields` | 「mapping 声明了但文档 struct 不产出」的白名单，当前 products 仅 `["desc"]`，orders 为 `[]` |
| `status` | `active` 进入可执行清单；`draft` 只解析不执行。缺省按 `active` |
| `date_format` | 可选。空 = 走 ES 默认 `strict_date_optional_time\|\|epoch_millis`（文档必须写 RFC3339）；非空 = 必须按该格式单独序列化 |
| `requires_plugins` | 该索引依赖的 ES 插件，供 `es:migrate` 前置 `_analyze` 探测 |

## 变更分级规则

判定实现：`IndexDefinition.IsDynamicSetting` / `IsStaticSetting` / `NormalizeSettingKey`（`internal/es/definition.go`）。
`es:migrate` 的 diff 必须复用这三个函数，且 **settings key 必须先归一化再比较**（声明文件混用
`settings` 顶层简写与 `settings.index.*`，而 ES 回读统一返回 `index.*`；不归一化会误判为漂移并
触发 15 分片索引的全量重建）。

| 变更内容 | 分级 | 说明 |
|---|---|---|
| `index.number_of_replicas`、`index.refresh_interval`、`index.max_result_window` | 可原地 | `PUT _settings`，无需 close |
| `index.routing.allocation.*`、`index.blocks.*`、`index.search.slowlog.*`、`index.indexing.slowlog.*` | 可原地 | 阈值/路由/封锁类调参 |
| 纯新增 `mappings.properties` 字段 | 可原地 | `PUT _mapping` 只提交新增字段子集；**必须先 migrate 成功，再部署产出该字段的代码** |
| `index.number_of_shards` | **必须蓝绿重建** | 静态项，products 当前 15 分片，重建磁盘/IO 接近翻倍 |
| `index.sort.field` / `index.sort.order` | **必须蓝绿重建** | 创建后不可改；排序字段必须在建模期一次定完 |
| `index.store.preload` | **必须蓝绿重建** | 按静态处理 |
| 任何 `index.analysis.*`（analyzer / tokenizer / filter） | **必须蓝绿重建** | 已分词数据不会重刷，新旧 token 体系不一致会静默错召 |
| 已有字段的 `type` / `analyzer` / `search_analyzer` / `copy_to` / `index_options` / `doc_values` / `fields` / `properties` | **必须蓝绿重建** | 同上 |
| `dynamic` 开关、`_source.excludes` | **必须蓝绿重建** | 两份事实源都依赖它们，diff 已纳入比较 |
| 仅 `_meta` 变化（版本号、注释） | 无操作 | 指纹只覆盖 settings+mappings，diff 为空时 Warn 提示 |
| 既非动态也非静态的未知 settings key | 保守按**必须重建** | 宁可重建，不可静默错判 |

## strict 模式发版约束（最高优先级）

两份 mapping 都是 `dynamic: strict`，未声明字段会被 ES **逐条拒绝**：

1. **先改索引，再改代码**：新增字段必须 `es:migrate`（`ActionInplace` 路径）成功后，才能部署产出该字段的代码。反向操作 = 写入全量失败。
2. **文档 struct 与 mapping 字段集合严格互等**：多一个 key 整条 bulk item 失败；少一个 key 必须登记在 `copy_to_only_fields`，否则视为漏灌数据。护栏在 `internal/es/doc_test.go`。
3. **`ProductIndex` 三处重复**：`product-consumer/internal/consumer/product_index.go`、`shop-search-api/internal/repo/es/product_repo`、`internal/es/doc.go`。改字段 = 跨仓库协议变更，三处必须同步。
4. **`shop-product` 割接后是别名**，不可再建同名物理索引（`product-consumer` 的 `IndexName` 硬编码因此无需改代码）。

## 字段口径速查（易踩坑项）

- 六个 `is_*` 是 **keyword**（Go `int8` 写入被强转为 `"0"`/`"1"`）：查询侧用 `term`，**不得改成 bool**（会变 `"true"`/`"false"`，存量查询全不命中）；装配侧**不得加 `omitempty`**。
- `price` = double，`sales`/`ficti` = long，`cate_id` = keyword 且与 `routing` 同值（`strconv.Itoa`）。
- `id` = keyword，与文档 `_id` 同值（`strconv.FormatInt`）。
- `create_time`/`update_time`：products **无显式 format** ⇒ 必须 RFC3339（`time.Time` 默认序列化）；`2006-01-02 15:04:05` 会被拒为 `date_parse_exception`。orders 变体**显式声明** `yyyy-MM-dd HH:mm:ss`，两者序列化不可共用函数。
- `desc` 是 `store_info`/`description`/`keyword` 的 `copy_to` 汇聚虚拟字段，**文档绝不产出该 key**。
- `product.jsonc` 的 `_source.excludes` 排除了 4 个大文本 ⇒ **无法经 `_source` 回查原值**，数据排查只能看 DB；失败样本日志写完整 doc 快照，不依赖 `_source`。

## 插件依赖

| 索引 | 依赖插件 | 内置组件 |
|---|---|---|
| `shop-product` | `analysis-ik`（`ik_max_word`/`ik_smart`）、`analysis-pinyin` | `html_strip` char_filter、`lowercase` filter、`ngram` tokenizer |
| `shop-order` | `analysis-pinyin`（**不依赖 ik**） | `edge_ngram` / `ngram` tokenizer |

两个索引的自定义 analyzer/tokenizer **完全不同**，插件健康检查不能共用；`es:migrate --dry-run` 前应逐索引 `_analyze` 探测，缺插件时报清晰错误，而不是在灌数阶段整批 400。

## 新增一个索引的步骤

1. 从线上导出 mapping，原样落成 `indices/<name>.jsonc`，只加注释与 `_meta`（`status` 先写 `draft`）。
2. 若有常驻增量写入方，先与其核对字段集合与时间序列化形态，把差异写进文件注释。
3. 实现文档 struct（`doc_*.go`）+ `BuildIndexDoc` 等价的装配 + `sync` Provider，并补 `copy_to_only_fields` 与 strict 护栏测试。
4. 把 `_meta.status` 改为 `active` —— 这是进入 `ExecutableDefinitions()` 的唯一开关，`registry.go` 不需要改动。
