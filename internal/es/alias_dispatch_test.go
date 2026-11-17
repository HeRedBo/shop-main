package es

// alias_dispatch_test.go 钉住任务 #37 的编排侧契约：migrate 按别名逐条循环时，
// 注入的窄接口必须收到**当条 Definition 自己的别名**，而不是恒定的第一个别名。
//
// 这是「按别名分发」能否成立的前提：cmd/cli 的适配器拿 SyncFunc/CountFunc 的 alias 参数
// 去查「别名 → 数据源 / 口径」注册表。编排若把别名传丢（写死、传空、传 opts.Alias="all"），
// 多业务下每条循环都会落到同一个数据源 —— 本用例当场变红。

import (
	"context"
	"sort"
	"strings"
	"sync"
	"testing"
)

// TestRunMigrate_PassesPerDefinitionAliasToCallbacks 两条 active 声明一起跑（--alias all 语义），
// 断言灌数与计数回调各自收到的 alias 就是当条 Definition 的别名。
// 若有人把 internal/es 的调用点改回 opts.Sync(ctx, next) / opts.DBCount(ctx)（丢掉别名），
// 编译期就会红；改成传常量别名则本用例红。
func TestRunMigrate_PassesPerDefinitionAliasToCallbacks(t *testing.T) {
	defProduct := mustMigrateDef(t, "shop-product", 1, migSettingsJSON, migMapsJSON)
	defOrder := mustMigrateDef(t, "shop-order", 1, migSettingsJSON, migMapsJSON)

	spy := newFirstCreateSpy() // 两个别名都不存在 => 都走首建
	spy.counts[defProduct.Name()] = 5
	spy.counts[defOrder.Name()] = 5

	var mu sync.Mutex
	var syncPairs, countAliases []string

	opts := migrateOpts(mustRegistry(t, defProduct, defOrder), spy, false)
	opts.Sync = func(_ context.Context, alias, target string) (SyncSummary, error) {
		mu.Lock()
		syncPairs = append(syncPairs, alias+"->"+target)
		mu.Unlock()
		return SyncSummary{Scanned: 5, Sent: 5}, nil
	}
	opts.DBCount = func(_ context.Context, alias string) (int64, error) {
		mu.Lock()
		countAliases = append(countAliases, alias)
		mu.Unlock()
		return 5, nil
	}

	res, err := RunMigrate(context.Background(), opts)
	if err != nil {
		t.Fatalf("两条声明都应首建成功，got err=%v（结论 %+v）", err, res)
	}

	mu.Lock()
	gotSync := append([]string(nil), syncPairs...)
	gotCount := append([]string(nil), countAliases...)
	mu.Unlock()

	sort.Strings(gotSync)
	sort.Strings(gotCount)
	wantSync := []string{"shop-order->" + defOrder.Name(), "shop-product->" + defProduct.Name()}
	if strings.Join(gotSync, ",") != strings.Join(wantSync, ",") {
		t.Errorf("灌数回调必须逐条收到自己的别名与目标，got=%v want=%v", gotSync, wantSync)
	}
	if strings.Join(gotCount, ",") != "shop-order,shop-product" {
		t.Errorf("DB 计数回调必须逐条收到自己的别名（口径不能共享），got=%v", gotCount)
	}
}
