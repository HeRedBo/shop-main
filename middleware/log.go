package middleware

import (
	"github.com/gin-gonic/gin"
	"regexp"
	"shop/internal/models"
	"shop/pkg/global"
	"shop/pkg/jwt"
	"strings"
	"time"
)

func Log() gin.HandlerFunc {
	return func(c *gin.Context) {
		url := c.Request.URL.Path
		global.GetLogger("http").Infof("request: %s", url)
		method := strings.ToLower(c.Request.Method)
		user, err := jwt.GetAdminUser(c)
		if err != nil {
			global.GetLogger("http").Errorf("GetAdminUser error: %v", err)
		}
		if err != nil {
			c.Next()
			return
		}

		reg := regexp.MustCompile(`[0-9]+`)
		newUrl := reg.ReplaceAllString(url, "*")
		menu := models.FindMenuByRouterAndMethod(newUrl, method)
		log := models.SysLog{
			Description: menu.Name,
			Method:      method,
			RequestIp:   c.ClientIP(),
			Username:    user.Username,
			Address:     newUrl,
			Browser:     "",
			Type:        0,
			Uid:         user.Id,
		}
		now := time.Now()
		c.Next()
		consume := time.Now().Sub(now)
		log.Time = consume.Microseconds()
		models.AddLog(&log)
	}
}
