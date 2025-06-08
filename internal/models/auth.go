package models

import (
	"gorm.io/gorm"
	"shop/pkg/global"
	"time"
)

const (
	IsUsedYES = 1  // 启用
	IsUsedNo  = -1 // 禁用
)

// Auth 开发者接口授权表

type Auth struct {
	Id                int64  `gorm:"primary_key" json:"id"`
	BusinessKey       string `json:"business_key" gorm:"UNIQUE:uniq_business_key"` // 调用方key
	BusinessSecret    string `json:"business_secret"`                              // 调用方secret
	BusinessDeveloper string `json:"business_developer"`                           // 调用方对接人
	Remark            string `json:"remark"`                                       // 备注
	IsUsed            int32  `json:"is_used"`                                      // 是否启用 1:是  -1:否
	//IsDeleted         soft_delete.DeletedAt `json:"is_deleted" gorm:"softDelete:flag;softDelete"`
	IsDeleted   int32     `gorm:"not null;default:-1;index" json:"is_deleted"` // 修改为int32并设置默认值
	CreatedUser string    `json:"created_user"`                                // 创建人
	UpdatedUser string    `json:"updated_user"`                                // 更新人
	UpdateAt    time.Time `json:"update_at"`
	CreateAt    time.Time `json:"create_at"`
}

// TableName 表名称
func (*Auth) TableName() string {
	return "auth"
}

// 建表
func CreateAuthTable() error {
	return global.Db.AutoMigrate(&Auth{})
}

// get all
func GetAllBusiness(pageNUm int, pageSize int, maps interface{}) (int64, []Auth) {
	var (
		total int64
		data  []Auth
	)

	global.Db.Model(&Auth{}).Where(maps).Scopes(NotDeleted).Count(&total)
	global.Db.Model(&Auth{}).Where(maps).Scopes(NotDeleted).Offset(pageNUm).Limit(pageSize).Order("id desc").Find(&data)

	return total, data
}

func GetBusinessByKey(ak string) (auth *Auth, err error) {
	err = global.Db.Model(&Auth{}).Scopes(NotDeleted).Where("business_key = ?", ak).First(&auth).Error
	return
}

func AddBusiness(a *Auth) error {
	var err error
	if err = global.Db.Select("business_key", "business_secret", "business_developer", "remark", "is_used", "created_user", "updated_user", "is_del").Create(a).Error; err != nil {
		return err
	}

	return err
}

func UpdateByID(id int64, a *Auth) error {
	var err error
	err = global.Db.Model(&Auth{}).Where("id = ?", id).Updates(a).Error
	if err != nil {
		return err
	}

	return err
}

func DelByIDs(ids []int64) error {
	var err error
	err = global.Db.Where("id in (?)", ids).Delete(&Auth{}).Error
	if err != nil {
		return err
	}

	return err
}

func NotDeleted(db *gorm.DB) *gorm.DB {
	return db.Where("is_deleted = ?", -1)
}
