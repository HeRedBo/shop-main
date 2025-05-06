package front

import (
	"github.com/gin-gonic/gin"
	"net/http"
	"shop/internal/params"
	"shop/internal/service/wechat_user_service"
	"shop/pkg/app"
	"shop/pkg/constant"
	"shop/pkg/jwt"
	"time"
)

// LoginController 登录API
type LoginController struct{}

// Login
// @Title 登录
// @Description 登录
// @Success 200 {object} app.Response
// @router /admin/login [post]
func (e *LoginController) Login(c *gin.Context) {
	var (
		param params.HLoginParam
		appG  = app.Gin{C: c}
	)
	paramErr := app.BindAndValidate(c, &param)
	if paramErr != nil {
		appG.Response(http.StatusBadRequest, paramErr.Error(), nil)
		return
	}

	userService := wechat_user_service.User{HLoginParam: &param}
	user, err := userService.HLogin()
	if err != nil {
		appG.Response(http.StatusInternalServerError, err.Error(), nil)
		return
	}

	ttl := time.Hour * 24 * 100
	token, _ := jwt.GenerateAppToken(user, ttl)
	appG.Response(http.StatusOK, constant.SUCCESS, gin.H{
		"token":        token,
		"expires_time": time.Now().Add(ttl).Unix(),
	})
}

// Verify
// @Title 短信验证码
// @Description 短信验证码
// @Success 200 {object} app.Response
// @router /register/verity [post]
func (e *LoginController) Verify(c *gin.Context) {
	var (
		param params.VerityParam
		appG  = app.Gin{C: c}
	)

	paramErr := app.BindAndValidate(c, &param)
	if paramErr != nil {
		appG.Response(http.StatusBadRequest, paramErr.Error(), nil)
		return
	}
	userService := wechat_user_service.User{VerityParam: &param}
	str, err := userService.Verify()
	if err != nil {
		appG.Response(http.StatusInternalServerError, err.Error(), nil)
		return
	}
	appG.Response(http.StatusOK, constant.SUCCESS, str)
}

// Reg
// @Title 注册
// @Description 注册
// @Success 200 {object} app.Response
// @router /admin/login [post]
func (e *LoginController) Reg(c *gin.Context) {
	var (
		param params.RegParam
		appG  = app.Gin{C: c}
	)

	paramErr := app.BindAndValidate(c, &param)
	if paramErr != nil {
		appG.Response(http.StatusBadRequest, paramErr.Error(), nil)
		return
	}

	userService := wechat_user_service.User{RegParam: &param, Ip: c.ClientIP()}
	if err := userService.Reg(); err != nil {
		appG.Response(http.StatusInternalServerError, err.Error(), nil)
		return
	}
	appG.Response(http.StatusOK, constant.SUCCESS, "success")
}

// Info
// @Title 获取用户信息
// @Description 获取用户信息
// @Success 200 {object} app.Response
// @router /info [get]
func (e *LoginController) Info(c *gin.Context) {
	var (
		appG = app.Gin{C: c}
	)
	appG.Response(http.StatusOK, constant.SUCCESS, jwt.GetAdminDetailUser(c))
}

// Logout
// @Title 退出登录
// @Description 退出登录
// @Success 200 {object} app.Response
// @router /logout [delete]
func (e *LoginController) Logout(c *gin.Context) {
	var (
		appG = app.Gin{C: c}
	)
	err := jwt.RemoveUser(c)
	if err != nil {
		appG.Response(http.StatusInternalServerError, constant.FAIL_LOGOUT_USER, nil)
		return
	}

	appG.Response(http.StatusOK, constant.SUCCESS, nil)
}
