package observers

import (
	"log"

	"shop/internal/models"
	"shop/internal/worker"
	"shop/pkg/global"

	"gorm.io/gorm"
)

// OrderObserver 订单模型观察者
type OrderObserver struct{}

// ObserveModel 返回观察的模型表名
func (o *OrderObserver) ObserveModel() string {
	return "store_order"
}

// AfterCreate 创建后回调
func (o *OrderObserver) AfterCreate(tx *gorm.DB, model interface{}) error {
	order, ok := model.(*models.StoreOrder)
	if !ok {
		return nil
	}

	if global.LOG != nil {
		global.LOG.Infof("[OrderObserver] 订单创建: %s, 用户: %d", order.OrderId, order.Uid)
	} else {
		log.Printf("[OrderObserver] 订单创建: %s, 用户: %d", order.OrderId, order.Uid)
	}

	// 写入 Outbox：订单创建事件
	return worker.WriteOutbox(
		tx,
		order.OrderId,
		"order.created",
		worker.TopicOrderEvents,
		order.OrderId,
		order,
	)
}

// AfterUpdate 更新后回调
func (o *OrderObserver) AfterUpdate(tx *gorm.DB, model interface{}) error {
	order, ok := model.(*models.StoreOrder)
	if !ok {
		return nil
	}

	if global.LOG != nil {
		global.LOG.Infof("[OrderObserver] 订单更新: %s, 用户: %d, 状态: %d", order.OrderId, order.Uid, order.Status)
	} else {
		log.Printf("[OrderObserver] 订单更新: %s, 用户: %d, 状态: %d", order.OrderId, order.Uid, order.Status)
	}

	// 写入 Outbox：订单状态变更事件
	return worker.WriteOutbox(
		tx,
		order.OrderId,
		orderEventName(order.Status),
		worker.TopicOrderEvents,
		order.OrderId,
		order,
	)
}

// orderEventName 根据订单状态返回对应的事件类型名称
func orderEventName(status int) string {
	switch status {
	case 0:
		return "order.created"     // 待支付
	case 1:
		return "order.paid"        // 已支付
	case 2:
		return "order.shipped"     // 已发货
	case 3:
		return "order.completed"   // 已完成
	case -1:
		return "order.canceled"    // 已取消
	default:
		return "order.updated"     // 其他状态变更
	}
}
