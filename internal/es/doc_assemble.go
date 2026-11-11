package es

// 本文件把一行 store_product 装配成索引文档，是「DB 行 -> ES 文档」的唯一入口，
// 同时被 Phase 4 的全量同步与 Phase 5 的差量补偿复用（与 product-consumer 的单条
// 写/删分支属同一份业务规则）。
//
// 一致性要求：这里的过滤条件（is_del / is_show）与 mapping 的 17 字段口径必须与
// product-consumer 完全一致，rebuild 的对账期望值 SELECT COUNT(*) WHERE is_del=0 AND
// is_show=1 也正是这两个条件；改一处就得同步改另外两处。

import (
	"strings"

	"shop/internal/models"
)

// htmlEntities 是常见实体的单次替换表。
// 包级预分配：NewReplacer 内部构建匹配树，重复创建在热路径上是纯浪费。
var htmlEntities = strings.NewReplacer(
	"&nbsp;", " ",
	"&ensp;", " ",
	"&emsp;", " ",
	"&thinsp;", " ",
	"&quot;", `"`,
	"&apos;", "'",
	"&ldquo;", `“`,
	"&rdquo;", `”`,
	"&middot;", "·",
	"&mdash;", "—",
	"&ndash;", "–",
	"&hellip;", "…",
	"&amp;", "&",
)

// BuildIndexDoc 将一行商品装配为 ProductIndex。
//
// 第二返回值 false 表示该行不应存在于索引中（调用方据此决定 index 还是 delete），
// 条件与 product-consumer 的「只存上架」约定一致：
//   - p 为 nil；
//   - p.IsDel != 0（GORM 软删标记，soft_delete.DeletedAt）；
//   - derefInt8(p.IsShow) != 1（指针为 nil 视为未上架，宁可不写也不要写入下架商品）。
//
// 返回 false 时第一返回值是零值 ProductIndex，调用方不得使用该值。
//
// 明确不产出 desc：它是 store_info/description/keyword 的 copy_to 汇聚目标，
// 由 ES 侧自动填充，文档写入该 key 会污染三路汇聚的倒排。
func BuildIndexDoc(p *models.StoreProduct) (ProductIndex, bool) {
	var zero ProductIndex
	if p == nil {
		return zero, false
	}
	if p.IsDel != 0 || derefInt8(p.IsShow) != 1 {
		return zero, false
	}

	return ProductIndex{
		Id:        p.Id,
		StoreName: p.StoreName,
		StoreInfo: p.StoreInfo,
		Keyword:   p.Keyword,
		CateId:    p.CateId,
		Price:     p.Price,
		Sales:     int32(p.Sales),
		Ficti:     int32(p.Ficti),
		// 六项 is_* 保持 int8 数字输出：mapping 侧是 keyword，ES 会强转成 "0"/"1"；
		// 若改成 bool 会写入 "true"/"false"，存量 term 查询全部不命中。
		IsHot:       derefInt8(p.IsHot),
		IsBenefit:   derefInt8(p.IsBenefit),
		IsBest:      derefInt8(p.IsBest),
		IsNew:       derefInt8(p.IsNew),
		IsPostage:   derefInt8(p.IsPostage),
		IsGood:      derefInt8(p.IsGood),
		Description: buildDescription(p.Description),
		// 时间直接赋 time.Time 原值：json.Marshal 输出 RFC3339，匹配 ES 默认 date format。
		// 这里绝不做 Format("2006-01-02 15:04:05")，那个格式会被 ES 拒为 date_parse_exception。
		CreateTime: p.CreateTime,
		UpdateTime: p.UpdateTime,
	}, true
}

// buildDescription 清洗商品描述：去 HTML 标签 -> 解实体 -> 收敛空白 -> 按 rune 截断。
//
// 为什么 Go 侧还要去标签：mapping 里 description 的 search_analyzer 带 html_strip
// char_filter，但那只在查询期生效；写入期的超长富文本原文仍会完整进倒排。
// _source 已 excludes 掉 description，所以截断不会丢失回查能力（回查看 DB）。
func buildDescription(desc string) string {
	if desc == "" {
		return ""
	}
	s := htmlEntities.Replace(stripHTML(desc))
	s = collapseSpace(s)
	return truncateRunes(s, MaxDescriptionRunes)
}

// stripHTML 移除 <...> 标签对。
//
// 判定细节：
//   - 只有 '<' 后紧跟字母 / '/' / '!' / '?' 才认定是标签起始，避免把「价格<100」这类
//     普通文本连同后面的 '>' 一起吞掉；
//   - 标签内部识别单/双引号，<a title="x>y"> 的 '>' 不会提前结束标签；
//   - 未闭合的标签（末尾没有 '>'）会一直吞到字符串结尾，这符合富文本截断的预期。
//
// 热路径实现：单次字节扫描 + strings.Builder 预分配，不用 fmt、不用正则。
func stripHTML(s string) string {
	if strings.IndexByte(s, '<') < 0 {
		return s // 快速路径：无标签直接返回，不分配
	}

	var sb strings.Builder
	sb.Grow(len(s))

	inTag := false
	var quote byte

	for i := 0; i < len(s); i++ {
		c := s[i]

		if inTag {
			switch {
			case quote != 0:
				if c == quote {
					quote = 0
				}
			case c == '"' || c == '\'':
				quote = c
			case c == '>':
				inTag = false
			}
			continue
		}

		if c == '<' && i+1 < len(s) && isTagStart(s[i+1]) {
			inTag = true
			continue
		}
		sb.WriteByte(c)
	}

	return sb.String()
}

func isTagStart(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		return true
	case c == '/', c == '!', c == '?':
		return true
	default:
		return false
	}
}

// collapseSpace 把连续空白（空格/制表/换行）收敛为单个空格并去掉首尾空白。
// 字节级判断是安全的：UTF-8 续字节恒 >= 0x80，不会误命中 ASCII 空白。
func collapseSpace(s string) string {
	if s == "" {
		return s
	}

	var sb strings.Builder
	sb.Grow(len(s))

	pendingSpace := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f' {
			pendingSpace = true
			continue
		}
		if pendingSpace && sb.Len() > 0 {
			sb.WriteByte(' ')
		}
		pendingSpace = false
		sb.WriteByte(c)
	}
	return sb.String()
}

// truncateRunes 按 rune（不是字节）截断到 max 个字符。
// 中文描述若按字节切会截出半个 UTF-8 序列，写进 ES 就是非法字符串。
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s // 字节数不超过 max 时，rune 数必然也不超过
	}
	count := 0
	for i := range s {
		if count == max {
			return s[:i]
		}
		count++
	}
	return s
}

// derefInt8 本地实现，不 import cmd/cli 的同名私有函数（逻辑包不得反向依赖入口层）。
// nil 统一按 0 处理：store_product 的 is_* 列在库里非空，取到 nil 只可能是列裁剪没查该列，
// 此时按「否」写入比写入垃圾值更安全。
func derefInt8(v *int8) int8 {
	if v == nil {
		return 0
	}
	return *v
}
