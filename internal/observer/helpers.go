package observer

import (
	"fmt"
	"reflect"

	"gorm.io/gorm"
)

// FetchFullModel 从 tx 中尝试获取完整模型数据
// 适用于 Update/Delete 等链式调用场景下 model 数据不完整的情况
// 通过 tx.Statement.Schema 获取主键字段，从 tx.Statement.Dest 中提取主键值，用新 Session 查询完整记录
func FetchFullModel(tx *gorm.DB, dest interface{}) error {
	if tx.Statement.Schema == nil {
		return fmt.Errorf("observer: schema not found")
	}

	// 获取主键字段
	primaryField := tx.Statement.Schema.PrioritizedPrimaryField
	if primaryField == nil {
		return fmt.Errorf("observer: primary field not found")
	}

	// 从 Dest 中获取主键值
	if tx.Statement.Dest != nil {
		destValue := reflect.ValueOf(tx.Statement.Dest)
		if destValue.Kind() == reflect.Ptr {
			destValue = destValue.Elem()
		}
		if destValue.Kind() == reflect.Struct {
			field := destValue.FieldByName(primaryField.Name)
			if field.IsValid() && !field.IsZero() {
				// 使用新 Session 查询完整数据，避免影响当前事务链
				return tx.Session(&gorm.Session{NewDB: true}).First(dest, field.Interface()).Error
			}
		}
	}

	return fmt.Errorf("observer: cannot determine primary key from context")
}
