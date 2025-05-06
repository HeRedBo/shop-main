package routers

import (
	"net/http"
	"shop/internal/controllers/admin"
	"shop/internal/controllers/front"
	"shop/middleware"
	"shop/pkg/upload"

	"github.com/gin-gonic/gin"
)

func InitRouter() *gin.Engine {
	r := gin.New()
	r.Use(gin.Logger())
	r.Use(gin.Recovery())
	r.Use(middleware.Cors())

	r.StaticFS("/upload/images", http.Dir(upload.GetImageFullPath()))
	loginController := admin.LoginController{}
	r.POST("/auth/login", loginController.Login)
	r.GET("/auth/captcha", loginController.Captcha)
	// 集成 Swagger
	//r.GET("/swagger/*any", ginSwagger.WrapHandler(swaggerFiles.Handler))
	menuController := admin.MenuController{}
	userController := admin.UserController{}
	deptController := admin.DeptController{}
	dictController := admin.DictController{}
	roleController := admin.RoleController{}
	dictDetailController := admin.DictDetailController{}
	logController := admin.LogController{}
	jobController := admin.JobController{}
	materialController := admin.MaterialController{}
	materialGroupController := admin.MaterialGroupController{}
	canvasController := admin.CanvasController{}
	adminRouter := r.Group("/admin")
	adminRouter.Use(middleware.Jwt()).Use(middleware.Log())
	{

		adminRouter.GET("/auth/info", loginController.Info)
		adminRouter.DELETE("/auth/logout", loginController.Logout)

		adminRouter.GET("/material", materialController.GetAll)
		adminRouter.POST("/material", materialController.Post)
		adminRouter.PUT("/material", materialController.Put)
		adminRouter.DELETE("/material/:id", materialController.Delete)
		adminRouter.POST("/material/upload", materialController.Upload)

		adminRouter.GET("/materialgroup", materialGroupController.GetAll)
		adminRouter.POST("/materialgroup", materialGroupController.Post)
		adminRouter.PUT("/materialgroup", materialGroupController.Put)
		adminRouter.DELETE("/materialgroup/:id", materialGroupController.Delete)
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
		// region 角色管理模块
		adminRouter.GET("/roles/:id", roleController.GetOne)
		adminRouter.GET("/roles", roleController.GetAll)
		adminRouter.POST("/roles", roleController.Post)
		adminRouter.PUT("/roles", roleController.Put)
		adminRouter.DELETE("/roles", roleController.Delete)
		adminRouter.PUT("/roles/menu", roleController.Menu)
		// endregion
		// region 数据字典详情模块
		adminRouter.GET("/dictDetail", dictDetailController.GetAll)
		adminRouter.POST("/dictDetail", dictDetailController.Post)
		adminRouter.PUT("/dictDetail", dictDetailController.Put)
		adminRouter.DELETE("/dictDetail/:id", dictDetailController.Delete)
		// endregion
		// region 工作模块
		adminRouter.GET("/job", jobController.GetAll)
		adminRouter.POST("/job", jobController.Post)
		adminRouter.PUT("/job", jobController.Put)
		adminRouter.DELETE("/job", jobController.Delete)
		// endregion

		// 日志模块
		adminRouter.GET("/logs", logController.GetAll)
		adminRouter.DELETE("/logs", logController.Delete)
		// endregion

		adminRouter.GET("/menu/build", menuController.Build)
		adminRouter.GET("/menu/listtree", menuController.GetTree)
		adminRouter.GET("/menu", menuController.GetAll)
		adminRouter.POST("/menu", menuController.Post)
		adminRouter.PUT("/menu", menuController.Put)
		adminRouter.DELETE("/menu", menuController.Delete)

		// region 画布模块
		adminRouter.GET("/canvas/getCanvas", canvasController.Get)
		adminRouter.POST("/canvas/saveCanvas", canvasController.Post)
		// endregion
	}
	cateController := admin.StoreCategoryController{}
	ruleController := admin.StoreProductRuleController{}
	productController := admin.StoreProductController{}
	orderController := admin.OrderController{}
	expressController := admin.ExpressController{}
	shopRouter := r.Group("/shop")
	shopRouter.Use(middleware.Jwt()).Use(middleware.Log())
	{
		// region 商品分类
		shopRouter.GET("/cate", cateController.GetAll)
		shopRouter.POST("/cate", cateController.Post)
		shopRouter.PUT("/cate", cateController.Put)
		shopRouter.DELETE("/cate", cateController.Delete)
		// endregion
		// region 商品分类
		shopRouter.GET("/product", productController.GetAll)
		shopRouter.GET("/product/info/:id", productController.GetInfo)
		shopRouter.POST("/product/isFormatAttr/:id", productController.FormatAttr)
		shopRouter.POST("/product/addOrSave", productController.Post)
		shopRouter.POST("/product/onsale/:id", productController.OnSale)
		shopRouter.DELETE("/product/:id", productController.Delete)
		// endregion
		shopRouter.GET("/order", orderController.GetAll)
		shopRouter.POST("/order/save/:id", orderController.Post)
		shopRouter.DELETE("/order/:id", orderController.Delete)
		shopRouter.POST("/order/remark", orderController.Put)
		shopRouter.PUT("/order", orderController.Deliver)
		//shopRouter.POST("/order/express", orderController.DeliverQuery)

		// region 快递模块
		shopRouter.GET("/express", expressController.GetAll)
		shopRouter.POST("/express", expressController.Post)
		shopRouter.PUT("/express", expressController.Put)
		shopRouter.DELETE("/express/:id", expressController.Delete)
		// endregion
		// region 商品规则值(规格)模块
		shopRouter.GET("/rule", ruleController.GetAll)
		shopRouter.POST("/rule/save/:id", ruleController.Post)
		shopRouter.DELETE("/rule", ruleController.Delete)
		// endregion
	}

	wechatMenuController := admin.WechatMenuController{}
	wechatUserController := admin.WechatUserController{}
	articleController := admin.ArticleController{}
	wechatRouter := r.Group("/weixin")
	wechatRouter.Use(middleware.Jwt()).Use(middleware.Log())
	{
		// region 微信菜单
		wechatRouter.GET("/menu", wechatMenuController.GetAll)
		wechatRouter.POST("/menu", wechatMenuController.Post)
		// endregion
		// region 微信用户
		wechatRouter.GET("/user", wechatUserController.GetAll)
		wechatRouter.PUT("/user", wechatUserController.Put)
		wechatRouter.POST("/user/money", wechatUserController.Money)
		// endregion
		// region 微信文章模块
		wechatRouter.GET("/article", articleController.GetAll)
		wechatRouter.POST("/article", articleController.Post)
		wechatRouter.PUT("/article", articleController.Put)
		wechatRouter.DELETE("/article/:id", articleController.Delete)
		wechatRouter.GET("/article/info/:id", articleController.Get)
		wechatRouter.GET("/article/publish/:id", articleController.Pub)
		// endregion
	}

	// region 用户端API

	// region 用户端-非授权
	ApiLoginController := new(front.LoginController)
	ApiIndexController := new(front.IndexController)
	ApiCategoryController := new(front.CategoryController)
	ApiProductController := new(front.ProductController)
	apiv1 := r.Group("/api/v1")
	{
		// region 授权模块
		apiv1.POST("/login", ApiLoginController.Login)
		apiv1.POST("/register", ApiLoginController.Reg)
		apiv1.POST("/register/verify", ApiLoginController.Verify)
		// endregion

		// region 首页部分
		apiv1.GET("/index", ApiIndexController.GetIndex)
		apiv1.POST("/getCanvas", ApiIndexController.GetCanvas)
		apiv1.POST("/upload", ApiIndexController.Upload)
		// endregion
		// region 分类
		apiv1.GET("/category", ApiCategoryController.GetCateList)
		// endregion
		// region 产品部分
		apiv1.GET("/products", ApiProductController.GoodsList)
		// apiv1.GET("/product/search", ApiProductController.GoodsSearch)
		apiv1.GET("/product/detail/:id", ApiProductController.GoodDetail)
		apiv1.GET("/product/hot", ApiProductController.GoodsRecommendList)
		apiv1.GET("/reply/list/:id", ApiProductController.ReplyList)
		// endregion
	}
	// endregion

	// region 需要授权接口
	ApiUserController := new(front.UserController)
	ApiCartController := new(front.CartController)
	authApiv1 := r.Group("/api/v1").Use(middleware.AppJwt())
	{
		// region 用户模块
		authApiv1.GET("/userinfo", ApiUserController.GetUerInfo)
		authApiv1.GET("/collect/user", ApiUserController.CollectUser)
		// endregion

		authApiv1.POST("/collect/add", ApiProductController.AddCollect)
		authApiv1.POST("/collect/del", ApiProductController.DelCollect)

		// region 购物车
		authApiv1.POST("/cart/add", ApiCartController.AddCart)
		authApiv1.GET("/cart/count", ApiCartController.Count)
		authApiv1.GET("/carts", ApiCartController.CartList)
		authApiv1.POST("/cart/num", ApiCartController.CartNum)
		authApiv1.POST("/cart/del", ApiCartController.DelCart)
		// endregion

	}
	// endregion
	return r
}
