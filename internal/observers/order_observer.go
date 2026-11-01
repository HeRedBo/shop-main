package observers

import (
	"log"

	"shop/internal/models"
	"shop/internal/worker"
	"shop/pkg/global"

	"gorm.io/gorm"
)

// OrderObserver 订单模型观察者
// 只处理 created / updated / deleted 三种事件
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
		global.LOG.Infof("[OrderObserver] 订单更新: %s, 用户: %d", order.OrderId, order.Uid)
	} else {
		log.Printf("[OrderObserver] 订单更新: %s, 用户: %d", order.OrderId, order.Uid)
	}

	// 写入 Outbox：订单更新事件
	return worker.WriteOutbox(
		tx,
		order.OrderId,
		"order.updated",
		worker.TopicOrderEvents,
		order.OrderId,
		order,
	)
}

// AfterDelete 删除后回调
func (o *OrderObserver) AfterDelete(tx *gorm.DB, model interface{}) error {
	order, ok := model.(*models.StoreOrder)
	if !ok {
		return nil
	}

	if global.LOG != nil {
		global.LOG.Infof("[OrderObserver] 订单删除: %s, 用户: %d", order.OrderId, order.Uid)
	} else {
		log.Printf("[OrderObserver] 订单删除: %s, 用户: %d", order.OrderId, order.Uid)
	}

	return worker.WriteOutbox(
		tx,
		order.OrderId,
		"order.deleted",
		worker.TopicOrderEvents,
		order.OrderId,
		order,
	)
}

