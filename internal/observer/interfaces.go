package observer

import "gorm.io/gorm"

// ModelObserver 模型观察者基础接口
// 每个 Observer 必须声明自己观察的模型（通过表名），用于事件路由
type ModelObserver interface {
	ObserveModel() string
}

// BeforeCreateObserver 创建前观察者接口
// Before 系列返回 error 可以中断操作（回滚事务）
type BeforeCreateObserver interface {
	BeforeCreate(tx *gorm.DB, model interface{}) error
}

// AfterCreateObserver 创建后观察者接口
type AfterCreateObserver interface {
	AfterCreate(tx *gorm.DB, model interface{}) error
}

// BeforeUpdateObserver 更新前观察者接口
type BeforeUpdateObserver interface {
	BeforeUpdate(tx *gorm.DB, model interface{}) error
}

// AfterUpdateObserver 更新后观察者接口
type AfterUpdateObserver interface {
	AfterUpdate(tx *gorm.DB, model interface{}) error
}

// BeforeDeleteObserver 删除前观察者接口
type BeforeDeleteObserver interface {
	BeforeDelete(tx *gorm.DB, model interface{}) error
}

// AfterDeleteObserver 删除后观察者接口
type AfterDeleteObserver interface {
	AfterDelete(tx *gorm.DB, model interface{}) error
}

// AfterFindObserver 查询后观察者接口
type AfterFindObserver interface {
	AfterFind(tx *gorm.DB, model interface{}) error
}
