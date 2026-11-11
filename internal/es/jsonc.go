package es

// 本文件实现 JSONC（JSON with Comments）到合法 JSON 的转换，是 indices/*.jsonc
// 声明文件能够被 go:embed 直接装载的前提。
//
// 设计约束：
//   - 必须逐字节扫描并跟踪「是否在字符串字面量内 / 是否处于转义序列」，
//     否则 "http://127.0.0.1:9200"、"a//b" 这类值中的 // 会被当成注释误删；
//   - 只处理 // 行注释与 /* */ 块注释，不处理 JS 尾随逗号（trailing comma）：
//     ES 的 settings/mappings 一律拒绝尾随逗号，声明文件里也不允许出现；
//   - 注释体里的 UTF-8 多字节字符（中文注释）按字节跳过即可，
//     续字节恒 >= 0x80，不会与 '/'、'*'、'"'、'\'、'\n' 这些 ASCII 定界符冲突。

import (
	"encoding/json"
)

// 状态机状态常量（非导出，仅 Strip 使用）。
const (
	jsOutside        = iota // 常规 JSON 文本
	jsInString              // 双引号字符串字面量内部
	jsInEscape              // 字符串内刚读到反斜杠，下一个字节无条件属于转义序列
	jsInLineComment         // // 行注释
	jsInBlockComment        // /* */ 块注释
)

// Strip 移除 JSONC 文本中的注释，返回可直接交给 encoding/json 解析的字节。
//
// 行为细则：
//   - // 行注释：丢弃到行尾，但保留换行符本身（否则注释后的首个 token 会与上一行粘连）；
//   - /* */ 块注释：整体丢弃（可跨行），替换为单个空格，避免 [1/*x*/2] 这类粘连；
//   - 字符串字面量内部的 // 与 /* 一律原样保留（URL、正则、含斜杠的中文文案都不受影响）；
//   - 支持 \" 转义引号：转义引号不结束字符串，因此 "a\"//b" 后的 // 仍在字符串内；
//   - 未闭合的注释或字符串不报错，交由后续 json.Unmarshal 暴露真实的语法错误。
//
// Strip 是纯函数，不修改入参。
func Strip(data []byte) []byte {
	out := make([]byte, 0, len(data))
	state := jsOutside

	for i := 0; i < len(data); i++ {
		c := data[i]

		switch state {
		case jsOutside:
			switch c {
			case '"':
				state = jsInString
				out = append(out, c)
			case '/':
				// 只有 / 后面紧跟 / 或 * 才是注释，单独的 / 原样保留（JSON 里本就非法，
				// 保留后由 Unmarshal 报错，比在这里静默吞掉更好定位）。
				if i+1 < len(data) {
					switch data[i+1] {
					case '/':
						state = jsInLineComment
						i++
					case '*':
						state = jsInBlockComment
						i++
					default:
						out = append(out, c)
					}
				} else {
					out = append(out, c)
				}
			default:
				out = append(out, c)
			}

		case jsInString:
			out = append(out, c)
			switch c {
			case '\\':
				state = jsInEscape
			case '"':
				state = jsOutside
			}

		case jsInEscape:
			// 转义序列的第二个字节（" \ / u 等）无条件吃掉，状态回到字符串内。
			out = append(out, c)
			state = jsInString

		case jsInLineComment:
			if c == '\n' {
				state = jsOutside
				out = append(out, '\n')
			} else if c == '\r' {
				// 兼容 CRLF：吃掉配对的 \n，统一输出一个 \n。
				if i+1 < len(data) && data[i+1] == '\n' {
					i++
				}
				state = jsOutside
				out = append(out, '\n')
			}

		case jsInBlockComment:
			if c == '*' && i+1 < len(data) && data[i+1] == '/' {
				i++
				state = jsOutside
				out = append(out, ' ')
			} else if c == '\n' {
				// 保留块注释自身的换行，方便剥离后的内容仍可读、行号大致对齐。
				out = append(out, c)
			}
		}
	}

	return out
}

// Unmarshal 剥离注释后解析 JSONC 文本。
// v 通常是 *map[string]any；需要保留数字字面量精度时请改用 Strip + json.Decoder.UseNumber()
// （registry.go 装载声明文件走的就是这条路径，避免 15 这类整数被降级成 float64）。
func Unmarshal(data []byte, v any) error {
	return json.Unmarshal(Strip(data), v)
}
