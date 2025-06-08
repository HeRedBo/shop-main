package dev

import (
	"github.com/gin-gonic/gin"
	"github.com/unknwon/com"
	"net/http"
	"shop/internal/service/order_service"
	"shop/pkg/app"
	"shop/pkg/constant"
	"shop/pkg/util"
)

// OrderController order api for developer
type OrderController struct{}

// GetUserOrders @Title 用户全部订单
// @Description 分批获取用户全部订单
// @Success 200 {object} app.Response
// @router / [get]
func (e *OrderController) GetUserOrders(c *gin.Context) {
	var (
		appG = app.Gin{C: c}
	)
	nextID := com.StrTo(c.DefaultQuery("next_id", "0")).MustInt64()
	userID := com.StrTo(c.Param("uid")).MustInt64()
	if userID <= 0 {
		appG.Response(http.StatusBadRequest, constant.INVALID_PARAMS, nil)
		return
	}
	//TODO 用户存在性校验？
	orderService := order_service.Order{
		PageSize: util.GetSize(c),
		Uid:      userID,
	}
	vo := orderService.GetUseCursor(nextID)
	appG.Response(http.StatusOK, constant.SUCCESS, vo)
}
