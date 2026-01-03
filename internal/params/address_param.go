package params

import "github.com/astaxie/beego/validation"

// AddressParam 地址请求参数
type AddressParam struct {
	Id        int64  `json:"id"`         // 用户地址id
	RealName  string `json:"real_name"`  // 收货人姓名
	Phone     string `json:"phone"`      // 收货人电话
	Detail    string `json:"detail"`     // 收货人详细地址
	PostCode  string `json:"post_code"`  // 邮编
	IsDefault bool   `json:"is_default"` // 是否默认
	Address   AddressDetailParan
}

func (p *AddressParam) Valid(v *validation.Validation) {
	if vv := v.MaxSize(p.RealName, 30, "姓名"); !vv.Ok {
		vv.Message("姓名不能超过30")
		return
	}
	if vv := v.MaxSize(p.Detail, 60, "姓名"); !vv.Ok {
		vv.Message("姓名不能超过60")
		return
	}
}
