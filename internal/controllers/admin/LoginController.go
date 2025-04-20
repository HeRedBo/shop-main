package admin

import (
	"github.com/gin-gonic/gin"
	"github.com/gookit/goutil/dump"
	"github.com/mojocn/base64Captcha"
	"net/http"
	"shop/internal/models/dto"
	"shop/internal/models/vo"
	"shop/internal/service/user_service"
	"shop/pkg/app"
	"shop/pkg/constant"
	"shop/pkg/logging"
	"shop/pkg/util"
	"time"
)

type LoginController struct {
}

// 设置自带的store
var store = base64Captcha.DefaultMemStore

type CaptachaResult struct {
	Id          string `json:"id"`
	Base64Blob  string `json:"base_64_blob"`
	VerifyValue string `json:"code"`
}

func (e *LoginController) login(c *gin.Context) {
	var (
		authUser dto.AuthUser
		appG     = app.Gin{C: c}
	)

	httpCode, errCode := app.BindAndValid(c, &authUser)
	logging.Info(authUser)
	if errCode != constant.SUCCESS {
		appG.Response(httpCode, errCode, nil)
		return
	}
	userService := user_service.User{Username: authUser.Username}
	currentUser, err := userService.GetUserOneByName()
	if err != nil {
		appG.Response(http.StatusInternalServerError, constant.ERROR_NOT_EXIST_USER, nil)
		return
	}
	//校验验证码
	if !store.Verify(authUser.Id, authUser.Code, true) {
		appG.Response(http.StatusInternalServerError, constant.ERROR_CAPTCHA_USER, nil)
		return
	}
	if !util.ComparePwd(currentUser.Password, []byte(authUser.Password)) {
		appG.Response(http.StatusInternalServerError, constant.ERROR_PASS_USER, nil)
		return
	}
	token, _ := jwt.GenerateToken(currentUser, time.Hour*24*100)
	var loginVO = new(vo.LoginVo)
	loginVO.Token = token
	loginVO.User = currentUser
	appG.Response(http.StatusOK, constant.SUCCESS, loginVO)
	dump.P(authUser)

}
