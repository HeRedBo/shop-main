package observers

import (
	"log"
	"shop/internal/models"
	"shop/internal/observer"
	"shop/pkg/global"

	"github.com/gookit/goutil/dump"
	"gorm.io/gorm"
)

// SysUserObserver 系统用户模型观察者（用于验证 Observer 机制的数据触发情况）
type SysUserObserver struct{}

// ObserveModel 返回观察的模型表名
func (o *SysUserObserver) ObserveModel() string {
	return "sys_user"
}

// AfterCreate 创建后回调
func (o *SysUserObserver) AfterCreate(tx *gorm.DB, model interface{}) error {
	user, ok := model.(*models.SysUser)
	if !ok {
		if global.LOG != nil {
			global.LOG.Warnf("[SysUserObserver] AfterCreate 类型断言失败, model类型: %T, model值: %+v", model, model)
		} else {
			log.Printf("[SysUserObserver] AfterCreate 类型断言失败, model类型: %T, model值: %+v", model, model)
		}
		return nil
	}

	if global.LOG != nil {
		global.LOG.Infof("[SysUserObserver] 系统用户创建: ID=%d, Username=%s, NickName=%s, Phone=%s, Email=%s, DeptId=%d, JobId=%d, Enabled=%d, Sex=%s",
			user.Id, user.Username, user.NickName, user.Phone, user.Email, user.DeptId, user.JobId, user.Enabled, user.Sex)
		global.LOG.Infof("[SysUserObserver] AfterCreate 详细数据 -> model类型: %T, model值: %+v", model, model)
		global.LOG.Infof("[SysUserObserver] AfterCreate Dest信息 -> Dest类型: %T, Dest值: %+v", tx.Statement.Dest, tx.Statement.Dest)
		global.LOG.Infof("[SysUserObserver] AfterCreate 表名: %s", tx.Statement.Table)
	} else {
		log.Printf("[SysUserObserver] 系统用户创建: ID=%d, Username=%s, NickName=%s, Phone=%s, Email=%s, DeptId=%d, JobId=%d, Enabled=%d, Sex=%s",
			user.Id, user.Username, user.NickName, user.Phone, user.Email, user.DeptId, user.JobId, user.Enabled, user.Sex)
		log.Printf("[SysUserObserver] AfterCreate 详细数据 -> model类型: %T, model值: %+v", model, model)
		log.Printf("[SysUserObserver] AfterCreate Dest信息 -> Dest类型: %T, Dest值: %+v", tx.Statement.Dest, tx.Statement.Dest)
		log.Printf("[SysUserObserver] AfterCreate 表名: %s", tx.Statement.Table)
	}

	return nil
}

// AfterUpdate 更新后回调
func (o *SysUserObserver) AfterUpdate(tx *gorm.DB, model interface{}) error {
	user, ok := model.(*models.SysUser)
	if !ok {
		if global.LOG != nil {
			global.LOG.Warnf("[SysUserObserver] AfterUpdate 类型断言失败, model类型: %T, model值: %+v", model, model)
		} else {
			log.Printf("[SysUserObserver] AfterUpdate 类型断言失败, model类型: %T, model值: %+v", model, model)
		}
		return nil
	}

	// Dirty Tracking 验证：获取字段变更信息
	dirty := observer.GetDirtyFromTx(tx)
	if dirty != nil && dirty.HasChanges() {
		// 打印变更摘要
		if global.LOG != nil {
			global.LOG.Infof("[SysUserObserver] 字段变更检测: %s", dirty.String())
			// 打印每个变更的详细信息
			for field, change := range dirty.GetDirty() {
				global.LOG.Infof("[SysUserObserver] 字段变更: %s (列:%s) %v → %v",
					field, change.Column, change.OldValue, change.NewValue)
			}
		} else {
			log.Printf("[SysUserObserver] 字段变更检测: %s", dirty.String())
			for field, change := range dirty.GetDirty() {
				log.Printf("[SysUserObserver] 字段变更: %s (列:%s) %v → %v",
					field, change.Column, change.OldValue, change.NewValue)
			}
		}
	} else {
		if global.LOG != nil {
			global.LOG.Infof("[SysUserObserver] 未检测到字段变更（可能是无法获取旧数据）")
		} else {
			log.Printf("[SysUserObserver] 未检测到字段变更（可能是无法获取旧数据）")
		}
	}

	dump.P(user)
	dump.P(tx.Statement.Table)
	dump.P(tx.Statement.Model)
	dump.P(tx.Statement.Dest)

	if global.LOG != nil {
		global.LOG.Infof("[SysUserObserver] 系统用户更新: ID=%d, Username=%s, NickName=%s, Phone=%s, Email=%s, DeptId=%d, JobId=%d, Enabled=%d, Sex=%s",
			user.Id, user.Username, user.NickName, user.Phone, user.Email, user.DeptId, user.JobId, user.Enabled, user.Sex)
		global.LOG.Infof("[SysUserObserver] AfterUpdate 详细数据 -> model类型: %T, model值: %+v", model, model)
		global.LOG.Infof("[SysUserObserver] AfterUpdate Model信息 -> Model类型: %T, Model值: %+v", tx.Statement.Model, tx.Statement.Model)
		global.LOG.Infof("[SysUserObserver] AfterUpdate Dest信息 -> Dest类型: %T, Dest值: %+v", tx.Statement.Dest, tx.Statement.Dest)
		global.LOG.Infof("[SysUserObserver] AfterUpdate 表名: %s", tx.Statement.Table)
	} else {
		log.Printf("[SysUserObserver] 系统用户更新: ID=%d, Username=%s, NickName=%s, Phone=%s, Email=%s, DeptId=%d, JobId=%d, Enabled=%d, Sex=%s",
			user.Id, user.Username, user.NickName, user.Phone, user.Email, user.DeptId, user.JobId, user.Enabled, user.Sex)
		log.Printf("[SysUserObserver] AfterUpdate 详细数据 -> model类型: %T, model值: %+v", model, model)
		log.Printf("[SysUserObserver] AfterUpdate Model信息 -> Model类型: %T, Model值: %+v", tx.Statement.Model, tx.Statement.Model)
		log.Printf("[SysUserObserver] AfterUpdate Dest信息 -> Dest类型: %T, Dest值: %+v", tx.Statement.Dest, tx.Statement.Dest)
		log.Printf("[SysUserObserver] AfterUpdate 表名: %s", tx.Statement.Table)
	}

	return nil
}

// AfterDelete 删除后回调
func (o *SysUserObserver) AfterDelete(tx *gorm.DB, model interface{}) error {
	user, ok := model.(*models.SysUser)
	if !ok {
		if global.LOG != nil {
			global.LOG.Warnf("[SysUserObserver] AfterDelete 类型断言失败, model类型: %T, model值: %+v", model, model)
		} else {
			log.Printf("[SysUserObserver] AfterDelete 类型断言失败, model类型: %T, model值: %+v", model, model)
		}
		return nil
	}

	if global.LOG != nil {
		global.LOG.Infof("[SysUserObserver] 系统用户删除: ID=%d, Username=%s, NickName=%s",
			user.Id, user.Username, user.NickName)
		global.LOG.Infof("[SysUserObserver] AfterDelete 详细数据 -> model类型: %T, model值: %+v", model, model)
		global.LOG.Infof("[SysUserObserver] AfterDelete Dest信息 -> Dest类型: %T, Dest值: %+v", tx.Statement.Dest, tx.Statement.Dest)
		global.LOG.Infof("[SysUserObserver] AfterDelete 表名: %s", tx.Statement.Table)
	} else {
		log.Printf("[SysUserObserver] 系统用户删除: ID=%d, Username=%s, NickName=%s",
			user.Id, user.Username, user.NickName)
		log.Printf("[SysUserObserver] AfterDelete 详细数据 -> model类型: %T, model值: %+v", model, model)
		log.Printf("[SysUserObserver] AfterDelete Dest信息 -> Dest类型: %T, Dest值: %+v", tx.Statement.Dest, tx.Statement.Dest)
		log.Printf("[SysUserObserver] AfterDelete 表名: %s", tx.Statement.Table)
	}

	return nil
}

// AfterFind 查询后回调
func (o *SysUserObserver) AfterFind(tx *gorm.DB, model interface{}) error {
	// AfterFind 的 model 可能是单条 *models.SysUser 或切片 []*models.SysUser
	// 这里不做类型断言，直接打印类型和值来验证
	if global.LOG != nil {
		global.LOG.Infof("[SysUserObserver] 系统用户查询 -> model类型: %T", model)
		global.LOG.Infof("[SysUserObserver] AfterFind 详细数据 -> model值: %+v", model)
		global.LOG.Infof("[SysUserObserver] AfterFind Dest信息 -> Dest类型: %T, Dest值: %+v", tx.Statement.Dest, tx.Statement.Dest)
		global.LOG.Infof("[SysUserObserver] AfterFind 表名: %s", tx.Statement.Table)
	} else {
		log.Printf("[SysUserObserver] 系统用户查询 -> model类型: %T", model)
		log.Printf("[SysUserObserver] AfterFind 详细数据 -> model值: %+v", model)
		log.Printf("[SysUserObserver] AfterFind Dest信息 -> Dest类型: %T, Dest值: %+v", tx.Statement.Dest, tx.Statement.Dest)
		log.Printf("[SysUserObserver] AfterFind 表名: %s", tx.Statement.Table)
	}

	return nil
}
