package order

const (
	// Topic 订单事件对外投递的 Kafka topic，外部 order-consumer 订阅此主题。
	// 历史误值为 "shop-product"（从商品枚举复制所致），已修正为 "shop-order"。
	Topic           = "shop-order"
	OperationCreate = "create"
	OperationUpdate = "update"
	OperationDelete = "delete"
)
