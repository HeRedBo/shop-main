package address_service

import (
	"encoding/json"
	"errors"
	"github.com/HeRedBo/pkg/cache"
	"shop/internal/models"
	"shop/internal/params"
	"shop/pkg/constant"
	"shop/pkg/global"
	"shop/pkg/util"
)

type Address struct {
	Id   int64
	Name string

	Enabled int

	PageNum  int
	PageSize int

	M *models.UserAddress

	Param *params.AddressParam

	Ids []int64
	Uid int64
}

// GetList 获取列表数据
func (d *Address) GetList() ([]models.UserAddress, int, int) {
	maps := make(map[string]any)
	maps["uid"] = d.Uid
	total, list := models.GetAllUserAddress(d.PageNum, d.PageSize, maps)
	totalNum := util.Int64ToInt(total)
	totalPage := util.GetTotalPage(totalNum, d.PageSize)
	return list, totalNum, totalPage
}

func (d *Address) AddOrUpdate() (int64, error) {
	var err error
	tx := global.Db.Begin()
	defer func() {
		if err != nil {
			tx.Rollback()
		} else {
			tx.Commit()
		}
	}()

	userAddress := &models.UserAddress{
		City:     d.Param.Address.City,
		CityId:   d.Param.Address.CityId,
		District: d.Param.Address.District,
		Province: d.Param.Address.Province,
		Detail:   d.Param.Detail,
		Uid:      d.Uid,
		Phone:    d.Param.Phone,
		PostCode: d.Param.PostCode,
		RealName: d.Param.RealName,
	}

	if d.Param.IsDefault {
		userAddress.IsDefault = 1
		err := tx.Model(&models.UserAddress{}).
			Where("id = ?", d.Uid).Update("is_default", 0).Error
		if err != nil {
			global.LOG.Error(err)
			return 0, errors.New("重置用户默认地址操作失败")
		}
	}
	if d.Param.Id == 0 {
		err := tx.Create(userAddress).Error
		if err != nil {
			global.LOG.Error(err)
			return 0, errors.New("新增操作失败")
		}
	} else {
		err := tx.Model(&models.UserAddress{}).
			Where("id = ?", d.Param.Id).
			Updates(userAddress).Error
		if err != nil {
			global.LOG.Error(err)
			return 0, errors.New("更新地址操作失败")
		}
	}
	return userAddress.Id, err
}

// DelAddress 删除地址
func (d *Address) DelAddress() error {
	err := global.Db.Where("uid = ?", d.Uid).
		Where("id = ?", d.Id).
		Delete(&models.UserAddress{}).Error
	if err != nil {
		global.LOG.Error(err)
		return errors.New("操作失败")
	}
	return nil
}

func (d *Address) SetDefault() error {
	var err error
	tx := global.Db.Begin()
	defer func() {
		if err != nil {
			tx.Rollback()
		} else {
			tx.Commit()
		}
	}()
	err = tx.Model(&models.UserAddress{}).
		Where("id = ?", d.Uid).Update("is_default", 0).Error
	if err != nil {
		global.LOG.Error(err)
		return errors.New("操作失败")
	}
	err = tx.Model(&models.UserAddress{}).
		Where("id = ?", d.Uid).Update("is_default", 1).Error
	if err != nil {
		global.LOG.Error(err)
		return errors.New("操作失败")
	}
	return nil
}

func (d *Address) GetCities() []models.SystemCity {
	// 从缓存获取数据
	key := constant.CityList
	val, err := cache.GetRedisClient(cache.DefaultRedisClient).GetStr(key)
	if err != nil {
		global.LOG.Error("redis error ", err, "key", key, "cmd : Get", "client", cache.DefaultRedisClient)
	} else {
		var cities []models.SystemCity
		err := json.Unmarshal([]byte(val), &cities)
		if err != nil {
			global.LOG.Error(" json.Unmarshal error val : ", val)
		}
		if len(cities) > 0 {
			return cities
		}
	}
	// 数据库获取数据
	maps := make(map[string]any)
	maps["is_show"] = 1
	list := models.GetAllSystemCity(maps)
	listCache, _ := json.Marshal(list)
	err = cache.GetRedisClient(cache.DefaultRedisClient).Set(key, listCache, 1000)
	global.LOG.Error("set Cities data error", err, "key", key)
	return list
}

func (d *Address) Insert() error {
	return models.AddUserAddress(d.M)
}

func (d *Address) Save() error {
	return models.UpdateByUserAddress(d.M)
}

func (d *Address) Del() error {
	return models.DelByUserAddress(d.Ids)
}
