package worker

// OrderEventPayload 订单事件统一消息体
// 用于延迟队列（超时取消、自动确认收货）及普通订单事件
// 统一结构避免业务层频繁类型断言或结构转换
type OrderEventPayload struct {
	OrderId   string `json:"order_id"`
	UserId    int64  `json:"user_id"`
	OrderType string `json:"order_type"` // "normal" / "seckill" / "bargain" / "combination" 等
	Action    string `json:"action"`     // "timeout_cancel" / "auto_confirm" / "created" / "paid" 等
}
