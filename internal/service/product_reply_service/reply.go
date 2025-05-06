package product_reply_service

import (
	"github.com/jinzhu/copier"
	"shop/internal/models"
	vo2 "shop/internal/service/product_service/vo"
	"shop/pkg/global"
	"shop/pkg/util"
)

type Reply struct {
	Id   int64
	Name string

	Enabled int

	PageNum  int
	PageSize int

	M *models.StoreProductReply

	Ids []int64

	Uid       int64
	ProductId int64
	Type      int
}

func (d *Reply) GetList() ([]vo2.ProductReply, int, int) {
	maps := make(map[string]interface{})
	if d.Name != "" {
		maps["name"] = d.Name
	}
	if d.ProductId > 0 {
		maps["product_id"] = d.ProductId
	}
	var replyVo []vo2.ProductReply
	total, list := models.GetAllProductReply(d.PageNum, d.PageSize, maps)
	e := copier.Copy(&replyVo, list)
	if e != nil {
		global.LOG.Error(e)
	}
	totalNum := util.Int64ToInt(total)
	totalPage := util.GetTotalPage(totalNum, d.PageSize)
	return replyVo, totalNum, totalPage
}

func (d *Reply) Insert() error {
	return models.AddStoreProductReply(d.M)
}

func (d *Reply) Save() error {
	return models.UpdateByStoreProductReply(d.M)
}

func (d *Reply) Del() error {
	return models.DelByStoreProductReply(d.Ids)
}
