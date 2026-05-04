package observers

import (
	"log"

	"shop/internal/models"
	"shop/pkg/global"

	"gorm.io/gorm"
)

// UserObserver 用户模型观察者
type UserObserver struct{}

// ObserveModel 返回观察的模型表名
func (o *UserObserver) ObserveModel() string {
	return "user"
}

// AfterCreate 创建后回调
func (o *UserObserver) AfterCreate(tx *gorm.DB, model interface{}) error {
	user, ok := model.(*models.ShopUser)
	if !ok {
		return nil
	}

	name := user.Username
	if name == "" {
		name = user.Nickname
	}

	if global.LOG != nil {
		global.LOG.Infof("[UserObserver] 用户注册: %s (ID: %d)", name, user.Id)
	} else {
		log.Printf("[UserObserver] 用户注册: %s (ID: %d)", name, user.Id)
	}

	return nil
}

// AfterUpdate 更新后回调
func (o *UserObserver) AfterUpdate(tx *gorm.DB, model interface{}) error {
	user, ok := model.(*models.ShopUser)
	if !ok {
		return nil
	}

	name := user.Username
	if name == "" {
		name = user.Nickname
	}

	if global.LOG != nil {
		global.LOG.Infof("[UserObserver] 用户更新: %s (ID: %d)", name, user.Id)
	} else {
		log.Printf("[UserObserver] 用户更新: %s (ID: %d)", name, user.Id)
	}

	return nil
}
