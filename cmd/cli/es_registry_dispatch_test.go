package main

// es_registry_dispatch_test.go 钉住任务 #37 的装配层契约：migrate / rebuild / cutover
// 三个命令的灌数、计数、差量补偿注入点都必须**按别名查表**，而不是恒调商品实现。
//
// 判别方式：拿一个「已登记声明但未接线 Provider」的别名（shop-order）直接调适配器。
// 查表实现会在触达 MySQL/ES 之前拒绝（单测环境没有 DB/ES，拒绝信息本身就是证据）；
// 一旦有人把它改回 sync.SyncProducts / sync.Compensate / es.ProductDocCount，报错就会
// 变成「MySQL 未初始化」之类的下游故障 —— 本用例变红。

import (
	"context"
	"strings"
	"testing"
	"time"

	"shop/internal/es/sync"
)

// 未接线的别名：既不在 providers，也不在 compensators / dbCounters。
const unwiredTestAlias = "shop-order"

func TestIngestAdapters_DispatchByAlias(t *testing.T) {
	cases := []struct {
		name     string
		run      func(ctx context.Context) error
		contains string
	}{
		{
			name: "migrate 灌数",
			run: func(ctx context.Context) error {
				_, err := newMigrateSyncFunc(migrateSpec{})(ctx, unwiredTestAlias, unwiredTestAlias+"_v1")
				return err
			},
			contains: "尚未接线数据源 Provider",
		},
		{
			name: "rebuild 灌数",
			run: func(ctx context.Context) error {
				_, err := newRebuildSyncFunc(rebuildSpec{})(ctx, unwiredTestAlias, unwiredTestAlias+"_v1")
				return err
			},
			contains: "尚未接线数据源 Provider",
		},
		{
			name: "cutover 灌数",
			run: func(ctx context.Context) error {
				_, err := newCutoverSyncFunc(cutoverSpec{})(ctx, unwiredTestAlias, unwiredTestAlias+"_v1")
				return err
			},
			contains: "尚未接线数据源 Provider",
		},
		{
			name: "migrate 补偿",
			run: func(ctx context.Context) error {
				_, err := newMigrateCompensateFunc(migrateSpec{})(ctx, unwiredTestAlias, unwiredTestAlias+"_v1", time.Unix(0, 0))
				return err
			},
			contains: "尚未接线差量补偿实现",
		},
		{
			name: "rebuild 补偿",
			run: func(ctx context.Context) error {
				_, err := newRebuildCompensateFunc(rebuildSpec{})(ctx, unwiredTestAlias, unwiredTestAlias+"_v1", time.Unix(0, 0))
				return err
			},
			contains: "尚未接线差量补偿实现",
		},
		{
			name: "cutover 补偿",
			run: func(ctx context.Context) error {
				_, err := newCutoverCompensateFunc(cutoverSpec{})(ctx, unwiredTestAlias, unwiredTestAlias+"_v1", time.Unix(0, 0))
				return err
			},
			contains: "尚未接线差量补偿实现",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run(context.Background())
			if err == nil {
				t.Fatal("未接线别名必须被拒，却返回成功（缺陷回归：注入点恒调商品实现）")
			}
			if !strings.Contains(err.Error(), tc.contains) {
				t.Errorf("报错必须来自注册表判定（%s），got=%q", tc.contains, err.Error())
			}
			if !strings.Contains(err.Error(), unwiredTestAlias) {
				t.Errorf("报错必须点名被拒别名，便于定位是哪条循环，got=%q", err.Error())
			}
		})
	}
}

// TestDBCountInjectionIsAliasScoped：三个命令注入的计数回调都必须是「按别名查表」的同一个
// 口径表实现（sync.DBCountByAlias），未登记别名一律拒绝，绝不回落商品口径。
func TestDBCountInjectionIsAliasScoped(t *testing.T) {
	if _, err := sync.DBCountByAlias(context.Background(), unwiredTestAlias); err == nil ||
		!strings.Contains(err.Error(), "未登记 DB 计数口径") {
		t.Errorf("未登记别名必须拒绝统计，got=%v", err)
	}
	if _, ok := sync.DBCounterFor(sync.ProductAlias); !ok {
		t.Errorf("商品别名必须登记 DB 计数口径（es:status 与对账共用同一张表）")
	}
}

// TestIngestProgressDecisionTable：进度条只在「TTY 且非 --json」时构造。
// progressbar v3.19.1 不做 TTY 检测也不降级，判定写反就会在 CI / 管道里往 stdout 刷控制字符。
func TestIngestProgressDecisionTable(t *testing.T) {
	cases := []struct{ quiet, tty, want bool }{
		{quiet: false, tty: true, want: true},   // 人在终端前看：渲染
		{quiet: true, tty: true, want: false},   // --json：stdout 必须是可解析结论
		{quiet: false, tty: false, want: false}, // 管道 / 重定向 / CI
		{quiet: true, tty: false, want: false},
	}
	for _, tc := range cases {
		if got := ingestProgressEnabled(tc.quiet, tc.tty); got != tc.want {
			t.Errorf("ingestProgressEnabled(quiet=%v,tty=%v)=%v，期望 %v", tc.quiet, tc.tty, got, tc.want)
		}
	}
}

// TestStartIngestProgressIsSafeWhenSuppressed：被抑制时返回的关闭函数必须非空且幂等，
// 调用点可以无条件 defer（否则三处注入点各要再加一层 if，正是历史上「数据在跑画面不动」的成因）。
func TestStartIngestProgressIsSafeWhenSuppressed(t *testing.T) {
	shutdown := startIngestProgress(nil, "ES 灌数(shop-product)", true)
	if shutdown == nil {
		t.Fatal("抑制路径也必须返回可 defer 的关闭函数")
	}
	shutdown()
	shutdown() // 幂等：重复 defer / 重复调用不得 panic
}
