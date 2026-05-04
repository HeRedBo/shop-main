package observer

import (
	"fmt"
	"reflect"

	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

// ComputeDirtyFields 计算本次更新相对于旧数据的字段变更
// oldModel: 数据库中的旧数据（完整模型）
// 新值从 tx.Statement.Dest 获取
func ComputeDirtyFields(tx *gorm.DB, oldModel interface{}) *DirtyFields {
	if oldModel == nil || tx.Statement.Dest == nil || tx.Statement.Schema == nil {
		return nil
	}

	// 判断是否是 map 更新
	if m, ok := tx.Statement.Dest.(map[string]interface{}); ok {
		return computeDirtyFromMap(tx, oldModel, m)
	}

	return computeDirtyFromStruct(tx, oldModel)
}

// computeDirtyFromStruct 从 struct 类型的 Dest 计算字段变更
func computeDirtyFromStruct(tx *gorm.DB, oldModel interface{}) *DirtyFields {
	dirty := NewDirtyFields()

	oldVal := reflect.ValueOf(oldModel)
	if oldVal.Kind() == reflect.Ptr {
		oldVal = oldVal.Elem()
	}
	if oldVal.Kind() != reflect.Struct {
		return dirty
	}

	newVal := reflect.ValueOf(tx.Statement.Dest)
	if newVal.Kind() == reflect.Ptr {
		newVal = newVal.Elem()
	}
	if newVal.Kind() != reflect.Struct {
		return dirty
	}

	// 构建 Selects 和 Omits 集合
	selectsSet := make(map[string]bool)
	selectAll := false // 是否选择所有字段（Save 操作会设置 Selects=["*"]）
	for _, s := range tx.Statement.Selects {
		if s == "*" {
			selectAll = true
		}
		selectsSet[s] = true
	}
	omitsSet := make(map[string]bool)
	for _, o := range tx.Statement.Omits {
		omitsSet[o] = true
	}

	for _, field := range tx.Statement.Schema.Fields {
		// 忽略无 DBName 的字段
		if field.DBName == "" {
			continue
		}

		// 检查 Omits
		if omitsSet[field.Name] || omitsSet[field.DBName] {
			continue
		}

		// 获取新值
		newField := newVal.FieldByName(field.Name)
		if !newField.IsValid() {
			continue
		}

		// 判断字段是否被更新
		// Save() 会设置 Selects=["*"]，表示更新所有字段，此时等同于无 Select 限制
		hasExplicitSelect := len(selectsSet) > 0 && !selectAll
		isSelected := selectsSet[field.Name] || selectsSet[field.DBName]
		isZero := newField.IsZero()

		if hasExplicitSelect {
			// 有明确的 Select（非 "*"），只检查被选中的字段
			if !isSelected {
				continue
			}
		} else {
			// 无 Select 或 Select("*")：Save 场景下所有非零值字段都参与比较
			if isZero {
				continue
			}
		}

		// 获取旧值
		oldField := oldVal.FieldByName(field.Name)
		if !oldField.IsValid() {
			continue
		}

		// 解引用并比较
		oldValue := dereferenceValue(oldField)
		newValue := dereferenceValue(newField)

		if !valuesEqual(oldValue, newValue) {
			dirty.Add(field.Name, field.DBName, oldValue, newValue)
		}
	}

	return dirty
}

// computeDirtyFromMap 从 map[string]interface{} 类型的 Dest 计算字段变更
func computeDirtyFromMap(tx *gorm.DB, oldModel interface{}, updateMap map[string]interface{}) *DirtyFields {
	dirty := NewDirtyFields()

	oldVal := reflect.ValueOf(oldModel)
	if oldVal.Kind() == reflect.Ptr {
		oldVal = oldVal.Elem()
	}
	if oldVal.Kind() != reflect.Struct {
		return dirty
	}

	// 构建字段名/列名到 SchemaField 的映射
	fieldMap := make(map[string]*schema.Field)
	for _, f := range tx.Statement.Schema.Fields {
		fieldMap[f.Name] = f
		fieldMap[f.DBName] = f
	}

	for key, newValue := range updateMap {
		field, ok := fieldMap[key]
		if !ok || field.DBName == "" {
			continue
		}

		oldField := oldVal.FieldByName(field.Name)
		if !oldField.IsValid() {
			continue
		}

		oldValue := dereferenceValue(oldField)
		if !valuesEqual(oldValue, newValue) {
			dirty.Add(field.Name, field.DBName, oldValue, newValue)
		}
	}

	return dirty
}

// fetchOldModel 通过主键查询旧数据
func fetchOldModel(tx *gorm.DB) (interface{}, error) {
	if tx.Statement.Schema == nil {
		return nil, fmt.Errorf("observer: schema not found")
	}

	primaryField := tx.Statement.Schema.PrioritizedPrimaryField
	if primaryField == nil {
		return nil, fmt.Errorf("observer: primary field not found")
	}

	// 尝试从 Dest 获取主键
	var pkValue interface{}
	if tx.Statement.Dest != nil {
		pkValue = extractPrimaryKeyFromStruct(tx.Statement.Dest, tx.Statement.Schema)
	}

	// 如果 Dest 中没有主键，尝试从 Model 获取
	if pkValue == nil && tx.Statement.Model != nil {
		pkValue = extractPrimaryKeyFromStruct(tx.Statement.Model, tx.Statement.Schema)
	}

	if pkValue == nil {
		return nil, fmt.Errorf("observer: cannot extract primary key from Dest(type=%T) or Model(type=%T), primary field=%s",
			tx.Statement.Dest, tx.Statement.Model, primaryField.Name)
	}

	// 创建新的模型实例来接收旧数据
	// Schema.ModelType 本身已经是 reflect.Type，直接用即可
	oldModel := reflect.New(tx.Statement.Schema.ModelType).Interface()
	if err := tx.Session(&gorm.Session{NewDB: true}).First(oldModel, pkValue).Error; err != nil {
		return nil, err
	}

	return oldModel, nil
}

// extractPrimaryKeyFromStruct 使用 GORM Schema 的 PrimaryField.Index 从对象中提取主键值
// 支持嵌入结构体中的字段（如 BaseModel.Id）
func extractPrimaryKeyFromStruct(obj interface{}, s *schema.Schema) interface{} {
	val := reflect.ValueOf(obj)
	if val.Kind() == reflect.Ptr {
		val = val.Elem()
	}
	if val.Kind() != reflect.Struct {
		return nil
	}

	primaryField := s.PrioritizedPrimaryField
	if primaryField == nil {
		return nil
	}

	// 使用 StructField.Index 定位字段（支持嵌入结构体的多层索引，如 [5, 0]）
	fieldValue := val.FieldByIndex(primaryField.StructField.Index)
	if !fieldValue.IsValid() || fieldValue.IsZero() {
		return nil
	}

	return fieldValue.Interface()
}

// dereferenceValue 解引用指针并获取实际值
func dereferenceValue(v reflect.Value) interface{} {
	if !v.IsValid() {
		return nil
	}
	if v.Kind() == reflect.Ptr {
		if v.IsNil() {
			return nil
		}
		v = v.Elem()
	}
	if !v.IsValid() {
		return nil
	}
	return v.Interface()
}

// valuesEqual 比较两个值是否相等
func valuesEqual(a, b interface{}) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return reflect.DeepEqual(a, b)
}
