package admin

import (
	"github.com/gin-gonic/gin"
	"github.com/unknwon/com"
	"net/http"
	"shop/internal/service/menu_service"
	"shop/pkg/app"
	"shop/pkg/constant"
)

// 菜单api
type MenuController struct {
}

// @Title 菜单列表
// @Description 菜单列表
// @Success 200 {object} app.Response
// @router / [get]
func (e *MenuController) GetAll(c *gin.Context) {
	var (
		appG = app.Gin{C: c}
	)
	name := c.DefaultQuery("blurry", "")
	enabled := com.StrTo(c.DefaultQuery("enabled", "-1")).MustInt()
	menuService := menu_service.Menu{Name: name, Enabled: enabled}
	vo := menuService.GetAll()
	appG.Response(http.StatusOK, constant.SUCCESS, vo)
}
