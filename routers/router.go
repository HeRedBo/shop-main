package routers

import (
	"github.com/gin-gonic/gin"
	"shop/internal/controllers/admin"
	"shop/middleware"
)

func InitRouter() *gin.Engine {
	r := gin.New()
	r.Use(gin.Logger())
	r.Use(gin.Recovery())
	r.Use(middleware.Cors())

	//r.StaticFS("/upload/images", http.Dir(upload.GetImageFullPath()))
	loginController := admin.LoginController{}
	r.POST("/auth/login", loginController.Login)
	r.GET("/auth/captcha", loginController.Captcha)
	adminRouter := r.Group("/admin")
	adminRouter.Use(middleware.Jwt())
	{
		adminRouter.GET("/auth/info", loginController.Info)
		adminRouter.DELETE("/auth/logout", loginController.Logout)
	}
	return r
}
