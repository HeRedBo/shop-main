package observers

import (
	"shop/internal/observer"

	"gorm.io/gorm"
)

// RegisterAll 注册所有模型观察者到 GORM DB
func RegisterAll(db *gorm.DB) error {
	registry := observer.NewRegistry()

	registry.Register(
		&ProductObserver{},
		&OrderObserver{},
		&UserObserver{},
		&SysUserObserver{},
	)

	return db.Use(observer.NewPlugin(registry))
}
