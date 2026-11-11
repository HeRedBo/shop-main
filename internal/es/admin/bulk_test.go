package admin

// bulk_test.go 仅覆盖不依赖真实 ES 的纯逻辑：
// 错误分类、失败明细 JSONL 序列化、文件名清洗、bulk 切块判定、WriteIndexName 版本推导、指数退避计算。
// 这些判定与拆分逻辑正是方案反复强调的 strict 模式安全边界与幂等护栏，必须在接入真实集群前锁死。

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClassifyBulkItem(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		errType string
		want    failureClass
	}{
		{"429 可重试", http.StatusTooManyRequests, "es_rejected_execution_exception", classRetryable},
		{"503 可重试", http.StatusServiceUnavailable, "service_unavailable_exception", classRetryable},
		{"503 无类型也重试", http.StatusServiceUnavailable, "", classRetryable},
		{"type 兜底 rejected", 0, "es_rejected_execution_exception", classRetryable},
		{"strict_dynamic 结构类不重试", http.StatusBadRequest, "strict_dynamic_mapping_exception", classStructural},
		{"mapper_parsing 结构类不重试", http.StatusBadRequest, "mapper_parsing_exception", classStructural},
		{"404 结构类", http.StatusNotFound, "index_not_found_exception", classStructural},
		{"409 create 冲突按结构类", http.StatusConflict, "version_conflict_engine_exception", classStructural},
		{"未知默认结构类", 418, "whatever", classStructural},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyBulkItem(c.status, c.errType); got != c.want {
				t.Fatalf("classifyBulkItem(%d,%q)=%v, want %v", c.status, c.errType, got, c.want)
			}
		})
	}
}

func TestSanitizeTarget(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"shop-product_v1", "shop-product_v1"},
		{"shop/product", "shop_product"},
		{"a b:c*d?e", "a_b_c_d_e"},
		{"中文索引", "____"},
		{"..", "index"},
		{"", "index"},
	}
	for _, c := range cases {
		if got := sanitizeTarget(c.in); got != c.want {
			t.Fatalf("sanitizeTarget(%q)=%q, want %q", c.in, got, c.want)
		}
	}
	// 清洗结果必须是文件名安全：不含路径分隔符
	if strings.ContainsAny(sanitizeTarget("a/../b"), "/\\") {
		t.Fatal("sanitizeTarget 结果仍含路径分隔符")
	}
}

func TestFailureFileName(t *testing.T) {
	ts := time.Date(2026, 11, 10, 12, 30, 45, 0, time.UTC)
	got := failureFileName("shop/product", ts)
	want := "failed_shop_product_20261110_123045.jsonl"
	if got != want {
		t.Fatalf("failureFileName=%q, want %q", got, want)
	}
}

func TestEncodeFailure(t *testing.T) {
	doc := map[string]any{"id": "1001", "price": 9.9}
	f := Failure{
		ID:     "1001",
		Index:  "shop-product_v1",
		Op:     OpIndex,
		Type:   "strict_dynamic_mapping_exception",
		Reason: "mapping set to strict, dynamic introduction of [foo] is not allowed",
		Status: http.StatusBadRequest,
		Doc:    doc,
	}
	line, err := encodeFailure(f)
	if err != nil {
		t.Fatalf("encodeFailure 返回 error: %v", err)
	}
	if line[len(line)-1] != '\n' {
		t.Fatal("JSONL 单行必须以换行结尾")
	}
	// 反序列化校验字段齐全且含完整文档快照
	var rec failureRecord
	if err := json.Unmarshal(line, &rec); err != nil {
		t.Fatalf("反序列化失败: %v (line=%s)", err, line)
	}
	if rec.ID != "1001" || rec.Op != OpIndex || rec.Status != http.StatusBadRequest {
		t.Fatalf("关键字段不符: %+v", rec)
	}
	if rec.Reason == "" || rec.Type == "" {
		t.Fatal("type/reason 不得为空")
	}
	snapshot, ok := rec.Doc.(map[string]any)
	if !ok || snapshot["id"] != "1001" {
		t.Fatalf("失败样本必须自带完整文档快照, got=%v", rec.Doc)
	}

	// 不可序列化的 Doc 应降级丢弃 Doc 而非整体失败
	bad := Failure{ID: "x", Op: OpIndex, Type: "t", Reason: "r", Status: 500, Doc: make(chan int)}
	bl, err := encodeFailure(bad)
	if err != nil {
		t.Fatalf("含不可序列化 Doc 时 encodeFailure 应降级而非报错: %v", err)
	}
	var brec failureRecord
	if err := json.Unmarshal(bl, &brec); err != nil {
		t.Fatalf("降级行反序列化失败: %v", err)
	}
	if brec.Doc != nil {
		t.Fatal("降级后 Doc 应为 nil")
	}
	if brec.ID != "x" {
		t.Fatal("降级后仍应保留 _id 等基本字段")
	}
}

func TestShouldFlush(t *testing.T) {
	cases := []struct {
		name                            string
		count, bytes, maxDocs, maxBytes int
		want                            bool
	}{
		{"条数达标", 3000, 100, 3000, 10 << 20, true},
		{"字节达标", 10, 11 << 20, 3000, 10 << 20, true},
		{"均未达标", 10, 1024, 3000, 10 << 20, false},
		{"条数不限", 999999, 1024, 0, 10 << 20, false},
		{"字节不限", 10, 999 << 20, 3000, 0, false},
		{"两者都不限", 10, 20, 0, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldFlush(c.count, c.bytes, c.maxDocs, c.maxBytes); got != c.want {
				t.Fatalf("shouldFlush(%d,%d,%d,%d)=%v, want %v", c.count, c.bytes, c.maxDocs, c.maxBytes, got, c.want)
			}
		})
	}
}

func TestBulkChunker(t *testing.T) {
	// maxBytes<=0 回落默认 10MB
	c := NewBulkChunker(0, 0)
	if c.maxBytes != DefaultBulkMaxBytes {
		t.Fatalf("默认 maxBytes=%d, want %d", c.maxBytes, DefaultBulkMaxBytes)
	}
	if c.Add(1024) {
		t.Fatal("首条不应触发 flush")
	}
	if c.Count() != 1 || c.Bytes() != 1024 {
		t.Fatalf("累计不符: count=%d bytes=%d", c.Count(), c.Bytes())
	}

	// 条数到阈值触发
	d := NewBulkChunker(2, 0)
	if d.Add(10) {
		t.Fatal("第 1 条不应触发")
	}
	if !d.Add(10) {
		t.Fatal("第 2 条应触发条数阈值")
	}
	d.Reset()
	if d.Count() != 0 || d.Bytes() != 0 || d.Flush() {
		t.Fatal("Reset 后应清零且不触发")
	}

	// 字节到阈值触发（先到者）
	e := NewBulkChunker(1000, 100)
	if !e.Add(150) {
		t.Fatal("字节先到应触发 flush")
	}
}

func TestWriteIndexName(t *testing.T) {
	cases := []struct {
		name           string
		alias, current string
		nextN          int
		want           string
	}{
		{"合规推导下一代", "shop-product", "shop-product_v1", 1, "shop-product_v2"},
		{"两位数版本", "shop-product", "shop-product_v9", 1, "shop-product_v10"},
		{"current 为空回落 nextN", "shop-product", "", 5, "shop-product_v5"},
		{"current 不合规回落 nextN", "shop-product", "shop-product", 3, "shop-product_v3"},
		{"尾部非纯数字回落", "shop-product", "shop-product_vX", 2, "shop-product_v2"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := WriteIndexName(c.alias, c.current, c.nextN); got != c.want {
				t.Fatalf("WriteIndexName(%q,%q,%d)=%q, want %q", c.alias, c.current, c.nextN, got, c.want)
			}
		})
	}
}

func TestBackoffDuration(t *testing.T) {
	base := 100 * time.Millisecond
	capv := 2 * time.Second
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{0, base},
		{1, 200 * time.Millisecond},
		{2, 400 * time.Millisecond},
		{3, 800 * time.Millisecond},
		{4, 1600 * time.Millisecond},
		{5, 2 * time.Second},  // 3200ms 封顶到 2s
		{10, 2 * time.Second}, // 远超仍封顶
	}
	for _, c := range cases {
		if got := backoffDuration(c.attempt, base, capv); got != c.want {
			t.Fatalf("backoffDuration(%d)=%v, want %v", c.attempt, got, c.want)
		}
	}
	// cap<=0 不封顶
	if got := backoffDuration(3, base, 0); got != 800*time.Millisecond {
		t.Fatalf("不封顶时 backoffDuration(3)=%v", got)
	}
	// base<=0 使用缺省基数
	if got := backoffDuration(0, 0, 0); got != 100*time.Millisecond {
		t.Fatalf("缺省 base 应为 100ms, got %v", got)
	}
}

func TestParseFailureLine(t *testing.T) {
	idx := []byte(`{"_id":"1001","op":"index","type":"t","reason":"r","status":429,"doc":{"id":"1001"}}`)
	d, err := parseFailureLine(idx)
	if err != nil {
		t.Fatalf("parseFailureLine index 行报错: %v", err)
	}
	if d.ID != "1001" || d.Delete {
		t.Fatalf("index 行应还原为非删除动作: %+v", d)
	}
	if d.Src == nil {
		t.Fatal("index 行应携带文档快照供复跑")
	}

	del := []byte(`{"_id":"2002","op":"delete","type":"t","reason":"r","status":503}`)
	d2, err := parseFailureLine(del)
	if err != nil {
		t.Fatalf("parseFailureLine delete 行报错: %v", err)
	}
	if d2.ID != "2002" || !d2.Delete || d2.Src != nil {
		t.Fatalf("delete 行还原不符: %+v", d2)
	}

	if _, err := parseFailureLine([]byte(`{"op":"index"}`)); err == nil {
		t.Fatal("缺 _id 应报错")
	}
	if _, err := parseFailureLine([]byte(`not-json`)); err == nil {
		t.Fatal("非法 JSON 应报错")
	}
}

func TestReadFailuresRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "failed_shop-product_v1_20261110_120000.jsonl")

	// 先用 encodeFailure 落盘两行，再读回，验证写入/读取对称
	fails := []Failure{
		{ID: "1", Index: "shop-product_v1", Op: OpIndex, Type: "strict_dynamic_mapping_exception", Reason: "r", Status: 400, Doc: map[string]any{"id": "1"}},
		{ID: "2", Index: "shop-product_v1", Op: OpDelete, Type: "es_rejected_execution_exception", Reason: "r", Status: 429},
	}
	var buf strings.Builder
	for _, f := range fails {
		line, err := encodeFailure(f)
		if err != nil {
			t.Fatalf("encodeFailure: %v", err)
		}
		buf.Write(line)
	}
	if err := os.WriteFile(path, []byte(buf.String()), 0o644); err != nil {
		t.Fatalf("写临时文件失败: %v", err)
	}

	docs, err := ReadFailures(path)
	if err != nil {
		t.Fatalf("ReadFailures 报错: %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("期望读回 2 条, got %d", len(docs))
	}
	if docs[0].ID != "1" || docs[0].Delete || docs[0].Src == nil {
		t.Fatalf("第 1 条应为 index 且带快照: %+v", docs[0])
	}
	if docs[1].ID != "2" || !docs[1].Delete {
		t.Fatalf("第 2 条应为 delete: %+v", docs[1])
	}
}

// ---------------------------------------------------------------------------
// 必修⑤ 回归防护：空 _id 文档不得静默丢弃；版本尾缀正则口径跨层统一
// ---------------------------------------------------------------------------

// TestSplitByID_CountsMissingIDAndKeepsValid 钉住「过滤即记账」：
// 空 _id 必须一条不漏地转成失败明细，合法文档保持原顺序可投递。
//
// 变异鉴别力：把 splitByID 的空 _id 分支改回裸 continue，missing 长度为 0 → 变红。
func TestSplitByID_CountsMissingIDAndKeepsValid(t *testing.T) {
	docs := []Doc{
		{ID: "1", Src: map[string]any{"id": "1"}},
		{ID: "", Src: map[string]any{"id": "bad1"}},
		{ID: "", Delete: true},
		{ID: "2", Routing: "7", Src: map[string]any{"id": "2"}},
	}
	pending, missing := splitByID("shop-product_v9", docs)

	if len(pending) != 2 || pending[0].ID != "1" || pending[1].ID != "2" {
		t.Fatalf("pending 应为按序的 2 条合法文档，实际 %+v", pending)
	}
	if len(missing) != 2 {
		t.Fatalf("空 _id 必须逐条计入失败，实际 %d 条", len(missing))
	}
	if missing[0].Type != "missing_id" || missing[0].Op != OpIndex || missing[0].Doc == nil {
		t.Errorf("第 1 条失败明细应含快照与 index 动作，实际 %+v", missing[0])
	}
	if missing[1].Op != OpDelete {
		t.Errorf("第 2 条失败明细应如实记录 delete 动作，实际 %+v", missing[1])
	}
	for i, f := range missing {
		if f.Index != "shop-product_v9" {
			t.Errorf("第 %d 条失败明细未记录 target，实际 %+v", i, f)
		}
	}
}

// TestIndexBulk_AllMissingIDReportedAndDumped 端到端钉住 IndexBulk：
// 一批全是空 _id 时，既不投递 ES（pending 为空即短路，不触达连接），也必须
// Failed=N 并落 failure 文件 —— 不得返回零值 result 假装成功。
//
// 变异鉴别力：把 len(pending)==0 的出口改回 `return &BulkResult{}, nil`，
// Failed 变 0、临时目录里不会有失败文件 → 两条断言同时变红。
func TestIndexBulk_AllMissingIDReportedAndDumped(t *testing.T) {
	dir := t.TempDir()
	docs := []Doc{
		{ID: "", Src: map[string]any{"id": "a"}},
		{ID: "", Delete: true},
	}
	res, err := IndexBulk(context.Background(), "zz_b17_missing_id", docs, WithFailureDir(dir))
	if err != nil {
		t.Fatalf("空 _id 批次应返回结果与计数，不应因短路而丢结果: %v", err)
	}
	if res == nil {
		t.Fatal("IndexBulk 必须返回非 nil 结果")
	}
	if res.Failed != len(docs) {
		t.Errorf("Failed = %d, 期望 %d（空 _id 必须计入失败）", res.Failed, len(docs))
	}
	if res.Sent != 0 {
		t.Errorf("Sent = %d, 期望 0（未投递 ES）", res.Sent)
	}
	if len(res.Failures) != len(docs) {
		t.Fatalf("失败明细 = %d 条, 期望 %d 条", len(res.Failures), len(docs))
	}

	files, _ := filepath.Glob(filepath.Join(dir, "failed_zz_b17_missing_id_*.jsonl"))
	if len(files) != 1 {
		t.Fatalf("空 _id 失败必须落盘 failure 文件，实际找到 %d 个文件（dir=%s）", len(files), dir)
	}
	lines := readLines(t, files[0])
	if len(lines) != len(docs) {
		t.Fatalf("落盘行数 = %d, 期望 %d", len(lines), len(docs))
	}
	var ops []string
	for _, ln := range lines {
		var rec failureRecord
		if err := json.Unmarshal([]byte(ln), &rec); err != nil {
			t.Fatalf("落盘行不可解析: %v", err)
		}
		if rec.Type != "missing_id" || rec.ID != "" {
			t.Errorf("落盘明细不如实: %+v", rec)
		}
		ops = append(ops, rec.Op)
	}
	if ops[0] != OpIndex || ops[1] != OpDelete {
		t.Errorf("落盘动作应如实区分 index/delete，实际 %v", ops)
	}
}

// TestIndexBulk_TrulyEmptyBatchIsHonest 区分两种「空」：
// 本来就没有文档（零失败零成功、不落盘）与「全是空 _id」（必须计失败）。
func TestIndexBulk_TrulyEmptyBatchIsHonest(t *testing.T) {
	dir := t.TempDir()
	res, err := IndexBulk(context.Background(), "zz_b17_empty", nil, WithFailureDir(dir))
	if err != nil {
		t.Fatalf("真·空批次无需连 ES，不应报错: %v", err)
	}
	if res.Sent != 0 || res.Failed != 0 || len(res.Failures) != 0 {
		t.Errorf("真·空批次结果应全为零，实际 %+v", res)
	}
	if files, _ := filepath.Glob(filepath.Join(dir, "*")); len(files) != 0 {
		t.Errorf("无失败却落了 failure 文件: %v", files)
	}
}

// TestReadFailures_SkipsMissingIDLines 钉住必修⑤ 的配套收敛：
// 空 _id 明细已落盘（可观测），但它们无法幂等重投 —— ReadFailures 必须跳过
// 这类行而不是让整份失败文件读不动（否则 --retry-failed 直接崩）。
func TestReadFailures_SkipsMissingIDLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "failed_shop-product_v9_20261110_120000.jsonl")

	var buf strings.Builder
	lines, err := encodeFailure(Failure{ID: "", Index: "shop-product_v9", Op: OpIndex, Type: "missing_id", Reason: "r"})
	if err != nil {
		t.Fatalf("encodeFailure: %v", err)
	}
	buf.Write(lines)
	ok, err := encodeFailure(Failure{ID: "1001", Index: "shop-product_v9", Op: OpIndex, Type: "retry_exhausted", Reason: "r", Doc: map[string]any{"id": "1001"}})
	if err != nil {
		t.Fatalf("encodeFailure: %v", err)
	}
	buf.Write(ok)
	if err := os.WriteFile(path, []byte(buf.String()), 0o644); err != nil {
		t.Fatalf("写临时文件失败: %v", err)
	}

	docs, err := ReadFailures(path)
	if err != nil {
		t.Fatalf("含空 _id 行的失败文件不应整体读失败: %v", err)
	}
	if len(docs) != 1 || docs[0].ID != "1001" {
		t.Fatalf("应只读回 1 条可复跑文档，实际 %+v", docs)
	}
}

// TestVersionSuffixRe_AlignsWithCleanupRedline 钉住跨层正则口径（必修⑤ 附带项）：
// admin/alias.go 的 versionSuffixRe 与 cmd/cli/es_cleanup.go 的 cleanupVersionRe
// 必须同为 `_(?:v)?(\d+)$` —— 否则 es:cleanup 的版本红线会与管理层「下一代索引」推导
// 对同一个名字给出相反答案（一边认为带版本、一边认为不带）。
func TestVersionSuffixRe_AlignsWithCleanupRedline(t *testing.T) {
	cases := []struct {
		name  string
		index string
		want  bool
	}{
		{"标准 *_vN", "shop-product_v1", true},
		{"简写 *_N", "shop-product_1", true},
		{"无版本尾缀", "shop-product", false},
		{"数字不在结尾", "shop-product_v1_tmp", false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := versionSuffixRe.MatchString(tt.index); got != tt.want {
				t.Errorf("versionSuffixRe.MatchString(%q) = %v, 期望 %v", tt.index, got, tt.want)
			}
		})
	}
	if versionSuffixRe.String() != `_(?:v)?(\d+)$` {
		t.Errorf("versionSuffixRe 口径漂移: %s", versionSuffixRe.String())
	}
}

// readLines 读取文件非空行（测试辅助）。
func readLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	var out []string
	for _, ln := range strings.Split(string(b), "\n") {
		if s := strings.TrimSpace(ln); s != "" {
			out = append(out, s)
		}
	}
	return out
}
