package sync

// delta_test.go 钉死 Phase 5 差量补偿最易静默漏数据的三个语义：
//   - 扫描 SQL 必须 Unscoped()（连软删行一起读，否则删过的商品永远留在索引里）、
//     带 update_time >= since 下界、带 id > lastID 主键游标，且 **WHERE 里绝不出现
//     is_show / is_del 过滤**（下架/软删的排除交给 BuildIndexDoc 逐行判，SQL 层排除就删不掉残留）；
//   - Compensate 必须把 since 原样透传给扫描口（取错下界＝静默漏掉窗口内变更）；
//   - 逐行按 BuildIndexDoc 分流：合格→index，不合格（软删/下架）→delete，_id 与全量口径一致。
//
// 不连真库：SQL 形态用 **gorm DryRun + 本文件内的最小 dialector** 现算（不注册 database/sql
// 驱动、不建连接、不探版本 ⇒ 零网络、零新依赖、无 CGO），
// 编排序列用 product.go 既证过的「可注入函数变量」范式（compensateScan / compensateBulk）。

import (
	"context"
	"strings"
	"testing"
	"time"

	"shop/internal/es/admin"
	"shop/internal/models"

	"gorm.io/gorm"
	"gorm.io/gorm/callbacks"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/schema"
)

func ptrI8(v int8) *int8 { return &v }

// dryRunDialector 是 gorm.Dialector 的最小实现，只为 DryRun 拼 SQL 存在：
// Initialize 不挂 ConnPool（DryRun 路径下 gorm 根本不碰连接池），但必须注册默认 callbacks
// ——SQL 是 gorm:query 回调里 BuildQuerySQL 写进 Statement.SQL 的，不注册就一行 SQL 都拼不出来。
// BindVarTo 一律写 `?`，QuoteTo 不加反引号（断言侧已做归一化，见 norm）。
// 全程零网络，且只依赖 gorm.io/gorm 自身的子包，不给 go.mod 引入任何新依赖。
type dryRunDialector struct{}

func (dryRunDialector) Name() string { return "dryrun" }

func (dryRunDialector) Initialize(db *gorm.DB) error {
	// 空 config = 用 gorm 自己的默认回调链（与 mysql 驱动 Initialize 里的做法同构）。
	callbacks.RegisterDefaultCallbacks(db, &callbacks.Config{})
	return nil
}

func (dryRunDialector) Migrator(*gorm.DB) gorm.Migrator { return nil }

func (dryRunDialector) DataTypeOf(*schema.Field) string { return "" }

func (dryRunDialector) DefaultValueOf(*schema.Field) clause.Expression {
	return clause.Expr{SQL: "DEFAULT"}
}

func (dryRunDialector) BindVarTo(writer clause.Writer, _ *gorm.Statement, _ any) {
	_, _ = writer.WriteString("?")
}

func (dryRunDialector) QuoteTo(writer clause.Writer, s string) {
	_, _ = writer.WriteString(s)
}

func (dryRunDialector) Explain(sql string, _ ...any) string { return sql }

// newDeltaDryRunDB 构造一台只拼 SQL、绝不联网的 DryRun gorm。
// DisableAutomaticPing 显式关掉 gorm.Open 的 ConnPool.Ping()：万一有人日后给
// Initialize 挂上真实连接池，这道闸也能保证不拨号。
func newDeltaDryRunDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(dryRunDialector{}, &gorm.Config{DryRun: true, DisableAutomaticPing: true})
	if err != nil {
		t.Fatalf("构造 DryRun gorm 失败: %v", err)
	}
	return db
}

// TestCompensateScope_SQL 是差量补偿「破坏即红」的核心：任何把 Unscoped 去掉、
// 给扫描加 is_show/is_del 过滤、或把下界方向写反的改动都会让本用例变红。
func TestCompensateScope_SQL(t *testing.T) {
	db := newDeltaDryRunDB(t)
	since := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	res := compensateScope(db.Session(&gorm.Session{}), since, 1000, 42).
		Find(&[]models.StoreProduct{})
	if res.Error != nil {
		t.Fatalf("DryRun 生成 SQL 失败: %v", res.Error)
	}
	full := res.Statement.SQL.String()
	if full == "" {
		t.Fatal("DryRun 未生成 SQL")
	}
	// 用 WHERE 切分：SELECT 列里本就显式选了 is_del/is_show（compensateColumns），
	// 它们出现在投影里是**对的**；红线是它们不得出现在过滤条件里。
	// 反引号无关紧要（原始条件串没写反引号，归一化后两边都比），先去枝节再比对。
	norm := func(s string) string { return strings.ReplaceAll(s, "`", "") }
	where := norm(full)
	if i := strings.Index(where, "WHERE"); i >= 0 {
		where = where[i:]
	} else {
		t.Fatalf("SQL 未生成 WHERE 子句: %s", full)
	}

	// ① 下界 + 游标必须落在 WHERE。
	if !strings.Contains(where, "update_time >=") {
		t.Errorf("WHERE 必须含 update_time >= 下界（差量窗口），实际: %s", where)
	}
	if !strings.Contains(where, "id >") {
		t.Errorf("WHERE 必须含 id > 主键游标，实际: %s", where)
	}
	// ② Unscoped：WHERE 里绝不能有软删过滤 is_del（否则删过的商品读不到、删不掉）。
	if strings.Contains(where, "is_del") {
		t.Errorf("WHERE 不该出现 is_del 过滤（说明漏了 Unscoped()，软删行读不到）: %s", where)
	}
	// ③ 不加 is_show 过滤（下架商品靠 BuildIndexDoc 判成 delete，SQL 排除就删不掉残留）。
	if strings.Contains(where, "is_show") {
		t.Errorf("WHERE 不该出现 is_show 过滤（会漏删历史残留）: %s", where)
	}
	// ④ since 作为第一个绑定变量下传（取错时间＝静默漏数据）。
	var sawSince bool
	for _, v := range res.Statement.Vars {
		if tv, ok := v.(time.Time); ok && tv.Equal(since) {
			sawSince = true
			break
		}
	}
	if !sawSince {
		t.Errorf("绑定变量里应含下界 since=%v，实际 Vars=%v", since, res.Statement.Vars)
	}
	// ⑤ SELECT 投影仍带 is_del（Unscoped 读回软删值供分流）。
	selPart := full
	if i := strings.Index(selPart, "WHERE"); i > 0 {
		selPart = selPart[:i]
	}
	if !strings.Contains(strings.ToLower(norm(selPart)), "is_del") {
		t.Errorf("SELECT 列应显式包含 is_del，实际: %s", full)
	}
}

// TestCompensate_ForwardsSinceAndBranches 验证 Compensate 把 since 原样透传给扫描口，
// 并逐行按 BuildIndexDoc 分成 index / delete。把 since 改成 time.Now() 或删除调用都会变红。
func TestCompensate_ForwardsSinceAndBranches(t *testing.T) {
	origScan, origBulk := compensateScan, compensateBulk
	t.Cleanup(func() { compensateScan, compensateBulk = origScan, origBulk })

	since := time.Now().Add(-2 * time.Hour)
	var gotSince time.Time
	var gotTarget string
	var allDocs []admin.Doc

	compensateScan = func(_ context.Context, s time.Time, _ int, cb func(compensateBatch) error) error {
		gotSince = s
		batch := compensateBatch{
			{CateId: 10, IsShow: ptrI8(1), BaseModel: models.BaseModel{Id: 1}},           // 合格 -> index
			{CateId: 20, IsShow: ptrI8(1), BaseModel: models.BaseModel{Id: 2, IsDel: 1}}, // 软删 -> delete
			{CateId: 30, IsShow: ptrI8(0), BaseModel: models.BaseModel{Id: 3}},           // 下架 -> delete
		}
		return cb(batch)
	}
	compensateBulk = func(_ context.Context, target string, docs []admin.Doc) (*admin.BulkResult, error) {
		gotTarget = target
		allDocs = append(allDocs, docs...)
		return &admin.BulkResult{}, nil
	}

	st, err := Compensate(context.Background(), "shop-product_v2", since, CompensateOptions{})
	if err != nil {
		t.Fatalf("Compensate 报错: %v", err)
	}
	if !gotSince.Equal(since) {
		t.Errorf("扫描下界必须原样透传 since（取错=静默漏数据），want=%v got=%v", since, gotSince)
	}
	if gotTarget != "shop-product_v2" {
		t.Errorf("补偿目标应为新物理索引，got=%q", gotTarget)
	}
	if st.Scanned != 3 || st.Indexed != 1 || st.Deleted != 2 {
		t.Errorf("统计应为 scanned=3 indexed=1 deleted=2，实际 %+v", st)
	}

	byID := map[string]admin.Doc{}
	for _, d := range allDocs {
		byID[d.ID] = d
	}
	if d, ok := byID["1"]; !ok || d.Delete || d.Src == nil {
		t.Errorf("id=1 应为 index（Src 非空、Delete=false），got %+v ok=%v", d, ok)
	}
	if d, ok := byID["2"]; !ok || !d.Delete || d.Src != nil {
		t.Errorf("id=2（软删）应为 delete（Src=nil、Delete=true），got %+v ok=%v", d, ok)
	}
	if d, ok := byID["3"]; !ok || !d.Delete {
		t.Errorf("id=3（下架）应为 delete，got %+v ok=%v", d, ok)
	}
	// _id 与路由必须与全量口径一致（delete 用同一 _id 才删得掉历史残留）。
	if d := byID["2"]; d.Routing != "20" {
		t.Errorf("id=2 的 routing 应为 cate_id=20，got=%q", d.Routing)
	}
}
