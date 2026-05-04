package observers

import (
	"log"

	"shop/internal/models"
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

	// TODO: 记录订单初始状态
	// TODO: 发送订单创建通知
	// TODO: 启动订单超时取消任务

	return nil
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

	// TODO: 处理订单状态变更（如支付成功、发货、完成、取消等）

	return nil
}
