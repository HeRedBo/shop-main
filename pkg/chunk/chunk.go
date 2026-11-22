package chunk

import (
	"fmt"
	"reflect"

	"gorm.io/gorm"
)

// extractId 通过反射获取模型实例的 Id 字段值。
// 模型必须嵌入 BaseModel（含 Id int64 主键），否则 panic。
// 仅供 ChunkById / ChunkByIdWithTotal 这两个主键便捷壳使用。
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

// ChunkBy 按指定字段做游标分块查询（column > last ORDER BY column ASC）。
// column 同时用于排序和游标推进，必须传入可信的字段名（直接拼入 SQL），
// 且该字段应唯一并有索引，否则同值行会被跳过或退化为全表排序。
// extract 从每批最后一条记录中取出游标值，类型 C 由调用方决定，
// int64、string、time.Time 等可直接作为查询参数的类型均可。
// 首批不带 WHERE 条件，避免零值游标漏掉边界数据。
// 每批查询使用独立 Session，避免条件累积。回调返回 error 时中断遍历。
func ChunkBy[T any, C any](db *gorm.DB, chunkSize int, column string,
	extract func(item T) C,
	callback func(batch []T, batchNum int) error) error {

	var last C
	started := false
	batchNum := 0

	for {
		var batch []T
		query := db.Session(&gorm.Session{}).
			Order(column + " ASC").
			Limit(chunkSize)
		if started {
			query = query.Where(column+" > ?", last)
		}
		result := query.Find(&batch)

		if result.Error != nil {
			return fmt.Errorf("chunk query failed at batch %d (last_%s=%v): %w", batchNum, column, last, result.Error)
		}
		if len(batch) == 0 {
			break
		}

		batchNum++
		if err := callback(batch, batchNum); err != nil {
			return fmt.Errorf("chunk callback failed at batch %d (last_%s=%v): %w", batchNum, column, last, err)
		}

		// 游标推进：取最后一条记录的指定字段值
		last = extract(batch[len(batch)-1])
		started = true
		if len(batch) < chunkSize {
			break
		}
	}
	return nil
}

// ChunkById 基于主键游标（id > last_id ORDER BY id ASC）对数据进行分块查询。
// 是 ChunkBy 固定 column="id" 的便捷壳，回调返回 error 时中断遍历。
func ChunkById[T any](db *gorm.DB, chunkSize int,
	callback func(batch []T, batchNum int) error) error {

	return ChunkBy(db, chunkSize, "id", extractId[T], callback)
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

	return ChunkBy(db, chunkSize, "id", extractId[T], func(batch []T, batchNum int) error {
		return callback(batch, batchNum, total)
	})
}
