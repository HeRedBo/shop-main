# ES 运维与开发手册（商品 / 订单双索引）

> **新人只读这一份就够。** 另外三份 —— [`docs/es-design.md`](./es-design.md)、[`docs/es-usage-and-php-diff.md`](./es-usage-and-php-diff.md)、[`internal/es/README.md`](../internal/es/README.md) —— 是设计原理与历史差异的**存档**，不是操作说明书；若它们与本文口径不一致，以本文为准并请回报差异。
>
> 证据口径：`文件:行号` 全部回源码逐条核对过 —— 一期部分核对于 **2026-11-11**，二期部分（订单接线、别名分发、进度条、割接前置预检）核对于 **2026-11-17**。现场集群为 ES 8.2.2 单节点，`shop-product` 与 `shop-order` 两个历史物理索引都已割接完成并各自挂上别名；文中出现的文档数 / 计数比对数字均为当天 `es:status --json` 的只读实测输出。

## 怎么读

| 你是谁 | 该读哪几章 | 读完你会得到 |
|---|---|---|
| 今天要干活的人（运维、值班、把数据灌进去的人） | 第 0 → 4 章 | 能照着敲命令，知道每条命令敲下去会发生什么、出问题时看哪一行 |
| 明天要加新索引的接手开发 | 第 5 章（前半本也至少要读第 1、3 章） | 知道要动哪几个文件、每处干什么、商品是怎么被写死进去的 |
| 想知道订单是怎么「不新增一条命令」就接上的人 | 第 6 章 | 知道别名分发注册表、进度条、割接前置预检、跨仓库 13 字段协议各自在哪 |

| 章 | 回答什么问题 |
|---|---|
| 第 0 章 | 那七个词到底是啥玩意，我该不该管 |
| 第 1 章 | 索引声明文件怎么写、写错会听到什么报错 |
| 第 2 章 | 一条 `es:migrate` 敲下去，中间发生了什么 |
| 第 3 章 | 六个命令各自的作用、时机、最小敲法 |
| 第 4 章 | 断点续传到底靠什么，为什么不会漏数据 |
| 第 5 章 | 加一个新索引 / 加一种导入要动哪些地方 |
| 第 6 章 | 二期：多业务怎么按别名分发、订单索引上线全貌、跨仓库字段协议 |

---

## 第 0 章 先认识七个名词

这七个词在本项目里不是装饰，每一个都对应一段真实存在的代码和一个真实的现场动作。下面每个词都回答三件事：**它到底干什么**（必须打个比方）、**它是谁的规矩**（ES 官方标准 / 你们线上历史包袱 / 本方案自己加的）、**你需要管吗**。

| 名词 | 它到底干什么（打比方） | 是谁的规矩 | 你需要管吗 |
|---|---|---|---|
| **蓝绿重建** | 像高速上换轮胎：不把车停下线，而是在旁边车道停一辆一模一样的新车（`shop-product_v2`），把货搬过去、清点完数量，再一次性把路标从旧车挪到新车（切别名）。旧车原地停着，随时能开回来 | ES 官方支持的玩法（别名天生就是为这个），但「先灌满并对账达标才敢挪路标」这道闸门是本方案加的 | **每次改字段类型都要用**。日常不用；`es:migrate` 判定到 `rebuild` 时会自动帮你走这条路 |
| **同名割接** | 你们线上那个物理索引就叫 `shop-product`，而路标也叫 `shop-product` —— ES 里这两个名字不能同时存在，所以只能「拆掉旧房子的那一瞬间同时立好新房子」，一次操作做完，中间不能有空档 | **纯粹因你们线上历史包袱才有**：正常新项目一开始就用 `_v1`，根本不会撞到这个 | **一辈子只用一次**（每个历史索引一次）。用完之后 `shop-product` 就变成别名，这个命令以后再也用不上 |
| **对账闸门** | 搬家清点箱子：MySQL 说该有 5 个箱子，新仓库只数出 4 个，那就**不许把门牌号挪过去**，先把差额查清楚。它不是日志、不是提醒，是「过不去就停在这一步」的硬闸 | 本方案自加的（ES 自己没有「灌完必须等于库里数」这种概念） | 日常不用管，它默默工作。只有它拦下来时你会看到一条明确报错，那时才需要管 |
| **差量补偿** | 搬家的中间那十分钟里，家里人还在往旧箱子里放东西。搬完把门牌号挪好后，必须再回看一眼「从搬家开始那一刻起被改动过的东西」，把它们补进新仓库；这段时间被删掉的东西也要在新仓库里删干净 | ES 官方没有，是你们业务在跑、数据一直在变这个事实逼出来的 | 完全不用管，`es:rebuild` / `es:cutover` 结尾自动跑。但要知道**它不能跳过**（`--force` 也放行不了它） |
| **断点续传** | 搬家搬到一半停电了。来电后不是从头再搬，而是看门口那本记了「搬到哪个编号」的登记簿，从那个编号接着搬 | 本方案自加的（ES 的 bulk API 本身不管进度） | 只在「灌数跑到一半挂了」时用一次：`--resume` |
| **失败重放** | 搬家时有三个箱子被门卡住了，你把它们单独登记在一张纸条上。障碍清掉后，**只搬纸条上这三箱**，不用重搬全屋 | 本方案自加的（失败明细由 bulk 逐条回执收集落盘） | 只在灌数报「有 N 条 item 级失败」时用一次：`--retry-failed <纸条路径>` |
| **灌入期调优护栏** | 搬家当天先把「每十分钟清点一次」的规矩临时关掉、把「每个房间必须复印两份」临时改成零份，搬完**必须原样恢复**。护栏 = 只允许对「还没有对外开放的那个仓库」这么做，一旦它挂着门牌号对外营业就绝不临时改 | ES 官方建议的批量导入手法；「对持有别名的索引禁止调优 + 无论成败都恢复」这两条约束是本方案自加的 | 不用管，默认开着（`--tune-ingest=true`）。只有一种情况要留意：单节点集群灌数期间健康度会呈 yellow，那是副本清零的正常表现，不是故障 |

一句话总结这七者的分工：**蓝绿重建 / 同名割接** 是两种「换索引」的路径，**对账闸门 / 差量补偿 / 灌入期调优护栏** 是换索引过程中的三道安全带，**断点续传 / 失败重放** 是灌数据半途出问题时的两条补救路。

### 三条红线（先把不能碰的地方记住）

| 红线 | 为什么 |
|---|---|
| 不要手工 `DELETE` 正在被别名指向的索引 | 搜索线上直接不可用；本方案所有删除动作都走 `es:cleanup`，它有「仍被别名持有即不可删」的硬护栏 |
| 不要把 `_aliases` 切换拆成两次请求 | 参见第 3 章割接一节：中间会出现「名字既不是索引也不是别名」的写入黑洞 |
| 不要手改 `runtime/es_sync/*.checkpoint.json` 里的数字 | 它是游标，改错一段就少灌一段；要重来请删掉该文件而不是编辑它 |

---

## 第 1 章 配置文件怎么写

本章是「我要改 mapping / 我要新加一个索引」的第一站。所有结构事实都在一个文件里，先把这个文件写对，后面所有命令才有意义。

### 1.1 文件放哪、叫什么名

| 位置 | 认什么扩展名 | 谁在用 |
|---|---|---|
| `internal/es/indices/*.jsonc` | **只认 `.jsonc`** | 内置进二进制的那份（正式路径） |
| `--indices-dir` 或 `elasticsearch.indices-dir` 指定的目录 | `.json` 与 `.jsonc` **都加载** | 外置覆盖（不发版改结构） |

三条源码依据：

- 内置那份必须是 `.jsonc`：编译期用 `//go:embed indices/*.jsonc` 挑选文件（`internal/es/embed.go:17`），目录名常量 `EmbeddedIndicesDir = "indices"`（`embed.go:21`），列清单时也是 `fs.Glob(FS, "indices/*.jsonc")`（`embed.go:26-27`）。你把内置文件改名成 `.json`，它根本不会进二进制。
- 外置目录两种都认：`var DefinitionFileExts = []string{".jsonc", ".json"}`（`internal/es/registry.go:30`，注释在 `:28-29` 说明「允许 `.json` 是为了让外置目录能直接堆放从 ES 导出的 mapping 文件」），命中判定在 `hasDefinitionExt`（`registry.go:195-201`）。
- 目录优先级：`--indices-dir` 显式值 > 配置里的 `elasticsearch.indices-dir` > 内置 go:embed（`resolveIndicesDir`，`registry.go:110-119`；配置键定义 `conf/conf.go:91`，`conf/config.yml:51` 现为空串＝用内置那份）。
- 外置目录里**一个声明文件都没有**会直接报错，不会静默回落到内置那份（`registry.go:101`）：`es: 索引目录 <绝对路径> 下没有任何 [....jsonc ....json] 文件（期望至少一份 *.jsonc 索引声明）`。

> **小流程（一句话记住加载顺序）**
> `--indices-dir` 给了吗？ → 给了就用它（空目录=报错）；没给 → 看 config 的 `indices-dir`；还没配 → 用二进制里那份。

### 1.2 文件里可以一行注释都没有

解析器是 JSONC 到 JSON 的注释剥离（`internal/es/jsonc.go` 的 `Strip`，`jsonc.go:27-37` 写明规则：逐字节扫描、字符串字面量里的 `//` 原样保留、块注释替换为一个空格、**不处理尾随逗号**）。所以：

- 一份纯 JSON、一行注释都没有的文件**完全可用**，注释只是可选的可读性增强。
- 反过来，**尾随逗号是非法的**：`Strip` 不帮你删，`encoding/json` 会直接拒绝，而 ES 的 settings/mappings 本身也拒绝尾随逗号。

### 1.3 最小可用文件模板

这是「能跑起来」的最短形状，可直接复制后替换 settings / mappings：

```jsonc
{
  "_meta": {
    "alias": "shop-product",
    "schema_version": 1
  },

  "settings": {
    "refresh_interval": "1s",
    "number_of_shards": 15,
    "number_of_replicas": 1
  },

  "mappings": {
    "dynamic": "strict",
    "properties": {
      "id":   { "type": "long" },
      "name": { "type": "text" }
    }
  }
}
```

对照现成的真实文件：`internal/es/indices/product.jsonc` 的 `_meta` 块起于 **第 21 行**（`alias` 在第 22 行、`schema_version` 第 23 行、`status` 第 24 行）；`internal/es/indices/orders.jsonc` 的 `_meta` 块起于第 34 行（`alias` 第 35 行、`schema_version` 第 36 行、`status: "active"` 第 37 行 —— 二期接线时已置为 `active`，见第 6 章）。

### 1.4 `_meta` 挂在哪，是给谁看的

**位置**：`_meta` 是**文件顶层**的键，与 `settings` / `mappings` 平级（看 `product.jsonc:21`）。它**不是** ES 的 `mappings._meta` —— 如果你把它写进 `mappings` 底下，本方案读不到它，会按「缺少 `_meta`」拒绝加载。

**它是给我们程序看的身份信息，不是给 ES 的结构**。拼装发往 ES 的请求体时，`_meta` 和 `aliases` 这两个顶层键会被整块剔除（`BodyJSON`，`internal/es/definition.go:209-218`，剔除逻辑就是那个 `if k == MetaKey || k == AliasesKey { continue }`；`:203-207` 的注释说明原因 —— ES 在 create_index 阶段会以 unknown key 拒绝它们）。因此：

- ES 侧完全看不到 `_meta`：2026-11-11 实测 `curl shop-product_v1/_mapping | grep -c _meta` 结果为 **0**。
- 想知道线上索引到底是按哪份声明建的，不能靠问 ES，只能靠 `es:status` / `es:migrate --dry-run` 打印的「声明来源 + 指纹」—— 指纹是 settings+mappings 算出来的（`Fingerprint`，`definition.go:191`）。

### 1.5 `_meta` 逐字段表

| 字段 | 必填 | 缺省 | 写错会怎样（原文报错） |
|---|---|---|---|
| `alias` | **是** | 无 | `_meta.alias 不能为空（物理索引名与别名都依赖它）`（`parseMeta`，`definition.go:514`） |
| `schema_version` | **是** | 无 | `_<meta>.schema_version=0 非法，必须是从 1 开始的整数版本号`（`definition.go:524`） |
| `status` | 否 | `active`（`DefaultStatus`，`definition.go:50`） | `_<meta>.status="xxx" 非法，只能是 active 或 draft`（`definition.go:540`）；常量在 `:42`（active）/ `:45`（draft） |
| `date_format` | 否 | 无 | 仅作约定说明 |
| `copy_to_only_fields` | 否 | 无 | 同上 |
| `requires_plugins` | 否 | 无 | 不填就没有「缺插件先拦住」这层保护；填了会进 `es:status` / migrate 前置校验 |
| `next_version_rule` | 否 | 无 | **纯注释性质**，程序不据此自动升版本（见 1.7） |
| `notes` | 否 | 无 | 无 |

整块 `_meta` 缺失是**硬失败**，文件根本装载不进来（`definition.go:118-120` 的 `objectAt(body, MetaKey)` 失败分支），报错原文（2026-11-11 实测）：

```
es: 索引声明 <路径> 缺少 _meta（本方案的结构事实源必须自带元数据）: 缺少 "_meta" 字段
```

外层文案在 `definition.go:120`，内层的 `缺少 "_meta" 字段` 来自通用取值函数 `objectAt`（`definition.go:585-591`，具体文案 `fmt.Errorf("缺少 %q 字段", key)` 在 `:588`）。

另外一条只有本方案才有的硬约束：`mappings.dynamic` 不是 `strict` 会被直接拒绝加载（`definition.go:131`）。**这条护栏只在走命令建索引时生效**，手工 `PUT` 建索引不享受。

### 1.6 物理索引名是拼出来的，不写在任何文件里

命名公式（这是全手册唯一需要背下来的公式）：

```
IndexName(alias, version)  =  alias + "_v" + version
例：shop-product + _v + 1  =  shop-product_v1
```

源码：`IndexName`（`internal/es/definition.go:221-223`），分隔符常量 `VersionSeparator = "_v"`（`definition.go:53`），装配点把结果写进 Definition 的 `name` 字段（`definition.go:139`）。

这张图在讲：别名、物理索引、文件里的版本号三者是怎么互相牵制的。

```mermaid
flowchart LR
    F["声明文件 _meta.alias 等于 shop-product"] --> AL["别名 shop-product"]
    V["声明文件 _meta.schema_version 等于 1"] --> N["拼出物理索引名"]
    F --> N
    N --> P1["物理索引 shop-product_v1"]
    AL --> P1
    V2["把 schema_version 改成 2"] --> P2["物理索引 shop-product_v2"]
    P2 -. 切换别名后由它接管 .-> AL
```

### 1.7 从 PHP 来的人最容易踩的两点

| 你以前习惯的 | 这里是相反的 | 后果与正确做法 |
|---|---|---|
| 程序自己数出 `v_0` / `v_1`，每次发布自动 +1 | **版本号来自文件，不是程序数的**。`_meta.schema_version` 不改，目标名就永远不变；`NextVersionName`（`definition.go:226`）与 `admin.WriteIndexName`（`internal/es/admin/alias.go:163`）在非测试代码里**零调用**，`next_version_rule` 也只是给人看的约定 | 改了 mapping 却忘了把 `schema_version` 从 1 改成 2，命令会认为「目标就是现在这个索引」，于是不重建、只可能报漂移；要出新版本必须**手改文件里的数字** |
| 迁移完把旧索引 `close` 掉 | 这里**不 close**。旧索引默认原地保留，用来随时回滚 | 保留的残留靠两条路收敛：`es:rebuild --drop-old --yes` 顺手删，或事后用 `es:cleanup` 列出来再删（见第 3 章） |

---

## 第 2 章 一条命令敲下去，中间发生了什么

本章以 `es:migrate --alias shop-product` 为主线做一次**逐步走查**。看懂这一条，其余五条的骨架是一样的。

### 2.1 第 0 步：先把命令名写对（两个 `es`）

```bash
go run ./cmd/cli es es:migrate --alias shop-product --dry-run
#             ^^^ 父命令   ^^^^^^^^^^ 子命令本身的叫法
```

六个子命令的 `Use` 自带 `es:` 前缀（`cmd/cli/es_status.go:29`、`es_migrate.go:29`、`es_rebuild.go:32`、`es_cutover.go:27`、`es_sync_products.go:23`、`es_cleanup.go:44`），父命令是 `es`（`cmd/cli/es.go:16`）。写成 `es migrate` 会被 `validateEsSubCommandArgs`（`cmd/cli/es.go:57`）拦住，**退码 1** 并给出建议：`es 没有子命令 "migrate"：… 正确用法 es es:<sub>（是否想用 es:migrate？）`（文案 `es.go:62` 与 `:64`）。无参 `es` 仍打印帮助并退码 0。

为什么要单独一棵 `es` 子树：`cmd/cli` 是三入口之一，ES 工具族被装配成 `es` 子树并独占一层 `PersistentPreRunE`（Cobra 只执行最近一层，`es.go:94`），于是 `es:*` 只初始化 ES / MySQL / Redis，**不初始化 Kafka**；`es` 父命令自身在该钩子里短路，所以参数写错时连不上数据库也不会先炸。

### 2.2 第 1 到第 6 步：走查表

| 步 | 发生了什么 | 源码 | 敲完会看到 |
|---|---|---|---|
| 1 | 读配置 `elasticsearch` 段建 ES 客户端 | `bootstrap.go:110`（`WithElasticsearch`）→ `:351`（`initES` 取 `global.CONFIG.Elasticsearch`）；`conf/config.yml:46-51` 是 enabled/hosts/username/password/indices-dir | `enabled=false` 时整条命令打「Elasticsearch 未启用（elasticsearch.enabled=false），跳过迁移」并**退码 0**（`cmd/cli/es_migrate.go:126-129`） |
| 2 | 按目录优先级加载索引声明，建 Registry | `es.LoadRegistry`（`cmd/cli/es_migrate.go:133` → `internal/es/registry.go:222`），实际干活的是 `LoadDefinitions`（`registry.go:39`） | 装载失败会带文件路径与原因，直接中止 |
| 3 | 按 `--alias` 圈定要处理的声明；`all`／空＝全部可执行声明 | `pickDefinitions`（`migrate.go:112` 调用，函数体 `:134`） | `--alias` 填了没登记的别名 → 立刻报错 |
| 4 | 确认范围（实写需要 `--yes` 或 TTY 交互；`--dry-run` 免确认），然后取共享客户端 | `es_migrate.go:144-154`；`admin.Client()` 在 `:156`；超时/信号上下文 `:160-167` | 非 TTY 又没带 `--yes`：`非 TTY 环境必须显式传 --yes 才能执行迁移（可能触发蓝绿重建/首建写操作）` |
| 5 | **拼目标索引名**：`def.Name()` ＝ `alias + "_v" + schema_version` | `definition.go:139` / `:221-223` | 不打印也没关系，下一步的分级结论里会带名字 |
| 6 | **探测别名位现状**：先 `GET /_alias/<alias>` 看有没有指向；没有再 `HEAD /<alias>` 看是不是同名物理索引 | `migrateOne`（`migrate.go:151`）里的 `c.AliasTargets`（`:170`）与 `c.IndexExists`（`:188`）；探测原始事实三件套定义在 `AliasProbe`（`internal/es/cutover.go:555-561`） | 每条声明一行 `[migrate] <alias>: <结论>` |

这张图在讲：`es:migrate` 拿到一条声明后，靠哪几个判断走到六个不同的出口。

```mermaid
flowchart TD
    A["es:migrate 读到一条声明"] --> B["拼出目标名 alias 加 _v 加版本号"]
    B --> C{"GET _alias 别名位指向几个索引"}
    C -->|"两个以上"| ERR["error：要求唯一指向，不猜首个"]
    C -->|"恰好一个"| D["HEAD 拿线上 settings 与 mappings 做 diff"]
    D -->|"完全一致"| NOOP["noop：零写操作，退出码 0"]
    D -->|"只涉及动态 settings 或新增字段"| INP["inplace：原地 PUT _settings 或 PUT _mapping"]
    D -->|"改了字段类型或静态 settings"| RB["rebuild：委托蓝绿重建，全流程带对账闸门"]
    C -->|"零个，且同名物理索引也不存在"| FC["first_create：先建后灌，对账达标再挂别名"]
    C -->|"零个，但同名物理索引占着位"| CT["cutover_required：拒绝执行，要求先 es:cutover"]
```

### 2.3 你昨天撞到的那段输出，是正确的

昨天实跑 `es es:migrate --alias shop-product` 得到（原样，一字未改）：

```
shop-product   cutover_required ✗ 存量未割接，需先执行 es:cutover
Error: es: migrate 有 1/1 条 Definition 失败：shop-product: 存量未割接，需先执行 es:cutover
exit status 1
```

逐行拆：

| 那一行 | 它是谁打的 | 含义 |
|---|---|---|
| `shop-product   cutover_required ✗ 存量未割接，需先执行 es:cutover` | `migrateCutoverRequired`（`internal/es/migrate.go:430-438`）：`Verdict` 在 `:432`、`Error` 文案 `"存量未割接，需先执行 es:cutover"` 在 `:437`；完整原因（dry-run 形态）在 `:433`：`别名位 shop-product 仍是物理索引（存量未割接），migrate 不做不可逆割接；请先执行 es:cutover --alias shop-product` | 走到了流程图最右那条出口：**你的 `shop-product` 是个物理索引，把别名的名字占住了** |
| `Error: es: migrate 有 1/1 条 Definition 失败：…` | `RunMigrate` 末尾聚合（`migrate.go:125-127`，函数体 `:88`），`1/1` ＝ 失败条数 / 总条数 | 不是崩溃，是「本次范围内有一条没做成」的汇总 |
| `exit status 1` | 上面那个非 nil error 让 Cobra 以 1 退出 | 脚本判错只看退码即可 |

**为什么这是正确行为**：割接是不可逆动作（旧物理索引连同数据会在这次原子操作里消失），migrate 作为「日常结构收敛」入口**绝不自作主张帮你做不可逆的事**，它只指路。

**当前线上状态**（2026-11-17 只读实测）：两个历史索引都已割接完成 —— 别名 `shop-product` → 物理索引 `shop-product_v1`（5 篇文档）、别名 `shop-order` → 物理索引 `shop-order_v1`（9 篇文档），`is_write_index` 均为 true；`es:status --json` 对两者的 `kind` 都是 `alias`、`decide_action` 都是 `noop`。所以现在再跑 `es:migrate` 会走图中左半边（noop / inplace / rebuild），不会再看到 `cutover_required`；`es:cutover` 对这两个别名已成幂等空动作（别名位已是别名 → 打印现状、退码 0、不做变更，判定 `DecideCutover` 在 `internal/es/cutover.go:565-570`，主流程消费点 `:1084-1089`）。shop-order 割接与首建的实况见第 6 章 6.4。

### 2.4 判定结果一共七种

`es:migrate` 的 `verdict` 全集（`internal/es/migrate.go:28-36`，七个值）：

| verdict | 意义 | 动写了没 | 退码 |
|---|---|---|---|
| `noop` | 线上结构与声明一致 | 没动（`migrate.go:232-234`） | 0 |
| `inplace` | 只提交动态 settings / 新增字段（`migrate.go:236` 转入，函数体 `:245`） | `PUT _settings` / `PUT _mapping` | 成功 0 |
| `rebuild` | 判定要重建，已委托蓝绿重建（`delegateRebuild`，`migrate.go:288`；日志 `:238`） | 全流程：建新索引＋灌＋对账＋切别名＋补偿 | 成功 0 |
| `first_create` | 别名与同名索引都不存在，首建（`migrateFirstCreate`，`migrate.go:331`；日志 `:372` / `:412`） | 建索引 → 灌数 → 对账 → **达标后**才单次挂别名 | 成功 0 |
| `cutover_required` | 别名位被同名物理索引占了（`migrate.go:430-438`） | **完全没动** | **1** |
| `skipped_not_active` | 该声明 `status=draft`，不进可执行清单（`migrate.go:161-165`） | 没动 | 0（只是跳过） |
| `error` | 判定或执行失败，例如别名指向多个索引（`migrate.go:178-181`） | 不确定 | 1 |

---

## 第 3 章 六个命令：作用、时机、敲哪条

本章只给**最小形式**。所有可选参数「默认值已调好，不用动」；真要动，第 1、2 章已给了源码位置可查默认值出处。六个 flag 表全部按源码里的注册行照抄（`cmd/cli/es_*.go` 的 `init()`）。

**命令面就只是这六条**：二期接订单**没有新增命令、也没有新增一个参数** —— 订单不是靠一条 `es:sync-orders` 接进来的，而是把 `es:sync-products` 的 `--index` 换成 `shop-order`（机制见第 6 章 6.1，命令名不改是为了让现有的 cron / 脚本一行不动）。

| 命令 | 一句话作用 | 什么时候敲 |
|---|---|---|
| `es:status` | 体检，只读 | 任何时候，出问题第一件事 |
| `es:migrate` | 结构收敛总入口，自动判五种路 | 每次发版前、改完声明文件后 |
| `es:sync-products` | 只灌数据，不碰结构不碰别名；**`--index` 换别名就换业务**（`--index shop-order` 灌订单） | 补数据、指定商品重灌、定时增量 |
| `es:rebuild` | 结构变了就蓝绿重建并切别名 | 改了字段类型 / 分片数 |
| `es:cutover` | 把占着别名位的物理索引换成同名别名 | 历史包袱清一次 |
| `es:cleanup` | 清理没别名挂载的 `*_vN` 残留 | 重建失败留下孤儿索引时 |

### 3.1 `es:status` —— 体检

```bash
go run ./cmd/cli es es:status
go run ./cmd/cli es es:status --alias shop-product --json   # 单索引、机器可读
```

`--alias` 留空＝检查 Registry 里**全部登记项**（含 `draft`，因为它走 `reg.All()`，`cmd/cli/es_status.go:300`）；`--json` 时进度日志改走 stderr（`es_migrate.go:169-171` 同款处理）。会报：集群健康、已装插件、每条声明的存在形态（别名 / 物理索引 / 不存在）、读写目标、计数对账。

**计数对账只对有登记口径的别名做**。二期把这张表从 `es_status.go` 里搬进了灌数层的唯一注册表 `sync.dbCounters`（`internal/es/sync/registry_provider.go:64-67`），现在两条：`shop-product → es.ProductDocCount`、`shop-order → es.OrderDocCount`。`es:status` 只查表（`sync.DBCounterFor(alias)`，`cmd/cli/es_status.go:366`），查不到就如实报「该别名未登记 DB 计数口径，跳过 ES/DB 比对」（`:369`）—— 这是刻意设计（口径错配比没比更有害）。

口径文案也是同源登记的（`dbCounterScopes`，`registry_provider.go:72-75`），`es_status.go:394` 拼进 `count_scope`；2026-11-17 实测两个别名分别是（`es:status --json` 原文）：

| 别名 | 实测 `count_scope` |
|---|---|
| `shop-product` | `MySQL 口径 is_del=0 AND is_show=1 vs shop-product_v1/_count` |
| `shop-order` | `MySQL 口径 is_del=0 vs shop-order_v1/_count` |

**新增业务不再往 `es_status.go` 里加表项**，而是在 `registry_provider.go` 的三张表各补一条（见第 5 章 5.2）。

### 3.2 `es:migrate` —— 结构迁移总入口

```bash
go run ./cmd/cli es es:migrate --alias shop-product --dry-run   # 永远先预演
go run ./cmd/cli es es:migrate --alias shop-product --yes       # 实跑（非 TTY 必须带 --yes）
```

自动判定 noop / 原地 / 需重建 / 需割接（判定表见第 2 章 2.2）。`--alias` 缺省是 `all`（`cmd/cli/es_migrate.go:77`），所以不带 `--alias` 就是把所有 active 声明都收敛一遍。想跳过 diff 强制重建用 `--force-rebuild`（`:85`）。

二期落地的两个细节（都以 `--alias shop-order` 实跑核对过）：

1. **首建路径在任何 ES 写请求之前先做编排预检**：`IngestCheck`（`cmd/cli/es_migrate.go:189-191`）调 `sync.IngestOrchestrationReady(alias)`（`internal/es/sync/registry_provider.go:122`），消费点在 `internal/es/migrate.go:336-343`；该别名没接编排就直接零写拒绝，**不会建出一个没人能灌的空索引**（不留孤儿）。
2. **灌数阶段接了进度条**（`startIngestProgress`，`es_migrate.go:226` → `cmd/cli/es_ingest_progress.go:37`）；内层容差固定用哨兵 `es.IngestToleranceIgnore`（`es_migrate.go:252`），对账只由 migrate 本层闸门判（`migrate.go:402`）。

### 3.3 `es:sync-products` —— 只灌数据

```bash
go run ./cmd/cli es es:sync-products                                   # 全量重灌默认目标
go run ./cmd/cli es es:sync-products --ids 12,15                       # 只灌指定 id
go run ./cmd/cli es es:sync-products --since 2026-11-01                # 只灌这天之后改过的
go run ./cmd/cli es es:sync-products --resume                          # 断点续传（第 4 章）
go run ./cmd/cli es es:sync-products --retry-failed runtime/es_sync/failed_shop-product_xxx.jsonl
go run ./cmd/cli es es:sync-products --dry-run                         # 只读预演，零写
go run ./cmd/cli es es:sync-products --index shop-order --dry-run      # 只换个别名就是灌订单（命令面不变）
```

flag 与默认值（全部照 `cmd/cli/es_sync_products.go:35-55`）：`--index` 默认 `shop-product`（`:35`）、`--chunk-size` 默认 1000（`:36`）、`--concurrency` 默认 4（`:37`）、`--range-multiplier` 默认 4（`:38`）、`--bulk-size` 默认 3000（`:39`）、`--bulk-max-mb` 默认 0＝交给 admin（`:40`）、`--limit`（`:41`）、`--ids`（`:42`）、`--since`（`:43`，接受 RFC3339 或 `2006-01-02`）、`--tolerance` 默认 0（`:44`）、`--max-failures` 默认 100（`:45`）、`--retry-failed`（`:46`）、`--resume`（`:47`）、`--dry-run`（`:48`）、`--tune-ingest` 默认 true（`:49`）、`--yes`（`:53`）。

**不新增命令就能换业务是怎么做的**：命令体只调一个入口 `sync.SyncProducts`（`cmd/cli/es_sync_products.go:108`），它内部按解析出的声明别名去查注册表（注释 `:103-107`）：同一个 `--index` 参数，给 `shop-product` 命中商品 Provider，给 `shop-order` 命中订单 Provider，两条路共用同一套 checkpoint / 对账 / 调优 / 失败落盘编排。实测输出（2026-11-17，`--index shop-order --dry-run`）：

```
[dry-run] 目标索引: shop-order
[dry-run] DB 命中行数: 9 (min=3, max=12)
[tune-ingest] 跳过调优：target=shop-order 在 ES 侧持有别名
```

这张图在讲：`es:sync-products` 内部一条数据是怎么从 MySQL 走到 ES 的，以及反压和游标发生在哪一环。

```mermaid
flowchart LR
    A["探测 id 上下界 Bounds"] --> B["按区间数切成多个 ID 区间"]
    B --> C["scanner 池 4 个协程并行读 MySQL"]
    C --> D["有界队列 docsCh 容量等于并发数乘 2"]
    D --> E["bulk writer 池 装配文档并写 ES"]
    E --> G["checkpoint 按区间记账"]
    G --> H["游标只推进到最小连续覆盖右边界"]
    D -. 队列满即反向压住读取速度 .-> C
    B -. 区间分发 .-> C
```

要点三条，都不需要调：① 队列是**有界**的（`internal/es/sync/runner.go:78`，缓冲 ＝ 并发 × `defaultDocsChBufferMulr`＝2，常量在 `sync/provider.go:248`），ES 变慢时自动阻塞 MySQL 读取，不需要手写限速；② scanner 与 writer 各自成池，用 errgroup 收口（errgroup 在 `runner.go:74`、writer 池 `:89-116`、scanner 池 `:120` 起）；③ writer 侧才做 HTML 清洗与序列化，scanner 只丢原始行（`runner.go:21` 的结构注释）。

**失败重放怎么来的**：item 级失败会落成一份 JSONL，文件名形如 `failed_<target>_<YYYYMMDD_HHMMSS>.jsonl`（`failureFileName`，`internal/es/admin/bulk.go:373-374`），默认落在 `runtime/es_sync`（`defaultFailureDir`，`bulk.go:39`）。攒够 `--max-failures` 就中断整任务。把它喂回 `--retry-failed` 即只重投这些文档（读取函数 `ReadFailures`，`bulk.go:456`）；缺 `_id` 的失败样本会被标记为 `missing_id`（`bulk.go:186`），这类**无法幂等重投**，读取时会告警跳过（`bulk.go:473` 的注释），只能按 id 手工 `--ids` 补。

### 3.4 `es:rebuild` —— 结构变更时的蓝绿重建

```bash
go run ./cmd/cli es es:rebuild --alias shop-product --yes                # --alias 必填
go run ./cmd/cli es es:rebuild --alias shop-product --yes --drop-old     # 成功后顺手删旧索引
```

`--alias` 默认空串且必填（`cmd/cli/es_rebuild.go:86`，帮助文本写明「rebuild 的前置是该别名已存在」）。`--drop-old` 默认 false＝**保留旧索引以便随时回滚**（`:94`）；`--tolerance` 负值表示**永不可达标**、不会被当成 0 用（`:88`）；`--force` 只放行对账这一道（`:96`）；`--keep-new-on-failure` 默认 true，失败时把灌到一半的新索引留着排查（`:95`）。

八步实况（顺序照 `internal/es/rebuild.go` 的日志）：

| 步 | 内容 | 源码 |
|---|---|---|
| 1/8 | 前置校验：别名指向、目标名、插件 | `rebuild.go:372` |
| 2/8 | 建**新索引**（此阶段**不带别名**） | `:453` |
| 3/8 | 灌入期提速：refresh 置 -1、replicas 置 0 | `:497` |
| 4/8 | 从 MySQL 全量灌入，`op_type=index` 幂等 | `:504` |
| 5/8 | `_refresh` + `_count` 对账（闸门在切别名**之前**） | `:516`、真实回读 `:531` |
| 6/8 | **单次** `POST /_aliases` 原子切换 | `:542` |
| 7/8 | 差量补偿，下界＝编排启动时刻 | `:558` |
| 8/8 | 完成，默认保留旧索引并打印回滚请求体 | `:577-580` |

这张图在讲：第 2 到第 8 步的时间先后，以及「哪一步之前旧索引还在服务」。

```mermaid
sequenceDiagram
    participant OP as 运维
    participant CLI as es rebuild
    participant MY as MySQL
    participant ES as Elasticsearch
    OP->>CLI: es:rebuild --alias shop-product --yes
    CLI->>ES: 创建 shop-product_v2 不带任何别名
    CLI->>ES: 临时提速 refresh 关闭 副本清零
    CLI->>MY: 分区间并行扫描全量商品
    MY->>CLI: 文档批次
    CLI->>ES: bulk 灌入 v2
    CLI->>ES: refresh 后 count 与库内期望对账
    Note over CLI,ES: 对账闸门 过不去就停在这一步 v1 仍在服务
    CLI->>ES: 单次 _aliases 原子切换 摘 v1 挂 v2
    CLI->>MY: 差量补偿 从本次启动时刻起
    CLI->>ES: 逐行决定 index 或 delete
    CLI->>OP: 打印回滚请求体 默认保留 v1
    OP->>ES: 可选 drop-old 或 es:cleanup 删除 v1
```

**为什么 `--force` 放行不了补偿**：`opts.Force` 只在第 5 步对账分支被读（`rebuild.go:533-540`）；第 7 步补偿（`:558-575`）没有任何 force 判断，补偿失败或有 item 级失败都会如实上抛 —— 别名已经切了，窗口增量没收敛完不算成功。灌数阶段同样接了进度条（`cmd/cli/es_rebuild.go:240`），影响面摘要打印在 `:289`。

### 3.5 `es:cutover` —— 一次性同名割接

```bash
go run ./cmd/cli es es:cutover --alias shop-product --dry-run
go run ./cmd/cli es es:cutover --alias shop-product --yes
```

`--alias` 默认就是 `shop-product`（`cmd/cli/es_cutover.go:87`），割接订单就显式传 `--alias shop-order`。七个阶段照 `internal/es/cutover.go` 的日志：`[1/7]` 前置校验 `:1041` → `[2/7]` 建目标索引 `:1135` → `[3/7]` 灌数 `:1182` → `[4/7]` 对账 `:1202` → `[5/7]` **原子割接** `:1230` → `[6/7]` 差量补偿 `:1254` → `[7/7]` 校验别名 `:1272`。

割接特有的两道护栏（都是二期加固）：

| 护栏 | 在哪 | 行为 |
|---|---|---|
| 编排前置预检 | `cmd/cli/es_cutover.go:211-213` 注入 `IngestCheck` → `sync.IngestOrchestrationReady`（`internal/es/sync/registry_provider.go:122`） | 目标别名的全量灌数编排不可用，在**发出任何 ES 写请求之前**零写拒绝——因为割接第 5 步会 `remove_index` 掉旧物理索引，先拦下才不会留下一个“旧房子已拆、新房子没人能灌”的空索引 |
| 影响面摘要按别名报口径 | `printCutoverSummary`（`cmd/cli/es_cutover.go:324`）调 `sync.DBCounterScope(alias)` | 按商品别名时打 `is_del=0 AND is_show=1`，按订单别名时打 `is_del=0`，不再对任意别名硬编码商品文案 |

关键三条：

1. **旧物理索引在同一次原子请求里被删掉**（不是只摘别名）。请求体是「先 `remove_index` 后 `add`」两条 action 装进一个 `POST /_aliases`（`CutoverAliasBody`，`cutover.go:643-661`，构造见 `:652-655`，`is_write_index=true`）。
2. **数据靠从 MySQL 重灌**：旧的那份连同索引一起消失，新索引里的内容全是第 3 步灌出来的。
3. **没有回滚，恢复手段只有一个**：重跑本命令（或 `es:sync-products`）再来一遍（`cmd/cli/es_cutover.go:37-40` 把这条写成帮助文本）。本环境按用户口径**不做备份**，MySQL 是唯一事实源。这与 `es:rebuild`「保留旧索引、一条回滚请求体就能切回去」是两套完全不同的语义。
   幂等护栏：别名位已经是别名 → 打印现状、退码 0、不做任何变更（判定 `DecideCutover` 在 `cutover.go:565-570`，主流程消费点 `:1084-1089`）；目标索引已存在 → 拒绝覆盖并中止，绝不 DROP（`:1091-1094`）。

这张图在讲：为什么同名割接只能有一条路，以及拆成两步会在中间掉进写入黑洞。

```mermaid
flowchart TD
    R["ES 规则 同一个名字要么是索引要么是别名 绝对互斥"]
    R --> BAD
    R --> GOOD
    subgraph BAD["错误做法 分两步"]
        X1["第一步 先摘掉旧物理索引的别名位"] --> X2["此刻 shop-product 既不是索引也不是别名"]
        X2 --> X3["读写全部落空 形成写入黑洞"]
        X3 --> X4["第二步再挂别名 黑洞期间数据永久丢失"]
    end
    subgraph GOOD["唯一安全做法 单次原子请求"]
        Y1["一个 _aliases 请求里同时提交 remove_index 与 add"] --> Y2["ES 内部原子生效 名字始终有归宿"]
    end
```

同名的冲突在代码里被硬拦两处：`CutoverAliasBody` 一见 `alias == target` 就报「目标索引名不能与别名同名（ES 中二者绝对互斥）」（`cutover.go:647-650`）；CLI 入口更早拦一次（`cmd/cli/es_cutover.go:163`）。

### 3.6 `es:cleanup` —— 清残留

```bash
go run ./cmd/cli es es:cleanup                                   # 默认 --list-only=true 只列不删
go run ./cmd/cli es es:cleanup --older-than 168h                 # 放宽/收紧年龄线
go run ./cmd/cli es es:cleanup --index shop-product_v2 --list-only=false --yes
```

三条护栏（照 `cmd/cli/es_cleanup.go:49-51` 的帮助文本与实现）：

| 护栏 | 实现 |
|---|---|
| 只碰形如 `*_v<数字>` 的版本化索引；`shop-product` 这种无版本尾缀的一律不碰 | 正则 `cleanupVersionRe`（`es_cleanup.go:41`）、批量模式判定 `:176-178` |
| 仍被**任何**别名指向的索引永不列入候选 | 先读全量别名持有集合（`:142-145`），命中即跳过（`:179-181`） |
| 创建时间不足 `--older-than`（默认 168h＝7 天）的不删；`creation.date` 缺失时 fail-closed 跳过并告警 | `passesAgeGate`（`:259`），调用点 `:185-190` |

`--index` 单索引模式**确实绕过年龄线**（`:156-169` 这条分支不进 `passesAgeGate`），但**前两条红线仍然强制**：`checkSingleIndexTarget`（`:242`）先过版本尾缀、再查别名持有，注释 `:156-157` 明确写「红线优先于『无别名即安全』的直觉判定」。`--yes` 在非 TTY 下是硬要求，而且在**任何 ES 连接之前**就拦（`:119-124`）。

### 3.7 两条必须如实知道的语义坑

**坑①：重灌不会删掉下架 / 已删除的商品（订单同理：不会删掉已软删的订单）。**

全量同步的扫描 SQL 带 `is_show = 1` 硬过滤（`internal/es/sync/product.go:103`，函数体 `filter` 在 `:102`，口径注释 `:100-101`；扫描列清单 `:38-40` 里专门补了 `is_show` 一列）。这意味着**下架商品根本没被扫到**，你重灌一百遍，它在索引里仍然是旧文档 —— 幽灵文档。

订单侧全量只有一条 `is_del = 0`（`internal/es/sync/order.go:106-107`，订单表根本**没有 `is_show` 这个维度**，把商品 WHERE 搬过去照抄会少算、对账永远不达标），所以它只会漏掉软删订单这一类幽灵文档。

要清掉幽灵文档**只有一个办法：`es:rebuild`**（或 `es:cutover` 结尾那一步补偿）。因为它结尾的差量补偿走的是另一条 SQL：`Unscoped()`、**不含 `is_show`、也不含 `is_del` 过滤**（商品：`internal/es/sync/delta.go:11-13` 与 `:61-62`，实现 `defaultCompensateScan` `:79` / `:123`，补偿入口 `Compensate` `:178`；订单：`CompensateOrder`（`sync/order.go:371`）走 `defaultOrderCompensateScan`（`:283`）），逐行按装配结果决定发 `index` 还是 `delete`，所以软删行与下架行都能被看见并删除（`delta.go:39` 的 `Scanned` 注释即「含软删/下架，Unscoped 口径」）。

> 一句话记法：**「重灌」是往索引里加/盖，只有「重建」才会从索引里删。**

**坑②：`es:sync-products --index` 不要乱传。**

```
高危：--index 传一个未注册或 draft 的索引名
```

这条曾经会**静默兜底**到商品定义，再经写死的 productProvider 把【商品】数据灌进你以为是订单的目标里，命令还报成功。该缺陷现在已被正面修掉：CLI 只要**显式传了** `--index` 就开启严格校验（`cmd/cli/es_sync_products.go:190` 用 `f.Changed("index")` 决定 `StrictIndex`，透传给 `opts.StrictIndexLookup`，消费点 `sync/product.go:296`），命中未登记 / `draft` 时在任何 MySQL 触达与 bulk 之前就报错并列出当前可用（active）名称（防线在 `sync/product.go:933-949`，回归测试 `internal/es/sync/index_lookup_test.go`）。二期 `shop-order` 已转 active，所以它现在是**合法目标**（而不是被拒绝）；仍未登记的别名与 draft 声明照样拦。

仍然要提醒的两点残余风险：① **不传** `--index` 时走的是友好兜底分支（`sync/product.go:950-963`：target 匹配不上会回落 `ProductAlias`），编程式调用（rebuild / cutover / migrate 的适配器）同样不严格；② 目标写成物理索引名（如 `shop-order_v1`）时灌的是别名当前指向的那个索引，容易和蓝绿过程中的新索引搞混。**结论：需要指定就老老实实写 active 别名，别靠猜。**

### 3.8 脚本里怎么判对错

| 现象 | 退码 | 该怎么读 |
|---|---|---|
| 正常完成（含 noop、含 `skipped_not_active`、含「已割接所以 cutover 什么都不做」） | 0 | 成功 |
| `cutover_required`、对账不达标、补偿有失败、目标索引已存在 | 1 | 真失败，看最后一条 `Error:` 行 |
| `enabled=false` 被跳过 | 0 | **注意**：这是「没做」而不是「做好了」；CI 里请先断言开关 |
| 命令名写错（漏 `es:` 前缀） | 1 | 参数错误，会附建议名 |

`--json` 输出时进度日志改走 stderr（`es_migrate.go:169-171` 等），但 `[dry-run]` 之类的计划打印仍可能混在同一通道里；脚本取 JSON 建议用 `sed -n '/^{/,$p'` 从第一个左花括号起截取。进度条则相反：非终端环境下它**根本不构造渲染器**（一个 `\r` 与 ESC 字节都不写），见第 6 章 6.2。

---

## 第 4 章 断点续传到底靠什么

今天被问的原题：「跑到一半断了，`--resume` 凭什么知道从哪儿接着灌？」答案就一个文件。

### 4.1 文件在哪、长什么样

| 项 | 值 | 源码 |
|---|---|---|
| 路径 | `runtime/es_sync/<target>.checkpoint.json` | 基目录变量 `checkpointBaseDir = "runtime/es_sync"`（`internal/es/sync/product.go:44`，可被单测注入）；拼名函数 `checkpointPath`（`product.go:1137-1139`，target 里非法字符会被净化） |
| 字段 | `target` / `started_at` / `total` / `last_id` | 结构体 `checkpoint`（`product.go:977-983`） |
| 是否被 git 忽略 | 是，`runtime/es_sync/` 在 `.gitignore` 里 | — |

内容样例（字段名照结构体的 tag）：

```json
{ "target": "shop-product", "started_at": "2026-11-11T09:12:33+08:00", "total": 5, "last_id": 128 }
```

### 4.2 游标只推进到「最小连续覆盖右边界」

这是整章唯一需要动脑的地方，也是「宁可重灌、绝不留漏」的落点。

全量灌是多个区间并行跑的。如果 `last_id` 就记「目前见过的最大 id」，那么 id 小的区间落后时，它的整段都会被游标越过 —— 断点续传就永久漏掉那一段。所以记账是按**区间**记的（`recordRange`，`product.go:1066`），游标推进由 `frontierLocked` 计算（`product.go:1117`，规则写在 `:1111-1116`）：

- 已完成的区间：把边界推到它的右边界，继续看下一个；
- 遇到的第一个「进行中」区间：只推到它区间内已连续读出的 `last_id`，然后**停**；
- 尚未开始的区间：立刻**停**，其后任何进度都不计入；
- 区间计划为空（没登记过 plan）：返回 0，保守不推进（`:1116`）；
- 算出的新边界不大于现有游标时不落盘、不回退（`:1083-1086`）。

### 4.3 续传时读回什么

这张图在讲：一次正常跑留下的文件，中断之后被 `--resume` 读回，两个字段各自派什么用场。

```mermaid
flowchart TD
    A["全量同步开跑 不带 --resume"] --> B["照常落盘 checkpoint 文件"]
    B --> C["每完成一个区间落一次盘"]
    C --> D{"进程是否被打断"}
    D -->|"没有打断"| E["跑完 文件留着 last_id 等于末段右边界"]
    D -->|"Ctrl-C 或故障"| F["游标停在最后一个连续覆盖边界 可能偏小"]
    F --> G["重新执行并加上 --resume"]
    G --> H["读回 last_id 作为扫描下界"]
    G --> I["读回首次 started_at 作为补偿下界"]
    H --> J["只补未覆盖的区间 重复灌是幂等的"]
    I --> J
```

四个细节都有源码兜底：

1. **正常跑也留文件**，不是只有 `--resume` 才落盘：checkpoint sink 始终创建（`product.go:336` 的注释「使首次全量也落盘，中断后 `--resume` 可达」，代码 `:337` / `:353`）。
2. `last_id` 变成扫描下界，并且**强制退化成单区间单游标**：`applyResumeState`（`product.go:603-606`）把逻辑下沉给 `applyResumeCommon`（`:608-616`）：`minExclusiveID = cp.LastID`，同时把 `Concurrency` 与 `RangeMultiplier` 都置 1（`:611-612`），打印 `[resume] 从 id=%d 续传（单区间模式）`（`:614`）。这样续传阶段 id 单调、不跳段，代价是并行度归零 —— 这是「不漏」换来的必然选择。
3. **`started_at` 用的是首次值**：加载到已有 checkpoint 时走 `restoreCheckpointSink`，原样保留 `StartedAt` 与 `Total`（调用点 `product.go:347-349`，函数与口径注释在 `:1018-1023`，`:1020-1021` 明说「差量补偿应以 `StartedAt`（任务首次启动时刻）为下界，而不是恢复时刻，写错会让补偿漏掉中断窗口内的变更」）；`Total` 也刻意**不**在 resume 路径覆盖（`:354-359`）。
4. 找不到文件不报错，如实从头来：打 `[resume] 未找到 checkpoint，将从头全量执行`（`product.go:344`）；文件内容损坏才失败（`loadCheckpoint`，`:1141`）。

**重灌为什么安全**：bulk 写入用 `op_type=index`（覆盖语义），同一 `_id` 重复灌只会盖掉旧值，不产生重复文档 —— 所以「宁可多灌一段」没有副作用，「少灌一段」才有。

---

## 第 5 章 后半本：新增一个索引 / 新增一种导入

这一章写给「下次要再加一个业务索引」的人，全程拿已经接完的 `shop-order` 当活例子。**本章是说明书：照它做不需要改本仓库任何现有代码，也不需要新增命令。**

先说一句最关键的（也是本章与一期文档最大的差别）：**光新增一份 `indices/x.jsonc` 是接不通的**。声明文件只让程序「看得见」这个索引；要让它跑得起来，必须去 `internal/es/sync/registry_provider.go` 的**三张表各补一条**。没补表就敲 `es:migrate` / `es:sync-products`，会得到一句**显式拒绝**（「尚未接线数据源 Provider」/「尚未接入全量灌数编排」），而**不会静默按商品逻辑跑** —— 这条护栏的实现在 `registry_provider.go:107`（`SyncByAlias`）与 `:122`（`IngestOrchestrationReady`，两条拒绝文案分别在 `:124` 和 `:130`）。

### 5.1 全景：要动哪些地方

这张图在讲：一个新索引从零到能跑，要经过五层，每层落在哪个目录。

```mermaid
flowchart TD
    A["第一层 声明文件 internal/es/indices 下新增 x.jsonc"] --> B["第二层 文档结构 internal/es 下新增 doc_x.go 与装配函数"]
    B --> C["第三层 数据源 internal/es/sync 下新增 x.go 实现扫描装配与 ingestTarget"]
    C --> D["第四层 注册表 registry_provider.go 三张表各补一条 键等于字面量别名"]
    D --> E["第五层 命令入口 零改动 直接复用 es:sync-products --index 新别名"]
    E --> F["验证顺序 dry-run 首建 status 与 count 对账 再跑一次应判 noop"]
    A -. 必填 _meta.alias 与 _meta.schema_version .-> G["元数据两行"]
    D -. 只补声明不补表 .-> H["migrate 与 sync 显式拒绝 不会静默按商品跑"]
```

五层里**只有前四层要写代码，第五层是零改动**。

### 5.2 最小闭环清单（七步，每步给到文件与函数）

| 步 | 动哪里 | 具体要干什么 | 照商品抄哪一段 |
|---|---|---|---|
| 1 | `internal/es/indices/<x>.jsonc`（新建） | 顶层 `_meta` 至少两行：`alias` + `schema_version`；`settings` + `mappings`（`dynamic` 必须是 `strict`） | `product.jsonc:21-29` 的 `_meta` 块，以及它的 settings 层级写法 |
| 2 | `internal/es/doc_<x>.go`（新建） | 文档 struct（json tag 就是 ES 字段名；`dynamic: strict` 下它必须与 mappings 逐字互等）＋ 从 DB 行到文档的装配函数，以及 `_id` / `routing` 口径 | 商品是 `doc.go`＋`doc_assemble.go` 两个文件（`ProductIndex` `doc.go:29`、`BuildIndexDoc` `doc_assemble.go:47`，后者同时负责「这行该不该入索引」的资格判定）；订单把结构与装配合在 `doc_order.go` 一个文件（`OrderIndex` `:43`、`BuildOrderDoc` `:93`、`OrderDocID` `:74`、`OrderRouting` `:79`）—— 两种布局都行，照哪个抄都行 |
| 3 | `internal/es/sync/<x>.go`（新建） | 实现 Provider：`Name` / `Definition` / `Bounds` 探上下界 / `ScanRange` 按区间扫描 / `Assemble` 装配文档；列裁剪必须包含装配判定要用的列。**并且必须实现 `ingestTarget` 形态**（一条空声明 `_ ingestTarget = (*xProvider)(nil)` 就够），否则三条编排都会在灌数步拒掉 | 商品的 `filter` / `Bounds` / `ScanRange` / `Assemble`（`sync/product.go:102` / `:119` / `:125` / `:193`）；订单的同一套加形态声明（`sync/order.go:106` / `:473`）；扫描列清单 `product.go:38-40`（`is_show` 是装配判定要用它而特意补的）、`order.go:47` |
| 4 | Registry 与状态 | **不需要注册代码**：`LoadDefinitions` 直接扫目录（`registry.go:39`）。落盘后 `status: "draft"` 表示「文件先存在、不进可执行清单」（缺省即 `active`，`definition.go:50`）；可执行判定 `Executable()`（`definition.go:164`）、清单 `ExecutableDefinitions()`（`registry.go:312`） | 商品是 `active`（`product.jsonc:24`）；**订单二期也已转 `active`**（`orders.jsonc:37`） |
| 5 | `cmd/cli` —— **零改动**（本章重点） | 不要新建 `es:sync-<x>`。注册表里有这个别名，`es:sync-products --index <新别名>` 就会走到你的 Provider；migrate / rebuild / cutover / status 本来就按别名逐条循环，也零改动 | 现状即答案：**命令面仍是六条，没有 `es:sync-orders`**（`cmd/cli/es_*.go` 的 `Use` 列表见 2.1） |
| 6 | **`internal/es/sync/registry_provider.go` 三张表**（真正的新增点） | 三张表的**键必须是字面量别名**（与 `_meta.alias` 逐字一致，查表不做任何归一化）：① `providers`（`:36-47`）填 Provider 工厂，商品条目 `:37`、订单条目 `:43`；② `compensators`（`:52-55`）填差量补偿实现（`:53` / `:54`）；③ `dbCounters`（`:64-67`）填 DB 期望数口径（`:65` / `:66`），并同步在 `dbCounterScopes`（`:72-75`）补一条人话描述（`:73` / `:74`），`es:status` / `es:cutover` 的口径文案就靠它 | 订单四条全部在位，直接抄形状就行 |
| 7 | 验证顺序 | `es:sync-products --index <新别名> --dry-run`（零写，看它是不是真读到你的表）→ `es:migrate --alias <新别名> --yes`（首建：先建后灌、对账达标才挂别名）→ `es:status --alias <新别名>`（看 `count_scope` 与差值）→ **再敲一次 migrate，应判 `noop`、零写** | 第 2、3 章的走查即是标准动作；订单实跑全貌见 5.4 |

#### 5.2.1 复制即用的最小步骤（把 `shop-xxx` / `store_xxx` / `Xxx` 换成你的名字）

```bash
# 0) 先确认集群里名字没被占（只读，安全）：别名位上是物理索引还是别名
curl -s 'http://127.0.0.1:9200/_cat/aliases/shop-xxx?v'
curl -s -o /dev/null -w '%{http_code}\n' 'http://127.0.0.1:9200/shop-xxx'   # 200 且上一行没结果 => 同名物理索引占位，要先割接

# 1) 声明文件（照抄一份改）
#    internal/es/indices/xxx.jsonc  ->  _meta.alias=shop-xxx, schema_version=1, status 先留 draft

# 2) 代码三处（只新增，不改现有文件）
#    internal/es/doc_xxx.go          ->  XxxIndex + BuildXxxDoc + DocID/Routing
#    internal/es/sync/xxx.go         ->  xxxProvider：filter/Bounds/ScanRange/Assemble + _ ingestTarget
#    internal/es/sync/registry_provider.go -> providers / compensators / dbCounters / dbCounterScopes 各补一条（键：shop-xxx）

# 3) 把 status 改成 active，然后跑验证（命令面零新增，全部是现成四条）
go run ./cmd/cli es es:status --alias shop-xxx                      # 应能看到声明
go run ./cmd/cli es es:sync-products --index shop-xxx --dry-run     # 目标行应写 shop-xxx，DB 命中数是你表的数
go run ./cmd/cli es es:migrate --alias shop-xxx --dry-run           # 应判 first_create（或 noop/inplace）
go run ./cmd/cli es es:migrate --alias shop-xxx --yes              # 首建 + 全量 + 对账达标后挂别名
go run ./cmd/cli es es:migrate --alias shop-xxx --dry-run           # 再跑一遍：必须 noop、零写
```

**四个名词的大白话**（本仓库里它们各自只对应一个东西，不是抽象概念）：

| 名词 | 大白话 |
|---|---|
| **字面量别名** | 就是 `indices/*.jsonc` 里 `_meta.alias` 那串字符（如 `shop-order`）。注册表拿它当键，所以**拼错一个字母就是另一条目**，程序不会帮你模糊匹配 |
| **Provider（数据源）** | 「这个索引的数据从哪张表怎么读」的那段代码。没登记它，灌数就不知道去哪拿数据，所以直接拒 |
| **`ingestTarget` 形态** | 一句 Go 空声明，意思是「这个 Provider 能接全量灌数编排（断点续传 / 对账 / 灌入调优）」。只实现了读数、没声明这个形态，割接/迁移会在灌数步半路失败，所以上面就提前拒（`_ ingestTarget = (*orderProvider)(nil)`，`sync/order.go:473`） |
| **DB 计数口径（dbCounters）** | 「库里到底该有多少篇文档」的数法。订单是 `is_del=0`（`es.OrderDocCount`），商品是 `is_del=0 AND is_show=1`（`es.ProductDocCount`）。**没登记就只能报「不可比」**，绝不拿商品口径去套别的索引 |

### 5.3 「商品」被写死的四处 —— 二期之后只剩两处

一期列的四处写死点，二期已经拆掉两处：

| 写死点 | 位置 | 现状 |
|---|---|---|
| 数据源 Provider（**已拆**） | 曾经恒调 `newProductProvider`（`sync/product.go:83`） | 数据源一律经注册表按 `def.Alias()` 构造（`NewProvider`，`registry_provider.go:93`）；「恒跑商品逻辑」正是当初的根因 |
| DB 期望数口径（**已拆**） | 曾经恒调 `es.ProductDocCount` | 按别名查 `dbCounters`（`:64-67`），未登记直接报错，绝不套商品口径 |
| 别名常量 | `sync/product.go:30` `const ProductAlias = "shop-product"`（订单同位：`sync/order.go:41` `OrderAlias`） | 仍在，但只干两件事：当注册表的键，以及**不传 `--index` 时的兜底目标**（`product.go:950-963`） |
| 命令入口 | `cmd/cli/es_sync_products.go:23` | 命令名带 products 是历史包袱。二期明确**不改命令面**：换业务换 `--index`（见 6.1） |
| 装配函数与文档结构 | 商品 `doc_assemble.go:47` `BuildIndexDoc` / `doc.go:29` `ProductIndex`；订单 `doc_order.go:93` `BuildOrderDoc` / `:43` `OrderIndex` | 本来就该各业务一份（字段由跨仓库协议决定），不是「待泛化」项 |

对账期望数现在两边各一条：商品 `ProductDocCount`（`internal/es/cutover.go:93-104`，`is_del=0 AND is_show=1`）、订单 `OrderDocCount`（`:112-122`，仅 `is_del=0`）。期望值不再由灌数层自己拼 SQL，而是统一走 `runReconcile`（`sync/product.go:542`），口径与扫描同源（都出自 provider 的 `filter` / `Bounds`），编排层再用 `ReconcileCounts` 判闸门。

### 5.4 活例子：`shop-order` 已经接完的复盘（含真机链路）

一期这节写的是「还没接、待办八步」；二期已经全部做完，下面照实复盘。接线机制、进度条、事件链路的展开在 **第 6 章**，本节只给索引本身的接线表与上线顺序。

**接线现状**（逐条实读代码，2026-11-17）：

| 事实 | 依据 |
|---|---|
| `indices/orders.jsonc` 已 `status: "active"`，13 个字段且 `dynamic: strict` | `orders.jsonc:37`（status）、`:135`（strict）、`properties` `:140` |
| `OrderIndex` 13 个 json tag 与 mappings 逐字互等；`_id = order_id`（字符串订单号，**不是**数字主键 `id`）、`routing = strconv.FormatInt(uid, 10)` | `doc_order.go:43` / `:74` / `:79` |
| 订单 Provider：全量扫描（仅 `is_del=0`）/ 区间扫描 / 装配 / 差量补偿 / 全量灌数形态声明 | `sync/order.go:106-107` / `:151` / `:211` / `:371` / `:473` |
| 注册表四条表项都已登记 `shop-order`（数据源 / 补偿 / DB 计数 / 口径文案） | `registry_provider.go:43` / `:54` / `:66` / `:74` |
| DB 期望数口径 `is_del=0`（订单没有 `is_show` 维度） | `cutover.go:112-122` |
| **没有** `es:sync-orders` 命令；灌订单就是 `es:sync-products --index shop-order` | `cmd/cli` 里只有六个 `Use: "es:…"`（见 2.1） |

**真机链路（当时上线的顺序，本期收尾复核现状）**：

| 步 | 敲什么 | 发生什么 |
|---|---|---|
| 1 同名割接 | `go run ./cmd/cli es es:cutover --alias shop-order --yes` | 单次 `POST /_aliases` 里先 `remove_index` 掉旧物理索引 `shop-order`、再 `add` 同名别名（`cutover.go:652-655`）。**不可逆、本环境按用户口径不做备份**（MySQL 为唯一事实源，新索引内容全部从库里重灌） |
| 2 首建 + 全量 + 对账挂别名 | `go run ./cmd/cli es es:migrate --alias shop-order --yes` | `first_create`：先建 `shop-order_v1`（不挂别名，`migrate.go:350-351`）→ 全量灌 → `_refresh`+`_count` → `is_del=0` 口径达标 → **单次** `POST /_aliases` 挂别名（`:412-414`）；不达标则保留已建索引但不挂别名并报错点名残留（`:404-409`） |
| 3 二次判定 | 同一条命令再敲一次 | 判 `noop`、**零写**（实测结论行：`结构指纹不等但归一化 diff 为空：仅版本号/元数据更新，结构无需变更`） |
| 4 幂等割接 | `go run ./cmd/cli es es:cutover --alias shop-order` | 别名位已是别名 → 走 `DecideCutover` 的 `already_cutover` 分支：打印现状、退码 0、不做变更（代码依据 `cutover.go:565-570`，消费点 `:1084-1089`） |

两个已收敛的历史坑（只留一句，不再展开）：

- **日期格式**：`orders.jsonc` 三个 date 字段**不写显式 format**，两侧统一用 `time.Time` 默认序列化（RFC3339）；order-consumer 已按此对齐，不再是「mapping 要 `yyyy-MM-dd HH:mm:ss` 而 consumer 写 RFC3339」的互不兼容状态。
- **事件 topic**：订单对外转发 topic 已从历史误值 `shop-product` 纠正为 `shop-order`（`pkg/enums/order`）；机制见 6.7。

### 5.5 追加导入时的五个常见自坑

| 坑 | 你会看到什么 | 正确做法 |
|---|---|---|
| ① 改了 mapping 忘了改 `schema_version` | `es:migrate` 报结构漂移或干脆 noop，新索引永远出不来 | 改结构必改版本号（版本号来自文件，程序不会帮你 +1，见 1.7） |
| ② 直接 `DELETE` 在线索引 | 搜索线上瞬间 404，且本集群**没有 snapshot 可回滚** | 走 `es:rebuild`，它默认保留旧索引并打印回滚请求体；清残留用 `es:cleanup` |
| ③ 把切别名拆成两步（先摘再挂） | 中间那段「名字既不是索引也不是别名」＝写入黑洞，期间的数据永久丢 | 永远只用一个 `POST /_aliases`；命令已经保证这点（`cutover.go:638-642` 注释：「本层不提供拆分能力」） |
| ④ 以为 `--index` 随便填 | 目标写错会灌到别处；不传时还会走友好兜底（见 3.7 坑②） | 只填 active 别名；显式传值时严格校验会帮你拦住错误名 |
| ⑤ 以为重灌会清掉下架商品 | 索引里留着已经不该搜得到的文档 | 只有 `es:rebuild` 的差量补偿会发 `delete`（见 3.7 坑①） |

---

## 第 6 章 二期：多业务按别名分发、订单上线全貌、跨仓库字段协议

一期这本手册只讲商品。二期把订单接了进来，但**命令面、配置文件形状、迁移判定算法一个字节都没改** —— 本章解释「不改这些，订单是怎么跑起来的」，以及只有二期才出现的三样东西：灌数进度条、割接前置预检、13 字段跨仓库协议。

### 6.1 别名分发注册表：换业务只换 `--index`

**一句话**：命令名里带不带 products 不决定跑谁的代码；决定的是 `--index`（或 `--alias`）传进去的那个**别名**，程序拿它去四张表里查。

四张表都在 `internal/es/sync/registry_provider.go`，键是 `indices/*.jsonc` 里 `_meta.alias` 的字面量：

| 表 | 位置 | 装的是什么 | shop-product | shop-order |
|---|---|---|---|---|
| `providers` | `:36-47` | 数据源工厂（扫哪张表、怎么装配成文档） | `:37` → `newProductProvider` | `:43` → `newOrderProvider` |
| `compensators` | `:52-55` | 差量补偿实现（窗口增量与软删收敛） | `:53` → `Compensate` | `:54` → `CompensateOrder` |
| `dbCounters` | `:64-67` | DB 期望数口径（对账的分母） | `:65` → `es.ProductDocCount` | `:66` → `es.OrderDocCount` |
| `dbCounterScopes` | `:72-75` | 上面那个口径的人话描述，只给摘要打印用 | `:73` `is_del=0 AND is_show=1` | `:74` `is_del=0` |

查表入口与「查不到时怎么办」：

| 入口 | 位置 | 别名未登记 / 未接编排时的行为 |
|---|---|---|
| `NewProvider` | `:93` | 返回点名的错误，绝不回落商品实现 |
| `SyncByAlias` | `:107` | `sync: 别名 xxx 尚未接线数据源 Provider（已接线：[...]），拒绝灌数`（文案 `:109`） |
| `IngestOrchestrationReady` | `:122` | 两条文案：未登记 `:124`；登记了但 provider 没实现 `ingestTarget` 编排形态 `:130` |
| `CompensateByAlias` / `DBCounterFor` / `DBCountByAlias` | `:136` / `:148` / `:154` | 同上，未登记即报错 |

**为什么不新开一条 `es:sync-orders`**：命令名不变，现有的 cron、脚本、运维习惯一行都不用动；换业务只是把 `--index` 从 `shop-product` 换成 `shop-order`（该 flag 默认值仍是 `shop-product`，`cmd/cli/es_sync_products.go:35`）。而「不改命令面」不会退化成「悄悄按商品跑」—— 上面那两条显式拒绝就是护栏，这也是第 5 章教程反复强调「三张表必须登记」的原因。

### 6.2 灌数进度条：接在哪、非终端为什么要「根本不构造」

三条会灌数的命令都接同一个入口 `startIngestProgress(stats, desc, quiet)`（`cmd/cli/es_ingest_progress.go:37`）：

| 命令 | 调用点 | 进度条文案 | `quiet` 传什么 |
|---|---|---|---|
| `es:migrate` | `cmd/cli/es_migrate.go:226` | `ES 灌数(<alias>)` | `spec.AsJSON`（带 `--json` 即静默） |
| `es:rebuild` | `cmd/cli/es_rebuild.go:240` | `ES 灌数(<alias>)` | `spec.AsJSON` |
| `es:sync-products` | `cmd/cli/es_sync_products.go:99` | `ES 全量同步` | 恒 `false` —— 这条命令没有 `--json`，只能靠 TTY 判定降级 |

要不要渲染只有一个决策点：`ingestProgressEnabled(quiet, tty) = tty && !quiet`（`:48`）。TTY 判定用标准库 `os.Stdout.Stat()` 看 `os.ModeCharDevice`，并额外把 `/dev/null` 判成非终端（`stdoutIsTerminal`，`:21-33`）——「是字符设备但没人看」不算有人在等进度条。

**必须记住的一条事实**：进度条库 `github.com/schollz/progressbar/v3 v3.19.1` **自己不做 TTY 检测、不会自动降级**；一旦构造并 `Start()`，它就周期性往 writer 里刷 `\r` 与控制字符，不管对面是终端、管道还是 CI 日志（源码注释原话，`es_ingest_progress.go:5-9`）。所以这里的做法是**非终端环境根本不构造渲染器**（直接返回一个空的关闭函数），而不是「构造出来再想办法静音」—— 后者只要漏一处就会把 `es:migrate --json` 的 stdout 污染成不可解析的 JSON。

配套两点：统计容器 `*sync.SyncStats` 照常创建并交给灌数入口，渲染器只是它的读侧（`sync.NewProgress`，`internal/es/sync/progress.go:50`），所以「没有进度条」不等于「没有计数」，结论行、对账数字、退出码全都不受影响；渲染失败也绝不中止灌数任务。

实测（2026-11-17，`go run ./cmd/cli es es:sync-products --index shop-order --dry-run > tmp/bar_nontty.out 2>/dev/null`，标准输出重定向到文件＝非终端）：进度条构造点（`es_sync_products.go:98-99`）在 dry-run 分支之前就已按同一判定执行，而输出文件 1428 字节里 CR(`0x0d`) 计数 **0**、ESC(`0x1b`) 计数 **0**，同时结论行「计划文档数: 9」照常打印——这就是「不构造而不是静音」的直接体现。

### 6.3 订单索引接线全貌

| 层 | 文件 | 关键点 |
|---|---|---|
| 声明 | `internal/es/indices/orders.jsonc` | `_meta` `:34`（`alias` `:35`、`status: "active"` `:37`）；`dynamic: strict` `:135`；`properties` `:140` 起 13 个字段 |
| 文档结构 | `internal/es/doc_order.go` | `OrderIndex` `:43`（13 个 json tag，`:44-56`）；`OrderDocID()` `:74`；`OrderRouting()` `:79`；装配 `BuildOrderDoc` `:93` |
| 数据源 | `internal/es/sync/order.go` | 口径 `filter` `:106-107`（仅 `is_del=0`）；边界 `Bounds` `:122`；区间扫描 `ScanRange` `:151`；装配 `Assemble` `:211`；差量补偿 `CompensateOrder` `:371`；全量灌数形态声明 `var _ ingestTarget = (*orderProvider)(nil)` `:473` |
| 注册表 | `internal/es/sync/registry_provider.go` | 四条表项 `:43` / `:54` / `:66` / `:74`（见 6.1） |
| 期望数 | `internal/es/cutover.go` | `OrderDocCount` `:112-122`，`Where("is_del = 0")` 在 `:117` |

`_id` 与 routing 的落片口径（跨仓库协议，改一处就得同步三处）：

- `_id = order_id`，**字符串订单号本身**，不是 `store_order.id` 那个数字主键（`OrderDocID`，`doc_order.go:71-74`）；
- `routing = strconv.FormatInt(uid, 10)`（`OrderRouting`，`:76-79`）；
- 灌数侧写文档统一走这两个方法、不手拼字符串（`sync/order.go:225`：`admin.Doc{ID: di.OrderDocID(), Routing: di.OrderRouting(), Src: di}`）。

这三条只要与 order-consumer 或 shop-search-api 任一处不一致，同一条订单就会在全量灌入与实时增量两条链路上落到不同文档 / 不同分片：轻则按 `uid` 查不到，重则同一订单出现两份、幂等彻底失效。

### 6.4 `shop-order` 现在到底是什么状态（随时可复核）

上线时的执行顺序见 5.4 那张表；本节只写**现在就能复核**的现状与证据。

| 事实 | 怎么复核 | 2026-11-17 实测 |
|---|---|---|
| 别名已指向首建出来的物理索引，且是显式写索引 | `go run ./cmd/cli es es:status --alias shop-order --json` | `kind=alias`、`targets=[shop-order_v1]`、`write_index=shop-order_v1`、`is_write_index=true` |
| 历史同名物理索引已消失（割接走的是 `remove_index`，不可逆） | `curl -s 127.0.0.1:9200/_cat/aliases/shop-order?v` 再看 `_cat/indices` | 别名位是别名；`shop-order` 不再作为索引存在 |
| 数据已灌完并对账达标 | 同上 `count_*` 字段 | `count_scope="MySQL 口径 is_del=0 vs shop-order_v1/_count"`、`db_expected=9`、`es_count=9`、`count_diff=0`、`count_ok=true`、`docs_count=9` |
| 结构与声明一致，重复执行不写 | 同上 `decide_action`，再把 `es:migrate --alias shop-order --dry-run` 敲一遍 | `decide_action=noop`、`reason=结构指纹不等但归一化 diff 为空：仅版本号/元数据更新，结构无需变更`、`declared_fingerprint=7aa10138...` |

**本环境的备份口径**（写死在手册里，免得下一个人再问一遍）：同名割接的 `remove_index` 会把旧物理索引连数据一起摘掉，本环境按用户要求**不做备份**，因为 MySQL 是唯一事实源 —— 恢复手段只有「重跑一次灌数」。这与 `es:rebuild` 保留旧索引、打印一条回滚请求体就能切回去，是两套完全不同的语义。

### 6.5 割接 / 迁移的前置预检：编排不可用就零写拒绝

一期有个真实缺陷形态：`es:cutover` 先建好空的 `shop-order_v1`，跑到灌数那一步才发现这个别名没有数据源，于是中止 —— 旧物理索引还没拆（算好事），但 ES 里留下一个没人认领的空索引（坏事）。二期用一次前置预检把它堵在写请求之前：`sync.IngestOrchestrationReady(alias, opts)`（`registry_provider.go:122`）。

| 位置 | 作用 |
|---|---|
| `cmd/cli/es_cutover.go:211-213` | 割接入口注入 `IngestCheck` |
| `cmd/cli/es_migrate.go:189-191` | 迁移入口注入同一个 `IngestCheck` |
| `internal/es/migrate.go:336-343` | 消费点：预检不过直接判失败返回，文案点名「已在发出任何 ES 写请求之前拒绝首建（不建索引、不留孤儿）」；dry-run 也先过这道闸 |
| `registry_provider.go:124` / `:130` | 两条拒绝文案：未登记 Provider；登记了但没实现 `ingestTarget` 编排形态 |

预检的判据与真正灌数的判据**同源**（同一张 `providers` 表 + 同一个 `ingestTarget` 类型断言），所以「预检通过」等价于「灌数不会在半路失败」。它还特意只做形态探测：构造 provider 时 `def`/`db` 传 `nil`（`:127-128` 注释写明不触达任何外部资源），既不查 MySQL 也不碰 ES，因此放在任何写请求之前是安全的。

### 6.6 对账口径：按别名分开，商品和订单不是一条 WHERE

| 别名 | 期望数口径 | 代码 | 打印出来的原话（实测） |
|---|---|---|---|
| `shop-product` | `is_del = 0 AND is_show = 1` | `ProductDocCount`，`cutover.go:93-104`（两个 Where 在 `:99`） | `MySQL 口径 is_del=0 AND is_show=1 vs shop-product_v1/_count` |
| `shop-order` | 仅 `is_del = 0`（订单没有 `is_show` 维度） | `OrderDocCount`，`:112-122`（`:117`） | `MySQL 口径 is_del=0 vs shop-order_v1/_count` |

扫描侧与统计侧必须是同一个口径，否则「对账达标」这个结论就没有意义：灌数的 `filter`（商品在 `sync/product.go`，订单在 `sync/order.go:106-107`）与期望数的 `dbCounters` 成对登记，人话描述由 `dbCounterScopes`（`:72-75`）统一供给 `es:status`（查表 `cmd/cli/es_status.go:366`、打印 `:394`）与 `es:cutover` 摘要（`cmd/cli/es_cutover.go:324`）。

二期修掉的一个具体错误：以前这些摘要对**任意**别名都硬编码打印商品口径，于是订单的影响面摘要会写着 `is_del=0 AND is_show=1`，数字对不上你还以为是数据问题。现在未登记的别名打印的是「该别名未登记 DB 计数口径，跳过 ES/DB 比对」（`es_status.go:369`），口径函数本身也绝不回落（`DBCounterScope`，`registry_provider.go:78`）—— **口径错配比没比更有害**。

### 6.7 订单事件链路：topic 纠正与「失败不再静默吞」

一条订单从业务库走到 ES 增量，中间经过两跳 topic：

```
MySQL store_order 变更 → outbox 表 → Relay 投递 → 内部 topic shop-order-events
    → shop-main 的 OrderHandler 消费 → 转发 → 外部 topic shop-order
    → order-consumer 消费 → 写 ES（见 6.8）
```

| 事实 | 依据 |
|---|---|
| 内部 topic 常量 `TopicOrderEvents = "shop-order-events"` | `internal/worker/outbox.go:17`；注册处 `internal/workers/register.go:25`（ProcessManager 模式同位 `:39`） |
| 对外转发 topic `Topic = "shop-order"` | `pkg/enums/order/event.go:6`（商品同位 `pkg/enums/product/event.go:4` 仍是 `shop-product`）。**历史误值**：订单转发曾打到 `shop-product`，二期纠正 |
| 转发失败不再静默吞 | `internal/workers/order_handler.go`：create / update / delete 三类事件的 `publishToKafka` 返回值一律 `return err`（`:106-107` / `:112-113` / `:118-119`），语义写在 `:101-102` |
| 幂等标记只在转发成功后才写 | `order_handler.go:126-141`，所以失败的消息能被重新投递，不会被「已处理」吃掉 |

**失败之后到底去哪（照代码实况，不照口号）**：`OrderHandler` 上抛 error 后，worker 层**不标记 offset**，由 Kafka 重投（`internal/worker/consumer.go:319-328`，注释原文「处理失败，不标记 offset → Kafka 会重投」）。真正意义上的死信表在**上一跳**：outbox → 内部 topic 投递失败且重试超限，由 Relay 写进 `event_dead_letter`（`internal/worker/relay.go:348-356` 判定，`moveToDeadLetter` `:382`），可用 `dlq` 命令族查看与重试（`cmd/cli/dlq.go:22`，子命令 `list` / `retry` / `discard` / `clean` / `stats`）。也就是说：订单消息「进死信」的确切含义是**卡在 outbox→内部 topic 那一跳超限才会落库**；已经进了 `shop-order-events` 之后的失败走 Kafka 重投。

### 6.8 order-consumer 最终行为与 13 字段协议

| 项 | 现状 | 依据 |
|---|---|---|
| 写入语义 | `op_type=index`（整篇覆盖、幂等），**不再是** `create`（同 `_id` 第二次写会被 409 拒） | `order-consumer/consumer/order_consumer.go:72` 用 `elastic.NewBulkIndexRequest()`；变更提交 `720df99`「订单 ES 写入语义由 create 改为 index，并停止静默丢弃写失败」 |
| `pay_type_name` | 已补写，与 shop-main **同表现算**（同一份 `order_service.PayTypeName` 口径），不再是一侧有一侧无 | `order-consumer/consumer/order_index.go:24`；提交 `a47798c` |
| 字段数 | **13 字段双向互等**（既不是 12，也不是谁多一个是超集） | consumer 侧 `order_index.go:14-29`；shop-main 侧 `doc_order.go:44-56` |
| 契约测试 | consumer 仓用真值表钉死：13 个 tag 全在、零值也必须显式带 `pay_type_name`、两条链路同口径 | `order-consumer/consumer/order_doc_contract_test.go:40` / `:110-112` / `:138-139`，配合 `pay_type_name_test.go` |

13 个字段（字段名、类型、tag、顺序逐字对齐）：`order_id`、`order_id_suffix`、`names`、`product_ids`、`uid`、`pay_time`、`pay_type`、`pay_type_name`、`refund_status`、`shipping_type`、`order_status`、`create_time`、`update_time`。

**一条以后一定会踩的注意事项**（`index` 整篇覆盖语义的直接后果）：consumer 必须**持续写全 13 个字段**。少写一个 —— 全量灌数灌进去的那个字段值会被覆盖写抹掉（`pay_type_name` 之前就是这么丢的）；多写一个 —— `dynamic: strict` 逐条拒写（`strict_dynamic_mapping_exception`，整批失败）。**将来任一侧加字段，两侧必须同时加、同时发版**，并同步改 `orders.jsonc` 的 `properties` 与 `schema_version`。

---

## 附录：一屏速查

| 我要做的事 | 就敲这一条 |
|---|---|
| 先看看现在啥情况 | `go run ./cmd/cli es es:status` |
| 改完声明文件收敛结构 | `go run ./cmd/cli es es:migrate --alias shop-product --dry-run` → 满意了把 `--dry-run` 换成 `--yes` |
| 只把商品数据补一遍 | `go run ./cmd/cli es es:sync-products` |
| 只补指定几个商品 | `go run ./cmd/cli es es:sync-products --ids 12,15` |
| 灌一半断了 | `go run ./cmd/cli es es:sync-products --resume` |
| 只重跑失败的那些 | `go run ./cmd/cli es es:sync-products --retry-failed runtime/es_sync/failed_shop-product_xxx.jsonl` |
| 字段类型变了要重建 | `go run ./cmd/cli es es:rebuild --alias shop-product --yes` |
| 重建确认没问题，删旧索引 | 同上再加 `--drop-old` |
| 历史物理索引第一次上线别名 | `go run ./cmd/cli es es:cutover --alias shop-product --yes` |
| **只把订单数据全量灌一遍** | `go run ./cmd/cli es es:sync-products --index shop-order`（没有 `es:sync-orders`，见 6.1） |
| 看订单索引体检 / 对账 | `go run ./cmd/cli es es:status --alias shop-order`（口径应为 `is_del=0`，见 6.6） |
| 订单结构收敛（改完 orders.jsonc 后） | `go run ./cmd/cli es es:migrate --alias shop-order --dry-run` → 满意了换 `--yes` |
| 订单蓝绿重建 | `go run ./cmd/cli es es:rebuild --alias shop-order --yes` |
| 看看有没有残留索引 | `go run ./cmd/cli es es:cleanup` |
| 删掉某个确认无用的残留 | `go run ./cmd/cli es es:cleanup --index shop-product_v2 --list-only=false --yes` |
