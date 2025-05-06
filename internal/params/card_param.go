package params

type CartParam struct {
	ProductId     int64  `json:"product_id"`
	UniqueId      string `json:"unique_id"`
	CartNum       int    `json:"cart_num"`
	IsNew         int8   `json:"is_new"`
	CombinationId int64  `json:"combination_id"`
	SeckillId     int64  `json:"seckill_id"`
	BargainId     int64  `json:"bargain_id"`
}
