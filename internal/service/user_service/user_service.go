package user_service

import (
	"errors"
	"shop/internal/models"
	"shop/internal/models/vo"
)

type User struct {
	Id       int64
	Username string

	DeptId  int64
	Enabled int

	PageNum  int
	PageSize int

	M *models.SysUser

	Ids []int64

	ImageUrl string
}

func (u *User) UpdateImage() error {
	user, err := models.GetUserById(u.Id)
	if err != nil {
		return errors.New("用户不存在")
	}

	user.Avatar = u.ImageUrl
	return models.UpdateCurrentUser(&user)
}
func (u *User) GetUserOneByName() (*models.SysUser, error) {
	return models.GetUserByUsername(u.Username)
}

func (u *User) GetUserAll() vo.ResultList {
	maps := make(map[string]interface{})
	if u.DeptId >= 0 {
		maps["dept_id"] = u.DeptId
	}
	if u.Enabled >= 0 {
		maps["enabled"] = u.Enabled
	}
	if u.Username != "" {
		maps["username"] = u.Username
	}

	total, list := models.GetAllUser(u.PageNum, u.PageSize, maps)
	return vo.ResultList{Content: list, TotalElements: total}
}

func (u *User) Insert() error {
	return models.AddUser(u.M)
}

//func (u *User) Save() error {
//	return models.UpdateByUser(u.M)
//}
//
//func (u *User) Del() error {
//	return models.DelByUser(u.Ids)
//}
