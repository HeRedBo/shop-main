package es

// JSONC 剥离器的验收重点：注释形态再多，剥离后必须是合法 JSON，且字符串值里的
// 斜杠一个字符都不能丢（否则 mapping 里的 analyzer 名、URL、正则会被静默改写）。

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func mustJSONEq(t *testing.T, got []byte, want string) {
	t.Helper()

	var g any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("剥离后不是合法 JSON: %v\n输出: %s", err, got)
	}
	var w any
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("用例期望值本身不是合法 JSON: %v", err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("解析结果与期望不等\n got: %v\nwant: %v", g, w)
	}
}

// TestJSONCStrip 覆盖方案第十节要求的 6 组用例。
func TestJSONCStrip(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name: "行注释",
			input: `{
  // 顶层行注释
  "a": 1, // 行尾注释
  "b": 2
}`,
			want: `{"a":1,"b":2}`,
		},
		{
			name: "块注释含跨行",
			input: `{
  /* 单行块注释 */ "a": 1,
  /*
     跨行块注释
     第二行
  */
  "b": [1, 2, 3]
}`,
			want: `{"a":1,"b":[1,2,3]}`,
		},
		{
			name:  "字符串内的双斜杠不能被误删（URL）",
			input: `{"hosts": ["http://127.0.0.1:9200"], "p": "a//b", "q": "https://x.y/z/*w"}`,
			want:  `{"hosts":["http://127.0.0.1:9200"],"p":"a//b","q":"https://x.y/z/*w"}`,
		},
		{
			name:  "转义引号后的双斜杠仍在字符串内",
			input: `{"a": "he said \"hi\" // not a comment", "b": 2} // 真注释`,
			want:  `{"a":"he said \"hi\" // not a comment","b":2}`,
		},
		{
			name: "中文注释与中文值",
			input: `{
  // 分片数：静态项，变更必须蓝绿重建
  "number_of_shards": 15, /* 副本数：动态项 */
  "name": "商品索引",
  "note": "描述里的中文 // 斜杠不能丢"
}`,
			want: `{"number_of_shards":15,"name":"商品索引","note":"描述里的中文 // 斜杠不能丢"}`,
		},
		{
			name:  "注释残留空行不影响解析",
			input: "{\n\n  // 注释一\n\n\n  \"a\": 1\n\n  // 注释二\n  , \"b\": \"x\"\n\n}\n",
			want:  `{"a":1,"b":"x"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mustJSONEq(t, Strip([]byte(tc.input)), tc.want)
		})
	}
}

// TestJSONCStripEdgeCases 覆盖状态机的边界：注释符出现在结构位置、孤立斜杠、CRLF、
// 块注释紧邻 token 时要用空格隔开。
func TestJSONCStripEdgeCases(t *testing.T) {
	t.Run("块注释紧贴 token 时替换为空格", func(t *testing.T) {
		got := string(Strip([]byte(`{"a":1,"b":/*c*/2}`)))
		mustJSONEq(t, []byte(got), `{"a":1,"b":2}`)
	})

	t.Run("数组元素间的块注释不粘连", func(t *testing.T) {
		// 若块注释被删成空串，"x""y" 会粘连成非法或错值
		mustJSONEq(t, Strip([]byte(`["x", /*c*/ "y"]`)), `["x","y"]`)
	})

	t.Run("未闭合块注释不 panic", func(t *testing.T) {
		in := `{"a": 1} /* 尾部注释未闭合`
		out := Strip([]byte(in))
		if strings.Contains(string(out), "尾部注释未闭合") {
			t.Fatalf("未闭合块注释应被吞掉，实际输出: %s", out)
		}
	})

	t.Run("孤立斜杠原样保留并由 json 报错", func(t *testing.T) {
		out := string(Strip([]byte(`{"a": /}`)))
		if !strings.Contains(out, "/") {
			t.Fatalf("单个 / 不应被当成注释吞掉: %s", out)
		}
		var v any
		if err := json.Unmarshal([]byte(out), &v); err == nil {
			t.Fatal("期望非法 JSON 报错")
		}
	})

	t.Run("CRLF 行注释保留换行", func(t *testing.T) {
		in := "{\r\n  // 注释\r\n  \"a\": 1\r\n}"
		out := string(Strip([]byte(in)))
		mustJSONEq(t, []byte(out), `{"a":1}`)
		if !strings.Contains(out, "\n") {
			t.Fatalf("行注释必须保留换行以免下一行 token 粘连: %q", out)
		}
	})

	t.Run("不修改入参", func(t *testing.T) {
		in := []byte(`{"a": 1} // 注释`)
		before := append([]byte(nil), in...)
		_ = Strip(in)
		if !reflect.DeepEqual(in, before) {
			t.Fatalf("Strip 修改了入参: %s", in)
		}
	})
}

// TestJSONCUnmarshal 校验便捷入口 Unmarshal 与 Strip+json.Unmarshal 等价。
func TestJSONCUnmarshal(t *testing.T) {
	var m map[string]any
	if err := Unmarshal([]byte(`{ // 注释
		"a": {"b": [1, 2]},
		/* 块 */ "c": "http://x//y"
	}`), &m); err != nil {
		t.Fatalf("Unmarshal 失败: %v", err)
	}
	if got := m["c"]; got != "http://x//y" {
		t.Fatalf("字符串里的 // 被破坏: %v", got)
	}
	m2 := map[string]any{
		"a": map[string]any{"b": []any{float64(1), float64(2)}},
		"c": "http://x//y",
	}
	if !reflect.DeepEqual(m, m2) {
		t.Fatalf("Unmarshal 结果不符: %v", m)
	}
}

// TestJSONCStripsRealDefinitionFiles 确认两份事实源里的注释规模不会破坏解析，
// 且 Strip 的幂等性：对已剥离内容再剥一次结果不变（无注释时）。
func TestJSONCStripsRealDefinitionFiles(t *testing.T) {
	defs, err := LoadDefinitions("")
	if err != nil {
		t.Fatalf("LoadDefinitions 失败: %v", err)
	}
	for _, d := range defs {
		data, err := FS.ReadFile(d.Source())
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", d.Source(), err)
		}
		stripped := Strip(data)
		var body map[string]any
		if err := json.Unmarshal(stripped, &body); err != nil {
			t.Fatalf("%s 剥离注释后不是合法 JSON: %v", d.Source(), err)
		}
		if !json.Valid(stripped) {
			t.Fatalf("%s 剥离结果未通过 json.Valid", d.Source())
		}
		// 二次剥离应完全幂等
		if again := string(Strip(stripped)); again != string(stripped) {
			t.Fatalf("%s 二次剥离不幂等", d.Source())
		}
	}
}
