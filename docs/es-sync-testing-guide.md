# ES 同步模块分层测试指南（runWriter → Run）

> 面向「自底向上层层测绿」的手动验证流程：从纯函数 → 单并发单元（runWriter 行为）→ Run 整合 → 真机。
> 所有命令均基于项目**真实存在的测试**，可直接复制执行。

---

## 一、开始之前

### 1.1 前置条件

- 已安装 Go，能在项目根目录执行 `go test`。
- 第 1～3 层**不需要**真实 ES / MySQL（用 fake 注入，纯内存跑）。
- 第 4 层真机集成需要本地 ES 起着（docker 单节点 8.x），并显式带 `ES_IT=1`。

### 1.2 命令约定

| 参数 | 作用 | 何时用 |
|---|---|---|
| `-count=1` | 禁用测试缓存，每次真正执行 | **开发阶段必带** |
| `-race` | 开启竞态检测器 | **并发层（第 2/3 层）必带** |
| `-v` | 打印每个用例的详细输出 | 想看清断言信息时 |
| `-run '正则'` | 只跑匹配的用例 | 分层 / 单点验证 |

> 所有命令都在**项目根目录**执行：`cd /Users/hehongbo/www/GO/go-search/shop-main`

### 1.3 一个关键认知：runWriter 是怎么被测的

项目里**没有直接叫 `TestRunWriter` 的用例**。runWriter 的并发行为是通过「**Run + 特制 fake**」隔离验证的——走真实 Run 整合路径，但用 fake 把某个行为「逼」出来。这比直接调 runWriter 更干净。

`internal/es/sync/runner_test.go` 里的 fake：

| fake | 模拟什么 | 钉住 runWriter 的哪个行为 |
|---|---|---|
| `fakeProvider` / `fakeWriter` | 正常 / 报错的数据源与写出口 | 基本写入、错误传播 |
| `holdWriter` | 收几批后**永久阻塞** | 反压 + emit 逃生口 |
| `multiBatchProvider` | 连续发射多批 | 反压、writer ctx 存活 |
| `tailHoldProvider` | 发一批后卡住，留尾部缓冲 | tailFlush 独立 ctx |

每个测试注释里都写了「**鉴别力**」（破坏哪行代码会变红）——这是理解设计动机的钥匙。

---

## 二、第 1 层：纯函数（无协程，先跑这些，秒绿）

这一层全是确定性逻辑，不碰协程。全绿说明「零件」本身是对的。

### 2.1 文档装配（es 包）

验证 `BuildIndexDoc` 的资格判定、字段映射、HTML 清洗、时间格式：

```bash
go test -count=1 -v ./internal/es/ -run 'TestBuildIndexDoc|TestBuildDescription|TestTimeSerializes|TestDocTags|TestIndexFlagFields'
```

| 用例 | 验证什么 |
|---|---|
| `TestBuildIndexDocFilter` | 软删 / 下架商品被判为不入索引 |
| `TestBuildIndexDocFieldMapping` | 模型字段 → 文档字段映射正确 |
| `TestBuildDescriptionHTMLAndRuneLimit` | 去 HTML 标签 + 按 rune 截断 |
| `TestTimeSerializesAsRFC3339` | 时间序列化为 RFC3339（不被 ES 拒） |
| `TestDocTagsAreSubsetOfMapping` | 文档字段是 mapping 的子集 |
| `TestIndexFlagFieldsMarshalAsNumbers` | is_* 输出数字 0/1 而非 bool |

### 2.2 区间切分（sync 包）

验证向上取整、左开右闭、互不相交：

```bash
go test -count=1 -v ./internal/es/sync/ -run 'TestComputeRange|TestFeedRanges'
```

| 用例 | 验证什么 |
|---|---|
| `TestComputeRangeCount` | K = Concurrency × RangeMultiplier |
| `TestComputeRanges_Empty` | 空表返回 nil 短路 |
| `TestComputeRanges_SingleValue` | Min==Max 不除零 |
| `TestComputeRanges_MultipleSegments` | 多段互不相交、并集覆盖全体 |
| `TestComputeRanges_LessThanK` | 段数不足 k 时正确收敛 |
| `TestFeedRanges_ContextCancel` | 投区间时 ctx 取消能退出 |

### 2.3 bulk 纯逻辑（admin 包）

验证错误分类、攒批阈值、切块、缺 _id、退避：

```bash
go test -count=1 -v ./internal/es/admin/ -run 'TestClassifyBulkItem|TestShouldFlush|TestBulkChunker|TestSplitByID|TestBackoffDuration|TestEncodeFailure|TestReadFailuresRoundTrip'
```

| 用例 | 验证什么 |
|---|---|
| `TestClassifyBulkItem` | 429/503 归可重试，4xx 归结构类 |
| `TestShouldFlush` | 攒批达到条数 / 字节阈值触发 flush |
| `TestBulkChunker` | 按条数 / 字节切块 |
| `TestSplitByID_CountsMissingIDAndKeepsValid` | 缺 _id 不静默丢，转失败 |
| `TestBackoffDuration` | 指数退避时长 |
| `TestReadFailuresRoundTrip` | 失败落盘 → 读回往返一致 |

### 2.4 provider 装配（sync 包）

```bash
go test -count=1 -v ./internal/es/sync/ -run 'TestProductAssemble|TestProductScanColumns'
```

| 用例 | 验证什么 |
|---|---|
| `TestProductAssemble_SkipsIneligible` | 不合格行跳过 |
| `TestProductAssemble_EmptyBatch` | 空批处理 |
| `TestProductScanColumns_ContainsIsShow` | 列裁剪含 is_show（资格判定依赖） |

---

## 三、第 2 层：runWriter 行为隔离（Run + fake，加 -race）

这一层就是你问的「runWriter 独立验证」——用特制 fake 通过 Run 逼出每个并发行为。

### 3.1 反压 + emit 逃生口

```bash
go test -count=1 -race -v ./internal/es/sync/ -run 'TestRun_Backpressure|TestRun_ContextCancelInterruptsSend'
```

- `TestRun_Backpressure`：writer 阻塞 → docsCh 满 → scanner 被反压；cancel 后 Run 必须返回。
  - **鉴别力**：删掉 `case <-gctx.Done()` → scanner 永久阻塞 → 超时红；docsCh 改无界 → 容量断言红。
- `TestRun_ContextCancelInterruptsSend`：writer 从第一批就阻塞，cancel 后 2s 内返回 `context.Canceled`。

### 3.2 tailFlush 独立 ctx（必修②）

```bash
go test -count=1 -race -v ./internal/es/sync/ -run 'TestRun_CancelFlushesTail|TestRun_CancelTailDrainFailure'
```

- `TestRun_CancelFlushesTailWithIndependentCtx`：ctx 取消时仍用 `WithoutCancel` 派生 ctx 提交尾部，且计入 SentDocs。
  - **鉴别力**：退化成 `flush(ctx)`（已取消）→ 尾部丢 / 计数红。
- `TestRun_CancelTailDrainFailureIsCounted`：尾部排空真实失败时，逐条计入 FailedDocs，原因不被假 canceled 覆盖。

### 3.3 writer 用 runCtx 而非 gctx（第六轮 P0）

```bash
go test -count=1 -race -v ./internal/es/sync/ -run 'TestRun_WriterCtxAliveDuringDrain'
```

- `TestRun_WriterCtxAliveDuringDrain`：scanner 正常完成 → gctx 被 errgroup 取消，但 writer 在途 bulk 不能被杀。
  - **鉴别力**：writer 改用 gctx → cancelled>0 红。

### 3.4 goroutine 泄漏（三条路径）

```bash
go test -count=1 -race -v ./internal/es/sync/ -run 'TestRun_GoroutineLeak'
```

- `TestRun_GoroutineLeak` / `...OnError` / `...OnCancel`：正常 / 错误 / 取消三条路径，Run 返回后 goroutine 数都必须回到基线（diff==0）。

---

## 四、第 3 层：Run 整合 + 全量 race

### 4.1 基本功能 + 错误传播

```bash
go test -count=1 -race -v ./internal/es/sync/ -run 'TestRun_EmptyTable|TestRun_NormalEnd|TestRun_ScannerError|TestRun_ScannerPanic|TestRun_WriterError'
```

| 用例 | 验证什么 |
|---|---|
| `TestRun_EmptyTable` | 空表不报错、计数为 0 |
| `TestRun_NormalEnd` | 端到端 SentDocs / Batches 计数 |
| `TestRun_ScannerError` | scanner 错误传播到 Run 返回 |
| `TestRun_ScannerPanic` | panic 转 error，不死锁（5s 超时保护） |
| `TestRun_WriterError` | writer 错误传播到 Run 返回 |

### 4.2 整个 sync 包全量 + race

```bash
go test -count=1 -race ./internal/es/sync/
```

整合无竞态、全部用例绿，说明 Run 这套调度引擎组合正确。

---

## 五、第 4 层：真机集成（需本地 ES）

> 单测全绿 ≠ 真机正确。项目曾踩过「单测 + race 全绿，但真机 ES 写入恒为 0」的坑（gctx 误传）。真机对账是最后一道闸。

确保本地 ES 起着，然后：

```bash
ES_IT=1 go test -count=1 -v ./internal/es/admin/ -run 'TestIT'
```

| 用例 | 验证什么 |
|---|---|
| `TestITClient` | ES 客户端连通 |
| `TestAliasIndexes` | 别名查询 |
| `TestGetMapping` | 读 mapping（含别名→物理名兜底） |
| `TestIndexAlias` | 索引 / 别名操作 |

> 不带 `ES_IT=1` 时这些集成用例会被 `requireIntegration` 跳过（只跑纯单测）。

---

## 六、一条完整手动流程（从上往下复制跑）

```bash
cd /Users/hehongbo/www/GO/go-search/shop-main

echo "===== 第1层：纯函数 ====="
go test -count=1 ./internal/es/ -run 'TestBuildIndexDoc|TestBuildDescription|TestTimeSerializes'
go test -count=1 ./internal/es/sync/ -run 'TestComputeRange|TestProductAssemble'
go test -count=1 ./internal/es/admin/ -run 'TestClassifyBulkItem|TestShouldFlush|TestBulkChunker|TestSplitByID'

echo "===== 第2层：runWriter 行为（race）====="
go test -count=1 -race ./internal/es/sync/ -run 'TestRun_Backpressure|TestRun_CancelFlushesTail|TestRun_WriterCtxAliveDuringDrain|TestRun_GoroutineLeak'

echo "===== 第3层：Run 整合（race）====="
go test -count=1 -race ./internal/es/sync/

echo "===== 第4层：真机（可选，需 ES）====="
ES_IT=1 go test -count=1 ./internal/es/admin/ -run 'TestIT'
```

---

## 七、怎么读结果 + 学习建议

### 7.1 读结果

- **PASS / ok**：该层绿，往上走。
- **FAIL**：看 `-v` 输出的断言信息，对照测试注释里的「鉴别力」定位被破坏的行为。
- **race 警告**：`WARNING: DATA RACE` 说明有竞态，必须修，不能忽略。

### 7.2 单点深入

想深入理解某个行为，单跑那一个 + `-v`，再读它的注释：

```bash
go test -count=1 -race -v ./internal/es/sync/ -run 'TestRun_Backpressure'
```

注释直接告诉你「删掉逃生口会怎样」——这是把代码读活的最快方式。

### 7.3 进阶：破坏即红学习法

故意改坏一行，跑对应测试看它变红，再改回来：

| 改坏什么 | 跑哪个测试 | 预期 |
|---|---|---|
| 删 emit 的 `case <-gctx.Done()` | `TestRun_Backpressure` | 超时红 |
| writer 的 runCtx 换成 gctx | `TestRun_WriterCtxAliveDuringDrain` | cancelled>0 红 |
| tailFlush 退化成 `flush(ctx)` | `TestRun_CancelFlushesTailWithIndependentCtx` | 尾部丢 / 计数红 |

**亲手验证「防护真的有效」，比读十遍注释都管用。**

---

## 八、附录：测试文件速查

| 文件 | 层级 | 内容 |
|---|---|---|
| `internal/es/doc_test.go` | 第 1 层 | 文档装配纯函数 |
| `internal/es/sync/range_test.go` | 第 1 层 | 区间切分纯函数 |
| `internal/es/admin/bulk_test.go` | 第 1 层 | bulk 纯逻辑 |
| `internal/es/sync/product_test.go` | 第 1 层 | provider 装配 / checkpoint / 对账决策 |
| `internal/es/sync/runner_test.go` | 第 2/3 层 | runWriter 行为 + Run 整合（fake 驱动） |
| `internal/es/admin/es_admin_integration_test.go` | 第 4 层 | 真机集成（ES_IT 门控） |

---

## 九、小结

**验证路径 = 第1层纯函数（零件对）→ 第2层 runWriter 行为（Run+fake 逼出并发行为）→ 第3层 Run 整合+race（组合对）→ 第4层真机对账（落地对）。**

关键认知：
1. runWriter 不直接测，而是用 `holdWriter` / `tailHoldProvider` 这些 fake 通过 Run 隔离验证，每个测试钉一个行为。
2. 并发层必带 `-race`，开发阶段必带 `-count=1`。
3. 单测全绿 ≠ 真机正确，最后一定要真机看 ES `_count` 对账。
4. 测试注释里的「鉴别力」是理解设计动机的钥匙，配合「破坏即红」学习法效果最佳。