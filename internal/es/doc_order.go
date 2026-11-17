package es

// 本文件是写入 shop-order 索引的文档结构与「DB 行 -> ES 文档」装配入口，
// 属于跨仓库协议的一部分，与下面两处保持逐字段一致：
//   - order-consumer/consumer/order_index.go 的 OrderIndex（增量写入方，12 字段）；
//   - shop-search-api/internal/service/order_service 读取侧对同名字段的依赖。
//
// 本仓库的 OrderIndex 是外部 12 字段的 **超集**：额外补一个 pay_type_name —— 它是
// orders.jsonc mappings 声明的第 13 个字段（dynamic: strict 要求文档 key 集合与之严格
// 互等），而 order-consumer 的 OrderIndex 没有该字段、由本仓库灌数时用 PayTypeName 现算。
// 除 pay_type_name 外，其余 12 字段的字段名/类型/json tag/顺序均与 order_index.go 逐字对齐。
//
// 三条禁止项（dynamic: strict + ES 默认 date format 的直接后果，与 doc.go 同源）：
//  1. 不得添加 omitempty：names/product_ids 为空也必须是 []（非 null），状态/类型零值
//     必须显式写入，json.Marshal(OrderIndex{}) 的 key 集合必须恰为 13 个 tag；
//  2. 不得写 MarshalJSON，也不得把时间 Format("2006-01-02 15:04:05")：pay_time /
//     create_time / update_time 在 mapping 中【没有显式 format】，走 ES 默认
//     strict_date_optional_time||epoch_millis，只有 time.Time 的 stdlib 默认序列化
//     （RFC3339Nano）能被接受；空格分隔格式会被拒为 date_parse_exception。
//     这也是 orders 与 products 绝不能共用一个时间格式化函数的原因；
//  3. _id / routing 口径（见 OrderDocID/OrderRouting）必须与 order-consumer 逐字一致，
//     否则同一条订单在全量灌入与增量写入时落到不同 shard 的两份文档，幂等失效。

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"shop/internal/models"
	"shop/internal/service/order_service"
)

// orderIDSuffixLen 是 order_id_suffix 取的尾串长度（按字节，不是 rune）。
// 与 order-consumer/consumer/order_consumer.go 的 `orderMsg.OrderId[len-4:]` 逐字一致：
// 只有订单号长度 >= 4 才置尾串，短于 4 位留空。改这个数就等于改跨仓库协议。
const orderIDSuffixLen = 4

// OrderIndex 是 shop-order 索引的单条文档，13 个字段与 orders.jsonc mappings.properties
// 严格互等（比 order-consumer 的 12 字段多出 pay_type_name 一个）。
type OrderIndex struct {
	OrderId       string    `json:"order_id"`        // 字符串订单号，同时作为 ES _id（见 OrderDocID，注意不是数字主键 id）
	OrderIdSuffix string    `json:"order_id_suffix"` // 订单号后 4 位，配合按尾号搜索；短于 4 位为空
	Names         []string  `json:"names"`           // 单内商品名数组（来自 cart_info.product_info.store_name）
	ProductIds    []int64   `json:"product_ids"`     // 单内商品 id 数组（来自 cart_info.product_info.id）
	Uid           int64     `json:"uid"`             // 下单用户 id，同时是 routing 值（见 OrderRouting）
	PayTime       time.Time `json:"pay_time"`        // 序列化为 RFC3339，禁止自定义格式
	PayType       string    `json:"pay_type"`        // 支付方式原始值（weixin/yue/integral...）
	PayTypeName   string    `json:"pay_type_name"`   // 支付方式中文名，本仓库灌数时现算（order-consumer 无此字段）
	RefundStatus  int       `json:"refund_status"`   // 0 未退款 / 1 申请中 / 2 已退款，mapping 为 keyword
	ShippingType  int       `json:"shipping_type"`   // 配送方式，keyword
	OrderStatus   int       `json:"order_status"`    // 派生聚合状态码，keyword（复用 order_service.OrderStatus）
	CreateTime    time.Time `json:"create_time"`     // 序列化为 RFC3339
	UpdateTime    time.Time `json:"update_time"`     // 序列化为 RFC3339
}

// orderCartPayload 是 store_order_cart_info.cart_info 列（一段 JSON）里装配关心的最小子集。
//
// 只解析 product_info.id / product_info.store_name —— 这是任务钦定的聚合口径，
// 顶层虽有 product_id 但不用它（product_info.id 才是商品真实 id）。刻意不复用
// cart_service/vo.Cart：那会把整棵商品富对象拉进灌数热路径，且它的字段协议比这里宽。
type orderCartPayload struct {
	ProductInfo struct {
		Id        int64  `json:"id"`
		StoreName string `json:"store_name"`
	} `json:"product_info"`
}

// OrderDocID 返回写 ES 用的文档 _id：订单号字符串本身（不是数字主键 id）。
// 必须与 order-consumer/consumer/order_consumer.go:55 的 BulkCreate(index, orderIndex.OrderId, ...)
// 逐字一致，否则全量与增量写入落到不同文档。
func (d OrderIndex) OrderDocID() string { return d.OrderId }

// OrderRouting 返回写 ES 用的 routing：strconv.FormatInt(uid, 10)。
// 必须与 order_consumer.go:55 的 strutil.Int64ToString(uid)（其实现即 strconv.FormatInt）
// 以及读侧 shop-search-api 的 routing 逐字一致，否则按 uid 查询路由到错误分片、命中不到。
func (d OrderIndex) OrderRouting() string { return strconv.FormatInt(d.Uid, 10) }

// BuildOrderDoc 将一行 store_order 连同其 cart_info 原始串装配为 OrderIndex。
//
// 第二返回值 false 表示该行不应存在于索引中（调用方据此决定 index 还是 delete）：
//   - o 为 nil；
//   - o.IsDel != 0（GORM 软删标记，soft_delete.DeletedAt，0 才是活单）；
//   - o.OrderId == ""（空订单号：既无法定 _id 也无法幂等回查/删除，直接判不合格）。
//
// cartInfos 是该订单在 store_order_cart_info 里的全部 cart_info 列（一单可多行 = 1:N）。
// 解析对畸形/空输入具备容错：单条解析失败只跳过该条，绝不 panic、绝不中断整单。
//
// order_status / pay_type_name 复用 order_service 的导出纯函数（全库唯一实现，不抄第二份）；
// order_id_suffix 按 order-consumer 口径取末 4 字节。
func BuildOrderDoc(o *models.StoreOrder, cartInfos []string) (OrderIndex, bool) {
	var zero OrderIndex
	if o == nil || o.IsDel != 0 || o.OrderId == "" {
		return zero, false
	}

	names, productIDs := aggregateOrderCart(cartInfos)

	di := OrderIndex{
		OrderId:      o.OrderId,
		Uid:          o.Uid,
		Names:        names,
		ProductIds:   productIDs,
		PayTime:      o.PayTime,
		PayType:      o.PayType,
		PayTypeName:  order_service.PayTypeName(o.PayType, o.Paid),
		RefundStatus: o.RefundStatus,
		ShippingType: o.ShippingType,
		OrderStatus:  order_service.OrderStatus(o.Paid, o.Status, o.RefundStatus),
		CreateTime:   o.CreateTime,
		UpdateTime:   o.UpdateTime,
	}
	// 尾串按字节切，与 order-consumer 完全一致：短于 4 位不置（留空串）。
	if len(o.OrderId) >= orderIDSuffixLen {
		di.OrderIdSuffix = o.OrderId[len(o.OrderId)-orderIDSuffixLen:]
	}
	return di, true
}

// aggregateOrderCart 把一批 cart_info 原始 JSON 聚合成 names / product_ids 两个等长数组。
//
// 容错契约（对应验收「正常 / 空 / 畸形三种输入不 panic 且结果正确」）：
//   - 空串 / 全空白：跳过；
//   - 非法 JSON：跳过（不 panic，不中断其余条目）；
//   - 解析成功但无商品信息（id==0 且 store_name=="")：跳过，不往数组里灌空条目；
//   - 返回的两个切片恒为非 nil（可能为空），配合无 omitempty 序列化出 []（而非 null）。
func aggregateOrderCart(cartInfos []string) ([]string, []int64) {
	names := make([]string, 0, len(cartInfos))
	ids := make([]int64, 0, len(cartInfos))
	for _, raw := range cartInfos {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		var p orderCartPayload
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			continue // 畸形 cart_info：跳过该条，绝不 panic
		}
		if p.ProductInfo.Id == 0 && p.ProductInfo.StoreName == "" {
			continue // 无商品信息的空条目不计入
		}
		names = append(names, p.ProductInfo.StoreName)
		ids = append(ids, p.ProductInfo.Id)
	}
	return names, ids
}
