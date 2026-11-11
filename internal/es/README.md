# internal/es —— ES 索引运维工具（CLI）交付说明

> 深度设计说明（设计取舍、六个核心功能点实现与坑、代码关联图、运维手册、经验复盘）见 [`docs/es-design.md`](../../docs/es-design.md)。
> 场景化操作手册（「我现在这个情况该按顺序敲哪几条命令」+ 索引命名规则 + 订单接入现状）见 [`docs/es-operations-guide.md`](../../docs/es-operations-guide.md)。
> 本 README 只做命令速查；两者描述冲突时以源码与 `docs/es-design.md` 为准。

本包是 `es:*` 命令族的编排层与底层封装：结构声明与解析（`indices/*.jsonc` + `registry.go` + `definition.go`）、
变更分级（`diff.go` / `migrate.go`）、蓝绿重建（`rebuild.go`）、同名割接（`cutover.go`）、
灌数流水线（`sync/`）、ES 原语封装（`admin/`）。CLI 入口在 `cmd/cli/es_*.go`，父命令与初始化隔离见 `cmd/cli/es.go`。

## 1. 命令一览

调用形式统一为 `go run ./cmd/cli es es:<sub> [flags]`（父命令 `Use: "es"`，子命令名自带 `es:` 前缀，所以是 `es` 后再接 `es:status` 这类全名；写成 `es status` 不会执行任何命令，只会回落打印父命令帮助且退码 0）。以下 flag 名与默认值直接对应 `cmd/cli/es_*.go` 源码，新增/变更以代码为准（与 `docs/es-design.md` §4 的命令示例一致）。

| 命令 | 用途（一行） | 关键 flag（默认值） | 什么时候用 |
|---|---|---|---|
| `es:status` | 只读体检：集群/插件/别名指向/结构漂移/计数差；任何时刻都可跑，不写任何东西 | `--alias=""`（空→全部登记项）、`--tolerance=0`、`--json=false`、`--timeout=30s`、`--indices-dir=""` | 变更前先跑；日常巡检；确认 cutover/rebuild 是否达到预期 |
| `es:cutover` | 一次性同名割接：别名位仍是【物理索引】时，单次 `POST /_aliases`（`remove_index`+`add`）把它换成同名别名 + `<alias>_vN`；旧物理索引连同数据消失，**不可回滚** | `--alias=shop-product`、`--tolerance=0`、`--dry-run=false`、`--yes=false`、`--json=false`、`--timeout=60m`、`--finalize-timeout=60s`、`--tune-ingest=true`、`--chunk-size=1000`、`--concurrency=4`、`--bulk-size=3000`、`--max-failures=100`、`--indices-dir=""` | 存量物理索引首次上别名；执行前必须已完成快照备份 |
| `es:migrate` | 变更分级入口：逐 Definition 只读探测，分为 首建 / Noop / Inplace / Rebuild / 引导 cutover；Rebuild 分支委托 `es:rebuild`，本身不做不可逆动作 | `--alias=all`、`--force-rebuild=false`、`--drop-old=false`、`--tune-ingest=true`、`--keep-new-on-failure=true`、`--resume=false`、`--tolerance=0`、`--dry-run=false`、`--yes=false`、`--json=false`、`--timeout=90m`、`--finalize-timeout=60s`、`--chunk-size=1000`、`--concurrency=4`、`--bulk-size=3000`、`--max-failures=100`、`--indices-dir=""` | 结构有变更（新增字段/调整）或首次部署；**新增字段务必先跑本命令再上线写入代码** |
| `es:rebuild` | 蓝绿重建（别名已存在的结构变更正路）：新建 `<alias>_vN` → 灌数 → Refresh+Count 对账 → 单次 `POST /_aliases` 原子切别名（保留旧索引）→ 差量补偿 | `--alias`（**必填**）、`--tolerance=0`（负值=永不可达标）、`--force=false`、`--drop-old=false`、`--keep-new-on-failure=true`、`--tune-ingest=true`、`--resume=false`、`--dry-run=false`、`--yes=false`、`--json=false`、`--timeout=90m`、`--finalize-timeout=60s`、`--chunk-size=1000`、`--concurrency=4`、`--bulk-size=3000`、`--max-failures=100`、`--indices-dir=""` | mapping/settings 漂移需要重建；`--drop-old` 仅当次调用内生效，旧索引切走别名后的磁盘回收走 `es:cleanup`（按版本红线 + 年龄护栏） |
| `es:sync-products` | 全量灌数（`--dry-run` 为只读预演：打印执行计划，仍会连 MySQL 探边界、仍只读探测 ES，但不打印 SQL/文档样本、不发起任何写）；`es:*` 家族里唯一直接暴露流水线全部旋钮的入口，其中 `--chunk-size / --concurrency / --range-multiplier / --bulk-max-mb` 四项可被 `config.yml` 覆盖（仅本命令） | `--index=shop-product`、`--limit=0`、`--ids=""`、`--since=""`（RFC3339 或 `2006-01-02`）、`--resume=false`、`--retry-failed=""`、`--tolerance=0`、`--max-failures=100`、`--chunk-size=1000`、`--concurrency=4`、`--range-multiplier=4`、`--bulk-size=3000`、`--bulk-max-mb=0`、`--tune-ingest=true`、`--dry-run=false`、`--yes=false`、`--gogc=0`、`--pprof=":0"`、`--timeout=30m`、`--failure-dir=""`、`--indices-dir=""` | 手工灌一批 / 小样本实测（`--limit=1000`）/ 复跑失败项（`--retry-failed=...`） |
| `es:cleanup` | 收敛蓝绿失败留下的无别名孤儿 `*_vN`；默认 `--list-only=true` 只列不删；仍被任何别名持有的索引**硬拒** | `--list-only=true`、`--older-than=168h`、`--index=""`、`--yes=false`、`--timeout=2m` | rebuild/cutover 失败后手工回收磁盘；本集群无 snapshot，删除不可逆 |

## 2. 安全语义（所有写命令共同契约）

- **别名 ↔ 物理索引绝对互斥**：ES 中同名不能共存；`es:cutover` 与 `es:rebuild` 都通过**单次** `POST /_aliases` 原子下发（拆两步会触发 201 自动建空索引，形成写入黑洞），绝不拆成两次。
- **灌入期调优 fail-closed**：`refresh_interval=-1 / number_of_replicas=0` 仅对**不持有别名**的物理索引下发；`isIngestTargetTunable`（`sync/product.go:594`）任何一次别名查询报错都返回 `(false, err)` 并跳过调优，不会误伤线上正在服务的索引。恢复用 `context.WithoutCancel` 派生独立 ctx，`--timeout` 或信号打断也要跑完恢复。
- **对账闸门归编排本层**：`--tolerance` 只在 rebuild 第 5 步的 `Refresh + Count`、cutover 收尾对账、migrate 首建 `ReconcileCounts`（`internal/es/migrate.go:378`）与 delegateRebuild 转发（`migrate.go:292`）这些**编排层**判定点生效；三条灌数装配路径的内层都固定注入哨兵 `es.IngestToleranceIgnore = math.MaxInt64`（`rebuild.go:89` → `es_rebuild.go:244`、`es_cutover.go:272`、`es_migrate.go:243`），避免 sync 抢先失败导致 `AbortedStep` 错报、结论行 `ES=0` 假数、`--force` 分支不可达。
- **`--dry-run` 硬拒所有写原语**：`auditedCluster` / `auditedRebuild` 覆盖 `CreateIndex / PutSettings / Refresh / SwitchAlias* / DeleteIndex / SwitchAliasKeepOld`，dry-run 下一切写调用立刻返回错误；`WriteOps()==[]` 是「零写操作」的单点证明（`TestDryRunAuditRejectsEveryWritePrimitive` 锁定）。
- **`--force` 才可在对账不达标时继续**：不带 `--force` 时 `Reconciled=false` 即在切别名之前中止，别名保持指向旧索引，线上读不受影响；`--tolerance` 负值按「永不可达标」如实中止，不会被静默归 0。
- **交互护栏**：`--yes` 或 TTY 交互确认，非 TTY 环境未带 `--yes` 直接退出码 1、零 ES 调用；`cutoverStdinIsInteractive` 会把 `< /dev/null` 判成非交互，比 `os.ModeCharDevice` 更严。

## 3. 续传与失败重放

- **checkpoint 落盘**：`runtime/es_sync/<sanitize(target)>.checkpoint.json`（`checkpointBaseDir="runtime/es_sync"`，`sync/product.go:44`）；结构 `{target, started_at, total, last_id}`。`last_id` **不由各 scanner 直接写**：区间计划在 scanner 启动前交给 sink（`PlanRanges`，`sync/product.go:186`），每批从 DB 读出后按区间登记进度（`observeRange`，`sync/product.go:151`），游标只推到**最小连续覆盖右边界**（`recordRange` / `frontierLocked`，`sync/product.go:962` / `:1013`）——并发乱序下只单调前进不回退，无法判定连续覆盖时保守不推进（宁可重扫不可漏扫）。首次全量也会写，中断后 `--resume` 才可达。
- **`--resume` 语义**：读取 checkpoint 后把游标下界推进到 `last_id` 之后，并**强制 `Concurrency=1 / RangeMultiplier=1`**（单区间单游标，避免多区间并发跳过低 id 区间丢数）；`StartedAt` 与 `Total` 原样保留，绝不覆盖为恢复时刻。**差量补偿下界 = 任务首次启动时刻 `StartedAt`**（不是恢复时刻），保证中断窗口内的所有变更都会被扫到。删除 checkpoint 文件即可回到「从头全量」。
- **失败明细 jsonl**：`admin.IndexBulk` 在 item 级失败时把每行 `{_id, index, op, type, reason, status, doc}` 追加到 `runtime/es_sync/failed_<sanitize(target)>_<YYYYMMDD_HHMMSS>.jsonl`（`--failure-dir` 覆盖目录）。`--max-failures`（默认 100）累计到阈值即中断整任务，别名不切，checkpoint 保留供 `--resume`。
- **`--retry-failed=<path>`**：仅 `es:sync-products` 支持；跳过区间扫描直接读入 JSONL 复跑，写入仍走 `op_type=index` 幂等（`admin/bulk.go`），重复灌不会 409。

## 4. 已知问题（如实记账）

1. **（本期已修）`es:migrate` 的 `AbortedStep` 文案失真**：下发给内层灌数的容差已改为哨兵 `es.IngestToleranceIgnore`（`cmd/cli/es_migrate.go:243`，构造提为纯函数 `migrateIngestOptions:236`，由 `cmd/cli/es_migrate_test.go:24` 钉住「透传真实容差即变红」），与 `es_rebuild.go:244`、`es_cutover.go:272` 同口径；`--tolerance` 仍在本层转发（`es_migrate.go:181` → `MigrateOptions.Tolerance`），对账闸门归编排本层——Rebuild 分支转发给 runRebuild 第 5 步（`internal/es/migrate.go:292`）、首建分支直接 `ReconcileCounts`（`migrate.go:378`）。
2. **product-consumer 使用 `op_type=create`**：跨仓库的 Kafka 消费端在文档已存在时返回 409 `version_conflict_engine_exception`。本仓库 `internal/es/admin/bulk.go` 已统一走 `op_type=index` 幂等；外部消费端不在本期范围。
3. **并行同步的吞吐收益未做基准实测**：`--concurrency` / `--range-multiplier` 默认值沿用 PHP 版经验，尚未在本地 ES 8.2.2 上做过多并发对比。
4. **集群健康恒 `yellow`**：本地 docker 单节点 + `replicas=1`，副本无处可放属正常态。`es:status` / `es:cutover` / `es:rebuild` 都不把 yellow 当故障，脚本不应据此告警。

## 5. 环境前置

- 本地 ES：docker 单节点 8.2.2，`http://127.0.0.1:9200`，Basic 认证 `elastic/elastic`（`runtime/es_probe/` 里的线上回读样本以此环境为基准，`diff_test.go` 的端到端用例依赖它）。
- `conf/config.yml` 的 `elasticsearch` 段关键字段（一行说明）：
  - `enabled` 总开关；`false` 时所有 `es:*` 命令按「无事可做」正常退出而非报错
  - `hosts` 节点地址列表；`es:status` 的 ping 取 `hosts[0]`（必须带 scheme，如 `http://...`）
  - `username` / `password` Basic 认证，无认证留空
  - `indices-dir` 外置声明目录；空串 → 走 `go:embed` 内置的 `internal/es/indices/*.jsonc`
  - `bulk-workers` / `bulk-action-size` / `bulk-request-size` / `flush-interval` 供 `pkg/es` 侧 BulkProcessor 使用（`flush-interval` 单位是秒，代码里必须乘 `time.Second`）
  - `query-log-enable` `true` 时把 DSL 查询写入 `es_dsl` zap 模块
  - `sync-concurrency` / `range-multiplier` / `bulk-max-mb` / `default-chunk-size` **已接线**：`es:sync-products` 按「显式 CLI flag > 本段配置 > 代码内置默认」三级取值（`ResolveSyncParams`，`sync/provider.go:265`；常量 `sync/provider.go:203-209`；是否显式传入看 cobra `Flag.Changed`，`cmd/cli/es_sync_products.go:189-202`），非法值经 `clampInt` 收敛到内置默认**并告警**，`bulk-max-mb` 的 `0` 是合法值（表示交 `admin.DefaultBulkMaxBytes`）。**边界**：目前只有这一条路径接线，`es:rebuild` / `es:cutover` / `es:migrate` 仍只认各自 flag 默认值
