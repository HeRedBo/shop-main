# ES 工具集：怎么用 + 与 PHP 版差异 + 接口/模式清单

> 面向 PHP 背景读者。只讲六件事：命令全景、与 PHP 差异、使用问答、用到的 ES 接口、内部技术与设计模式。
> 分工：深度原理见 `docs/es-design.md`（892 行，下文简称「设计文档」，按 §x.y 引用）；速查表见 `internal/es/README.md`（56 行）。本文不重复它们的正文。
> 全部行号、flag 名、默认值均为当前工作树实测取，非引用设计文档。

## 1. 命令全景与功能点

### 1.1 调用形式（实测，脚本必须注意）

六个子命令的 `Use:` 自带 `es:` 前缀（`es_sync_products.go:22`、`es_migrate.go:30`、`es_rebuild.go:32`、`es_cutover.go:27`、`es_status.go:28`、`es_cleanup.go:44`），父命令是 `es`（`cmd/cli/es.go:13-36`），所以**完整形式有两个 es**：

```
go run ./cmd/cli es es:status
go run ./cmd/cli es es:migrate --dry-run
```

实测坑：`go run ./cmd/cli es status`（漏前缀）**不报错**，退码 0、只打印父命令帮助 —— 静默 no-op。CI 脚本里拼错命令名会得到「绿灯但什么都没做」的结果，脚本请判退码之外再校验输出。
ES 初始化只在 `es` 父命令的 `PersistentPreRun` 里发生（`es.go:42-44` → `bootstrap.BootstrapWith(configPath, bootstrap.WithElasticsearch())`），`product:sync`、`worker`、`cron` 都不建 ES 连接（`es.go:38-47`，设计文档 §1.2）。

### 1.2 六个命令逐个

#### es:sync-products —— 全量灌数（不碰结构）
- 编排落点：`runESSyncProducts`（`es_sync_products.go:62`）→ `sync.SyncProducts`（`sync/product.go:248`）→ 流水线内核 `sync.Run`（`sync/runner.go:30`）
- flag 21 项（注册 `es_sync_products.go:35-55`，**默认值未变**；其中 `--chunk-size / --concurrency / --range-multiplier / --bulk-max-mb` 四项现可被 `config.yml` 覆盖，见 §3.3）：`--index=shop-product`、`--chunk-size=1000`、`--concurrency=4`、`--range-multiplier=4`、`--bulk-size=3000`、`--bulk-max-mb=0`、`--limit=0`、`--ids=""`、`--since=""`、`--tolerance=0`、`--max-failures=100`、`--retry-failed=""`、`--resume=false`、`--dry-run=false`、`--tune-ingest=true`、`--gogc=0`、`--pprof=":0"`、`--timeout=30m`、`--yes=false`、`--indices-dir=""`、`--failure-dir=""`
- 最小可用：`go run ./cmd/cli es es:sync-products --yes --limit=1000`
- 典型生产：`go run ./cmd/cli es es:sync-products --yes --concurrency=8 --range-multiplier=6 --bulk-size=5000 --timeout=6h --failure-dir=runtime/es_failed`
- 护栏：非 TTY 且未带 `--yes` → 退出 1、零写入（`es_sync_products.go:69-77`）；SIGINT/SIGTERM 与 `--timeout` 合成 ctx（`:86-93`）；进度条独立 goroutine，渲染失败不影响同步（`:95-100`）
- `--dry-run` 是**只读预演**（`runDryRun`，`sync/product.go:433-473`）：打印执行计划——目标、DB 命中行数与 min/max id、区间数与 scanner/writer 数、chunk/bulk 参数、是否执行灌入调优、收尾三件事与 tolerance；**不打印 SQL、不打印文档样本、不发起任何写请求、不改 settings**，但仍会连 MySQL 探边界、仍只读探测 ES（灌入调优的安全判定）。父命令帮助 `es.go:27-28` 与 flag usage `es_sync_products.go:48` 已按这个口径写。

#### es:migrate —— 变更分级入口（首建 / Noop / Inplace / Rebuild / 引导 cutover）
- 编排落点：`es.RunMigrate`（`internal/es/migrate.go:84`，CLI 调用点 `es_migrate.go:174`）→ 逐声明 `migrateOne`（`migrate.go:147`）；Rebuild 分支委托 `delegateRebuild`（`migrate.go:284`）→ `RunRebuild`；下发给内层灌数的容差固定为哨兵（`es_migrate.go:243`），本层对账闸门在 Rebuild 分支转发 `opts.Tolerance`（`migrate.go:292`）、首建分支直接 `ReconcileCounts`（`migrate.go:378`）
- flag 17 项（注册 `es_migrate.go:78-94`）：`--alias=all`（all = 全部已登记且可执行）、`--indices-dir=""`、`--tolerance=0`、`--dry-run=false`、`--yes=false`、`--json=false`、`--timeout=90m`、`--finalize-timeout=60s`、`--force-rebuild=false`、`--drop-old=false`、`--tune-ingest=true`、`--keep-new-on-failure=true`、`--resume=false`、`--chunk-size=1000`、`--concurrency=4`、`--bulk-size=3000`、`--max-failures=100`
- 最小可用：`go run ./cmd/cli es es:migrate --dry-run`
- 典型生产：`go run ./cmd/cli es es:migrate --alias=shop-product --yes --concurrency=8 --timeout=4h`
- 边界：`es:migrate` **永不下发 `remove_index`**；判定「该割接」时只中止并引导去 `es:cutover`（`migrate.go:406`，设计文档 §2.6）

#### es:rebuild —— 蓝绿重建（别名已存在时的结构变更正路，8 步）
- 编排落点：`es.RunRebuild`（`internal/es/rebuild.go:322`，CLI 调用点 `es_rebuild.go:190`）；步骤打印为 `第 n/8 步`（`:371` 起）
- flag 17 项（注册 `es_rebuild.go:86-102`）：`--alias=""`（**必填**，前置是该别名已存在）、`--indices-dir=""`、`--tolerance=0`（负值=永不可达标，不当 0 用）、`--dry-run=false`、`--yes=false`、`--json=false`、`--timeout=90m`、`--finalize-timeout=60s`、`--drop-old=false`、`--keep-new-on-failure=true`、`--force=false`、`--tune-ingest=true`、`--resume=false`、`--chunk-size=1000`、`--concurrency=4`、`--bulk-size=3000`、`--max-failures=100`
- 最小可用：`go run ./cmd/cli es es:rebuild --alias=shop-product --dry-run`
- 典型生产：`go run ./cmd/cli es es:rebuild --alias=shop-product --yes --tolerance=200 --concurrency=8 --timeout=4h`（旧索引默认保留，随时可回滚）

#### es:cutover —— 同名物理索引割接为别名（7 步，不可逆）
- 编排落点：`es.RunCutover`（`internal/es/cutover.go:948`，CLI 调用点 `es_cutover.go:196`）；步骤打印为 `第 n/7 步`（`:997` 起）
- flag 14 项（注册 `es_cutover.go:87-101`）：`--alias=shop-product`、`--indices-dir=""`、`--tolerance=0`、`--force=false`（**不能跳过切换后的差量补偿**，`:90` 描述）、`--dry-run=false`、`--yes=false`、`--json=false`、`--timeout=60m`、`--finalize-timeout=60s`、`--tune-ingest=true`、`--chunk-size=1000`、`--concurrency=4`、`--bulk-size=3000`、`--max-failures=100`（无 `--resume`、无 `--drop-old`）
- 最小可用：`go run ./cmd/cli es es:cutover --alias=shop-product --dry-run`
- 典型生产：`go run ./cmd/cli es es:cutover --alias=shop-product --yes --tolerance=500 --concurrency=8`（执行前必须已完成备份）

#### es:status —— 只读体检（集群 / 别名指向 / 结构漂移 / 计数差）
- 编排落点：无 internal/es 编排函数，逻辑就在 CLI 内：`runESStatus`（`es_status.go:187`）→ `es.NewAdminCluster()`（`:247`）+ `es.LoadRegistry`（`:287`）+ 纯判定 `es.Decide`（`:429`）/ `es.ReconcileCounts`（`:391`）
- flag 5 项（注册 `es_status.go:159-164`）：`--alias=""`（留空=检查 Registry 全部登记项）、`--indices-dir=""`、`--json=false`、`--tolerance=0`（负值直接报错，`:181-183`）、`--timeout=30s`
- 最小可用：`go run ./cmd/cli es es:status`
- 典型生产：`go run ./cmd/cli es es:status --alias=shop-product --json`（退出码纪律见 `:42-43`；`enabled=false` 时正常退出不报错 `:189-192`）

#### es:cleanup —— 回收无别名孤儿 `*_vN`
- 编排落点：`runESCleanup`（`es_cleanup.go:109`），直连原生客户端（`admin.Raw():137`）+ `admin.Delete`（`:222`）
- flag 5 项（注册 `es_cleanup.go:67-71`）：`--list-only=true`、`--older-than=168h`、`--index=""`、`--yes=false`、`--timeout=2m`
- 最小可用：`go run ./cmd/cli es es:cleanup`（默认只列不删）
- 典型生产：`go run ./cmd/cli es es:cleanup --list-only=false --older-than=720h --yes`
- 两道护栏：版本尾缀 + 别名持有（`checkSingleIndexTarget:242-248`、`heldByAlias:147-150`）、创建年龄（`passesAgeGate:259`，`creation.date` 缺失 fail-closed 跳过）；删除确认发生在**任何 ES 调用之前**（`:119-126`）。设计文档 §2.6 有完整推导。

### 1.3 命令 → 能力矩阵（✅ 承担 / 可选 附带 / — 不做）

| 能力 | sync-products | migrate | rebuild | cutover | status | cleanup |
|---|---|---|---|---|---|---|
| 新建索引 | — | ✅ 首建 `migrate.go:317`（**不带别名**） | ✅ 建 `*_vN:453` | ✅ 建 target `:1093` | — | — |
| 更新索引（Inplace） | — | ✅ `migrate.go:264` | 可选（以重建替代） | — | 可选（只报漂移） | — |
| 重建索引（蓝绿） | — | 可选（委托 `:284`） | ✅ | — | — | — |
| 全量同步 | ✅ | 可选（经委托） | ✅ 第 4 步 | ✅ 灌 target | — | — |
| 平滑切换（原子切别名） | — | 可选（经委托） | ✅ 保留旧 `rebuild.go:106` | ✅ 摘旧不可逆 `:614` | 可选（只校验指向） | — |
| 状态查看 | 可选（统计） | 可选（分级结论） | 可选（结论） | 可选（结论） | ✅ | 可选（候选清单） |
| 旧索引清理 | — | 可选（`--drop-old`） | 可选（`--drop-old:580`） | — | — | ✅ |
| 续传 | ✅ `--resume` | ✅ 透传 `:90` | ✅ `--resume:98` | — 显式 `Resume:false`（`es_cutover.go:276`） | — | — |
| 失败重放 | ✅ `--retry-failed` 唯一入口 | — | — | — | — | — |
| 差量补偿 | — | ✅ `es_migrate.go:253` | ✅ `第 7/8 步` | ✅ `第 6/7 步` | — | — |
| dry-run | ✅ | ✅ | ✅ | ✅ | — 天然只读 | 可选（`--list-only`） |

重叠处只有两组，且都是刻意分工：**全量同步**四个写命令最终都汇聚到同一个 `sync.SyncProducts`（不存在第二套灌数实现）；**平滑切换**由 rebuild（可回滚）与 cutover（不可逆）各承担一次，判据是「别名位上现在是别名还是物理索引」。

### 1.4 分层图（每层标实际文件）

```
CLI 装配层        cmd/cli/es.go 父命令 + PersistentPreRun:42-44
                  cmd/cli/es_sync_products.go  ·  es_migrate.go  ·  es_rebuild.go  ·
                            es_cutover.go  ·  es_status.go  ·  es_cleanup.go
        ↓ 只做三件事：解析 flag、注入窄接口实现（SyncFunc / CompensateFunc / CountFunc）、与人交互
编排层            internal/es/migrate.go RunMigrate:85  ·  rebuild.go RunRebuild:322  ·
                  cutover.go RunCutover:948  ·  diff.go Decide（三级分级）
        ↓ 只认接口不认实现，不 import sync 内部类型（rebuild.go:23-25）
同步层            internal/es/sync/product.go SyncProducts:248  →  runner.go Run:30
                  range.go 区间切分  ·  provider.go 接口与默认常量  ·  delta.go 补偿  ·  progress.go
        ↓ admin.Doc  ·  WriteFunc
admin 适配层      internal/es/admin/indices.go  ·  alias.go  ·  bulk.go  ·  client.go Raw():30
        ↓ 唯一直接触碰 *elastic.Client 的一层
共享库 / 客户端   github.com/HeRedBo/pkg/es（global.EsClient）→ olivere/elastic v7.0.32
        ↓ HTTP
集群              Elasticsearch 8.2.2（docker 单节点）

旁路（喂给上面各层）
  定义层   internal/es/indices/product.jsonc · orders.jsonc → embed.go → jsonc.go →
           definition.go（命名/指纹）→ registry.go（按 alias 登记）
  配置层   conf/config.yml elasticsearch 段 → conf/conf.go:86 → internal/bootstrap/bootstrap.go initES
           → pkg/global.EsClient  ·  pkg/chunk（主键游标）  ·  internal/models/store_product.go
```

### 1.5 一次全量同步的数据流（标 ctx 边界）

```
MySQL 表 store_product（models/store_product.go:47-49 定表名）
  → Bounds 取 min/max/total（product.go:119 → boundsLookup:572）
  → ComputeRanges 切 K = range-multiplier × concurrency 段（range.go:12 / :28，默认 4×4=16）
  → rangeCh work-queue（feedRanges:63，scanner 自取，把删除空洞与热点摊平）
  → scanner × concurrency        ctx 边界：挂 errgroup 的 gctx（runner.go:74），取消只打断读
      每批 pkg/chunk.ChunkById 读 --chunk-size 行（product.go:128 → pkg/chunk/chunk.go:32）
        → 装配 BuildIndexDoc（doc_assemble.go:47，含下架/软删资格判定）
        → checkpoint 按区间记账，emit 之前（product.go:150-152 → observeRange:951）
        → 投递 docsCh                容量 = concurrency × 2（runner.go:78 + provider.go:209）= 反压点
  → bulk writer × concurrency    ctx 边界：必须挂 runCtx，不能挂 gctx（runner.go:113-115 注释）
      攒到 bulk-size 3000 条或 10MB → admin.IndexBulk（bulk.go:124 → POST /_bulk :242）
        item 级失败 → 逐条落盘 jsonl（finishBulkResult:208 → dumpFailures:411）
        整批请求级失败 → uncommittedFailures 记账 + 取消整组（runner.go:239-248 / :338）
  → tailFlush                    ctx 边界：context.WithoutCancel + drainTimeout 30s（runner.go:265-278）
  → 灌入期调优的恢复             ctx 边界：tuningContext 独立 ctx（product.go:700-704，defer 先注册后变更）
  → 对账（编排层回读 _count）→ 达标才允许切别名 → 差量补偿（delta.go:178）
  → 全部成功才删 checkpoint（product.go:378）；失败路径一律保留（:352 / :375）

外部中断：SIGINT/SIGTERM 与 --timeout 在 CLI 合成 runCtx（es_sync_products.go:83-91）
```

## 2. 与 PHP 版本的差异对照

**声明**：PHP 参考实现是 artisan 命令 `SyncProducts.php` / `Rebuild.php` / `Migrate.php` / `ProjectIndex.php`，**不在本仓库**，本文档作者未逐行读过这些文件。下表的「PHP 大概怎么做」一列来自原设计的参考意图与本项目早期记录，凡属推测均标 **（待确认）**，不含编造的代码细节。Go 侧每一格都给了本仓库 `文件:行`。

### 2.1 能力级对照

| 能力项 | PHP 大概怎么做 | Go 怎么做（实测出处） | 差异本质 | 值不值 |
|---|---|---|---|---|
| 单命令一把梭全量同步 | 一条 artisan 命令既建索引又灌数（待确认） | 结构侧 `es:migrate` / 数据侧 `es:sync-products` 拆开；但 rebuild、cutover、migrate 各自内部也会灌 | 建结构与写数据被分层，中间可插对账 | **值得** |
| 索引名与 mapping 来源 | 命令内或 config 里硬编码（待确认） | `indices/*.jsonc` 的 `_meta.alias:22` + `IndexName()` 拼 `<alias>_v<schema_version>`（`definition.go:52-53/:220-222`） | 结构从「散在代码里的字符串」变成带指纹的单一事实源 | **值得** |
| 是否串行 | 单进程 chunk 串行（设计文档 §1.1 表已记） | 区间 fan-out + scanner/writer 双池（`runner.go:74-78`） | 并行度可配；但收益未在 8.2.2 基准实测（`README.md:43` 记账） | **值得，但收益未证** |
| 中断后怎么办 | 重跑整条命令，从头再灌（待确认） | checkpoint 区间记账 + frontier 单调推进 + `--resume`（`product.go:962/:1013/:539`） | 小时级任务可续，且不丢低 id 区间 | **值得** |
| 切换是否原子 | 两步或 close→改→open（`diff.go:197` 注释明确写了「不要照搬 PHP 的无条件 close/open」） | 单次 `POST /_aliases`（`cutover.go:265-281`） | 拆两步会掉进「写入 201 + 自动建同名空索引」黑洞（`rebuild.go:17-18`） | **值得，属 P0 级** |
| 失败文档怎么处理 | 抛异常整批停，或忽略（待确认） | item 级分类 + jsonl 落盘 + `--retry-failed` 重放（`bulk.go:208/:411`、`product.go:383`） | 「100 条脏数据不该废掉一次小时级全量」 | **值得** |
| 是否存在差量补偿 | 无；窗口期变更依赖消费端兜（待确认） | 三个命令都有第 n 步补偿（`delta.go:178`） | 灌数耗时带来的窗口漂移被显式处理 | **值得** |
| 是否存在灌入期调优 | 无（待确认） | `--tune-ingest` 临时关 refresh/副本，defer 先注册恢复再变更（`product.go:716`、`rebuild.go:492`） | 只在「target 不持有别名」时才做，安全判定失败就降级跳过（`product.go:310-314`） | **值得** |
| 并发是否可配 | 无 | flag 有，且 `config.yml:57-60` 四个同名字段**现已接线**（仅 `es:sync-products` 一条路径，见 §3.3） | 三级优先级「显式 flag > 配置 > `provider.go:203-209` 常量」 | **值得**；其余三条灌数路径仍只认 flag |
| 命令面拆成 6 个 | 4 条 artisan 命令（待确认） | 6 条：多出的 status / cleanup 是纯运维读数 | 只读诊断与破坏性动作彻底分离 | **值得**；migrate 与 rebuild 的职责重叠属**可砍** |
| dry-run 与 `--yes` 护栏 | 无或仅一句提示确认（待确认） | 审计层硬拒写调用（`cutover.go:344-478`）+ 非 TTY 零 ES 调用 | 破坏性动作可预演、可脚本化且不靠人自觉 | **值得** |

### 2.2 Go 版本文件多的成本拆解（`wc -l` 实数）

| 买的东西 | 文件数 | 行数 | 构成（实测行数） |
|---|---|---|---|
| 并发提速 + 续传 + 失败重放 | 6 | 2235 | product.go 1073、runner.go 392、delta.go 236、provider.go 302、progress.go 160、range.go 72 |
| 破坏性操作安全护栏（分级/审计/原子切换/命名与指纹） | 10 | 4415 | cutover.go 1255、diff.go 754、definition.go 601、rebuild.go 604、migrate.go 433、registry.go 343、doc_assemble.go 206、jsonc.go 118、embed.go 35、doc.go 66 |
| CLI 装配与交互（6 命令 + 父命令） | 7 | 2268 | es_status.go 576、es_cutover.go 371、es_rebuild.go 346、es_cleanup.go 311、es_migrate.go 298、es_sync_products.go 315、es.go 51 |
| admin 适配层（含 8.2.2 坑的封装修正） | 4 | 1118 | bulk.go 557、indices.go 337、alias.go 171、client.go 53 |
| 离线可测（无集群可跑） | 18 | 8817 | internal/es 16 个测试文件 8561 + cmd/cli 2 个 256；Test 函数 **192** 个，Benchmark 0 个 |
| 非测试代码合计 | 27 | **10036** | internal/es 7768（含 admin/sync）+ cmd/cli es 系列 2268 |

对照：PHP 侧四条 artisan 命令合计约几百到一千余行量级（**未实测，待确认**）。差额几乎全部落在上表前三块。

### 2.3 若砍掉某块能省多少（仅陈述，不构成建议；命令面按要求不裁剪）

| 砍掉 | 可省（实数行数／跨切面为约数） | 代价 |
|---|---|---|
| `--retry-failed` 重放 | product.go 第 383-431 行（约 49 行）+ bulk.go 第 456-498 行（约 43 行）+ 1 个 flag | 脏数据只能人工读 jsonl 逐条改 |
| checkpoint 续传 | product.go 第 902-1073 行的 sink 与路径函数（约 170 行）+ 3 处 flag | 小时级任务中断只能从头重灌，并重复扫 MySQL |
| dry-run 审计层 | cutover.go 第 344-478 行（约 135 行，含 IsReadOnlyMethod:486） | 破坏性动作无法预演，只能靠人 review 计划 |
| 灌入期调优 | product.go 第 716-852 行（约 137 行）+ 编排层恢复调用 | 全量更慢、集群压力更高；`--tune-ingest` 现有默认要改 |
| 进度条 | progress.go 全部 160 行 | 长任务无可见性，只能看日志 |
| migrate 与 rebuild 二选一 | migrate.go 433 + es_migrate.go 298 = 731 行 | 失去分级判定入口，每次变更都要人自己判断该走 Inplace 还是重建 |
| 区间并行 fan-out | range.go 全部 72 行 + runner.go 中 scanner 池与 work-queue 分支（约一半，估） | 回到串行，吞吐按并发数线性掉（当前收益本就未基准实测） |

## 3. 使用问题问答

### 3.1 能否一条命令把现有产品数据全量同步进 ES？

**能，但前提是索引已经建好。** 命令：

```
go run ./cmd/cli es es:sync-products --yes
```

- 前置条件：① `elasticsearch.enabled: true`（为 `false` 时 `initES` 跳过初始化，命令在 `admin.Client()` 处拿到「客户端未初始化」错误而不是 panic，`admin/client.go:20-24`）；② `hosts` 带 scheme（`http://127.0.0.1:9200`），`username/password` 按 `elastic/elastic` 配；③ MySQL 可达（`global.Db`）；④ **索引或别名必须已存在** —— `SyncProducts` 全程不做存在性检查（`product.go:248-300` 无 `Exists` 调用），默认 target 是别名 `shop-product`，若别名不存在，`POST /_bulk` 会让 ES 自动创建一个同名空索引（无 mapping、dynamic 猜测），即「写入黑洞」（`rebuild.go:17-18` 注释）。所以首次上 ES 必须先 `es:migrate`（首建路径 `migrate.go:317`，首建**不带别名**、对账达标后才挂）。
- 跑完如何验证：`go run ./cmd/cli es es:status --alias=shop-product`，看 `count_ok` 与结构漂移；或直接对账 `GET shop-product/_count` 与 `es.ProductDocCount` 的口径 `is_del=0 AND is_show=1`（`cutover.go:82-90`）。
- 会写哪些文件：`runtime/es_sync/shop-product.checkpoint.json`（基目录 `product.go:44`，路径生成 `:1033`）；有 item 级失败时追加 `runtime/es_sync/failed_shop-product_20060102_150405.jsonl`（命名 `bulk.go:373`，`--failure-dir` 可改目录）。两者都不会自动清理旧文件。
- 中断后怎么续：`... --resume`。语义：游标下界推进到 `last_id` 之后，并**强制 `Concurrency=1 / RangeMultiplier=1`**（`applyResumeState`，`product.go:539`）；`StartedAt` 与 `Total` 保留首次值（`restoreCheckpointSink:918`）；补偿下界取 `StartedAt` 而非恢复时刻。删掉 checkpoint 文件即回到「从头全量」。

### 3.2 索引在哪里配置？

| 声明文件 | 别名 | 状态 | 物理索引名（拼接结果） |
|---|---|---|---|
| `internal/es/indices/product.jsonc` | `shop-product`（`_meta.alias:22`） | `status: active`（:24）→ 进可执行清单 | `shop-product_v1`（`schema_version: 1` :23） |
| `internal/es/indices/orders.jsonc` | `shop-order`（`_meta.alias:32`） | `status: draft`（:34）→ 解析校验算指纹，但 **migrate/rebuild 不处理** | `shop-order_v1` |

- **文件名与注册键无关**：注册键是 `_meta.alias`（`registry.go:124-149 loadDefinitionFiles` 以 `def.Alias()` 建 `byAlias` 并拒绝同一批里重复 alias；查询走 `Registry.Get(alias)` `:272`）。文件名只进错误信息与日志（`IndexDefinition.source`）。
- **别名名写在哪**：`_meta.alias`。**物理索引名不写在任何文件里**：由 `IndexName(alias, version)` = `alias + "_v" + schema_version` 拼出（`definition.go:52-53` 的 `VersionSeparator`、`:220-222`），下一代名走 `NextVersionName`（`:226`）。
- **顶层 `aliases` 段被刻意剔除**：`BodyJSON()` 丢掉 `_meta` 与 `aliases`（`definition.go:203-218`），**建索引一律不带别名**：蓝绿新建直接用它的原样（`rebuild.go:453` 的 createBody），migrate 首建现在也用它的原样（`migrate.go:327`）——别名只在闸门之后由 `POST /_aliases` 挂上（`migrate.go:389`）；旧 `firstCreateBody` 已删除。
- 新增一个索引要改哪几处：
  1. 新建 `internal/es/indices/<name>.jsonc`，含 `_meta.alias` / `schema_version` / `status: active`；
  2. **`embed.go` 不用改** —— `//go:embed indices/*.jsonc` 是通配（`embed.go:17`），新文件自动进二进制；
  3. 文档 struct 与装配函数：`internal/es/doc.go` 加 struct（json tag 集合必须与 `mappings.properties` key 严格互等，虚拟字段要登记进 `_meta.copy_to_only_fields`），`doc_assemble.go` 加装配；并把 `doc_test.go` 的互等断言扩到新 struct；
  4. 同步侧新增一个 `DocProvider` 实现（`sync/provider.go:162` 的接口 + 类似 `product.go` 的实现与 `ProductAlias:30` 那样的别名常量）；
  5. 想让 `es:status` 做 ES/DB 计数比对，需要在 `cmd/cli/es_status.go:154` 的 `esStatusDBCounters` 加一条 `CountFunc`；不加则如实报 `count_comparable=false`（`:151-152` 注释明确「口径错配比对没比更有害」）；
  6. 若涉及跨仓库字段协议，更新 `internal/es/indices/README.md:57` 的「三处重复」清单。

### 3.3 是否需要修改配置？

`conf/config.yml` 的 `elasticsearch` 段在第 45-60 行，结构体 `conf/conf.go:86`。逐字段的读取方（全仓 grep `CONFIG.Elasticsearch.` 非测试仅 7 处命中）：

| 字段（实取默认值） | 是否真被读取 | 读取点 / 改了才有用的正确姿势 |
|---|---|---|
| `enabled: true` | ✅ | 6 处 `Enabled` 判定（如 `es_status.go:189`）；置 `false` 则 `initES` 不建客户端 |
| `hosts: ["http://127.0.0.1:9200"]` | ✅ | `registry` 之外唯一读取点是 `es_status.go:205`（ping 取 `hosts:0`）；节点地址本身由 `bootstrap.initES` 传给共享库 |
| `username / password`（`elastic/elastic`） | ✅ | `initES` 组装 Basic 认证 |
| `indices-dir: ''` | ✅ | **唯一读取点 `registry.go:117`**；空 → 内嵌；指定但目录空 → 报错而非回落（`:101-104`） |
| `bulk-workers: 10`、`bulk-action-size: 1000`、`bulk-request-size: 5242880`、`flush-interval: 5` | ✅ 但**不影响本期 CLI 灌数** | 供共享库 BulkProcessor（Kafka 消费侧）；CLI 走 `admin.IndexBulk` 自己切块。`flush-interval` 单位秒，代码必须 ×`time.Second`（`conf.go:95`） |
| `query-log-enable: false` | ✅ | 打开后 DSL 落 zap 的 `es_dsl`（`bootstrap.go:359-360`） |
| `sync-concurrency: 4` | ✅ **已接线（仅 `es:sync-products`）** | `ResolveSyncParams`（`provider.go:265`）由 `es_sync_products.go:189-202` 调用；优先级「显式 `--concurrency` > 配置 > 常量 `defaultConcurrency`（`provider.go:205`）」，是否显式传入看 cobra `Flag.Changed` |
| `range-multiplier: 4` | ✅ 同上 | 区间数 K = 该值 × concurrency；`--range-multiplier` 显式传入时优先 |
| `bulk-max-mb: 10` | ✅ 同上 | `0` 是**合法值**，含义为「交 `admin.DefaultBulkMaxBytes = 10MB`」（`bulk.go:42`），所以这一项的代码内置兜底就是 0 |
| `default-chunk-size: 1000` | ✅ 同上 | 对应 Go 字段 `conf.go:100 DefaultChunk`；回落常量 `defaultChunkSize`（`provider.go:204`） |

- 非法值口径：配置值或显式 flag 的非正值经 `clampInt`（`provider.go:213`）收敛到内置默认，**同时追加告警**（`es_sync_products.go:203-205` 落 stderr），绝不静默采纳；`provider.go:197-202` 的注释也已改为「本包常量是最后一级兜底」，不再有「与 config.yml 一致」那类同值硬编码假一致。
- 想调并发的正确姿势：跑 `es:sync-products` 时改配置或传 flag 都有效；但 `es:migrate / es:rebuild / es:cutover` 三条灌数路径**没接配置**，仍只认各自 flag（默认 `1000 / 4 / 3000 / 100` 在四个 CLI 文件里重复声明，改默认值要动 4 个文件）；`docsCh` 缓冲乘子（2，`provider.go:209`）没有 flag，只能改代码。
- 除上表 4 项外，生产需要动的其实只有 `hosts` / `username` / `password` / `enabled`。

### 3.4 生产首次上手的推荐命令顺序

1. `go run ./cmd/cli es es:status` —— 先确认连接、ES 版本、集群健康、插件齐不齐（`requires_plugins: analysis-ik, analysis-pinyin`，`product.jsonc:27`；缺插件会在报告里点名 `MissingPlugins`，`es_status.go:317`）。yellow 属正常（单节点副本无处可放，`:472-474` 只记录不判故障）。
2. `go run ./cmd/cli es es:migrate --dry-run` —— 看每个声明被判成哪一级（首建 / Noop / Inplace / Rebuild / 引导 cutover）与将执行的请求序列；首建的计划现在是四行，末行即「对账达标后 POST /_aliases 单次原子挂别名」（`migrate.go:336-341`）。
3. `go run ./cmd/cli es es:migrate --yes` —— 集群里什么都没有时走首建：建**不带别名**的 `shop-product_v1` → 灌全量 → 对账 → 达标后才挂别名。
4. `go run ./cmd/cli es es:status --alias=shop-product` —— 复核 `count_ok` 与结构无漂移。
5. 之后按变更类型分流：改动态 settings / 加字段 → `es:migrate`（Inplace，秒级）；改分词器、静态项、删字段 → `es:rebuild`（默认可回滚）；别名位上还是老的物理索引 → `es:cutover`（不可逆，先备份）。
6. 残留孤儿索引用 `go run ./cmd/cli es es:cleanup` 先看，再 `--list-only=false --yes` 删。

**第 3 步的顺序（本期已修）**：`migrateFirstCreate`（`migrate.go:317`）先 `PUT /<index>`（body **不含 `aliases`**，`migrate.go:327`）→ 灌数 → `_refresh`+`_count`（`migrate.go:366`/`:371`）→ `ReconcileCounts`（`:378`，与蓝绿同一口径）→ 达标才**单次** `POST /_aliases` 纯 add 挂别名（`:389`）；不达标则索引存在但绝不挂别名，报错点名残留索引名并给两条后续动作（`:382-384`）。旧「先挂别名再灌数」的 `firstCreateBody` 已删除。
**migrate 首建与 `es:rebuild` 的适用差异**：首建本身已过闸门，只适合「别名与同名物理索引都不存在」的环境；一旦**别名已存在或有线上读流量**，结构变更应走 `es:rebuild`——只有它有 `--force` 退路、`--resume`、灌入期调优与切换后的差量补偿（`rebuild.go:514-538`），且默认保留旧索引可回滚。首建留下的无别名残留索引，也可以在手工挂上别名后改用 rebuild 重做。

## 4. 用到的 ES 接口清单

| REST 端点 | 用途 | 发出封装（文件:行） | 走 olivere v7 / PerformRequest 逃生舱 | 实测踩到的坑 |
|---|---|---|---|---|
| `HEAD /`（ping） | 确认集群可达、取版本与节点名 | `es_status.go:218` `rawCli.Ping(hosts:0)` | olivere PingService | 传空串会被 `TrimSuffix` 退化成 `GET /`，触发 `unsupported protocol scheme`；**必须传带 scheme 的地址**（`:216-217` 注释） |
| `GET /_cluster/health` | 集群健康 | `es_status.go:234` | olivere | 单节点 + `replicas=1` → **恒 yellow 属正常**，三个命令都不据此判故障 |
| `PUT /<index>`（settings+mappings 一次带上，**不带 aliases**） | 建索引 | `admin/indices.go:49` `CreateIndex().BodyString`；调用点 `rebuild.go:453`、`cutover.go:1093`、`migrate.go:349` | olivere CreateIndexService | `_meta` 必须剔除，否则 create 阶段 `unknown key` 被拒（`definition.go:203-209`）；蓝绿新建与 migrate 首建都**绝不带别名**，别名一律留到对账达标后由 `_aliases` 挂 |
| `GET /<index>/_mapping` | 结构 diff 的线上侧事实源 | `admin/indices.go:100 GetMapping` → `fetchMapping:110`、路径 `mappingPath:130` | **PerformRequest** | olivere `GetMappingService` 在未显式传 type 时兜底成 `_all` → 请求 `GET /<idx>/_mapping/_all`，8.2.2 无 handler **恒 400**（实测三行：不带 type 200 / `_all` 400 / `_doc` 400，`indices.go:89-99` 注释）；索引不存在返回 `(nil, nil)` 而不是 error |
| `GET /<index>/_settings?flat_settings=true&include_defaults=true` | settings diff | `admin/indices.go:179 GetSettings` | **PerformRequest** | `include_defaults` 未被 olivere 服务暴露；`flat_settings` 是归一化成 `index.` 前缀 key 集合的前提 |
| `PUT /<index>/_settings` | Inplace 变更 + 灌入期临时调优/恢复 | `admin/indices.go:232`（`IndexPutSettings`）；调用点 `migrate.go:264`、`rebuild.go:473/:492`、`cutover.go:1101/:1124`、`sync/product.go:684 putSettings` | olivere | 只对「不持有别名的物理索引」下发（安全判定 `product.go:594/:647`）；恢复必须用独立 ctx，否则 `--timeout` 一到索引就留在 `refresh_interval=-1` 态 |
| `PUT /<index>/_mapping` | Inplace 新增字段 | `admin/indices.go:220`（`PutMapping`） | olivere | PHP 那套「close → 改 → open」不照搬（`diff.go:197` 注释）；`admin.Open/Close` 仍在 `:276/:289` 但主流程不用 |
| `GET /<index>/_count` | 对账 | `admin/indices.go:259`；调用点 `rebuild.go:520`、`cutover.go:1165`、`es_status.go:384` | olivere Count | 必须先 `_refresh`，否则刚灌的数据不可见、count 偏小 |
| `POST /<index>/_refresh` | 让灌入可见 | `admin/indices.go:246`；调用点 `rebuild.go:517`、`cutover.go:1160`、`migrate.go:366` | olivere | 它是 POST，**不属于 dry-run 的 GET/HEAD 白名单**，预演一律拦下（`cutover.go:458-461`） |
| `GET /_cat/indices` | 磁盘/健康/文档数读数 | `admin/indices.go:304`、`cutover.go:199 IndexStats`、`es_cleanup.go:171/:283` | olivere CatIndices | `_cat/indices/<缺失索引>` 在 8.x 返回 **404 而非空数组**，需 `IsNotFound` 退化成空清单（`cutover.go:206-208`）；`creation.date` 缺失时 cleanup fail-closed 跳过 |
| `GET /_cat/aliases` | 别名 → 索引 + `is_write_index` | `cutover.go:166`（Columns alias,index,is_write_index）、`es_cleanup.go:143` | olivere CatAliases | 判断「谁持有别名」只能靠它或 `GET /_alias/<名>`，别用 Registry 猜测 |
| `GET /_alias/<别名名>` | 取别名当前指向 | `admin/alias.go:44`（`cli.Aliases().Alias(alias)`）`AliasIndexes:39` | olivere AliasesService | 入参语义是**别名名** |
| `GET /<index>/_alias` | 取某物理索引挂了哪些别名（调优前置判定） | `admin/alias.go:64-76 IndexAliases` | **PerformRequest** | 原实现写成 `GET /_alias/<index>`，ES 把入参当**别名名**解析 → 物理索引名查不到返回 404 → `(nil,nil)` → 上层误判「无别名，可安全调优」，方向反了（`:60-62` 注释记录了这次修复） |
| `POST /_aliases`（单请求 `remove_index` + `add` 且带 `is_write_index`） | 割接 / 蓝绿切换 | `cutover.go:265-281 SwitchAlias`、`rebuild.go:106/:172/:192`、`migrate.go:335` + `:389`（首建达标后纯 add 挂别名）、`admin/alias.go:107-144 AtomicSwitchAlias` | olivere Alias service（多 Action 一次 `Do`） | ① `is_write_index` **必须与 `remove_index` 同请求**下发，两个写索引在 8.2.2 触发 500 `illegal_state_exception` 并整体拒绝（`cutover.go:261-263`）；② **拆成两次请求**时，第二步之前的写入返回 **201 且 ES 自动建同名空索引**，黑洞比割接更彻底（`rebuild.go:17-18`）；③ 物理索引名与别名**绝对互斥**，`_close` 不解除互斥；④ 割接场景刻意不走 `AtomicSwitchAlias`：它会先断言「待移除索引确实持有该别名」，而割接时 `shop-product` 是物理名、必然误报「别名指向异常」（`cutover.go:255-264`） |
| `POST /_bulk?refresh=false&error_trace=true` | 灌数 | `admin/bulk.go:220` 构造、`:242` `Do`，入口 `IndexBulk:124` | olivere BulkService | **HTTP 恒 200 也可能整批里有失败**：`resp.Errors` 为真时逐 item 扫（`doBulk:246-261`，`seen` 去重）；`dynamic:strict` 违例是 **item 级 400** `strict_dynamic_mapping_exception`；`op_type=create` 遇重复 `_id` → 409 `version_conflict_engine_exception`，所以写入固定 `op_type=index`（`bulk.go:34`、`runner.go:382-383` 注释「绝不改用 create」） |
| `DELETE /<index>` | drop-old / cleanup | `admin/indices.go:66 Delete`；调用点 `rebuild.go:147/:580`、`es_cleanup.go:222` | olivere | 删完必须 invalidateCache（`indices.go:320`），否则共享库 `sync.Map` 存在性缓存（无 TTL）会说索引还在 |
| `GET /_cat/plugins` | 分词插件前置探测 | `cutover.go:183-196 ListPlugins` | **PerformRequest** | v7.0.32 **没有 CatPlugins 服务**（只有基于 cluster stats 的 `Plugins()`），故裸请求（`:180` 注释） |
| `POST /<index>/_clone` | **未使用（实测否决）** | — | — | olivere v7.0.32 **无 CloneIndex** 服务，硬上得自己拼 PerformRequest；权衡后放弃（`cutover.go:26` 决策注释） |
| `snapshot / restore` | **未使用（实测否决）** | — | — | 本地集群**无 `path.repo` 注册仓库**，`GET /_snapshot/_all` 为空，restore 无从谈起（`cutover.go:27`）；连带后果：删除不可逆，`es:cleanup` 是唯一手工回收手段（`es_cleanup.go:54/:124`） |

## 5. 内部技术与设计模式索引

| 模式 / 技术 | 具体落点（实测） | 解决什么问题 | 换别的写法会怎样 | 项目其他模块同款复用 |
|---|---|---|---|---|
| 薄封装 / 适配器（下钻原生客户端） | `admin/client.go:20 Client()` / `:30 Raw()`，四个 admin 文件全部从这里取客户端 | 共享库 `pkg/es` 根包缺 `DeleteIndex / GetMapping / Settings` 等能力（`indices.go:5` 注释） | 业务各处直接持有 `elastic.Client` → nil 防御、缓存坑、错误上下文散落各处 | 无同款（这是本功能独有的分层） |
| 注册表模式 | `registry.go:219 NewRegistry` / `:224 LoadRegistry` / `:239 Register` / `:314 ExecutableDefinitions`；`:124-149` 按 `_meta.alias` 判重 | 结构声明的唯一入口与可执行性开关（draft 不进清单） | 每个命令各自 `ReadDir` 解析 → 同一份 mapping 在六个命令里有六种解释 | `internal/observers/register.go:10 RegisterAll`（事件观察者集中登记） |
| `go:embed` + 外置目录覆盖 | `embed.go:17 //go:embed indices/*.jsonc`；优先级 `--indices-dir` > 配置 > 内嵌（`registry.go:112 resolveIndicesDir`） | 单二进制分发时结构声明必须跟着走，同时保留「不发版改结构」 | mapping 进不了二进制，或外置目录变成唯一来源（现场改了线上无人知） | 无同款（全仓唯一 `go:embed` 使用点） |
| 选项 / 配置装配（functional options） | `bootstrap.BootstrapWith(configPath, bootstrap.WithElasticsearch())`（`cmd/cli/es.go:42-44`） | 让 `es` 命令族独享 ES 初始化，别的命令不连不该连的集群 | 全局无条件初始化 → 跑队列命令也顺手连了 ES | `cmd/worker/main.go:40`、`cmd/cron/main.go:24` 同一函数不同选项组合 |
| 哨兵值 | `IngestToleranceIgnore int64 = math.MaxInt64`（`rebuild.go:89`），注入点 `es_rebuild.go:244`、`es_cutover.go:272`、`es_migrate.go:243`（提为纯函数 `migrateIngestOptions:236`） | 灌数层「只灌不判」，把达标判定权留在编排层真实回读之后 | 内层抢先对账 → 中止步骤错报成 sync、结论行 `ES=0` 是从未回读的假数（三条装配路径本期已全部改掉：migrate 侧由 `es_migrate_test.go:24` 钉住「透传真实容差即变红」） | 无同款 |
| fan-out / fan-in 区间并行 | `range.go:12 ComputeRangeCount`（K = multiplier × concurrency）、`:28 ComputeRanges`、`runner.go` scanner 池 + `docsCh` 汇聚 | 删除空洞与 `cate_id` 倾斜造成热点，多切区间交给 work-queue 自取 | 静态均分 → 长尾区间决定总耗时 | 无同款（`errgroup` 在本仓仅此一处使用） |
| 有界 channel 反压 | `runner.go:78` `make(chan ProviderBatch, concurrency × 2)`（乘子常量 `provider.go:209`） | ES 慢时自动把 MySQL 读堵住，不需要手写限速 | 无界队列 → 内存随数据量线性涨；手动限速 → 多一组要调的参数 | 无同款 |
| pipeline（scanner / writer 解耦） | writer **先启动**再接 scanner（`runner.go:113-115` 注释），关闭顺序 `:166-172` | 读写速率差被缓冲吸收，错误优先级有唯一定义（`:192-201`） | 单 goroutine 串行读+写 → 吞吐 = 两者之和的倒数 | 无同款 |
| errgroup + ctx 取消边界 | `runner.go:74 g, gctx := errgroup.WithContext(runCtx)`；emit 用 `gctx`（`:149-156`）；writer 用 `runCtx` | 「取消只打断读，绝不丢弃已攒好的写」 | writer 也挂 gctx → 任一 scanner 出错就把尾部缓冲一起取消掉 | 无同款 |
| `context.WithoutCancel`（三处用途） | ① tailFlush `runner.go:275`（配 `drainTimeout=30s`）② 灌入调优 apply/restore `product.go:700-704` ③ 编排收尾（恢复 settings + 校验别名）`cutover.go:1115` / `rebuild.go:465` | `--timeout` 或 Ctrl-C 打断后，收尾动作仍必须跑完 | 用 `Background()` 会丢 ctx 里的值（`product.go:338` 注释记录过这条修正）；沿用 gctx 会把超时放大成收尾失败 | 无同款 |
| fail-closed 安全判定 | cleanup 的 `creation.date` 缺失即跳过（`es_cleanup.go:259-264`）；调优安全判定失败 → 降级跳过并告警、零 `PUT /_settings`（`product.go:310-314`）；status 拿不到 DB 口径就报 `count_comparable=false`（`es_status.go:151-152`） | 「判不准」时选择不做破坏性动作 | 判不准当作「安全」→ 对持有别名的线上索引执行 settings 调优 | 无同款 |
| defer-before-mutate + 只回滚已生效项 | `applyIngestTuning`：恢复闭包**先注册再变更**（`product.go:739-758`），回滚条件 `idx < appliedCount && !success`（`:752`），`appliedCount = i+1` 在成功后推进（`:766`） | 部分 PUT 成功时绝不回滚那些没生效的索引；`--timeout` 打断也要恢复 | 变更后再注册 → 中途失败就永久留在 `refresh_interval=-1`；全量回滚 → 对未改动的索引发无关 PUT | 无同款 |
| checkpoint 状态机（按区间记账，frontier 单调推进） | `RangePlanCarrier`（`provider.go:98`）→ `PlanRanges:186` → `planRanges:925`（只认无空洞连续前缀）→ `observeRange:951` → `recordRange:962` → `frontierLocked:1013`；`LastID` 只推到最小连续覆盖右边界，`next <= cp.LastID` 不落盘不回退（`:979`） | 并发乱序完成时，「谁先跑完谁写 LastID」会得到非单调游标，续传静默跳过低 id 区间 → 丢数据且无报错 | 每批直接写 last_id（早期版本就这样）＝ P1 级丢数据 | 无同款 |
| 幂等写入 `op_type=index` | `bulk.go:34 OpIndex="index"`，`defaultWriter`（`runner.go:384`，注释 `:382-383`） | 重放同一条不产生 409；区间不相交所以天然无竞争 | 用 `create` → 重放必然造一批新失败（`README.md:42` 记的是外部 product-consumer 踩过的同款坑） | 消费端另有一套（`op_type=create`，本期不改） |
| 错误分类 + 退避重试 | `classifyBulkItem`（`bulk.go:296`，仅 429/503 与 3 个可重试 type 走退避）；耗尽/取消 → `giveUpFailures`（`:309`，type `retry_exhausted`） | 结构类失败不重试（重试只会重复失败并拖时间），瞬时类才退避 | 一律重试 → 小时级任务里为一批永远坏的数据反复打集群 | 无同款 |
| 失败落盘 JSONL + 重放 | `finishBulkResult:208`（`Failed = len(Failures)`）→ `dumpFailures:411`（`O_APPEND`）；`splitByID:177`（空 `_id` 预置成 type `missing_id`）；`ReadFailures:456` + `parseFailureLine:443`；重放 `runRetryFailed`（`product.go:383`） | strict 违例是 item 级失败、且 `_source.excludes` 让样本无法从 ES 回查 → 落盘的 jsonl 是唯一现场证据 | 只看 HTTP 200 → 结构失败被读成成功；静默 `continue` 丢脏数据（早期真实发生过，设计文档 §5.1） | 无同款 |
| dry-run 写原语拦截 | 审计层 `auditedCluster` / `auditedRebuild`（`cutover.go:344-478`），白名单 `IsReadOnlyMethod:486`（只放 GET/HEAD，`_refresh` 也拦）；非交互判定 `cutoverStdinIsInteractive`（`es_cutover.go:238`，用 `os.SameFile` 认 `< /dev/null`，比 `ModeCharDevice` 更严） | 预演必须只发读请求，且脚本环境不能靠人点确认 | 靠「记得加 --dry-run」→ 忘一次就是一次真实变更 | 无同款 |
| `Unscoped` 差量补偿下界 | `compensateScope`（`delta.go:121-126`）：`Unscoped()` + `update_time >= since` + `id > lastID`；下界取**任务首次启动时刻** `StartedAt` | 不带 `Unscoped` 读不到软删行 → 已删商品永远留在索引里；下界取错 → 漏掉中断窗口内的变更 | 加 `is_show` 过滤会重复全量的资格判定口径，补偿与全量不一致 | `pkg/chunk` 的分块扫描思路（`chunk.go:32`，本功能复用且未改其一行） |
| 幂等 `_aliases` 原子切换 | `CutoverAliasBody:614` / `RebuildSwitchBody:172` / `RebuildRollbackBody:192`（请求体是纯函数产出，可单测可回读）；再跑一次割接判 `already_cutover` 退码 0 | 切换动作可重放、可回滚、可预演 | 「先删后加」两步 → 中间写入自动建同名空索引（§4 表同条） | 无同款 |
| 纯函数拆分以便脱机单测 | `mappingPath`（`indices.go:130`）、`unwrapMappingBody:152`、`ParsePluginComponents`、`DecideCutover:536` / `ReconcileCounts:724` / `ResolveSchemaVersion:774`、`targetVersionRe:738`、SQL 侧 `delta_test.go:69` 用 `gorm.Config.DryRun` + `dryRunDialector` 断言补偿 SQL；包级 var 注入（`reconcileForTest` `product.go:560`、`putSettings:684` 等） | 192 个 Test 不需要真实集群（参数优先级 `ResolveSyncParams`、首建挂别名时机已被纯函数 / spy 钉测）；请求路径/拆封层级这类「一错就静默」的判定被单测钉死 | 判定与 IO 混写 → 只能端到端测，回归成本极高 | `internal/service/*` 的 service 层可测约定（同款思路，非同款机制）；代价见设计文档 §1.1「买贵了」 |

### 5.1 PHP 开发者理解这套架构的最短路径

| 你熟的 artisan / PHP 概念 | 这套 Go 代码里对应的东西 |
|---|---|
| `php artisan xxx:command` 一条命令 | `cmd/cli/es.go` 父命令 + 6 个 Cobra 子命令；入口注册是各文件自己的 `init()` 里 `esRootCmd.AddCommand(...)`（新增命令不用改 `es.go`，`:9-12` 注释） |
| artisan 每次执行都是独立进程、跑完退出 | `cmd/cli`、`cmd/worker`、`cmd/cron` 是**三个独立二进制**，各自 `BootstrapWith` 一套依赖（`cmd/cli/main.go:19`、`cmd/worker/main.go:40`、`cmd/cron/main.go:24`）；没有常驻的 HTTP 进程，所以「缓存」这类状态全在落盘文件里（checkpoint / jsonl） |
| `Model::chunk(1000, fn)` / `->orderBy('id')->cursor()` | `pkg/chunk.ChunkById`（`chunk.go:32`）被 `product.go:128` 复用；`--chunk-size` 就是那个 1000 |
| Eloquent 软删除 `delete_time` | `Unscoped()` 显式读回软删行才能发 delete（`delta.go:11` 注释；GORM 的软删默认自动加 `is_del` 条件，方向与 Eloquent 一致） |
| `config('elasticsearch.hosts')` 一坨数组 | `conf/config.yml` → `conf/conf.go:86` 强类型结构体 → `global.CONFIG`；读不到的字段编译期不会提醒你（§3.3 那四个字段曾因此写成「配了不生效」，本期已接线） |
| `.env` 决定环境 | 启动参数 `-c/--config` 指定 yml 路径（`es.go:43` 的 `configPath`），部署环境靠换文件而非环境变量 |
| 迁移文件 `database/migrations/xxx.php` 的顺序与幂等 | `indices/*.jsonc` + `_meta.schema_version` + `es:migrate` 的三级分级：mapping 变更是「声明文件 + 一次 migrate」，而不是一个有序 migration 队列（这是与 Laravel 最不一样的地方：**没有可回滚的序列，只有期望态 diff**） |
| 队列 job / `retry_after` | `--max-failures` 阈值 + jsonl 落盘 + `--retry-failed` 复跑；分类与退避在 `bulk.go:296/:309` |
| 你可以立刻在别的模块复用的模式 | ① 注册表集中登记 —— `internal/observers/register.go:10 RegisterAll` 已是同款；② service 层共享 + 多入口架构 —— 破坏性动作集中在一处判定、命令层只做装配（设计文档 §1.2 的分层理由照抄即可）；③ functional options —— `bootstrap.WithElasticsearch()` 这种「默认不连、显式才连」的依赖开关，比在 `main` 里堆 if 好维护 |
