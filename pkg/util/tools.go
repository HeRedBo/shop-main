package util

import (
	"fmt"
	"golang.org/x/crypto/bcrypt"
	"strings"
)

// 加密
func HashAndSalt(pwd []byte) string {
	hash, err := bcrypt.GenerateFromPassword(pwd, bcrypt.MinCost)
	if err != nil {
		//controllers.ErrMsg(err.Error())
	}
	return string(hash)
}

// [a] -> a -> a
// [a b c] -> a b c -> a,b,c
func Convert(array interface{}) string {
	return strings.Replace(strings.Trim(fmt.Sprint(array), "[]"), " ", ",", -1)
}
