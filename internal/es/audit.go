package es

// audit.go 承载各编排共用的 dry-run 写操作审计层：auditedCluster 装饰器。
// cutover / rebuild / migrate 的「--dry-run 零写操作」都由这一个关口证明。
//
// 历史说明：原在 cutover.go，现按职责拆出（同包内纯搬家，零行为变化）。
// 能力面定义在 cluster.go，rebuild 专属原语的审计扩展 auditedRebuild 在 rebuild.go。

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// auditedCluster 记录每一次集群调用，并在 dry-run 下硬拒任何写操作。
//
// 这是「--dry-run 必须证明零写操作」的实现手段：不靠代码走查，而是让写路径
// 一旦被调用就立刻返回错误，同时把读调用清单原样打印出来。
type auditedCluster struct {
	inner  Cluster
	dryRun bool

	mu     sync.Mutex
	reads  []string
	writes []string
}

func newAuditedCluster(inner Cluster, dryRun bool) *auditedCluster {
	return &auditedCluster{inner: inner, dryRun: dryRun}
}

func (a *auditedCluster) note(kind, op string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if kind == "write" {
		if a.dryRun {
			return fmt.Errorf("es: dry-run 拒绝写操作 [%s]（本命令只允许 GET/HEAD）", op)
		}
		a.writes = append(a.writes, op)
		return nil
	}
	a.reads = append(a.reads, op)
	return nil
}

// ReadOps 返回只读调用清单（GET/HEAD），按发生顺序。
func (a *auditedCluster) ReadOps() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.reads...)
}

// WriteOps 返回写调用清单；dry-run 下恒为空即为「零写操作」的证据。
func (a *auditedCluster) WriteOps() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.writes...)
}

func (a *auditedCluster) IndexExists(ctx context.Context, idx string) (bool, error) {
	if err := a.note("read", "HEAD /"+idx); err != nil {
		return false, err
	}
	return a.inner.IndexExists(ctx, idx)
}

func (a *auditedCluster) AliasTargets(ctx context.Context, alias string) ([]string, error) {
	if err := a.note("read", "GET /_alias/"+alias); err != nil {
		return nil, err
	}
	return a.inner.AliasTargets(ctx, alias)
}

func (a *auditedCluster) ListAliasRows(ctx context.Context) ([]AliasTarget, error) {
	if err := a.note("read", "GET /_cat/aliases"); err != nil {
		return nil, err
	}
	return a.inner.ListAliasRows(ctx)
}

func (a *auditedCluster) ListPlugins(ctx context.Context) ([]string, error) {
	if err := a.note("read", "GET /_cat/plugins"); err != nil {
		return nil, err
	}
	return a.inner.ListPlugins(ctx)
}

func (a *auditedCluster) IndexStats(ctx context.Context, idx string) ([]IndexStat, error) {
	if err := a.note("read", "GET /_cat/indices/"+idx); err != nil {
		return nil, err
	}
	return a.inner.IndexStats(ctx, idx)
}

func (a *auditedCluster) Count(ctx context.Context, idx string) (int64, error) {
	if err := a.note("read", "GET /"+idx+"/_count"); err != nil {
		return 0, err
	}
	return a.inner.Count(ctx, idx)
}

func (a *auditedCluster) GetMapping(ctx context.Context, idx string) (map[string]any, error) {
	if err := a.note("read", "GET /"+idx+"/_mapping"); err != nil {
		return nil, err
	}
	return a.inner.GetMapping(ctx, idx)
}

func (a *auditedCluster) GetSettings(ctx context.Context, idx string) (map[string]any, error) {
	if err := a.note("read", "GET /"+idx+"/_settings"); err != nil {
		return nil, err
	}
	return a.inner.GetSettings(ctx, idx)
}

func (a *auditedCluster) CreateIndex(ctx context.Context, idx, bodyJSON string) error {
	if err := a.note("write", "PUT /"+idx); err != nil {
		return err
	}
	return a.inner.CreateIndex(ctx, idx, bodyJSON)
}

func (a *auditedCluster) PutSettings(ctx context.Context, idx, bodyJSON string) error {
	if err := a.note("write", "PUT /"+idx+"/_settings"); err != nil {
		return err
	}
	return a.inner.PutSettings(ctx, idx, bodyJSON)
}

func (a *auditedCluster) Refresh(ctx context.Context, idx string) error {
	// _refresh 是 POST，虽然不改动数据，但不属于「只读 GET 白名单」，dry-run 一律拦下。
	if err := a.note("write", "POST /"+idx+"/_refresh"); err != nil {
		return err
	}
	return a.inner.Refresh(ctx, idx)
}

func (a *auditedCluster) SwitchAlias(ctx context.Context, alias, target string) error {
	if err := a.note("write", "POST /_aliases"); err != nil {
		return err
	}
	return a.inner.SwitchAlias(ctx, alias, target)
}

func (a *auditedCluster) Perform(ctx context.Context, method, path, body string) (string, error) {
	write := !IsReadOnlyMethod(method)
	if err := a.note(opKind(write), strings.ToUpper(method)+" "+path); err != nil {
		return "", err
	}
	return a.inner.Perform(ctx, method, path, body)
}

func opKind(write bool) string {
	if write {
		return "write"
	}
	return "read"
}

// IsReadOnlyMethod 报告 HTTP 方法是否属于 dry-run 白名单（只允许 GET/HEAD）。
func IsReadOnlyMethod(method string) bool {
	switch strings.ToUpper(strings.TrimSpace(method)) {
	case "GET", "HEAD":
		return true
	default:
		return false
	}
}
