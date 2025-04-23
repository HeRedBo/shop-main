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
	userController := admin.UserController{}
	deptController := admin.DeptController{}
	dictController := admin.DictController{}
	dictDetailController := admin.DictDetailController{}
	logController := admin.LogController{}
	jobController := admin.JobController{}
	adminRouter := r.Group("/admin")
	adminRouter.Use(middleware.Jwt())
	{
		adminRouter.GET("/auth/info", loginController.Info)
		adminRouter.DELETE("/auth/logout", loginController.Logout)

		// region 用户中心模块
		adminRouter.GET("/user", userController.GetAll)
		adminRouter.POST("/user", userController.Post)
		adminRouter.PUT("/user", userController.Put)
		adminRouter.DELETE("/user", userController.Delete)
		adminRouter.PUT("/user/center", userController.Center)
		adminRouter.POST("/user/updatePass/", userController.Pass)
		adminRouter.POST("/user/updateAvatar", userController.Avatar)
		// endregion

		// region 部门模块
		adminRouter.GET("/dept", deptController.GetAll)
		adminRouter.POST("/dept", deptController.Post)
		adminRouter.PUT("/dept", deptController.Put)
		adminRouter.DELETE("/dept", deptController.Delete)
		// endregion

		// region 数据字典模块
		adminRouter.GET("/dict", dictController.GetAll)
		adminRouter.POST("/dict", dictController.Post)
		adminRouter.PUT("/dict", dictController.Put)
		adminRouter.DELETE("/dict/:id", dictController.Delete)
		// endregion

		// region 数据字典详情模块
		adminRouter.GET("/dictDetail", dictDetailController.GetAll)
		adminRouter.POST("/dictDetail", dictDetailController.Post)
		adminRouter.PUT("/dictDetail", dictDetailController.Put)
		adminRouter.DELETE("/dictDetail/:id", dictDetailController.Delete)
		// endregion

		adminRouter.GET("/job", jobController.GetAll)
		adminRouter.POST("/job", jobController.Post)
		adminRouter.PUT("/job", jobController.Put)
		adminRouter.DELETE("/job", jobController.Delete)

		// 日志模块
		adminRouter.GET("/logs", logController.GetAll)
		adminRouter.DELETE("/logs", logController.Delete)
		// endregion
	}

	expressController := admin.ExpressController{}
	shopRouter := r.Group("/shop")
	shopRouter.Use(middleware.Jwt())
	{
		// region 快递模块
		shopRouter.GET("/express", expressController.GetAll)
		shopRouter.POST("/express", expressController.Post)
		shopRouter.PUT("/express", expressController.Put)
		shopRouter.DELETE("/express/:id", expressController.Delete)
		// endregion
	}
	return r
}
