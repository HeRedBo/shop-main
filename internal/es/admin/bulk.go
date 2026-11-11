package admin

// 本文件自实现同步 bulk 写入（下钻 olivere *elastic.Client），不复用 pkg/es 的
// BulkCreateDocs/BulkReplaceDocs（其从未调用 .Doc()）与 BulkUpdateDoc（update 字段小写不可导出）。
//
// 关键约束：
//   - 一律使用 NewBulkIndexRequest（op_type=index，幂等覆盖），绝不用 NewBulkCreateRequest
//     （op_type=create，对已存在 _id 返回 409），全量同步与差量补偿都属「重复灌数必须幂等」场景；
//   - HTTP 200 也可能含逐条错误，必须检查 resp.Errors 并遍历 resp.Items；
//   - 429/503 视为可重试（指数退避 + jitter），strict_dynamic_mapping_exception 等 4xx 结构类
//     错误不重试直接落失败集，让结构漂移第一时间暴露而非静默丢数据；
//   - 空批次在调用 Do 前短路，否则 olivere 报 "No bulk actions to commit"；
//   - 缺 _id 的文档不得静默丢弃：计入 Failed 并与既有格式一致地落失败明细（必修⑤）；
//   - BulkService 没有 Stats()，观测用 NumberOfActions()/EstimatedSizeInBytes()。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/olivere/elastic/v7"
)

// bulk 动作类型常量。
const (
	OpIndex  = "index"
	OpDelete = "delete"
)

// defaultFailureDir 失败明细 JSONL 默认落盘目录（相对进程工作目录，即项目根）。
const defaultFailureDir = "runtime/es_sync"

// DefaultBulkMaxBytes bulk 单次请求体字节上限默认值（ES 官方建议 5-15MB）。
const DefaultBulkMaxBytes = 10 * 1024 * 1024

// Doc 是待写入/删除的单条文档。
//
// 删除动作的表示采用「在 Doc 上带 op 标志」方案（而非单独的 DeleteBulk）：
// Delete=true 时组装 NewBulkDeleteRequest 并与 index 动作混入同一 bulk 请求，
// 此时 Src 可为 nil。
type Doc struct {
	ID      string // 文档 _id，必填（幂等与失败回查都依赖它）
	Routing string // 可选；空串则不设置 routing
	Src     any    // 已可 json.Marshal 的结构体；Delete 动作时忽略
	Delete  bool   // true 表示删除动作
}

// Failure 是单条 item 级失败的明细。
type Failure struct {
	ID     string
	Index  string
	Op     string
	Type   string
	Reason string
	Status int
	Doc    any // 完整文档快照：因 _source.excludes 排除了大文本字段，事后无法回查原值，样本必须自带快照
}

// BulkResult 汇总一次 IndexBulk 的结果。
type BulkResult struct {
	Sent     int       // 成功写入条数
	Failed   int       // 失败条数（= len(Failures)）
	Failures []Failure // 失败明细（结构类失败 + 退避耗尽的可重试失败）
}

// bulkConfig 是 IndexBulk 的可选项。
type bulkConfig struct {
	maxRetries    int           // 可重试错误最大重试次数
	baseBackoff   time.Duration // 退避基数
	maxBackoff    time.Duration // 退避上限
	failureDir    string        // 失败明细落盘目录
	writeFailures bool          // 是否落盘失败明细
}

func defaultBulkConfig() bulkConfig {
	return bulkConfig{
		maxRetries:    3,
		baseBackoff:   100 * time.Millisecond,
		maxBackoff:    5 * time.Second,
		failureDir:    defaultFailureDir,
		writeFailures: true,
	}
}

// BulkOption 函数式选项。
type BulkOption func(*bulkConfig)

// WithMaxRetries 设置可重试错误的最大重试次数（<=0 表示不重试）。
func WithMaxRetries(n int) BulkOption { return func(c *bulkConfig) { c.maxRetries = n } }

// WithBackoff 设置指数退避的基数与上限。
func WithBackoff(base, cap time.Duration) BulkOption {
	return func(c *bulkConfig) {
		if base > 0 {
			c.baseBackoff = base
		}
		if cap > 0 {
			c.maxBackoff = cap
		}
	}
}

// WithFailureDir 覆盖失败明细落盘目录。
func WithFailureDir(dir string) BulkOption { return func(c *bulkConfig) { c.failureDir = dir } }

// WithWriteFailures 关闭/开启失败明细落盘。
func WithWriteFailures(b bool) BulkOption { return func(c *bulkConfig) { c.writeFailures = b } }

// IndexBulk 以同步 bulk 将 docs 写入 target（物理索引或别名）。
//
// 返回的 BulkResult 只统计 item 级成功/失败；传输/HTTP 级错误（如整体 503、网络故障）
// 直接以 error 上抛，交由调用方外层重试。失败明细默认以 JSONL 追加落盘，
// 落盘本身失败不掩盖 bulk 结果，仅附带一条 warning error。
//
// Phase 4 的 runner 会用 result.Failed 配合 --max-failures 决定是否中断整任务。
func IndexBulk(ctx context.Context, target string, docs []Doc, opts ...BulkOption) (*BulkResult, error) {
	cfg := defaultBulkConfig()
	for _, o := range opts {
		o(&cfg)
	}

	// 必修⑤：无 _id 的文档既无法幂等写入也无法回查，过去被 continue 静默丢弃 ——
	// 既不报错也不计入 Failed，等于把数据无声吞掉。现在如实转成失败明细（与既有
	// item 级失败同一结构、同一落盘格式）计入 Failed，由 finishBulkResult 统一落盘。
	pending, missing := splitByID(target, docs)
	result := &BulkResult{Failures: missing}

	// 无可投递动作时短路（不发请求，也不会因空 actions 被 olivere 拒绝）：
	// 但若本批全因缺 _id 被判失败，结果里必须如实反映 Failed>0，不得退化成零值“成功”。
	if len(pending) == 0 {
		return finishBulkResult(target, result, cfg)
	}

	cli, err := Raw()
	if err != nil {
		return nil, err
	}

	for attempt := 0; ; attempt++ {
		sent, retryDocs, structFails, err := doBulk(ctx, cli, target, pending)
		if err != nil {
			return nil, err
		}
		result.Sent += sent
		result.Failures = append(result.Failures, structFails...)

		if len(retryDocs) == 0 {
			break
		}
		if attempt >= cfg.maxRetries {
			// 退避耗尽：剩余可重试项落失败集
			result.Failures = append(result.Failures, giveUpFailures(target, retryDocs)...)
			break
		}
		if err := sleepBackoff(ctx, attempt, cfg); err != nil {
			// ctx 取消/超时：把在途可重试项落失败集后收敛
			result.Failures = append(result.Failures, giveUpFailures(target, retryDocs)...)
			break
		}
		pending = retryDocs
	}
	return finishBulkResult(target, result, cfg)
}

// splitByID 把待投递文档拆成「可投递」与「缺 _id」两组（必修⑤）。
//
// 过滤与计数必须出自同一个函数：一旦分家就会出现「过滤了但没记账」的静默丢数据。
// pending 保持原顺序，missing 逐条生成失败明细（index/delete 动作各自如实记录）。
func splitByID(target string, docs []Doc) (pending []Doc, missing []Failure) {
	pending = make([]Doc, 0, len(docs))
	for _, d := range docs {
		if d.ID == "" {
			// 必修⑤：空 _id 既无法幂等写入也无法回查，如实转成失败明细（不投递）。
			missing = append(missing, Failure{
				ID:     "",
				Index:  target,
				Op:     bulkOpName(d),
				Type:   "missing_id",
				Reason: "文档缺少 _id，无法幂等写入与失败回查，需回源修数据",
				Status: 0,
				Doc:    d.Src,
			})
			continue
		}
		pending = append(pending, d)
	}
	return pending, missing
}

// bulkOpName 返回 Doc 对应的 bulk 动作名，与 Failure.Op 口径一致。
func bulkOpName(d Doc) string {
	if d.Delete {
		return OpDelete
	}
	return OpIndex
}

// finishBulkResult 结算 Failed 计数并按配置落盘失败明细。
// 落盘失败不掩盖 bulk 结果，仅附带一条 warning error（与既有口径一致）。
func finishBulkResult(target string, result *BulkResult, cfg bulkConfig) (*BulkResult, error) {
	result.Failed = len(result.Failures)
	if result.Failed > 0 && cfg.writeFailures {
		if derr := dumpFailures(target, result.Failures, cfg.failureDir); derr != nil {
			return result, fmt.Errorf("bulk 完成但 %d 条失败，且失败明细落盘失败: %w", result.Failed, derr)
		}
	}
	return result, nil
}

// doBulk 发送一次 bulk 请求并逐条解析 item，按可重试/结构类分流。
func doBulk(ctx context.Context, cli *elastic.Client, target string, docs []Doc) (sent int, retry []Doc, structFails []Failure, err error) {
	bs := cli.Bulk().Index(target).Refresh("false").ErrorTrace(true)
	byID := make(map[string]Doc, len(docs))
	for _, d := range docs {
		byID[d.ID] = d
		if d.Delete {
			dr := elastic.NewBulkDeleteRequest().Index(target).Id(d.ID)
			if d.Routing != "" {
				dr = dr.Routing(d.Routing)
			}
			bs.Add(dr)
		} else {
			ir := elastic.NewBulkIndexRequest().Index(target).Id(d.ID).Doc(d.Src)
			if d.Routing != "" {
				ir = ir.Routing(d.Routing)
			}
			bs.Add(ir)
		}
	}
	if bs.NumberOfActions() == 0 {
		return 0, nil, nil, nil
	}

	resp, err := bs.Do(ctx)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("bulk 请求失败 [target=%s, actions=%d]: %w", target, len(docs), err)
	}
	if !resp.Errors {
		return len(docs), nil, nil, nil
	}

	// 逐条解析：resp.Items 每个元素是只含 1 个 key 的 map（index/create/update/delete）
	seen := make(map[string]bool, len(docs))
	for _, m := range resp.Items {
		for op, it := range m {
			if it == nil {
				continue
			}
			d := byID[it.Id]
			if it.Error == nil {
				if !seen[it.Id] {
					sent++
					seen[it.Id] = true
				}
				continue
			}
			f := Failure{
				ID:     it.Id,
				Index:  it.Index,
				Op:     op,
				Type:   it.Error.Type,
				Reason: it.Error.Reason,
				Status: it.Status,
				Doc:    d.Src,
			}
			if classifyBulkItem(it.Status, it.Error.Type) == classRetryable {
				retry = append(retry, d)
			} else {
				structFails = append(structFails, f)
			}
		}
	}
	return sent, retry, structFails, nil
}

// failureClass 是 item 级失败的分类。
type failureClass int

const (
	classStructural failureClass = iota // 结构类：不重试，直接落失败集
	classRetryable                      // 过载/临时：指数退避重试
)

// classifyBulkItem 依据 HTTP status 与 error type 判定单条 bulk 失败的可重试性。纯函数，便于单测。
//
// 429（es_rejected_execution_exception）与 503 归为可重试；其余（含 strict_dynamic_mapping_exception、
// mapper_parsing_exception 等 4xx）一律视为结构漂移，不重试。
func classifyBulkItem(status int, errType string) failureClass {
	switch status {
	case http.StatusTooManyRequests, http.StatusServiceUnavailable:
		return classRetryable
	}
	switch errType {
	case "es_rejected_execution_exception", "service_unavailable_exception", "cluster_operation_grouped_exception":
		return classRetryable
	}
	return classStructural
}

// giveUpFailures 把退避耗尽仍未成功的文档转成失败明细。
func giveUpFailures(target string, docs []Doc) []Failure {
	out := make([]Failure, 0, len(docs))
	for _, d := range docs {
		out = append(out, Failure{
			ID:     d.ID,
			Index:  target,
			Op:     bulkOpName(d),
			Type:   "retry_exhausted",
			Reason: "可重试错误在退避上限内仍未成功",
			Status: 0,
			Doc:    d.Src,
		})
	}
	return out
}

// backoffDuration 计算第 attempt 次（从 0 起）的指数退避时长：base * 2^attempt，封顶 cap（cap<=0 不封顶）。
// 纯函数，不含 jitter，便于单测。
func backoffDuration(attempt int, base, cap time.Duration) time.Duration {
	if base <= 0 {
		base = 100 * time.Millisecond
	}
	d := base
	for i := 0; i < attempt; i++ {
		d *= 2
		if cap > 0 && d >= cap {
			return cap
		}
	}
	if cap > 0 && d > cap {
		return cap
	}
	return d
}

// sleepBackoff 按 attempt 计算退避并叠加 [0, d/2] 抖动后睡眠，可被 ctx 打断。
func sleepBackoff(ctx context.Context, attempt int, cfg bulkConfig) error {
	d := backoffDuration(attempt, cfg.baseBackoff, cfg.maxBackoff)
	jitter := time.Duration(rand.Int63n(int64(d/2) + 1))
	t := time.NewTimer(d + jitter)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// -- 失败明细落盘 --

// unsafeFileRe 匹配单个非 [A-Za-z0-9._-] 字符；逐个替换（不折叠连续串）以尽量保留名称区分度。
var unsafeFileRe = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// sanitizeTarget 把 target 清洗为文件名安全形态（每个非 [A-Za-z0-9._-] 字符替换为下划线）。纯函数。
func sanitizeTarget(name string) string {
	s := unsafeFileRe.ReplaceAllString(name, "_")
	if s == "" || s == "." || s == ".." {
		return "index"
	}
	return s
}

// failureFileName 生成 failed_<target>_<ts>.jsonl 形式的文件名。
func failureFileName(target string, ts time.Time) string {
	return "failed_" + sanitizeTarget(target) + "_" + ts.Format("20060102_150405") + ".jsonl"
}

// failureRecord 是失败明细 JSONL 单行的序列化结构。
type failureRecord struct {
	ID     string `json:"_id"`
	Index  string `json:"index,omitempty"`
	Op     string `json:"op"`
	Type   string `json:"type"`
	Reason string `json:"reason"`
	Status int    `json:"status"`
	Doc    any    `json:"doc,omitempty"`
}

// encodeFailure 把一条失败序列化成 JSONL 单行（含完整文档快照，末尾带换行）。纯函数。
func encodeFailure(f Failure) ([]byte, error) {
	rec := failureRecord{
		ID:     f.ID,
		Index:  f.Index,
		Op:     f.Op,
		Type:   f.Type,
		Reason: f.Reason,
		Status: f.Status,
		Doc:    f.Doc,
	}
	b, err := json.Marshal(rec)
	if err != nil {
		// 文档快照不可序列化时降级：丢弃 Doc 再试，保证失败明细本身能落盘
		rec.Doc = nil
		if b, err = json.Marshal(rec); err != nil {
			return nil, err
		}
	}
	return append(b, '\n'), nil
}

// dumpFailures 以 JSONL 追加方式把失败明细写入 <dir>/failed_<target>_<ts>.jsonl。
func dumpFailures(target string, failures []Failure, dir string) error {
	if len(failures) == 0 {
		return nil
	}
	if dir == "" {
		dir = defaultFailureDir
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建失败明细目录失败 [%s]: %w", dir, err)
	}
	path := filepath.Join(dir, failureFileName(target, time.Now()))
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("打开失败明细文件失败 [%s]: %w", path, err)
	}
	defer f.Close()

	w := bufio.NewWriter(f)
	for _, fa := range failures {
		line, err := encodeFailure(fa)
		if err != nil {
			return fmt.Errorf("序列化失败明细失败 [id=%s]: %w", fa.ID, err)
		}
		if _, err := w.Write(line); err != nil {
			return fmt.Errorf("写入失败明细文件失败 [%s]: %w", path, err)
		}
	}
	return w.Flush()
}

// parseFailureLine 把一行失败明细 JSONL 解析为可复跑的 Doc。纯函数，便于单测。
// op=delete 时还原为删除动作（Src 置 nil）；其余按 index 动作还原，Src 为落盘时保存的文档快照。
func parseFailureLine(line []byte) (Doc, error) {
	var rec failureRecord
	if err := json.Unmarshal(line, &rec); err != nil {
		return Doc{}, err
	}
	if rec.ID == "" {
		return Doc{}, fmt.Errorf("失败明细缺少 _id: %s", line)
	}
	return Doc{ID: rec.ID, Delete: rec.Op == OpDelete, Src: rec.Doc}, nil
}

// ReadFailures 读取 failed_*.jsonl，返回可直接投喂 IndexBulk 的 []Doc，
// 供 Phase 4 的 --retry-failed 单独复跑失败项而不重跑全量。
func ReadFailures(path string) ([]Doc, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("打开失败明细文件失败 [%s]: %w", path, err)
	}
	defer f.Close()

	var docs []Doc
	skippedNoID := 0
	sc := bufio.NewScanner(f)
	// 单行可能内联完整文档快照，放宽扫描上限避免 token too long
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for n := 1; sc.Scan(); n++ {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		// 必修⑤ 配套：缺 _id 的明细（type=missing_id）本身就是「无法幂等重投」的样本，
		// 落盘只为可观测；复跑必须跳过并告警，绝不能让整份失败文件因此读不动。
		var rec failureRecord
		if err := json.Unmarshal(line, &rec); err == nil && rec.ID == "" {
			skippedNoID++
			continue
		}
		d, err := parseFailureLine(line)
		if err != nil {
			return nil, fmt.Errorf("解析失败明细第 %d 行 [%s]: %w", n, path, err)
		}
		docs = append(docs, d)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("读取失败明细文件失败 [%s]: %w", path, err)
	}
	if skippedNoID > 0 {
		fmt.Fprintf(os.Stderr, "⚠️  已跳过 %d 条缺少 _id 的失败明细（无法幂等复跑，需回源修数据）[%s]\n",
			skippedNoID, path)
	}
	return docs, nil
}

// -- bulk 切块辅助（按条数或字节先到者）--

// EstimateSize 用 json.Marshal 估算单文档序列化后的字节数，供 BulkChunker 累计估长。
func EstimateSize(v any) (int, error) {
	if v == nil {
		return 0, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return 0, err
	}
	return len(b), nil
}

// shouldFlush 判定「条数或字节先到者」是否触发 flush；maxDocs/maxBytes <=0 表示该维度不设限。纯函数。
func shouldFlush(count, bytes, maxDocs, maxBytes int) bool {
	if maxDocs > 0 && count >= maxDocs {
		return true
	}
	if maxBytes > 0 && bytes >= maxBytes {
		return true
	}
	return false
}

// BulkChunker 按条数（maxDocs）或字节（maxBytes，默认 10MB）先到者给出 flush 信号，
// 供 Phase 4 流水线在 writer 侧切块。
type BulkChunker struct {
	maxDocs  int
	maxBytes int
	count    int
	bytes    int
}

// NewBulkChunker 创建切块器；maxBytes<=0 时回落到 DefaultBulkMaxBytes（10MB）。
func NewBulkChunker(maxDocs, maxBytes int) *BulkChunker {
	if maxBytes <= 0 {
		maxBytes = DefaultBulkMaxBytes
	}
	return &BulkChunker{maxDocs: maxDocs, maxBytes: maxBytes}
}

// Add 累加一条文档的字节数，返回当前是否达到 flush 阈值。
func (c *BulkChunker) Add(size int) bool {
	c.count++
	c.bytes += size
	return c.Flush()
}

// Flush 报告当前是否应触发一次 bulk 提交。
func (c *BulkChunker) Flush() bool {
	return shouldFlush(c.count, c.bytes, c.maxDocs, c.maxBytes)
}

// Reset 清空累计（提交后调用）。
func (c *BulkChunker) Reset() { c.count, c.bytes = 0, 0 }

// Count 返回当前累计条数。
func (c *BulkChunker) Count() int { return c.count }

// Bytes 返回当前累计字节。
func (c *BulkChunker) Bytes() int { return c.bytes }
