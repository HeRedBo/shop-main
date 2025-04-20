package jwt

import (
	"github.com/dgrijalva/jwt-go"
	"shop/internal/models"
	"shop/internal/models/vo"
	"shop/pkg/global"
	"shop/pkg/logging"
	"strconv"
	"time"
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

func GenerateAppToken(m *models.ShopUser, d time.Duration) (string, error) {
	m.Password = ""
	//m.Permissions = []string{}
	//expireTime := time.Now().Add(d)
	stdClaims := jwt.StandardClaims{
		ExpiresAt: time.Now().Add(d).Unix(),
		Id:        strconv.FormatInt(m.Id, 10),
		Issuer:    "shopAppGo",
	}

	var jwtUser = vo.JwtUser{
		Id:       m.Id,
		Avatar:   m.Avatar,
		Username: m.Username,
		Phone:    m.Phone,
	}

	uClaims := userStdClaims{
		StandardClaims: stdClaims,
		JwtUser:        jwtUser,
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, uClaims)
	tokenString, err := token.SignedString(jwtSecret)
	if err != nil {
		logging.Error(err)
	}
	//set redis
	//var key = constant.AppRedisPrefixAuth + tokenString
	//json, _ := json.Marshal(m)
	//err = cache.GetRedisClient(cache.DefaultRedisClient).Set(key, json, d)
	//if err != nil {
	//	global.LOG.Error("GenerateAppToken cache set error", err, "key", key)
	//}

	return tokenString, err
}
