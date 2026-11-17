package es

// rebuild_test.go 只钉蓝绿重建「调用点破坏必须变红」的六条锚点（方案 7.8）：
//   ① 对账（Refresh + Count）必须发生在 POST /_aliases **之前** —— 删掉对账检查，
//      用例立刻红（不达标就切别名 = 线上读到空索引）；
//   ② 切换必须**单请求**原子下发 add + remove，且绝不出现 remove_index —— 拆两步、
//      或误用 cutover 的 remove_index，httptest 记录层立刻红；
//   ③ 差量补偿必须在切换之后、下界取本次任务首次启动时刻 —— 删调用点或改成
//      time.Now()，补偿回调里取样的别名状态与 since 双双对不上；
//   ④ --dry-run 零写操作 —— 放行任何一条写就红；
//   ⑤（必修③）对账容差归本层判定 —— 把 --tolerance 下发给内层灌数（或删掉本层闸门），
//      中止步骤会错报成 sync、ES 计数退成没回读的假 0，用例立刻红。
//   ⑥（必修②）补偿统计必须带出 —— 回调只返回 error 或吞掉 Failed>0，报表会把
//      没收敛的窗口增量写成成功，用例立刻红。
//
// 断言一律打在**调用序列与状态**上（别名此刻指向谁、旧索引还在不在），不打调用次数，
// 免得把 bug 固化成期望。全部用例不连真 ES / MySQL。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/olivere/elastic/v7"
)

// ---------------------------------------------------------------------------
// 夹具：fakeCluster + rebuild 两个专属原语
// ---------------------------------------------------------------------------

// fakeRebuildTarget 给 fakeCluster 补上 RebuildOps，并按真机语义改状态：
// SwitchAliasKeepOld 只把别名改挂到 next，**cur 索引与它的数据原样保留**
// （与 fakeCluster.SwitchAlias 的 remove_index 正好相反，两条路径必须能被区分开）。
type fakeRebuildTarget struct {
	*fakeCluster

	keepOldBodies []string // 每次切换实际下发的 _aliases 请求体（语义比对用）
	deleted       []string // DeleteIndex 记名
	keepOldErr    error
	deleteErr     error
}

func newFakeRebuildTarget() *fakeRebuildTarget {
	return &fakeRebuildTarget{fakeCluster: newFakeCluster()}
}

func (f *fakeRebuildTarget) SwitchAliasKeepOld(ctx context.Context, alias, next, cur string) error {
	if err := f.enter("POST /_aliases", ctx); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.keepOldErr != nil {
		return f.keepOldErr
	}
	if !f.indexes[next] {
		return fmt.Errorf("fake: 目标 %s 不存在，无法加别名", next)
	}
	if !f.indexes[cur] {
		return fmt.Errorf("fake: 旧索引 %s 不存在，无从解除别名", cur)
	}
	f.keepOldBodies = append(f.keepOldBodies, fmt.Sprintf("%s|%s|%s", alias, next, cur))
	f.aliasRows[alias] = []AliasTarget{{Alias: alias, Index: next, WriteIndex: "true"}}
	// cur 与其 counts 刻意不动：蓝绿保留旧索引，这正是回滚退路。
	return nil
}

func (f *fakeRebuildTarget) DeleteIndex(ctx context.Context, idx string) error {
	if err := f.enter("DELETE /"+idx, ctx); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteErr != nil {
		return f.deleteErr
	}
	delete(f.indexes, idx)
	delete(f.counts, idx)
	f.deleted = append(f.deleted, idx)
	return nil
}

// aliasIndexOf 取样「此刻别名指向谁」，用于在回调内部证明时序（不加锁会与编排写状态竞争）。
func (f *fakeRebuildTarget) aliasIndexOf(alias string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if rows := f.aliasRows[alias]; len(rows) == 1 {
		return rows[0].Index
	}
	return ""
}

func (f *fakeRebuildTarget) holds(alias, idx string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.aliasRows[alias] {
		if r.Index == idx {
			return true
		}
	}
	return false
}

func (f *fakeRebuildTarget) indexExists(idx string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.indexes[idx]
}

func opsHave(ops []string, sub string) bool {
	for _, o := range ops {
		if strings.Contains(o, sub) {
			return true
		}
	}
	return false
}

func opPos(ops []string, sub string) int {
	for i, o := range ops {
		if strings.Contains(o, sub) {
			return i
		}
	}
	return -1
}

// newRebuildFixture 造一个「别名已唯一指向 <alias>_v<curV>、旧索引 5 篇文档」的可重建现场。
func newRebuildFixture(t *testing.T, alias string, curV, nextV int) (*fakeRebuildTarget, *IndexDefinition) {
	t.Helper()
	def := mustCutoverDef(t, alias, nextV)
	cur := fmt.Sprintf("%s_v%d", alias, curV)
	f := newFakeRebuildTarget()
	f.indexes[cur] = true
	f.counts[cur] = 5
	f.aliasRows[alias] = []AliasTarget{{Alias: alias, Index: cur, WriteIndex: "true"}}
	return f, def
}

// recordingAliasTool 起一台按方法分派的假 ES：POST /_aliases 恒回 acknowledged，
// 其它（AtomicSwitchAlias 的持有性预检 GET）回「cur 确实持有 alias」。
// 记录同样要加锁：handler 跑在独立 goroutine，-race 下裸 slice 会报数据竞争。
func recordingAliasTool(t *testing.T) (*elastic.Client, func() []recordedReq) {
	t.Helper()
	var (
		mu   sync.Mutex
		reqs []recordedReq
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		reqs = append(reqs, recordedReq{method: r.Method, path: r.URL.Path, body: string(raw)})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodPost {
			_, _ = w.Write([]byte(`{"acknowledged":true}`))
			return
		}
		_, _ = w.Write([]byte(`{"shop-product_v1":{"aliases":{"shop-product":{}}}}`))
	}))
	t.Cleanup(srv.Close)

	cli, err := elastic.NewClient(
		elastic.SetURL(srv.URL),
		elastic.SetSniff(false),
		elastic.SetHealthcheck(false),
		elastic.SetMaxRetries(0),
	)
	if err != nil {
		t.Fatalf("构造测试用 ES 客户端失败: %v", err)
	}
	return cli, func() []recordedReq {
		mu.Lock()
		defer mu.Unlock()
		return append([]recordedReq(nil), reqs...)
	}
}

// ---------------------------------------------------------------------------
// ① 对账闸门必须压在 _aliases 之前
// ---------------------------------------------------------------------------

func TestRunRebuild_ReconcileGatePrecedesAliasSwitch(t *testing.T) {
	const alias = "shop-product"
	f, def := newRebuildFixture(t, alias, 1, 2)
	next := def.Name()
	f.counts[next] = 999 // 灌完数与 DB 期望严重不符

	compensated := false
	res, err := RunRebuild(context.Background(), RebuildOptions{
		Alias:      alias,
		Definition: def,
		Target:     f,
		Sync:       cutoverSync(SyncSummary{Scanned: 999, Sent: 999}, nil),
		Compensate: func(context.Context, string, string, time.Time) (CompensateSummary, error) {
			compensated = true
			return CompensateSummary{}, nil
		},
		DBCount:          fixedCount(5),
		Tolerance:        0,
		KeepNewOnFailure: true,
		Logf:             silentLogf,
	})
	if err == nil {
		t.Fatalf("对账不达标必须中止，实际通过： %+v", res)
	}
	if !strings.Contains(err.Error(), "对账") {
		t.Errorf("报错应点名对账，got=%v", err)
	}
	if res.Reconciled || res.Switched || compensated {
		t.Errorf("对账失败后不得继续任何后续步骤，got reconciled=%v switched=%v", res.Reconciled, res.Switched)
	}
	if res.AbortedStep != "reconcile" {
		t.Errorf("中止步骤应为 reconcile，got=%q", res.AbortedStep)
	}

	// 破坏即红的核心：删掉对账检查 → 这里必然出现 _aliases 请求。
	ops := f.allOps()
	if opsHave(ops, "/_aliases") {
		t.Errorf("对账不达标时绝不能下发 _aliases（切换必须排在后面对账之后）：%v", ops)
	}
	if got := f.aliasIndexOf(alias); got != alias+"_v1" {
		t.Errorf("别名必须仍唯一指向旧索引（线上读不受影响），got=%q", got)
	}
	// 时序证据：Refresh 与 Count 都发生在任何别名变更之前，且 Count 之后编排就断了。
	if rp, cp := opPos(ops, "/_refresh"), opPos(ops, "/"+next+"/_count"); rp < 0 || cp < 0 || rp > cp {
		t.Errorf("必须先 Refresh 再 Count 对账，ops=%v", ops)
	}
	if !f.indexExists(next) || len(f.orphanOnFail(res, next)) == 0 {
		t.Errorf("--keep-new-on-failure 下应保留半成品新索引并登记孤儿，orphans=%v", res.Orphans)
	}
}

// orphanOnFail 把「失败路径上留下的新索引」读成清单，供断言（不改变结果对象）。
func (f *fakeRebuildTarget) orphanOnFail(res *RebuildResult, next string) []string {
	if res != nil && len(res.Orphans) > 0 {
		return res.Orphans
	}
	if f.indexExists(next) {
		return []string{next}
	}
	return nil
}

// ---------------------------------------------------------------------------
// ② 单请求原子切换（httptest 证明生产下发形态）
// ---------------------------------------------------------------------------

func TestAdminRebuildSwitchAliasKeepOldSendsSingleAliasesRequest(t *testing.T) {
	cli, seen := recordingAliasTool(t)
	injectSharedESClient(t, cli)

	if err := (AdminRebuild{}).SwitchAliasKeepOld(
		context.Background(), "shop-product", "shop-product_v2", "shop-product_v1"); err != nil {
		t.Fatalf("SwitchAliasKeepOld 报错: %v", err)
	}

	reqs := seen()
	var posts []recordedReq
	for _, r := range reqs {
		if r.method == http.MethodPost {
			posts = append(posts, r)
		}
	}
	// 拆成「先 remove 再 add」两步会在这里现形：写请求必须恰好 1 个，且就在 /_aliases 上。
	if len(posts) != 1 || posts[0].path != "/_aliases" {
		t.Fatalf("必须恰好一次 POST /_aliases（拆两步会留下 ES 自动建同名空索引的黑洞），实际写请求=%s",
			describeReqs(posts))
	}

	byKey := actionsByKey(t, posts[0].body)
	if _, bad := byKey["remove_index"]; bad {
		t.Errorf("蓝绿严禁 remove_index：它会把旧索引连数据一起摘掉，回滚退路就此消失：%s", posts[0].body)
	}
	if len(byKey) != 2 {
		t.Errorf("同一 body 里只能有 add + remove 两个 action，got=%s", posts[0].body)
	}
	add, ok := byKey["add"]
	if !ok || add["index"] != "shop-product_v2" || add["alias"] != "shop-product" {
		t.Errorf("add 必须把别名挂到新索引：%#v", byKey)
	}
	rem, ok := byKey["remove"]
	if !ok || rem["index"] != "shop-product_v1" || rem["alias"] != "shop-product" {
		t.Errorf("remove 必须只解除旧索引与别名的关联：%#v", byKey)
	}
	// 真下发体必须与 --dry-run 打印的计划体同源同义。
	want, err := RebuildSwitchBody("shop-product", "shop-product_v2", "shop-product_v1")
	if err != nil {
		t.Fatalf("RebuildSwitchBody 报错: %v", err)
	}
	if got, exp := byKey, actionsByKey(t, want); !reflect.DeepEqual(got, exp) {
		t.Errorf("实际下发体与计划体不一致\n  实际: %#v\n  计划: %#v", got, exp)
	}
}

// ---------------------------------------------------------------------------
// ③ 差量补偿排在切换之后，下界＝本次任务首次启动时刻
// ---------------------------------------------------------------------------

func TestRunRebuild_CompensateRunsAfterSwitchWithStartedAtLowerBound(t *testing.T) {
	const alias = "shop-product"
	f, def := newRebuildFixture(t, alias, 1, 2)
	next := def.Name()
	cur := alias + "_v1"
	f.counts[next] = 5

	var (
		aliasAtComp  string
		targetAtComp string
		sinceAtComp  time.Time
		nowAtComp    time.Time
		ran          bool
	)
	res, err := RunRebuild(context.Background(), RebuildOptions{
		Alias:      alias,
		Definition: def,
		Target:     f,
		Sync:       cutoverSync(SyncSummary{Scanned: 5, Sent: 5}, nil),
		Compensate: func(_ context.Context, alias, target string, since time.Time) (CompensateSummary, error) {
			ran = true
			aliasAtComp, targetAtComp, sinceAtComp = f.aliasIndexOf(alias), target, since
			nowAtComp = time.Now()
			return CompensateSummary{Scanned: 2, Indexed: 1, Deleted: 1}, nil
		},
		DBCount:    fixedCount(5),
		Tolerance:  0,
		TuneIngest: true, // 顺带钉住「提速 → 恢复声明值」这条只有跑起来才看得见的时序
		Logf:       silentLogf,
	})
	if err != nil {
		t.Fatalf("蓝绿重建失败: %v", err)
	}
	// 删掉 Compensate 调用点 → ran=false 红。
	if !ran {
		t.Fatal("切换后必须执行差量补偿（灌数窗口内的改/删只能靠它收敛）")
	}
	// 把补偿挪到切换之前 → 取样到的别名还指着旧索引，红。
	if aliasAtComp != next {
		t.Errorf("补偿必须在别名切到 %s 之后发生（补偿写入要落到新索引），取样时别名指向 %q", next, aliasAtComp)
	}
	if targetAtComp != next {
		t.Errorf("补偿目标必须是新物理索引，got=%q", targetAtComp)
	}
	// 下界取错（例如灌完再取 time.Now()）→ 窗口内的变更被静默漏掉，红。
	if !sinceAtComp.Equal(res.StartedAt) {
		t.Errorf("补偿下界必须是本次任务首次启动时刻，got=%v want=%v", sinceAtComp, res.StartedAt)
	}
	if sinceAtComp.After(nowAtComp.Add(-time.Nanosecond)) {
		t.Errorf("下界不可能晚于补偿执行时刻：%v vs %v", sinceAtComp, nowAtComp)
	}
	if !res.Reconciled || !res.Switched || !res.Compensated || !res.Verified {
		t.Errorf("八步应全部走完，got %+v", res)
	}

	// 状态而非次数：默认保留旧索引（--drop-old 未开），且打印的回滚请求能把它切回来。
	if !f.indexExists(cur) || !f.holds(alias, next) {
		t.Errorf("默认必须保留旧索引并把别名挂到新索引：cur 存在=%v 别名指向=%q", f.indexExists(cur), f.aliasIndexOf(alias))
	}
	if len(f.deleted) > 0 {
		t.Errorf("未开 --drop-old 却删了索引：%v", f.deleted)
	}
	if res.OldDropped {
		t.Errorf("OldDropped 应为 false")
	}
	rb := actionsByKey(t, res.PlannedRollbackBody)
	if add, ok := rb["add"]; !ok || add["index"] != cur {
		t.Errorf("回滚请求应把别名加回旧索引 %s，got=%s", cur, res.PlannedRollbackBody)
	}
	if rem, ok := rb["remove"]; !ok || rem["index"] != next {
		t.Errorf("回滚请求应解除新索引 %s，got=%s", next, res.PlannedRollbackBody)
	}

	// 灌入期调优：变更在前、恢复在后，且恢复值来自声明（refresh_interval/number_of_replicas）。
	if len(f.putSettings) != 2 {
		t.Fatalf("应恰好两次 _settings（提速 + 恢复），got %v", f.putSettings)
	}
	if !strings.Contains(f.putSettings[0], `"refresh_interval":"-1"`) {
		t.Errorf("第一次应是灌数提速，got=%s", f.putSettings[0])
	}
	if !strings.Contains(f.putSettings[1], `"refresh_interval":"1s"`) ||
		!strings.Contains(f.putSettings[1], `"number_of_replicas":1`) ||
		strings.Contains(f.putSettings[1], "-1") {
		t.Errorf("恢复必须回到声明值（不能把 -1/0 留在索引上），got=%s", f.putSettings[1])
	}
	if !res.Restored {
		t.Errorf("settings 应已恢复")
	}
}

func TestRunRebuild_MissingCompensateAbortsAfterSwitch(t *testing.T) {
	const alias = "shop-product"
	f, def := newRebuildFixture(t, alias, 1, 2)
	next := def.Name()
	f.counts[next] = 5

	res, err := RunRebuild(context.Background(), RebuildOptions{
		Alias:      alias,
		Definition: def,
		Target:     f,
		Sync:       cutoverSync(SyncSummary{Scanned: 5, Sent: 5}, nil),
		DBCount:    fixedCount(5),
		Logf:       silentLogf,
	})
	if err == nil {
		t.Fatal("未注入 CompensateFunc 必须报错（窗口增量未收敛不能当成功）")
	}
	if res.AbortedStep != "compensate" || !res.Switched {
		t.Errorf("应停在 compensate 且别名已切换，got step=%q switched=%v", res.AbortedStep, res.Switched)
	}
	if !strings.Contains(err.Error(), next) || !strings.Contains(err.Error(), "补偿") {
		t.Errorf("报错必须点名目标与补偿，让人工可接手指令，got=%v", err)
	}
}

// ---------------------------------------------------------------------------
// ④ --dry-run 零写操作
// ---------------------------------------------------------------------------

func TestRunRebuild_DryRunRejectsEveryWrite(t *testing.T) {
	const alias = "shop-product"
	f, def := newRebuildFixture(t, alias, 1, 2)
	next := def.Name()
	cur := alias + "_v1"

	synced := false
	compensated := false
	res, err := RunRebuild(context.Background(), RebuildOptions{
		Alias:      alias,
		Definition: def,
		Target:     f,
		Sync: func(context.Context, string, string) (SyncSummary, error) {
			synced = true
			return SyncSummary{}, nil
		},
		Compensate: func(context.Context, string, string, time.Time) (CompensateSummary, error) {
			compensated = true
			return CompensateSummary{}, nil
		},
		DBCount:    fixedCount(5),
		TuneIngest: true,
		DropOld:    true,
		DryRun:     true,
		Logf:       silentLogf,
	})
	if err != nil {
		t.Fatalf("dry-run 不该报错: %v", err)
	}
	if len(res.WriteOps) > 0 {
		t.Errorf("审计层必须拦下所有写操作，got=%v", res.WriteOps)
	}
	if writes := f.opsOfKind("write"); len(writes) > 0 {
		t.Errorf("raw fake 侧也必须零写请求，got=%v", writes)
	}
	if synced || compensated || res.Created || res.Tuned || res.Switched || res.OldDropped || res.Compensated {
		t.Errorf("dry-run 不得进入任何写步骤（补偿回调也不能被调到），got %+v synced=%v compensated=%v", res, synced, compensated)
	}
	// 状态证明：新索引没被建出来，旧索引与别名原封不动。
	if f.indexExists(next) || f.indexExists(cur) == false || !f.holds(alias, cur) {
		t.Errorf("dry-run 后现场必须原样（next 未创建、cur 仍在、别名未动）：%v / %q", f.allOps(), f.aliasIndexOf(alias))
	}
	if len(f.deleted) > 0 {
		t.Errorf("dry-run 里 --drop-old 也不能删索引：%v", f.deleted)
	}
	// 计划必须如实打印：切换体与回滚体都在结果里，供运维核对。
	if res.PlannedSwitchBody == "" || res.PlannedRollbackBody == "" || res.PlannedCreateBody == "" {
		t.Errorf("dry-run 应给出完整计划体，got %+v", res)
	}
	var created map[string]any
	if err := json.Unmarshal([]byte(res.PlannedCreateBody), &created); err != nil {
		t.Fatalf("计划创建体不是合法 JSON: %v", err)
	}
	if _, bad := created["aliases"]; bad {
		t.Errorf("蓝绿新建必须不带 aliases（否则 ES 拒绝：别名与索引名互斥）：%s", res.PlannedCreateBody)
	}
}

// ---------------------------------------------------------------------------
// ⑤（必修③回归）对账容差归 rebuild 判定，不下发给内层灌数
// ---------------------------------------------------------------------------

// TestRunRebuild_ToleranceGateIsOwnedByRebuildNotIngest 钉住真机坐实的缺陷：
// 把 --tolerance 下发给灌数层后，sync 会在自己的收尾对账里抢跑失败，于是中止步骤报成
// sync、结论行 ES=0/差=+0/达标=false 全是没回读的假数、--force 分支不可达。
// 正确形态：内层只负责灌，判定在 rebuild 第 5 步，用真实回读的 ES 计数下结论。
func TestRunRebuild_ToleranceGateIsOwnedByRebuildNotIngest(t *testing.T) {
	const alias = "shop-product"

	// ---- A：负容差 = 永不可达标，但必须中止在 reconcile、且报真实 ES 计数 ----
	fA, defA := newRebuildFixture(t, alias, 1, 2)
	nextA := defA.Name()
	fA.counts[nextA] = 5 // 灌完后的真实回读：与 DB 期望一致
	var ingestRan bool
	resA, err := RunRebuild(context.Background(), RebuildOptions{
		Alias:      alias,
		Definition: defA,
		Target:     fA,
		Sync: func(context.Context, string, string) (SyncSummary, error) {
			ingestRan = true
			return SyncSummary{Scanned: 5, Sent: 5}, nil
		},
		Compensate: func(context.Context, string, string, time.Time) (CompensateSummary, error) {
			return CompensateSummary{}, nil
		},
		DBCount:          fixedCount(5),
		Tolerance:        -1,
		KeepNewOnFailure: true,
		Logf:             silentLogf,
	})
	if err == nil {
		t.Fatal("负容差必须不达标并在切别名之前中止")
	}
	if !ingestRan {
		t.Error("灌数必须照常执行：容差判定不得下发给内层，内层不该提前失败")
	}
	// 缺陷现场就集中在这一行：容差被内层消化时 AbortedStep="sync"。
	if resA.AbortedStep != "reconcile" {
		t.Errorf("中止步骤必须是 reconcile，got=%q（报 sync 即对账被内层灌数抢跑）", resA.AbortedStep)
	}
	// 结论三要素必须来自真实回读，不能是没跑过对账的零值。
	if !resA.ReconcileAttempted || resA.ESCount != 5 || resA.Diff != 0 || resA.Reconciled {
		t.Errorf("ES 计数必须取真实回读值（5）并如实记 达标=false，got %+v", resA)
	}
	if resA.Tolerance != -1 {
		t.Errorf("结论里必须带上本次生效的容差（负值不被当 0 用），got=%d", resA.Tolerance)
	}
	if got := fA.aliasIndexOf(alias); got != alias+"_v1" {
		t.Errorf("别名必须仍唯一指向旧索引（线上读不受影响），got=%q", got)
	}
	if opsHave(fA.allOps(), "/_aliases") {
		t.Errorf("不达标绝不能下发 _aliases：%v", fA.allOps())
	}
	if !fA.indexExists(nextA) || len(fA.orphanOnFail(resA, nextA)) == 0 {
		t.Errorf("应保留灌好的新索引供排查，orphans=%v", resA.Orphans)
	}

	// ---- B：同一现场 + --force 必须真正放行（缺陷版本里这条分支不可达）----
	fB, defB := newRebuildFixture(t, alias, 1, 2)
	nextB := defB.Name()
	fB.counts[nextB] = 5
	resB, err := RunRebuild(context.Background(), RebuildOptions{
		Alias:      alias,
		Definition: defB,
		Target:     fB,
		Sync:       cutoverSync(SyncSummary{Scanned: 5, Sent: 5}, nil),
		Compensate: func(context.Context, string, string, time.Time) (CompensateSummary, error) {
			return CompensateSummary{}, nil
		},
		DBCount:   fixedCount(5),
		Tolerance: -1,
		Force:     true,
		Logf:      silentLogf,
	})
	if err != nil {
		t.Fatalf("--force 必须能越过不达标继续切换（缺陷版本里该分支不可达）: %v", err)
	}
	if !resB.Switched || resB.Reconciled {
		t.Errorf("--force 下应已切换但如实记 达标=false，got switched=%v reconciled=%v", resB.Switched, resB.Reconciled)
	}
	if got := fB.aliasIndexOf(alias); got != nextB {
		t.Errorf("别名必须已切到新索引，got=%q", got)
	}
	if !fB.indexExists(alias + "_v1") {
		t.Errorf("切换后旧索引必须保留（rebuild 的回滚退路）")
	}
}

// ---------------------------------------------------------------------------
// ⑥ 补偿统计必须带出：Failed>0 / error 都不能被吞成 Compensated=true（审查必修②）
// ---------------------------------------------------------------------------

// TestRunRebuild_CompensateFailuresAreNotSwallowedAsSuccess 钉的是修复前的致命签名：
// 当时 CompensateFunc 只返回 error，sync.Compensate 累计的 item 级失败（含 strict_dynamic
// 结构类失败）整条被丢弃，失败 bulk 仍被置成 Compensated=true —— 报表骗人。
// 现在签名已带统计；把签名改回去、或删掉 Failed>0 分支，本用例必红。
func TestRunRebuild_CompensateFailuresAreNotSwallowedAsSuccess(t *testing.T) {
	const alias = "shop-product"

	t.Run("补偿 error 路径：已切换但未收敛", func(t *testing.T) {
		f, def := newRebuildFixture(t, alias, 1, 2)
		next := def.Name()
		f.counts[next] = 5

		res, err := RunRebuild(context.Background(), RebuildOptions{
			Alias:      alias,
			Definition: def,
			Target:     f,
			Sync:       cutoverSync(SyncSummary{Scanned: 5, Sent: 5}, nil),
			Compensate: func(context.Context, string, string, time.Time) (CompensateSummary, error) {
				return CompensateSummary{Scanned: 3, Indexed: 1, Deleted: 1}, errors.New("bulk boom")
			},
			DBCount:   fixedCount(5),
			Tolerance: 0,
			Logf:      silentLogf,
		})
		if err == nil || !strings.Contains(err.Error(), "补偿") {
			t.Fatalf("补偿失败必须报错且点名补偿，got=%v", err)
		}
		if res.AbortedStep != "compensate" || !res.Switched || res.Compensated {
			t.Errorf("应停在 compensate：别名已切走但不得标记补偿完成，got %+v", res)
		}
		// 统计必须原样带出来，否则报表里看不出扫描了多少。
		if res.Compensate != (CompensateSummary{Scanned: 3, Indexed: 1, Deleted: 1}) {
			t.Errorf("失败时也要带出补偿统计，got %+v", res.Compensate)
		}
		if got := f.aliasIndexOf(alias); got != next {
			t.Errorf("补偿失败不回滚切换，别名应仍指向新索引 %s，got=%q", next, got)
		}
	})

	t.Run("Failed>0 但无 error：旧版本正是把这一类报成成功", func(t *testing.T) {
		f, def := newRebuildFixture(t, alias, 1, 2)
		next := def.Name()
		f.counts[next] = 5

		res, err := RunRebuild(context.Background(), RebuildOptions{
			Alias:      alias,
			Definition: def,
			Target:     f,
			Sync:       cutoverSync(SyncSummary{Scanned: 5, Sent: 5}, nil),
			Compensate: func(context.Context, string, string, time.Time) (CompensateSummary, error) {
				return CompensateSummary{Scanned: 5, Indexed: 2, Failed: 3, Batches: 1}, nil
			},
			DBCount:   fixedCount(5),
			Tolerance: 0,
			Logf:      silentLogf,
		})
		if err == nil {
			t.Fatalf("补偿存在 item 级失败绝不能当成功，got err=nil res=%+v", res)
		}
		if !strings.Contains(err.Error(), "3") {
			t.Errorf("报错必须给出失败条数（运维要知道漏了多少），got=%v", err)
		}
		if res.AbortedStep != "compensate_failures" || !res.Switched || res.Compensated {
			t.Errorf("应停在 compensate_failures 且 compensated=false，got %+v", res)
		}
		if res.Compensate.Failed != 3 || res.Compensate.Scanned != 5 {
			t.Errorf("失败数必须写进结果供报表使用，got %+v", res.Compensate)
		}
		// 状态而非次数：别名已切到新索引、旧索引保留可回滚，失败只能重跑补偿收敛。
		if got := f.aliasIndexOf(alias); got != next || !f.indexExists(alias+"_v1") {
			t.Errorf("别名应已切到 %s 且旧索引保留，got=%q", next, got)
		}
	})
}
