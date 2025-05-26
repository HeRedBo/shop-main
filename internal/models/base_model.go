package models

import (
	"gorm.io/plugin/soft_delete"
	"time"
)

//// 自定义时间类型（内嵌 time.Time）
//type CustomTime struct {
//	Time time.Time
//}
//
//// MarshalJSON 实现
//func (t CustomTime) MarshalJSON() ([]byte, error) {
//	return json.Marshal(t.Time.Format("2006-01-02 15:04:05"))
//}
//
//// UnmarshalJSON 实现
//func (t *CustomTime) UnmarshalJSON(data []byte) error {
//	str := string(data)
//	if str == "null" || str == `""` {
//		return nil
//	}
//	parsedTime, err := time.Parse(`"2006-01-02 15:04:05"`, str)
//	if err != nil {
//		return err
//	}
//	t.Time = parsedTime
//	return nil
//}
//
//// 实现 Scanner 接口（从数据库读取）
//func (t *CustomTime) Scan(value interface{}) error {
//	if value == nil {
//		return nil
//	}
//	switch v := value.(type) {
//	case time.Time:
//		// 转换为本地时区
//		t.Time = v.In(time.Local)
//		//t.Time = v
//		return nil
//	default:
//		return fmt.Errorf("无法扫描非时间类型到 CustomTime")
//	}
//}
//
//// 实现 Valuer 接口（写入数据库）
//func (t CustomTime) Value() (driver.Value, error) {
//	return t.Time, nil
//}

type BaseModel struct {
	Id         int64                 `gorm:"primary_key" json:"id"`
	UpdateTime time.Time             `json:"update_time" gorm:"autoUpdateTime"`
	CreateTime time.Time             `json:"create_time" gorm:"autoUpdateTime"`
	IsDel      soft_delete.DeletedAt `json:"is_del" gorm:"softDelete:flag"`
}

//
//func (m *BaseModel) BeforeCreate(tx *gorm.DB) error {
//	m.CreateTime = CustomTime{Time: time.Now()}
//	m.UpdateTime = CustomTime{Time: time.Now()}
//	return nil
//}
//
//func (m *BaseModel) BeforeUpdate(tx *gorm.DB) error {
//	m.UpdateTime = CustomTime{Time: time.Now()}
//	return nil
//}
