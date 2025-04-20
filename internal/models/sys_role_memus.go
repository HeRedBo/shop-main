package models

type SysRolesMenus struct {
	ID     int64 `gorm:"primaryKey;autoIncrement"`
	MenuId int64 `gorm:"column:sys_menu_id;"`
	RoleId int64 `gorm:"column:sys_role_id;"`
}
