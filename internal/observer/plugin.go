package observer

import (
	"fmt"
	"log"

	"gorm.io/gorm"
)

// ObserverPlugin 实现 gorm.Plugin 接口
// 桥接 GORM Callback 和 Observer 注册中心，将数据库事件分发到对应的观察者
type ObserverPlugin struct {
	Registry *ObserverRegistry
}

// NewPlugin 创建 Observer 插件
func NewPlugin(registry *ObserverRegistry) *ObserverPlugin {
	return &ObserverPlugin{Registry: registry}
}

// Name 返回插件名称，实现 gorm.Plugin 接口
func (p *ObserverPlugin) Name() string {
	return "observer"
}

// Initialize 初始化插件，注册 GORM 回调，实现 gorm.Plugin 接口
func (p *ObserverPlugin) Initialize(db *gorm.DB) error {
	// ========== Create 回调 ==========
	db.Callback().Create().Before("gorm:create").Register("observer:before_create", func(tx *gorm.DB) {
		if tx.Error != nil {
			return
		}
		tableName := tx.Statement.Table
		for _, obs := range p.Registry.GetObservers(tableName) {
			if o, ok := obs.(BeforeCreateObserver); ok {
				if err := o.BeforeCreate(tx, tx.Statement.Model); err != nil {
					_ = tx.AddError(err)
					return
				}
			}
		}
	})

	db.Callback().Create().After("gorm:create").Register("observer:after_create", func(tx *gorm.DB) {
		if tx.Error != nil {
			return
		}
		tableName := tx.Statement.Table
		for _, obs := range p.Registry.GetObservers(tableName) {
			if o, ok := obs.(AfterCreateObserver); ok {
				if err := o.AfterCreate(tx, tx.Statement.Model); err != nil {
					_ = tx.AddError(err)
					return
				}
			}
		}
	})

	// ========== Compute Dirty (Before Update) ==========
	// 在 observer:before_update 之前执行，先计算字段变更，再触发 Before 观察者
	db.Callback().Update().Before("gorm:update").Register("observer:compute_dirty", func(tx *gorm.DB) {
		if tx.Error != nil {
			return
		}
		tableName := tx.Statement.Table
		if !p.Registry.HasObservers(tableName) {
			return // 没有观察者，跳过计算
		}

		// 尝试获取旧数据
		oldModel, err := fetchOldModel(tx)
		if err != nil {
			var destType, modelType string
			if tx.Statement.Dest != nil {
				destType = fmt.Sprintf("%T", tx.Statement.Dest)
			} else {
				destType = "nil"
			}
			if tx.Statement.Model != nil {
				modelType = fmt.Sprintf("%T", tx.Statement.Model)
			} else {
				modelType = "nil"
			}
			log.Printf("[observer:compute_dirty] fetchOldModel failed: table=%s, err=%v, Dest=%s, Model=%s",
				tableName, err, destType, modelType)
			return // 无法获取旧数据，不设置 dirty
		}

		// 计算 DirtyFields
		dirty := ComputeDirtyFields(tx, oldModel)
		if dirty != nil && dirty.HasChanges() {
			tx.Set("observer:dirty", dirty)
		}
	})

	// ========== Update 回调 ==========
	db.Callback().Update().Before("gorm:update").Register("observer:before_update", func(tx *gorm.DB) {
		if tx.Error != nil {
			return
		}
		tableName := tx.Statement.Table
		for _, obs := range p.Registry.GetObservers(tableName) {
			if o, ok := obs.(BeforeUpdateObserver); ok {
				if err := o.BeforeUpdate(tx, tx.Statement.Model); err != nil {
					_ = tx.AddError(err)
					return
				}
			}
		}
	})

	db.Callback().Update().After("gorm:update").Register("observer:after_update", func(tx *gorm.DB) {
		if tx.Error != nil {
			return
		}
		tableName := tx.Statement.Table
		for _, obs := range p.Registry.GetObservers(tableName) {
			if o, ok := obs.(AfterUpdateObserver); ok {
				if err := o.AfterUpdate(tx, tx.Statement.Model); err != nil {
					_ = tx.AddError(err)
					return
				}
			}
		}
	})

	// ========== Delete 回调 ==========
	db.Callback().Delete().Before("gorm:delete").Register("observer:before_delete", func(tx *gorm.DB) {
		if tx.Error != nil {
			return
		}
		tableName := tx.Statement.Table
		for _, obs := range p.Registry.GetObservers(tableName) {
			if o, ok := obs.(BeforeDeleteObserver); ok {
				if err := o.BeforeDelete(tx, tx.Statement.Model); err != nil {
					_ = tx.AddError(err)
					return
				}
			}
		}
	})

	db.Callback().Delete().After("gorm:delete").Register("observer:after_delete", func(tx *gorm.DB) {
		if tx.Error != nil {
			return
		}
		tableName := tx.Statement.Table
		for _, obs := range p.Registry.GetObservers(tableName) {
			if o, ok := obs.(AfterDeleteObserver); ok {
				if err := o.AfterDelete(tx, tx.Statement.Model); err != nil {
					_ = tx.AddError(err)
					return
				}
			}
		}
	})

	// ========== Query 回调 ==========
	db.Callback().Query().After("gorm:query").Register("observer:after_find", func(tx *gorm.DB) {
		if tx.Error != nil {
			return
		}
		tableName := tx.Statement.Table
		for _, obs := range p.Registry.GetObservers(tableName) {
			if o, ok := obs.(AfterFindObserver); ok {
				if err := o.AfterFind(tx, tx.Statement.Model); err != nil {
					_ = tx.AddError(err)
					return
				}
			}
		}
	})

	return nil
}
