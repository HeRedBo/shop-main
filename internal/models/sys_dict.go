package models

import "shop/pkg/global"

type SysDict struct {
	Name   string `json:"name" valid:"Required;"`
	Remark string `json:"remark" valid:"Required;"`
	BaseModel
}

func (SysDict) TableName() string {
	return "sys_dict"
}

// GetAllDict get all
func GetAllDict(pageNUm int, pageSize int, maps interface{}) (int64, []SysDict) {
	var (
		total int64
		dicts []SysDict
	)

	global.Db.Model(&SysDict{}).Where(maps).Count(&total)
	global.Db.Where(maps).Offset(pageNUm).Limit(pageSize).Preload("Dept").Find(&dicts)

	return total, dicts
}

// AddDict last inserted Id on success.
func AddDict(m *SysDict) error {
	var err error
	if err = global.Db.Create(m).Error; err != nil {
		return err
	}

	return err
}

func UpdateByDict(m *SysDict) error {
	var err error
	err = global.Db.Save(m).Error
	if err != nil {
		return err
	}

	return err
}
