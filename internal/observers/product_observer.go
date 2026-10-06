package observers

import (
	"fmt"
	"log"

	"shop/internal/models"
	"shop/internal/worker"
	"shop/pkg/global"

	"gorm.io/gorm"
)

// ProductObserver 商品模型观察者
type ProductObserver struct{}

// ObserveModel 返回观察的模型表名
func (o *ProductObserver) ObserveModel() string {
	return "store_product"
}

// AfterCreate 创建后回调
func (o *ProductObserver) AfterCreate(tx *gorm.DB, model interface{}) error {
	product, ok := model.(*models.StoreProduct)
	if !ok {
		return nil
	}

	if global.LOG != nil {
		global.LOG.Infof("[ProductObserver] 商品创建: %s (ID: %d)", product.StoreName, product.Id)
	} else {
		log.Printf("[ProductObserver] 商品创建: %s (ID: %d)", product.StoreName, product.Id)
	}

	// 写入 Outbox：商品创建事件
	return worker.WriteOutbox(
		tx,
		fmt.Sprintf("%d", product.Id),
		"product.created",
		worker.TopicProductEvents,
		fmt.Sprintf("%d", product.Id),
		product,
	)
}

// AfterUpdate 更新后回调
func (o *ProductObserver) AfterUpdate(tx *gorm.DB, model interface{}) error {
	product, ok := model.(*models.StoreProduct)
	if !ok {
		return nil
	}

	if global.LOG != nil {
		global.LOG.Infof("[ProductObserver] 商品更新: %s (ID: %d)", product.StoreName, product.Id)
	} else {
		log.Printf("[ProductObserver] 商品更新: %s (ID: %d)", product.StoreName, product.Id)
	}

	// 写入 Outbox：商品更新事件
	return worker.WriteOutbox(
		tx,
		fmt.Sprintf("%d", product.Id),
		"product.updated",
		worker.TopicProductEvents,
		fmt.Sprintf("%d", product.Id),
		product,
	)
}

// AfterDelete 删除后回调
func (o *ProductObserver) AfterDelete(tx *gorm.DB, model interface{}) error {
	product, ok := model.(*models.StoreProduct)
	if !ok {
		return nil
	}

	if global.LOG != nil {
		global.LOG.Infof("[ProductObserver] 商品删除: %s (ID: %d)", product.StoreName, product.Id)
	} else {
		log.Printf("[ProductObserver] 商品删除: %s (ID: %d)", product.StoreName, product.Id)
	}

	// 写入 Outbox：商品删除事件
	return worker.WriteOutbox(
		tx,
		fmt.Sprintf("%d", product.Id),
		"product.deleted",
		worker.TopicProductEvents,
		fmt.Sprintf("%d", product.Id),
		product,
	)
}

