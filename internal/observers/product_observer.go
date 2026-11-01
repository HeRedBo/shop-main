package observers

import (
	"fmt"
	"log"

	"shop/internal/models"
	"shop/internal/observer"
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
// 利用 Observer 框架的 dirty tracking 机制，精确区分事件类型：
//   - is_show 由 0→1：product.on_sale（上架）
//   - is_show 由 1→0：product.un_sale（下架）
//   - 其他字段变更：product.updated（普通编辑）
func (o *ProductObserver) AfterUpdate(tx *gorm.DB, model interface{}) error {
	product, ok := model.(*models.StoreProduct)
	if !ok {
		return nil
	}

	// 通过 dirty tracking 精确判断事件类型
	eventType := o.resolveEventType(tx, product)

	if global.LOG != nil {
		global.LOG.Infof("[ProductObserver] 商品更新: %s (ID: %d), 事件类型: %s", product.StoreName, product.Id, eventType)
	} else {
		log.Printf("[ProductObserver] 商品更新: %s (ID: %d), 事件类型: %s", product.StoreName, product.Id, eventType)
	}

	// 写入 Outbox
	return worker.WriteOutbox(
		tx,
		fmt.Sprintf("%d", product.Id),
		eventType,
		worker.TopicProductEvents,
		fmt.Sprintf("%d", product.Id),
		product,
	)
}

// resolveEventType 根据 dirty tracking 判断更新事件的具体类型
// 优先使用 dirty tracking 精确判断 is_show 是否真正发生变化；
// 若 dirty 不可用（如批量更新场景），fallback 到 product.updated
func (o *ProductObserver) resolveEventType(tx *gorm.DB, product *models.StoreProduct) string {
	dirty := observer.GetDirtyFromTx(tx)
	if dirty == nil {
		return "product.updated"
	}

	if !dirty.IsDirty("IsShow") {
		return "product.updated"
	}

	// IsShow 字段发生了变化，根据新值判断上架/下架
	newVal := dirty.GetNew("IsShow")
	switch v := newVal.(type) {
	case int8:
		if v == 1 {
			return "product.on_sale"
		}
		if v == 0 {
			return "product.un_sale"
		}
	case *int8:
		if v != nil && *v == 1 {
			return "product.on_sale"
		}
		if v != nil && *v == 0 {
			return "product.un_sale"
		}
	}

	// 无法判断新值，fallback
	return "product.updated"
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

