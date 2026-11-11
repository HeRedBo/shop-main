package es

// 本文件负责把 indices/*.jsonc 变成可查询的 Registry。
//
// 三个装载来源按优先级从高到低：
//  1. explicitDir —— CLI 的 --indices-dir 参数（临时验证一份候选 mapping，不发版）；
//  2. conf 的 elasticsearch.indices-dir —— 部署级外置目录（空串表示使用内嵌）；
//  3. go:embed 的 FS —— 随二进制发布，是默认与兜底来源。
//
// Registry 同时承载「已解析的全部定义」与「可执行清单」两件事：
// 本期 orders.jsonc 只落盘不接线（_meta.status=draft），es:migrate 只能处理
// ExecutableDefinitions()，否则会给一个还没有文档 struct 与 Provider 的索引建结构。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"shop/pkg/global"
	"sort"
	"strings"
	"sync"
)

// DefinitionFileExts 是被视为索引声明的文件扩展名。
// 允许 .json 是为了让外置目录能直接堆放从 ES 导出的 mapping 文件。
var DefinitionFileExts = []string{".jsonc", ".json"}

// LoadDefinitions 装载全部索引声明。
//
// explicitDir 传空串表示「用配置，配置也为空则用内嵌」。
// 返回的 Definition 按文件名字典序排列，保证多索引 migrate 的执行顺序可复现。
// 任何一个文件解析失败都会返回错误（错误信息含文件路径与失败原因），
// 由调用方决定是否跳过该索引 —— 但正常流程应当直接中止，因为结构事实源不完整
// 会让 diff 判定基于缺失的索引进行。
func LoadDefinitions(explicitDir string) ([]*IndexDefinition, error) {
	dir, embedded := resolveIndicesDir(explicitDir)
	var (
		names []string
		read  func(name string) (data []byte, source string, err error)
	)

	if embedded {
		entries, err := fs.ReadDir(FS, EmbeddedIndicesDir)
		if err != nil {
			return nil, fmt.Errorf("es: 读取内嵌索引目录 %s 失败: %w", EmbeddedIndicesDir, err)
		}
		names = collectNames(entries, func(e fs.DirEntry) (string, bool) {
			if e.IsDir() {
				return "", false
			}
			return e.Name(), hasDefinitionExt(e.Name())
		})
		read = func(name string) ([]byte, string, error) {
			p := EmbeddedIndicesDir + "/" + name
			b, err := FS.ReadFile(p)
			return b, p, err
		}
	} else {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return nil, fmt.Errorf("es: 索引目录路径无效 %q: %w", dir, err)
		}
		st, err := os.Stat(abs)
		if err != nil {
			return nil, fmt.Errorf("es: 索引目录不可访问 %s: %w", abs, err)
		}
		if !st.IsDir() {
			// 允许直接指向单个声明文件，便于运维拿一份候选 mapping 就地验证。
			if !hasDefinitionExt(abs) {
				return nil, fmt.Errorf("es: 索引目录 %s 不是目录，也不是 %v 文件", abs, DefinitionFileExts)
			}
			return loadDefinitionFiles([]string{abs}, func(p string) ([]byte, string, error) {
				b, err := os.ReadFile(p)
				return b, p, err
			}, false)
		}
		entries, err := os.ReadDir(abs)
		if err != nil {
			return nil, fmt.Errorf("es: 读取索引目录 %s 失败: %w", abs, err)
		}
		names = collectNames(entries, func(e fs.DirEntry) (string, bool) {
			if e.IsDir() {
				return "", false
			}
			return e.Name(), hasDefinitionExt(e.Name())
		})
		for i := range names {
			names[i] = filepath.Join(abs, names[i])
		}
		sort.Strings(names)
		read = func(full string) ([]byte, string, error) {
			b, err := os.ReadFile(full)
			return b, full, err
		}
		// 外置目录一个文件都没有属于配置事故：静默回落到内嵌会让「改了没生效」极难发现。
		if len(names) == 0 {
			return nil, fmt.Errorf("es: 索引目录 %s 下没有任何 %s 文件（期望至少一份 *.jsonc 索引声明）", abs, DefinitionFileExts)
		}
		return loadDefinitionFiles(names, read, false)
	}

	return loadDefinitionFiles(names, read, true)
}

// resolveIndicesDir 依据优先级决定装载来源。explicitDir 覆盖配置。
func resolveIndicesDir(explicitDir string) (dir string, embedded bool) {
	if s := strings.TrimSpace(explicitDir); s != "" {
		return s, false
	}
	// 注意：测试与未初始化配置的进程里 global.CONFIG 是零值，IndicesDir 为空即走内嵌。
	if s := strings.TrimSpace(global.CONFIG.Elasticsearch.IndicesDir); s != "" {
		return s, false
	}
	return EmbeddedIndicesDir, true
}

// loadDefinitionFiles 逐份读取 + 解析 + 校验，并拒绝同一批里重复的 alias。
func loadDefinitionFiles(names []string, read func(name string) ([]byte, string, error), embedded bool) ([]*IndexDefinition, error) {
	defs := make([]*IndexDefinition, 0, len(names))
	byAlias := make(map[string]string, len(names))

	for _, name := range names {
		data, source, err := read(name)
		if err != nil {
			return nil, fmt.Errorf("es: 读取索引声明 %s 失败: %w", name, err)
		}
		def, err := ParseDefinitionJSONC(source, data, embedded)
		if err != nil {
			return nil, err
		}
		if prev, dup := byAlias[def.Alias()]; dup {
			return nil, fmt.Errorf("es: 索引声明 %s 与 %s 声明了同一个 alias=%s（物理索引名与别名必须一一对应）", prev, source, def.Alias())
		}
		byAlias[def.Alias()] = source
		defs = append(defs, def)
	}

	if len(defs) == 0 {
		return nil, fmt.Errorf("es: 未装载到任何索引声明（内嵌目录 %s 下缺少 *.jsonc？）", EmbeddedIndicesDir)
	}
	return defs, nil
}

// ParseDefinitionJSONC 解析单份 JSONC 声明文本（含注释）为 IndexDefinition。
// source 只用于错误信息与日志，不参与结构判定。
func ParseDefinitionJSONC(source string, data []byte, embedded bool) (*IndexDefinition, error) {
	body, err := parseJSONCBody(source, data)
	if err != nil {
		return nil, err
	}
	def, err := NewIndexDefinition(source, body, embedded)
	if err != nil {
		return nil, err
	}
	return def, nil
}

// parseJSONCBody 剥离注释 -> 以 UseNumber 解码为 map[string]any -> 拒绝尾部多余内容。
// 用 UseNumber 是为了让 number_of_shards 的 15 保持整数字面量，
// 否则 float64 往返会让指纹在跨版本 Go 之间漂移。
func parseJSONCBody(source string, data []byte) (map[string]any, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, fmt.Errorf("es: 索引声明 %s 为空文件", sourceLabel(source))
	}

	dec := json.NewDecoder(bytes.NewReader(Strip(data)))
	dec.UseNumber()

	var body map[string]any
	if err := dec.Decode(&body); err != nil {
		return nil, fmt.Errorf("es: 索引声明 %s 解析失败: %w", sourceLabel(source), err)
	}
	// 顶层对象之后不应再有 token；有则说明手改时多敲了括号/内容。
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("es: 索引声明 %s 在顶层对象之后存在多余内容（请检查注释剥离后是否遗留了半个 JSON）", sourceLabel(source))
	}
	return body, nil
}

func collectNames[E any](entries []E, pick func(E) (string, bool)) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if n, ok := pick(e); ok {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

func hasDefinitionExt(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	for _, e := range DefinitionFileExts {
		if ext == e {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Registry
// ---------------------------------------------------------------------------

// Registry 是 alias -> IndexDefinition 的并发安全索引表。
// 零值不可用，必须用 NewRegistry（或 LoadRegistry）。
type Registry struct {
	mu      sync.RWMutex
	byAlias map[string]*IndexDefinition
}

// NewRegistry 构造空 Registry。
func NewRegistry() *Registry {
	return &Registry{byAlias: make(map[string]*IndexDefinition)}
}

// LoadRegistry 装载声明并注册进一个新 Registry，等价 LoadDefinitions + Register。
func LoadRegistry(explicitDir string) (*Registry, error) {
	defs, err := LoadDefinitions(explicitDir)
	if err != nil {
		return nil, err
	}
	r := NewRegistry()
	if err := r.Register(defs...); err != nil {
		return nil, err
	}
	return r, nil
}

// Register 注册定义；同一 alias 重复注册视为错误（两份事实源打架必须显式暴露）。
// 本方法是原子的：任一定义非法时整批不生效，不会留下「注册了一半」的状态，
// 否则 migrate 会基于不完整的 Registry 做分级判定。
func (r *Registry) Register(defs ...*IndexDefinition) error {
	seen := make(map[string]string, len(defs))
	for _, d := range defs {
		if d == nil {
			return fmt.Errorf("es: 不允许注册空的索引定义")
		}
		if d.Alias() == "" {
			return fmt.Errorf("es: 索引定义 %s 的 alias 为空，拒绝注册", d.Source())
		}
		if prev, dup := seen[d.Alias()]; dup {
			return fmt.Errorf("es: 本批内别名 %s 重复（%s 与 %s）", d.Alias(), prev, d.Source())
		}
		seen[d.Alias()] = d.Source()
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.byAlias == nil {
		r.byAlias = make(map[string]*IndexDefinition)
	}
	for _, d := range defs {
		if old, exists := r.byAlias[d.Alias()]; exists {
			return fmt.Errorf("es: 别名 %s 已注册（来源 %s），拒绝再注册 %s", d.Alias(), old.Source(), d.Source())
		}
	}
	for _, d := range defs {
		r.byAlias[d.Alias()] = d
	}
	return nil
}

// Get 按别名查找。
func (r *Registry) Get(alias string) (*IndexDefinition, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.byAlias[alias]
	return d, ok
}

// MustGet 按别名查找，找不到返回含已注册别名清单的错误（比 bool 更好排障）。
func (r *Registry) MustGet(alias string) (*IndexDefinition, error) {
	d, ok := r.Get(alias)
	if ok {
		return d, nil
	}
	return nil, fmt.Errorf("es: 未找到别名 %s 的索引定义，已注册：%v", alias, r.Aliases())
}

// GetByName 按物理索引名反查（es:status 拿到 AliasIndexes 之后定位声明用）。
func (r *Registry) GetByName(name string) (*IndexDefinition, bool) {
	for _, d := range r.All() {
		if d.Name() == name {
			return d, true
		}
	}
	return nil, false
}

// All 返回全部定义，按别名升序，保证遍历顺序可复现。
func (r *Registry) All() []*IndexDefinition {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]*IndexDefinition, 0, len(r.byAlias))
	for _, d := range r.byAlias {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].alias < out[j].alias })
	return out
}

// ExecutableDefinitions 返回可执行清单（_meta.status=active）。
// es:migrate / es:rebuild / es:sync 的逐条循环必须用它，而不是 All()：
// 本期 orders.jsonc 只落盘不接线，All() 会让 migrate 去给一个没有 Provider 的索引建结构。
func (r *Registry) ExecutableDefinitions() []*IndexDefinition {
	all := r.All()
	out := make([]*IndexDefinition, 0, len(all))
	for _, d := range all {
		if d.Executable() {
			out = append(out, d)
		}
	}
	return out
}

// Aliases 返回已注册别名（升序），仅用于错误信息与状态输出。
func (r *Registry) Aliases() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]string, 0, len(r.byAlias))
	for a := range r.byAlias {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// Len 返回已注册定义数量。
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.byAlias)
}
