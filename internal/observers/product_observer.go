package observers

import (
	"log"

	"shop/internal/models"
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

	// TODO: 同步商品数据到 Elasticsearch
	// TODO: 发送商品上架通知

	return nil
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

	// TODO: 更新 Elasticsearch 索引

	return nil
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

	// TODO: 清理 Elasticsearch 索引

	return nil
}

