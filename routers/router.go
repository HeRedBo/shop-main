package routers

import (
	"github.com/gin-gonic/gin"
	"shop/middleware"
)

func InitRouter() *gin.Engine {
	r := gin.New()
	r.Use(gin.Logger())
	r.Use(gin.Recovery())
	r.Use(middleware.Cors())

	//r.StaticFS("/upload/images", http.Dir(upload.GetImageFullPath()))

	return r
}
