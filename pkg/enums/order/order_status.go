package order

// 订单状态常量
const (
	StatusUnpaid    = 0 // 未支付（默认状态）
	StatusDelivered = 1 // 待收货（已发货）
	StatusReceived  = 2 // 待评价（已收货）
	StatusCompleted = 3 // 已完成
	StatusCancelled = 4 // 已取消
)
