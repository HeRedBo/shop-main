package jwt

import (
	"github.com/dgrijalva/jwt-go"
	"shop/internal/models/vo"
	"shop/pkg/global"
)

var jwtSecret []byte

const bearerLength = len("Bearer ")

var (
	ErrAbsent  = "token absent"  // 令牌不存在
	ErrInvalid = "token invalid" // 令牌无效
	ErrExpired = "token expired" // 令牌过期
	ErrOther   = "other error"   // 其他错误
)

type userStdClaims struct {
	vo.JwtUser
	//*models.User
	jwt.StandardClaims
}

func Init() {
	jwtSecret = []byte(global.CONFIG.App.JwtSecret)
}
