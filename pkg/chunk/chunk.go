package chunk

import (
	"fmt"
	"reflect"

	"gorm.io/gorm"
)

// extractId 通过反射获取模型实例的 Id 字段值。
// 模型必须嵌入 BaseModel（含 Id int64 主键），否则 panic。
func extractId[T any](item T) int64 {
	v := reflect.ValueOf(item)
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		panic("chunk: item must be a struct or pointer to struct")
	}
	field := v.FieldByName("Id")
	if !field.IsValid() {
		panic("chunk: struct does not have an 'Id' field; embed BaseModel to use ChunkById")
	}
	if field.Kind() != reflect.Int64 {
		panic(fmt.Sprintf("chunk: 'Id' field must be int64, got %s", field.Kind()))
	}
	return field.Int()
}

// ChunkById 基于主键游标（id > last_id ORDER BY id ASC）对数据进行分块查询。
// 每批查询使用独立 Session，避免条件累积。回调返回 error 时中断遍历。
func ChunkById[T any](db *gorm.DB, chunkSize int,
	callback func(batch []T, batchNum int) error) error {

	var lastId int64
	batchNum := 0

	for {
		var batch []T
		result := db.Session(&gorm.Session{}).
			Where("id > ?", lastId).
			Order("id ASC").
			Limit(chunkSize).
			Find(&batch)

		if result.Error != nil {
			return fmt.Errorf("chunk query failed at batch %d (last_id=%d): %w", batchNum, lastId, result.Error)
		}
		if len(batch) == 0 {
			break
		}

		batchNum++
		if err := callback(batch, batchNum); err != nil {
			return fmt.Errorf("chunk callback failed at batch %d (last_id=%d): %w", batchNum, lastId, err)
		}

		// 游标推进：取最后一条记录的 Id
		lastId = extractId(batch[len(batch)-1])
		if len(batch) < chunkSize {
			break
		}
	}
	return nil
}

// ChunkByIdWithTotal 先 Count 获取总数，再按主键游标分块遍历。
// 回调中传入 total 参数，方便进度条计算。
func ChunkByIdWithTotal[T any](db *gorm.DB, model *T, chunkSize int,
	callback func(batch []T, batchNum int, total int64) error) error {

	// 先获取总数
	var total int64
	countResult := db.Session(&gorm.Session{}).Model(model).Count(&total)
	if countResult.Error != nil {
		return fmt.Errorf("chunk count failed: %w", countResult.Error)
	}

	var lastId int64
	batchNum := 0

	for {
		var batch []T
		result := db.Session(&gorm.Session{}).
			Where("id > ?", lastId).
			Order("id ASC").
			Limit(chunkSize).
			Find(&batch)

		if result.Error != nil {
			return fmt.Errorf("chunk query failed at batch %d (last_id=%d): %w", batchNum, lastId, result.Error)
		}
		if len(batch) == 0 {
			break
		}

		batchNum++
		if err := callback(batch, batchNum, total); err != nil {
			return fmt.Errorf("chunk callback failed at batch %d (last_id=%d): %w", batchNum, lastId, err)
		}

		// 游标推进：取最后一条记录的 Id
		lastId = extractId(batch[len(batch)-1])
		if len(batch) < chunkSize {
			break
		}
	}
	return nil
}
