package dev

import (
	"fmt"
	"github.com/HeRedBo/pkg/httpclient"
	"github.com/HeRedBo/pkg/sign"
	"github.com/gookit/goutil/dump"
	"net/http"
	"net/url"
	"shop/pkg/constant"
	"testing"
	"time"
)

const GetOrdersHost = "http://127.0.0.1:8000"
const GetOrdersUri = "/dev/v1/orders/user"

var (
	ak  = "AK20220808327988"
	sk  = "xOBYfykyFVixXFziF8XN5F9crzpC0XrW"
	ttl = time.Minute * 3
)

func TestGetUserOrders(t *testing.T) {
	params := url.Values{}
	userID := "3"
	nextID := "0"
	params.Add("next_id", nextID)
	uri := GetOrdersUri + "/" + userID
	authorization, date, err := sign.New(ak, sk, ttl).Generate(uri, http.MethodGet, params)
	if err != nil {
		fmt.Println(err)
		return
	}
	headerAuth := httpclient.WithHeader(constant.HeaderAuthField, authorization)
	headerAuthDate := httpclient.WithHeader(constant.HeaderAuthDateField, date)
	c, r, e := httpclient.Get(GetOrdersHost+uri, params, headerAuth, headerAuthDate)
	dump.P(string(r))
	fmt.Println(c, string(r), e)
}
