package app

import (
	"github.com/astaxie/beego/validation"
	"shop/pkg/global"
)

// MarkErrors logs error logs
func MarkErrors(errors []*validation.Error) {
	for _, err := range errors {
		global.GetLogger("http").Infof("validation error: %s %s", err.Key, err.Message)
	}
	return
}
