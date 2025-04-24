package product_service

import (
	"github.com/jinzhu/copier"
	"github.com/unknwon/com"
	"shop/internal/models"
	productDto "shop/internal/service/product_service/dto"
	proVo "shop/internal/service/product_service/vo"
	productEnum "shop/pkg/enums/product"
	"shop/pkg/global"
	"shop/pkg/util"
)

type Product struct {
	Id   int64
	Name string

	Enabled int

	PageNum  int
	PageSize int

	M *models.StoreProductRule

	Ids []int64

	Dto productDto.StoreProduct

	SaleDto productDto.OnSale

	JsonObj map[string]interface{}

	Order int

	News       string
	PriceOrder string
	SalesOrder string
	Sid        string

	Uid int64

	Unique string

	Type string
}

// 搜索结果响应结构
type searchResponse struct {
	Success bool                  `json:"success"`
	Code    int                   `json:"code"`
	Msg     string                `json:"msg"`
	Data    productSearchResponse `json:"data"`
}
type productSearchResponse struct {
	Total int64           `json:"total"`
	Hits  []*productIndex `json:"hits"`
}

type productIndex struct {
	Id int64 `json:"id"`
}

// get stock
func (d *Product) GetStock() int {
	var productAttrValue models.StoreProductAttrValue
	err := global.Db.Model(&models.StoreProductAttrValue{}).
		Where("`unique` = ?", d.Unique).
		Where("product_id = ?", d.Id).First(&productAttrValue).Error
	if err != nil {
		global.LOG.Error(err)
		return 0
	}
	return productAttrValue.Stock
}

func (d *Product) GetList() ([]proVo.Product, int, int) {
	maps := make(map[string]interface{})
	if d.Name != "" {
		maps["store_name"] = d.Name
	}
	if d.Enabled >= 0 {
		maps["is_show"] = d.Enabled
	}
	switch d.Order {
	case productEnum.STATUS_1:
		maps["is_best"] = 1
	case productEnum.STATUS_2:
		maps["is_new"] = 1
	case productEnum.STATUS_3:
		maps["is_benefit"] = 1
	case productEnum.STATUS_4:
		maps["is_hot"] = 1
	}

	if d.Sid != "" {
		maps["cate_id"] = com.StrTo(d.Sid).MustInt()
	}
	if d.News != "" {
		news := com.StrTo(d.News).MustInt()
		if news == 1 {
			maps["is_new"] = 1
		}
	}
	order := ""
	if d.SalesOrder != "" {
		if productEnum.ASC == d.SalesOrder {
			order = "sales asc"
		} else if productEnum.DESC == d.SalesOrder {
			order = "sales desc"
		}
	}
	if d.PriceOrder != "" {
		if productEnum.ASC == d.PriceOrder {
			order = "price asc"
		} else if productEnum.DESC == d.PriceOrder {
			order = "price desc"
		}
	}

	var productListVo []proVo.Product

	total, list := models.GetFrontAllProduct(d.PageNum, d.PageSize, maps, order)
	e := copier.Copy(&productListVo, list)
	if e != nil {
		global.LOG.Error(e)
	}
	totalNum := util.Int64ToInt(total)
	totalPage := util.GetTotalPage(totalNum, d.PageSize)
	return productListVo, totalNum, totalPage
}

// GetProductByIDs
func (d *Product) GetProductByIDs() []proVo.Product {
	var productListVo []proVo.Product
	if len(d.Ids) == 0 {
		return productListVo
	}
	list := models.GetProductByIDs(map[string]interface{}{"id": d.Ids})
	e := copier.Copy(&productListVo, list)
	if e != nil {
		global.LOG.Error(e)
	}
	return productListVo
}
