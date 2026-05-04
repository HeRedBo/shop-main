package observer

import (
	"fmt"
	"strings"

	"gorm.io/gorm"
)

// FieldChange 单个字段的变更记录
type FieldChange struct {
	Field    string      // 字段名（Go 结构体字段名）
	Column   string      // 数据库列名
	OldValue interface{} // 旧值
	NewValue interface{} // 新值
}

// DirtyFields 变更字段集合
type DirtyFields struct {
	Changes map[string]FieldChange // key: 字段名
}

// NewDirtyFields 创建空的 DirtyFields
func NewDirtyFields() *DirtyFields {
	return &DirtyFields{
		Changes: make(map[string]FieldChange),
	}
}

// Add 添加一个字段变更
func (d *DirtyFields) Add(field, column string, oldVal, newVal interface{}) {
	if d.Changes == nil {
		d.Changes = make(map[string]FieldChange)
	}
	d.Changes[field] = FieldChange{
		Field:    field,
		Column:   column,
		OldValue: oldVal,
		NewValue: newVal,
	}
}

// IsDirty 检查某字段是否变更
func (d *DirtyFields) IsDirty(field string) bool {
	if d == nil || d.Changes == nil {
		return false
	}
	_, ok := d.Changes[field]
	return ok
}

// GetDirty 获取所有变更字段
func (d *DirtyFields) GetDirty() map[string]FieldChange {
	if d == nil || d.Changes == nil {
		return nil
	}
	return d.Changes
}

// GetOriginal 获取某字段的原始值
func (d *DirtyFields) GetOriginal(field string) interface{} {
	if d == nil || d.Changes == nil {
		return nil
	}
	if fc, ok := d.Changes[field]; ok {
		return fc.OldValue
	}
	return nil
}

// GetNew 获取某字段的新值
func (d *DirtyFields) GetNew(field string) interface{} {
	if d == nil || d.Changes == nil {
		return nil
	}
	if fc, ok := d.Changes[field]; ok {
		return fc.NewValue
	}
	return nil
}

// HasChanges 是否有任何变更
func (d *DirtyFields) HasChanges() bool {
	return d != nil && len(d.Changes) > 0
}

// Fields 获取所有变更的字段名列表
func (d *DirtyFields) Fields() []string {
	if d == nil || d.Changes == nil {
		return nil
	}
	fields := make([]string, 0, len(d.Changes))
	for field := range d.Changes {
		fields = append(fields, field)
	}
	return fields
}

// String 输出可读的变更摘要
func (d *DirtyFields) String() string {
	if d == nil || len(d.Changes) == 0 {
		return "no changes"
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("DirtyFields(%d changes): ", len(d.Changes)))
	first := true
	for _, fc := range d.Changes {
		if !first {
			sb.WriteString(", ")
		}
		sb.WriteString(fmt.Sprintf("%s: %v -> %v", fc.Field, fc.OldValue, fc.NewValue))
		first = false
	}
	return sb.String()
}

// GetDirtyFromTx 从 tx 中获取 DirtyFields（Observer 中使用的便捷方法）
func GetDirtyFromTx(tx *gorm.DB) *DirtyFields {
	if val, ok := tx.Get("observer:dirty"); ok {
		if df, ok := val.(*DirtyFields); ok {
			return df
		}
	}
	return nil
}

// ========== 批量操作上下文信息 ==========

// BatchContext 批量操作的上下文信息
// 当批量 Update/Delete 时，Observer 可通过此结构获取操作的 WHERE 条件和影响行数
type BatchContext struct {
	Table        string        // 表名
	SQL          string        // 完整的 SQL 语句
	Vars         []interface{} // SQL 绑定变量（即 WHERE 条件中的值，如 ids 列表）
	RowsAffected int64         // 影响行数
}

// GetBatchContext 从 tx 中提取批量操作的上下文信息
// 适用于批量 Update/Delete 场景，Observer 中通过此方法获取 WHERE 条件进行反查
//
// 使用示例:
//
//	func (o *MyObserver) AfterUpdate(tx *gorm.DB, model interface{}) error {
//	    // 先尝试单条 Dirty Tracking
//	    dirty := observer.GetDirtyFromTx(tx)
//	    if dirty != nil && dirty.HasChanges() {
//	        // 单条更新，处理字段变更...
//	        return nil
//	    }
//	    // 批量更新场景
//	    batch := observer.GetBatchContext(tx)
//	    if batch.RowsAffected > 0 {
//	        // 通过 batch.Vars 拿到 WHERE 条件中的值（如 ID 列表）
//	        // 反查受影响的记录，执行业务逻辑
//	    }
//	    return nil
//	}
func GetBatchContext(tx *gorm.DB) *BatchContext {
	return &BatchContext{
		Table:        tx.Statement.Table,
		SQL:          tx.Statement.SQL.String(),
		Vars:         tx.Statement.Vars,
		RowsAffected: tx.RowsAffected,
	}
}

// GetVarsAs 从 BatchContext 的 Vars 中提取指定索引的值并转为 int64 切片
// 常用于提取 WHERE id IN (?) 中的 ID 列表
//
// 示例:
//
//	batch := observer.GetBatchContext(tx)
//	ids := observer.GetVarsAsInt64Slice(batch, 0)
func GetVarsAsInt64Slice(batch *BatchContext, index int) []int64 {
	if batch == nil || index >= len(batch.Vars) {
		return nil
	}
	switch v := batch.Vars[index].(type) {
	case []int64:
		return v
	case []int:
		result := make([]int64, len(v))
		for i, val := range v {
			result[i] = int64(val)
		}
		return result
	case []interface{}:
		result := make([]int64, 0, len(v))
		for _, val := range v {
			switch id := val.(type) {
			case int64:
				result = append(result, id)
			case int:
				result = append(result, int64(id))
			case float64:
				result = append(result, int64(id))
			}
		}
		return result
	case int64:
		return []int64{v}
	case int:
		return []int64{int64(v)}
	default:
		return nil
	}
}

// GetVar 从 BatchContext 中按索引获取 WHERE 条件的原始绑定值
// 索引对应 SQL 中 ? 占位符的出现顺序
//
// 示例: WHERE dept_id = ? AND enabled = ? AND create_time < ?
//
//	batch := observer.GetBatchContext(tx)
//	deptId := batch.GetVar(0)       // 第1个 ? → dept_id 的值
//	enabled := batch.GetVar(1)      // 第2个 ? → enabled 的值
//	createTime := batch.GetVar(2)   // 第3个 ? → create_time 的值
func (b *BatchContext) GetVar(index int) interface{} {
	if b == nil || index >= len(b.Vars) {
		return nil
	}
	return b.Vars[index]
}

// GetVarInt64 获取指定索引的绑定值并转为 int64
func (b *BatchContext) GetVarInt64(index int) (int64, bool) {
	v := b.GetVar(index)
	if v == nil {
		return 0, false
	}
	switch val := v.(type) {
	case int64:
		return val, true
	case int:
		return int64(val), true
	case int32:
		return int64(val), true
	case float64:
		return int64(val), true
	default:
		return 0, false
	}
}

// GetVarString 获取指定索引的绑定值并转为 string
func (b *BatchContext) GetVarString(index int) (string, bool) {
	v := b.GetVar(index)
	if v == nil {
		return "", false
	}
	if s, ok := v.(string); ok {
		return s, true
	}
	return fmt.Sprintf("%v", v), true
}

// VarsCount 返回绑定变量的数量（即 WHERE 中有多少个 ? 占位符）
func (b *BatchContext) VarsCount() int {
	if b == nil {
		return 0
	}
	return len(b.Vars)
}
