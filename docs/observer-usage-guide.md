# Observer 模式使用指南

本文档面向开发者，说明如何在项目中快速创建和使用 GORM Observer。

---

## 一、框架能力概览

| 能力类别 | 具体功能 | 说明 |
|---------|---------|------|
| **模型事件监听** | 7 个生命周期事件 | BeforeCreate、AfterCreate、BeforeUpdate、AfterUpdate、BeforeDelete、AfterDelete、AfterFind |
| **字段变更追踪** | `IsDirty(field)` | 检查指定字段是否发生变更 |
| | `GetDirty()` | 获取所有变更字段的映射 |
| | `GetOriginal(field)` | 获取某字段的原始值 |
| | `GetNew(field)` | 获取某字段的新值 |
| | `HasChanges()` | 是否有任何字段变更 |
| | `Fields()` | 获取所有变更的字段名列表 |
| | `String()` | 输出可读的变更摘要 |
| **批量操作上下文** | `GetBatchContext(tx)` | 提取批量 Update/Delete 的上下文 |
| | `WhereSQL` | 提取出的 WHERE 子句（不含 WHERE 关键字） |
| | `GetVar(index)` | 按索引获取 WHERE 绑定值 |
| | `GetVarInt64(index)` | 获取绑定值并转为 int64 |
| | `GetVarString(index)` | 获取绑定值并转为 string |
| | `GetVarsAsInt64Slice(batch, index)` | 提取 ID 列表等切片 |
| | `VarsCount()` | 绑定变量数量 |
| **批量操作反查** | `batch.ReQuery(&dest)` | 用原始 WHERE + Vars 反查受影响的记录 |
| | `batch.ReQueryWithScope(&dest, scope)` | 反查 + 额外条件（Select/Limit/Order 等） |
| **数据补全** | `FetchFullModel(tx, dest)` | 在 Observer 中通过主键反查完整记录 |

---

## 二、快速开始：创建一个 Observer

### 步骤

1. 在 `internal/observers/` 目录下新建文件（如 `article_observer.go`）
2. 定义一个 Observer 结构体
3. 实现 `ObserveModel()` 方法，返回要监听的表名
4. 按需实现事件接口（见第三节）
5. 在 `register.go` 中注册该 Observer

### 完整示例

```go
package observers

import (
    "log"

    "shop/internal/models"
    "shop/internal/observer"

    "gorm.io/gorm"
)

// ArticleObserver 文章模型观察者
type ArticleObserver struct{}

// ObserveModel 返回观察的模型表名
func (o *ArticleObserver) ObserveModel() string {
    return "wechat_article"
}

// AfterCreate 创建后回调
func (o *ArticleObserver) AfterCreate(tx *gorm.DB, model interface{}) error {
    article, ok := model.(*models.WechatArticle)
    if !ok {
        return nil
    }
    log.Printf("[ArticleObserver] 文章创建: %s (ID: %d)", article.Title, article.Id)
    return nil
}

// AfterUpdate 更新后回调（带 Dirty Tracking）
func (o *ArticleObserver) AfterUpdate(tx *gorm.DB, model interface{}) error {
    article, ok := model.(*models.WechatArticle)
    if !ok {
        return nil
    }

    dirty := observer.GetDirtyFromTx(tx)
    if dirty != nil && dirty.HasChanges() {
        log.Printf("[ArticleObserver] 文章更新: %s, 变更字段: %v", article.Title, dirty.Fields())
    }

    return nil
}

// AfterDelete 删除后回调
func (o *ArticleObserver) AfterDelete(tx *gorm.DB, model interface{}) error {
    article, ok := model.(*models.WechatArticle)
    if !ok {
        return nil
    }
    log.Printf("[ArticleObserver] 文章删除: %s (ID: %d)", article.Title, article.Id)
    return nil
}
```

在 `register.go` 中注册：

```go
func RegisterAll(db *gorm.DB) error {
    registry := observer.NewRegistry()

    registry.Register(
        &ProductObserver{},
        &OrderObserver{},
        &UserObserver{},
        &SysUserObserver{},
        &ArticleObserver{}, // 新增
    )

    return db.Use(observer.NewPlugin(registry))
}
```

---

## 三、事件接口参考

| 接口名 | 方法签名 | 触发时机 | 参数说明 | 返回值效果 |
|--------|---------|---------|---------|-----------|
| `BeforeCreateObserver` | `BeforeCreate(tx *gorm.DB, model interface{}) error` | INSERT 语句执行前 | `model` 为待插入的数据对象 | 返回 error 会回滚事务，中断创建 |
| `AfterCreateObserver` | `AfterCreate(tx *gorm.DB, model interface{}) error` | INSERT 语句执行后 | `model` 为刚插入的数据对象（含自增 ID） | 返回 error 不影响已完成的 INSERT，但事务仍可用 |
| `BeforeUpdateObserver` | `BeforeUpdate(tx *gorm.DB, model interface{}) error` | UPDATE 语句执行前 | `model` 为更新后的数据对象 | 返回 error 会回滚事务，中断更新 |
| `AfterUpdateObserver` | `AfterUpdate(tx *gorm.DB, model interface{}) error` | UPDATE 语句执行后 | `model` 为更新后的数据对象 | 返回 error 不影响已完成的 UPDATE |
| `BeforeDeleteObserver` | `BeforeDelete(tx *gorm.DB, model interface{}) error` | DELETE 语句执行前 | `model` 为待删除的数据对象或条件 | 返回 error 会回滚事务，中断删除 |
| `AfterDeleteObserver` | `AfterDelete(tx *gorm.DB, model interface{}) error` | DELETE 语句执行后 | `model` 为删除前的数据对象 | 返回 error 不影响已完成的 DELETE |
| `AfterFindObserver` | `AfterFind(tx *gorm.DB, model interface{}) error` | SELECT 查询完成后 | `model` 可能是单条对象或切片 | 返回 error 会中断后续查询处理 |

> **注意**：`AfterFind` 的 `model` 参数可能是 `*models.T`（单条）或 `[]*models.T`（多条），类型断言时需判断。

---

## 四、Dirty Tracking 使用方法

### 4.1 获取字段变更

在 `BeforeUpdate` 或 `AfterUpdate` 中，通过以下方式获取变更信息：

```go
import "shop/internal/observer"

dirty := observer.GetDirtyFromTx(tx)
```

`dirty` 为 `*observer.DirtyFields`，可用方法如下：

| 方法 | 签名 | 说明 |
|------|------|------|
| `IsDirty` | `IsDirty(field string) bool` | 检查某字段是否发生变更 |
| `GetDirty` | `GetDirty() map[string]FieldChange` | 获取所有变更字段，key 为字段名，value 包含列名、旧值、新值 |
| `GetOriginal` | `GetOriginal(field string) interface{}` | 获取某字段的原始值 |
| `GetNew` | `GetNew(field string) interface{}` | 获取某字段的新值 |
| `HasChanges` | `HasChanges() bool` | 是否有任何字段变更 |
| `Fields` | `Fields() []string` | 获取所有变更的字段名列表 |
| `String` | `String() string` | 输出可读的变更摘要，如 `DirtyFields(2 changes): Name: a -> b, Status: 0 -> 1` |

`FieldChange` 结构体：

```go
type FieldChange struct {
    Field    string      // Go 结构体字段名（如 "StoreName"）
    Column   string      // 数据库列名（如 "store_name"）
    OldValue interface{} // 旧值
    NewValue interface{} // 新值
}
```

### 4.2 适用场景

| 场景 | Dirty Tracking 是否可用 | 说明 |
|------|------------------------|------|
| `db.Save(&model)` | ✅ 可用 | Save 操作会加载完整模型，能获取旧数据 |
| `db.Model(&model).Updates(...)` | ✅ 可用 | Dest 为完整模型时可用 |
| `db.Model(&model).Select("xxx").Updates(...)` | ✅ 可用 | 仅追踪被 Select 的字段 |
| `db.Model(&T{}).Where("...").Update(...)` | ❌ 不可用 | 链式调用，无法获取旧数据 |
| `db.Model(&T{}).Where("...").Updates(map[string]interface{}{...})` | ❌ 不可用 | 同上 |
| `db.Table("...").Where("...").Update(...)` | ❌ 不可用 | 同上 |
| 批量更新（`Where(...).Updates(...)`） | ❌ 不可用 | 需用 BatchContext 处理 |

**不可用时的替代方案**：使用 `BatchContext` 获取 WHERE 条件，反查受影响的记录后处理（见第五节）。

### 4.3 完整示例

记录字段变更日志的 Observer：

```go
package observers

import (
    "log"

    "shop/internal/models"
    "shop/internal/observer"

    "gorm.io/gorm"
)

type SysUserObserver struct{}

func (o *SysUserObserver) ObserveModel() string {
    return "sys_user"
}

func (o *SysUserObserver) AfterUpdate(tx *gorm.DB, model interface{}) error {
    user, ok := model.(*models.SysUser)
    if !ok {
        return nil
    }

    dirty := observer.GetDirtyFromTx(tx)
    if dirty != nil && dirty.HasChanges() {
        log.Printf("[SysUserObserver] 字段变更检测: %s", dirty.String())

        for field, change := range dirty.GetDirty() {
            log.Printf("[SysUserObserver] 字段变更: %s (列:%s) %v → %v",
                field, change.Column, change.OldValue, change.NewValue)
        }

        // 敏感字段变更检测
        if dirty.IsDirty("Enabled") {
            if newVal, _ := dirty.GetNew("Enabled").(int); newVal == 0 {
                log.Printf("[SysUserObserver] 用户 %d 被禁用", user.Id)
            }
        }
    }

    return nil
}
```

---

## 五、批量操作处理

### 5.1 获取批量操作上下文

```go
batch := observer.GetBatchContext(tx)
```

适用于 `db.Model(&T{}).Where("...").Updates(...)` 等批量更新/删除场景。

### 5.2 BatchContext API

| 方法/字段 | 签名/类型 | 说明 |
|-----------|----------|------|
| `Table` | `string` | 表名 |
| `SQL` | `string` | 完整的 SQL 语句 |
| `WhereSQL` | `string` | 提取出的 WHERE 子句（不含 WHERE 关键字） |
| `Vars` | `[]interface{}` | SQL 绑定变量（WHERE 条件中的值） |
| `RowsAffected` | `int64` | 影响行数 |
| `GetVar` | `GetVar(index int) interface{}` | 按索引获取绑定值 |
| `GetVarInt64` | `GetVarInt64(index int) (int64, bool)` | 获取绑定值并转为 int64 |
| `GetVarString` | `GetVarString(index int) (string, bool)` | 获取绑定值并转为 string |
| `VarsCount` | `VarsCount() int` | 绑定变量数量 |
| `GetVarsAsInt64Slice` | `GetVarsAsInt64Slice(batch *BatchContext, index int) []int64` | 提取 ID 列表（包级函数） |
| `ReQuery` | `ReQuery(dest interface{}) *gorm.DB` | 用原始 WHERE + Vars 反查受影响的记录 |
| `ReQueryWithScope` | `ReQueryWithScope(dest interface{}, scope func(*gorm.DB) *gorm.DB) *gorm.DB` | 反查 + 额外条件 |

### 5.3 多条件 WHERE 场景

Vars 的索引规则：**对应 SQL 中 `?` 占位符的出现顺序**。

示例 SQL：

```sql
UPDATE `sys_user` SET `enabled` = ? WHERE dept_id = ? AND enabled = ? AND create_time < ?
```

对应的 Vars 索引：

| 索引 | 值 | 含义 |
|------|-----|------|
| 0 | `enabled` 的新值 | SET 子句 |
| 1 | `dept_id` 的值 | WHERE 第 1 个 `?` |
| 2 | `enabled` 的旧值 | WHERE 第 2 个 `?` |
| 3 | `create_time` 的值 | WHERE 第 3 个 `?` |

代码示例：

```go
batch := observer.GetBatchContext(tx)

// 获取 WHERE 条件中的值
newEnabled := batch.GetVar(0)      // SET 的新值
deptId, _ := batch.GetVarInt64(1)  // WHERE dept_id = ?
oldEnabled, _ := batch.GetVarInt64(2) // WHERE enabled = ?

log.Printf("批量更新: dept_id=%d, enabled %d -> %v", deptId, oldEnabled, newEnabled)
```

批量 `WHERE id IN (?)` 场景：

```go
batch := observer.GetBatchContext(tx)
ids := observer.GetVarsAsInt64Slice(batch, 0) // 获取 ID 列表
log.Printf("批量更新 %d 条记录, IDs: %v", batch.RowsAffected, ids)
```

### 5.4 推荐的 Observer 编写模式

同时处理单条和批量更新的模板：

```go
package observers

import (
    "log"

    "shop/internal/models"
    "shop/internal/observer"

    "gorm.io/gorm"
)

type MyModelObserver struct{}

func (o *MyModelObserver) ObserveModel() string {
    return "my_table"
}

func (o *MyModelObserver) AfterUpdate(tx *gorm.DB, model interface{}) error {
    // 1. 先尝试单条场景的 Dirty Tracking
    dirty := observer.GetDirtyFromTx(tx)
    if dirty != nil && dirty.HasChanges() {
        // 单条更新，有完整字段变更信息
        for field, change := range dirty.GetDirty() {
            log.Printf("字段变更: %s: %v -> %v", field, change.OldValue, change.NewValue)
        }
        return nil
    }

    // 2. 批量场景：通过 BatchContext 获取 WHERE 条件
    batch := observer.GetBatchContext(tx)
    if batch.RowsAffected == 0 {
        return nil
    }

    // 根据 SQL 中的 ? 顺序提取参数
    // 例如: UPDATE my_table SET status = ? WHERE id IN (?)
    ids := observer.GetVarsAsInt64Slice(batch, 1) // 第2个 ? 是 id IN (?)

    // 用新 Session 反查受影响记录
    var records []models.MyModel
    if err := tx.Session(&gorm.Session{NewDB: true}).
        Where("id IN ?", ids).Find(&records).Error; err != nil {
        log.Printf("反查失败: %v", err)
        return nil
    }

    for _, r := range records {
        log.Printf("批量更新记录: ID=%d", r.Id)
    }

    return nil
}
```

### 5.5 批量操作反查模式（ReQuery）

批量 Update/Delete 操作中，Observer 无法获取单条记录的字段变更（Dirty Tracking 不可用），但可以通过提取原始 WHERE 条件来反查受影响的记录，然后执行后续业务逻辑。

#### 逻辑流程图

```
Service 层执行批量操作：
  db.Model(&SysUser{}).Where("dept_id = ? AND status = ?", 5, 1).Updates(data)
        ↓
GORM 生成 SQL：
  SQL:  "UPDATE sys_user SET ... WHERE dept_id = ? AND status = ?"
  Vars: [5, 1]
        ↓
Observer 回调触发（AfterUpdate）：
  batch := observer.GetBatchContext(tx)
  batch.WhereSQL = "dept_id = ? AND status = ?"    ← 自动从 SQL 提取
  batch.Vars     = [5, 1]                          ← 原始绑定变量
        ↓
用 WHERE 条件反查受影响的记录：
  batch.ReQuery(&users)
  → 内部执行: db.Where("dept_id = ? AND status = ?", 5, 1).Find(&users)
        ↓
遍历记录，执行业务逻辑：
  for _, u := range users { /* 发通知、写日志、清缓存... */ }
```

#### ReQuery API 参考

| 方法/字段 | 签名/类型 | 说明 |
|-----------|----------|------|
| `WhereSQL` | `string` | 提取出的 WHERE 子句字符串（不含 WHERE 关键字） |
| `ReQuery` | `ReQuery(dest interface{}) *gorm.DB` | 用原始 WHERE + Vars 直接反查数据，一行搞定 |
| `ReQueryWithScope` | `ReQueryWithScope(dest interface{}, scope func(*gorm.DB) *gorm.DB) *gorm.DB` | 反查 + 额外条件（Select/Limit/Order 等） |

#### 完整使用示例

**示例 1：批量更新后通知**

```go
func (o *SysUserObserver) AfterUpdate(tx *gorm.DB, model interface{}) error {
    // 1. 先尝试单条 Dirty Tracking
    dirty := observer.GetDirtyFromTx(tx)
    if dirty != nil && dirty.HasChanges() {
        // 单条更新逻辑...
        return nil
    }

    // 2. 批量更新场景 — 用 WHERE 条件反查
    batch := observer.GetBatchContext(tx)
    if batch.RowsAffected > 0 {
        var users []models.SysUser
        batch.ReQuery(&users)

        for _, u := range users {
            log.Printf("用户 %d (%s) 被批量更新", u.Id, u.NickName)
            // 发通知、清缓存等业务逻辑...
        }
    }
    return nil
}
```

**示例 2：批量删除前记录审计日志（BeforeDelete）**

```go
func (o *SysUserObserver) BeforeDelete(tx *gorm.DB, model interface{}) error {
    batch := observer.GetBatchContext(tx)
    if batch.RowsAffected == 0 && batch.WhereSQL != "" {
        // BeforeDelete 中 RowsAffected 还是 0，但 WHERE 已经有了
        var users []models.SysUser
        batch.ReQuery(&users)

        for _, u := range users {
            log.Printf("[审计] 即将删除用户: id=%d, name=%s", u.Id, u.NickName)
        }
    }
    return nil
}
```

**示例 3：带额外条件的反查**

```go
batch := observer.GetBatchContext(tx)
var users []models.SysUser
batch.ReQueryWithScope(&users, func(db *gorm.DB) *gorm.DB {
    return db.Select("id, nick_name, phone").Order("id desc").Limit(100)
})
```

**示例 4：手动使用 WhereSQL**

```go
batch := observer.GetBatchContext(tx)
// 如果你需要更灵活的控制
fmt.Println(batch.WhereSQL) // "dept_id = ? AND status = ?"
fmt.Println(batch.Vars)     // [5, 1]

// 手动构建查询
var count int64
tx.Session(&gorm.Session{NewDB: true}).
    Model(&models.SysUser{}).
    Where(batch.WhereSQL, batch.Vars...).
    Count(&count)
```

#### 注意事项

- `ReQuery` 在 AfterUpdate 中使用时，查到的是**更新后**的数据
- `ReQuery` 在 BeforeDelete 中使用时，查到的是**即将被删除**的数据（推荐在 Before 中记录）
- AfterDelete 中 ReQuery 将查不到数据（已经被删了），所以删除审计应放在 BeforeDelete
- `WhereSQL` 是从完整 SQL 中通过字符串提取的，会自动去除 ORDER BY、LIMIT 等尾部子句

---

## 六、数据补全（FetchFullModel）

### 何时需要

在 `BeforeUpdate` / `AfterUpdate` / `BeforeDelete` 中，如果 `model` 只包含部分字段（如链式调用时），需要补全完整数据。

### 示例

```go
func (o *MyObserver) BeforeUpdate(tx *gorm.DB, model interface{}) error {
    // model 可能只有 id 和要更新的字段，需要补全
    var full models.MyModel
    if err := observer.FetchFullModel(tx, &full); err != nil {
        log.Printf("补全数据失败: %v", err)
        return nil
    }

    // 现在 full 包含数据库中的完整记录
    log.Printf("补全后: %+v", full)
    return nil
}
```

> `FetchFullModel` 内部使用 `tx.Session(&gorm.Session{NewDB: true})` 做补全查询，不会影响当前事务链。

---

## 七、注册与生命周期

### Observer 的注册时机

Observer 在应用启动时注册，**必须在 DB 初始化之后**：

```go
// main.go
import "shop/internal/observers"

func main() {
    // 1. 初始化数据库
    db := initDB()

    // 2. 注册 Observer（在 DB 初始化之后）
    if err := observers.RegisterAll(db); err != nil {
        log.Fatalf("注册 Observer 失败: %v", err)
    }

    // 3. 启动应用...
}
```

### 临时禁用 Observer

使用 GORM 的 `SkipHooks` 会话选项，可在单次操作中跳过所有 Callback（包括 Observer）：

```go
// 本次操作不触发 Observer
db.Session(&gorm.Session{SkipHooks: true}).Create(&user)
db.Session(&gorm.Session{SkipHooks: true}).Model(&user).Update("status", 1)
```

### Observer 的执行顺序

Observer 按**注册顺序**执行。在 `register.go` 中先注册的 Observer 先执行：

```go
registry.Register(
    &ProductObserver{},  // 先执行
    &LogObserver{},      // 后执行
)
```

---

## 八、目录结构

```
internal/
├── observer/       # 框架层（通常不需要改动）
│   ├── interfaces.go       # 事件接口定义
│   ├── registry.go         # 观察者注册中心
│   ├── plugin.go           # GORM 插件实现
│   ├── dirty.go            # DirtyFields / BatchContext 定义
│   ├── dirty_tracker.go    # 字段变更计算逻辑
│   └── helpers.go          # FetchFullModel 等辅助函数
├── observers/      # 业务层（你的代码在这里）
│   ├── xxx_observer.go     # 各模型的观察者
│   └── register.go         # 统一注册入口
```

> 原则：`internal/observer/` 是框架层，不要修改；所有业务 Observer 写在 `internal/observers/` 中。

---

## 九、注意事项和最佳实践

| 注意事项 | 说明 |
|---------|------|
| **Before 系列慎用 error** | `BeforeCreate` / `BeforeUpdate` / `BeforeDelete` 返回 error 会回滚事务，可能导致主操作失败。务必确认需要中断操作时再返回 error。 |
| **AfterXxx 统一返回 nil** | `After` 系列返回 error 不影响已完成的操作，但可能干扰后续处理。建议始终返回 `nil`，如需记录错误用日志代替。 |
| **耗时操作异步化** | Observer 在主线程中同步执行，耗时操作（如发送通知、同步 ES）应投递到队列或 Goroutine 异步处理，避免阻塞请求。 |
| **避免循环触发** | Observer 中修改同表数据时，注意可能再次触发 Observer，导致循环。可用 `SkipHooks` 或添加条件判断避免。 |
| **批量操作没有 Dirty Tracking** | 批量 Update/Delete 无法获取单条字段变更，必须用 `BatchContext` 处理。 |
| **补全查询用 NewDB** | 在 Observer 中做额外查询时，务必使用 `tx.Session(&gorm.Session{NewDB: true})`，避免污染当前事务的 Statement。 |
| **类型断言要安全** | `model` 参数做类型断言时，先判断 `ok`，断言失败时返回 `nil` 不中断流程。 |
| **AfterFind 的 model 可能是切片** | `AfterFind` 的单条查询和多行查询都会触发，model 可能是 `*models.T` 或 `[]*models.T`，需分别处理。 |

