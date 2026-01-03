package models

import "shop/pkg/global"

// SystemCity 城市表
type SystemCity struct {
	ID         int64        `gorm:"column:id" json:"id"`
	CityId     int64        `gorm:"column:city_id" json:"city_id"`         // 城市id
	Level      int64        `gorm:"column:level" json:"level"`             // 省市级别
	ParentId   int64        `gorm:"column:parent_id" json:"parent_id"`     // 父级id
	AreaCode   string       `gorm:"column:area_code" json:"area_code"`     // 区号
	Name       string       `gorm:"column:name" json:"n"`                  // 名称
	MergerName string       `gorm:"column:merger_name" json:"merger_name"` // 合并名称
	Lng        string       `gorm:"column:lng" json:"lng"`                 // 经度
	Lat        string       `gorm:"column:lat" json:"lat"`                 // 纬度
	IsShow     int8         `gorm:"column:is_show" json:"is_show"`         // 是否展示
	Children   []SystemCity `gorm:"-" json:"c"`
}

// TableName 表名称
func (*SystemCity) TableName() string {
	return "system_city"
}

func GetAllSystemCity(maps interface{}) []SystemCity {
	var data []SystemCity
	global.Db.Where(maps).Find(&data)
	return RecursionCityList(data, 0)
}

// RecursionCityList 递归函数
func RecursionCityList(data []SystemCity, pid int64) []SystemCity {
	var listTree = make([]SystemCity, 0)
	for _, value := range data {
		if value.ParentId == pid {
			value.Children = RecursionCityList(data, value.CityId)
			listTree = append(listTree, value)
		}
	}
	return listTree
}

func UpdateBySystemCity(m *SystemCity) error {
	var err error
	err = global.Db.Save(m).Error
	if err != nil {
		return err
	}

	return err
}

func DelBySystemCity(ids []int64) error {
	var err error
	err = global.Db.Where("id in (?)", ids).Delete(&SystemCity{}).Error
	if err != nil {
		return err
	}

	return err
}
