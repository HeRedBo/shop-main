package cart_service

import (
	"errors"
	"shop/internal/models"
	"shop/internal/service/product_service"
	"shop/pkg/global"
	"strconv"
)

func CheckStock(productId int64, cartNum int, unique string) error {
	var (
		storeProduct models.StoreProduct
		err          error
	)
	err = global.Db.Model(&models.StoreProduct{}).
		Where("id = ?", productId).
		Where("is_show", 1).
		First(&storeProduct).Error
	if err != nil {
		global.LOG.Error(err)
		return errors.New("该商品已下架或者删除")
	}
	productService := product_service.Product{
		Id:     productId,
		Unique: unique,
	}

	stock := productService.GetStock()
	if stock < cartNum {
		return errors.New(storeProduct.StoreName + "库存不足" + strconv.Itoa(cartNum))
	}
	return nil
}
