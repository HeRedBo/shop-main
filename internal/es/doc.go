package es

// 本文件是写入 products 索引的文档结构定义，属于跨仓库协议的一部分。
//
// 与下面两处 struct 逐字段一致（字段名、类型、json tag、顺序全都相同）：
//   - product-consumer/internal/consumer/product_index.go 的 ProductIndex（增量写入方）
//   - shop-search-api/internal/repo/es/product_repo 的 ProductIndex（查询读取方）
//
// 改任何一个字段 = 跨仓库协议变更，必须三处同步 + 先 es:migrate 改索引再发版；
// 只改本仓库不会报错，但会让 consumer 写入或 api 查询静默错位。
//
// 三条禁止项（dynamic: strict + ES 默认 date format 的直接后果）：
//  1. 不得添加 desc 字段：desc 是 store_info/description/keyword 的 copy_to 汇聚目标，
//     mapping 里有声明但文档绝不能产出该 key（写进去会污染三路汇聚的倒排）；
//  2. 不得添加 omitempty：price/sales/ficti/is_* 的零值必须写入，index.sort 预置字段缺值会破坏排序段，
//     且 json.Marshal(ProductIndex{}) 的 key 集合必须恰为 17 个 tag，doc_test.go 会断言这一点；
//  3. 不得写 MarshalJSON，也不得把时间 Format("2006-01-02 15:04:05")：
//     create_time/update_time 在 mapping 中没有显式 format，走 ES 默认
//     strict_date_optional_time||epoch_millis，只有 time.Time 的 stdlib 默认序列化（RFC3339Nano）
//     能被接受；空格分隔格式会直接被拒为 date_parse_exception。

import (
	"strconv"
	"time"
)

// ProductIndex 是 shop-product 索引的单条文档，17 个字段与 mappings.properties 严格对应
// （mapping 共 18 个 key，多出的那一个就是虚拟字段 desc）。
type ProductIndex struct {
	Id          int64     `json:"id"`          // 商品 id，同时作为 ES _id（见 BulkDocID）
	StoreName   string    `json:"store_name"`  // 商品名称，ik 分词 + pinyin/single 子字段
	StoreInfo   string    `json:"store_info"`  // 商品简介，copy_to desc
	Keyword     string    `json:"keyword"`     // 关键字，copy_to desc
	CateId      int       `json:"cate_id"`     // 分类 id，keyword 类型且与 routing 同值（注意是 int）
	Price       float64   `json:"price"`       // 商品价格，mapping 为 double
	Sales       int32     `json:"sales"`       // 销量，mapping 为 long，参与 index.sort
	Ficti       int32     `json:"ficti"`       // 虚拟销量，mapping 为 long，参与 index.sort
	IsHot       int8      `json:"is_hot"`      // 是否热卖 0/1，mapping 为 keyword（不得改 bool）
	IsBenefit   int8      `json:"is_benefit"`  // 是否优惠 0/1，keyword
	IsBest      int8      `json:"is_best"`     // 是否精品 0/1，keyword
	IsNew       int8      `json:"is_new"`      // 是否新品 0/1，keyword
	Description string    `json:"description"` // 产品描述，已去 HTML 标签并按 rune 截断
	IsPostage   int8      `json:"is_postage"`  // 是否包邮 0/1，keyword
	IsGood      int8      `json:"is_good"`     // 是否优品推荐 0/1，keyword
	CreateTime  time.Time `json:"create_time"` // 序列化为 RFC3339，禁止自定义格式
	UpdateTime  time.Time `json:"update_time"` // 序列化为 RFC3339，禁止自定义格式
}

// MaxDescriptionRunes 是 description 进索引前的截断长度（按 rune，不是字节）。
// 超长富文本进倒排的代价远大于检索收益；_source 已 excludes 该字段，截断不影响回查
// （回查一律看 DB）。
const MaxDescriptionRunes = 2000

// BulkDocID 返回写 ES 用的文档 _id。
// 必须与 product-consumer 的 strconv.FormatInt(id, 10) 逐字一致，否则同一条商品在
// 全量灌入与增量写入时会落到不同 shard 的两份文档，_id 幂等失效。
func (d ProductIndex) BulkDocID() string {
	return strconv.FormatInt(d.Id, 10)
}

// BulkRouting 返回写 ES 用的 routing 值，取 cate_id。
// 同样必须与 consumer 逐字一致：这里固定 strconv.Itoa，不得用 %d.0 之类写法，
// 因为 mapping 里 cate_id 是 keyword，routing 字符串必须与文档值同形。
func (d ProductIndex) BulkRouting() string {
	return strconv.Itoa(d.CateId)
}
