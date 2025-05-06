package models

import "shop/pkg/global"

type StoreProductRelation struct {
	Uid       int64         `json:"uid"`
	ProductId int64         `json:"productId"`
	Type      string        `json:"type"`
	Category  string        `json:"category"`
	Product   *StoreProduct `json:"product" gorm:"foreignKey:ProductId;"`
	BaseModel
}

func (StoreProductRelation) TableName() string {
	return "store_product_relation"
}

// GetAllProductRelation get all 获取列表数据
func GetAllProductRelation(pageNUm int, pageSize int, maps interface{}) (int64, []StoreProductRelation) {
	var (
		total int64
		data  []StoreProductRelation
	)
	global.Db.Model(&StoreProductRelation{}).Where(maps).Count(&total)
	global.Db.Where(maps).Offset(pageNUm).Limit(pageSize).Preload("Product").Order("id desc").Find(&data)

	return total, data
}

// AddStoreProductRelation  创建数据
func AddStoreProductRelation(m *StoreProductRelation) error {
	var err error
	if err = global.Db.Create(m).Error; err != nil {
		return err
	}
	return err
}

// UpdateByStoreProductRelation 更新数据
func UpdateByStoreProductRelation(m *StoreProductRelation) error {
	var err error
	err = global.Db.Save(m).Error
	if err != nil {
		return err
	}

	return err
}

// DelByStoreProductRelations 删除数据
func DelByStoreProductRelations(ids []int64) error {
	var err error
	err = global.Db.Where("id in (?)", ids).Delete(&StoreProductRelation{}).Error
	if err != nil {
		return err
	}

	return err
}
