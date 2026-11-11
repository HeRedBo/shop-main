package es

import (
	"embed"
	"io/fs"
	"sort"
)

// FS 是内嵌的索引声明文件系统。
//
// 之所以要 go:embed：本项目 CLI 以单二进制分发，运维现场没有源码树，
// indices/*.jsonc 必须跟着二进制走；外置目录（--indices-dir / elasticsearch.indices-dir）
// 只作为「不发版改结构」的覆盖手段存在，优先级见 registry.go 的 LoadDefinitions。
//
// 只嵌入 *.jsonc：README.md 是变更规则的说明文档，不是结构事实源，不进二进制。
//
//go:embed indices/*.jsonc
var FS embed.FS

// EmbeddedIndicesDir 是 FS 内声明文件所在的目录名。
const EmbeddedIndicesDir = "indices"

// EmbeddedDefinitionFiles 返回内嵌进二进制的声明文件路径（升序）。
// 用途：es:status / es:migrate --dry-run 打印「当前二进制内置了哪些索引结构」，
// 便于确认线上跑的版本是否包含预期的 mapping。
func EmbeddedDefinitionFiles() []string {
	matches, err := fs.Glob(FS, EmbeddedIndicesDir+"/*.jsonc")
	if err != nil || len(matches) == 0 {
		return nil
	}
	out := make([]string, len(matches))
	copy(out, matches)
	sort.Strings(out) // Glob 通常已按字典序返回，这里只做防御性排序，保证输出稳定。
	return out
}
