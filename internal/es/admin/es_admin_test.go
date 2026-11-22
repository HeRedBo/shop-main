package admin

import (
	"net/http"
	"strings"
	"testing"

	"shop/pkg/global"
)

// TestClientUninitialized 钉「未初始化」防御路径（纯单元测试，零外部依赖）：
// go test 不执行 main()/bootstrap，global.EsClient 天然是 nil。
// 此时 Client()/Raw() 必须返回带指引的 error 而非 panic，Close() 必须安全跳过。
func TestClientUninitialized(t *testing.T) {
	prev := global.EsClient
	global.EsClient = nil
	t.Cleanup(func() { global.EsClient = prev })

	c, err := Client()
	if err == nil {
		t.Fatal("未初始化时 Client() 必须返回 error")
	}
	if c != nil {
		t.Errorf("未初始化时 Client() 必须返回 nil 客户端，got %v", c)
	}
	if !strings.Contains(err.Error(), "未初始化") {
		t.Errorf("error 必须带初始化指引，got %q", err.Error())
	}

	if _, err := Raw(); err == nil {
		t.Error("未初始化时 Raw() 必须返回 error")
	}

	if err := Close(); err != nil {
		t.Errorf("未初始化时 Close() 应安全返回 nil，got %v", err)
	}
}

// TestClientInitialized 钉「已初始化」正常路径：
// 复用同包 indices_url_test.go 的假 ES + 全局注入工具，
// 不需要真 ES、不需要 bootstrap、不需要 config.yml。
func TestClientInitialized(t *testing.T) {
	cli, _ := newFakeESTool(t, http.StatusOK, `{}`)
	injectSharedESClient(t, cli)

	c, err := Client()
	if err != nil {
		t.Fatalf("已初始化时 Client() 不该报错: %v", err)
	}
	if c == nil || c.Client != cli {
		t.Fatal("Client() 必须原样返回注入的客户端")
	}

	raw, err := Raw()
	if err != nil {
		t.Fatalf("Raw() 不该报错: %v", err)
	}
	if raw != cli {
		t.Error("Raw() 必须返回底层 *elastic.Client 本体")
	}

	// 注入的 fake 客户端未注册进共享库 clients map，CloseAll 遍历空 map 是 no-op，安全。
	if err := Close(); err != nil {
		t.Errorf("Close() 不该报错: %v", err)
	}
}
