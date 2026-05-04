# GORM 模型观察者（Observer）模式设计方案

## 一、背景

从 PHP/Laravel 转到 Go 开发，Laravel 的模型观察者（Observer）模式是一个非常优秀的设计：注册好服务、做好监听、开发好自己的功能，代码耦合度低。本文探讨如何在 Go + GORM 中实现类似的设计模式。

## 二、GORM 原生机制

### 2.1 Model Hook（模型钩子）

GORM 支持在模型上实现特定方法签名来触发钩子：

```go
BeforeSave(tx *gorm.DB) error
AfterSave(tx *gorm.DB) error
BeforeCreate(tx *gorm.DB) error
AfterCreate(tx *gorm.DB) error
BeforeUpdate(tx *gorm.DB) error
AfterUpdate(tx *gorm.DB) error
BeforeDelete(tx *gorm.DB) error
AfterDelete(tx *gorm.DB) error
AfterFind(tx *gorm.DB) error
```

完整的生命周期执行顺序：

**创建流程：**
```
BeforeSave → BeforeCreate → 执行SQL → AfterCreate → AfterSave
```

**更新流程：**
```
BeforeSave → BeforeUpdate → 执行SQL → AfterUpdate → AfterSave
```

**删除流程：**
```
BeforeDelete → 执行SQL → AfterDelete
```

**查询流程：**
```
执行SQL → AfterFind
```

局限性：Hook 直接写在模型里，业务逻辑和模型强耦合。一个模型只能有一组 Hook 方法，无法为同一事件注册多个处理器。

### 2.2 Callback 系统（全局回调）

GORM 有一个更底层的 Callback 机制，可以在全局层面注册：

```go
db.Callback().Create().After("gorm:create").Register("my_callback", func(tx *gorm.DB) {
    // tx.Statement.Table  → 表名
    // tx.Statement.Model  → 模型实例
    // tx.Statement.Schema → 模型 Schema 信息
})
```

可以针对 Create / Update / Delete / Query / Row / Raw 注册 Before 或 After 回调。

### 2.3 Plugin 接口

GORM 提供了 Plugin 接口，可以把 Callback 封装成可复用的插件：

```go
type Plugin interface {
    Name() string
    Initialize(db *gorm.DB) error
}

// 使用方式
db.Use(myPlugin)
```

## 三、Laravel Observer vs GORM Hook 对比

| 维度 | Laravel Observer | GORM Hook |
|------|-----------------|-----------|
| 解耦程度 | Observer 独立于 Model，通过服务容器注册 | Hook 直接写在 Model 上 |
| 注册方式 | `$model->observe(ProductObserver::class)` | 实现接口方法即自动触发 |
| 多观察者 | 一个 Model 可以注册多个 Observer | 一个 Model 只有一个 Hook 方法 |
| 可插拔性 | 随时注册/取消 | 编译时确定，无法动态开关 |
| 测试友好 | Observer 可独立单元测试 | 依赖 GORM 实例 |

核心差异：Laravel 的 Observer 是「注册制 + 事件驱动」，GORM 的 Hook 是「接口实现制」。

## 四、Observer 模式实现方案

核心思路：用 GORM 的 Callback + Plugin 作为底层事件源，在上层构建注册制的 Observer 分发系统。

### 4.1 架构设计

```
Service 层操作 Model
        ↓
    GORM 执行 SQL
        ↓
    Callback 拦截事件
        ↓
  ObserverRegistry 分发
        ↓
 匹配到的 Observer 执行业务逻辑
```

### 4.2 Observer 接口定义

```go
package observer

import "gorm.io/gorm"

// ModelObserver 模型观察者基础接口
// 每个 Observer 必须声明自己观察的模型（通过表名）
type ModelObserver interface {
    ObserveModel() string
}

// 按需实现的事件接口（Go 小接口设计哲学，类似 io.Reader/io.Writer）
type BeforeCreateObserver interface {
    BeforeCreate(tx *gorm.DB, model interface{}) error
}

type AfterCreateObserver interface {
    AfterCreate(tx *gorm.DB, model interface{}) error
}

type BeforeUpdateObserver interface {
    BeforeUpdate(tx *gorm.DB, model interface{}) error
}

type AfterUpdateObserver interface {
    AfterUpdate(tx *gorm.DB, model interface{}) error
}

type BeforeDeleteObserver interface {
    BeforeDelete(tx *gorm.DB, model interface{}) error
}

type AfterDeleteObserver interface {
    AfterDelete(tx *gorm.DB, model interface{}) error
}

type AfterFindObserver interface {
    AfterFind(tx *gorm.DB, model interface{}) error
}
```

设计要点：
- 用多个小接口而非一个大接口，Observer 只需实现自己关心的事件
- `ObserveModel()` 返回表名用于事件路由
- Before 系列返回 error 可以中断操作，After 系列的 error 用于记录但不中断

### 4.3 注册中心（Registry）

```go
package observer

import "sync"

// ObserverRegistry 观察者注册中心
type ObserverRegistry struct {
    mu        sync.RWMutex
    observers map[string][]ModelObserver // key: 表名, value: 观察者列表（有序）
}

func NewRegistry() *ObserverRegistry {
    return &ObserverRegistry{
        observers: make(map[string][]ModelObserver),
    }
}

// Register 注册观察者 — 类似 Laravel 的 $model->observe()
func (r *ObserverRegistry) Register(obs ...ModelObserver) {
    r.mu.Lock()
    defer r.mu.Unlock()
    for _, o := range obs {
        model := o.ObserveModel()
        r.observers[model] = append(r.observers[model], o)
    }
}

// GetObservers 获取某个模型的所有观察者
func (r *ObserverRegistry) GetObservers(tableName string) []ModelObserver {
    r.mu.RLock()
    defer r.mu.RUnlock()
    return r.observers[tableName]
}

// HasObservers 检查某模型是否有观察者
func (r *ObserverRegistry) HasObservers(tableName string) bool {
    r.mu.RLock()
    defer r.mu.RUnlock()
    return len(r.observers[tableName]) > 0
}
```

### 4.4 GORM Plugin 实现 — 桥接 Callback 和 Observer

```go
package observer

import "gorm.io/gorm"

// ObserverPlugin 实现 gorm.Plugin 接口
type ObserverPlugin struct {
    Registry *ObserverRegistry
}

func NewPlugin(registry *ObserverRegistry) *ObserverPlugin {
    return &ObserverPlugin{Registry: registry}
}

func (p *ObserverPlugin) Name() string {
    return "observer"
}

func (p *ObserverPlugin) Initialize(db *gorm.DB) error {
    // ========== Create 回调 ==========
    db.Callback().Create().Before("gorm:create").Register("observer:before_create", func(tx *gorm.DB) {
        if tx.Error != nil {
            return
        }
        tableName := tx.Statement.Table
        for _, obs := range p.Registry.GetObservers(tableName) {
            if o, ok := obs.(BeforeCreateObserver); ok {
                if err := o.BeforeCreate(tx, tx.Statement.Model); err != nil {
                    _ = tx.AddError(err)
                    return
                }
            }
        }
    })

    db.Callback().Create().After("gorm:create").Register("observer:after_create", func(tx *gorm.DB) {
        if tx.Error != nil {
            return
        }
        tableName := tx.Statement.Table
        for _, obs := range p.Registry.GetObservers(tableName) {
            if o, ok := obs.(AfterCreateObserver); ok {
                if err := o.AfterCreate(tx, tx.Statement.Model); err != nil {
                    _ = tx.AddError(err)
                    return
                }
            }
        }
    })

    // ========== Update 回调 ==========
    db.Callback().Update().Before("gorm:update").Register("observer:before_update", func(tx *gorm.DB) {
        if tx.Error != nil {
            return
        }
        tableName := tx.Statement.Table
        for _, obs := range p.Registry.GetObservers(tableName) {
            if o, ok := obs.(BeforeUpdateObserver); ok {
                if err := o.BeforeUpdate(tx, tx.Statement.Model); err != nil {
                    _ = tx.AddError(err)
                    return
                }
            }
        }
    })

    db.Callback().Update().After("gorm:update").Register("observer:after_update", func(tx *gorm.DB) {
        if tx.Error != nil {
            return
        }
        tableName := tx.Statement.Table
        for _, obs := range p.Registry.GetObservers(tableName) {
            if o, ok := obs.(AfterUpdateObserver); ok {
                if err := o.AfterUpdate(tx, tx.Statement.Model); err != nil {
                    _ = tx.AddError(err)
                    return
                }
            }
        }
    })

    // ========== Delete 回调 ==========
    db.Callback().Delete().Before("gorm:delete").Register("observer:before_delete", func(tx *gorm.DB) {
        if tx.Error != nil {
            return
        }
        tableName := tx.Statement.Table
        for _, obs := range p.Registry.GetObservers(tableName) {
            if o, ok := obs.(BeforeDeleteObserver); ok {
                if err := o.BeforeDelete(tx, tx.Statement.Model); err != nil {
                    _ = tx.AddError(err)
                    return
                }
            }
        }
    })

    db.Callback().Delete().After("gorm:delete").Register("observer:after_delete", func(tx *gorm.DB) {
        if tx.Error != nil {
            return
        }
        tableName := tx.Statement.Table
        for _, obs := range p.Registry.GetObservers(tableName) {
            if o, ok := obs.(AfterDeleteObserver); ok {
                if err := o.AfterDelete(tx, tx.Statement.Model); err != nil {
                    _ = tx.AddError(err)
                    return
                }
            }
        }
    })

    // ========== Query 回调 ==========
    db.Callback().Query().After("gorm:query").Register("observer:after_find", func(tx *gorm.DB) {
        if tx.Error != nil {
            return
        }
        tableName := tx.Statement.Table
        for _, obs := range p.Registry.GetObservers(tableName) {
            if o, ok := obs.(AfterFindObserver); ok {
                if err := o.AfterFind(tx, tx.Statement.Model); err != nil {
                    _ = tx.AddError(err)
                    return
                }
            }
        }
    })

    return nil
}
```

### 4.5 具体 Observer 示例

#### ProductObserver — 商品观察者

```go
package observers

import (
    "gorm.io/gorm"
    "log"
    "shop/internal/models"
)

type ProductObserver struct{}

func (o *ProductObserver) ObserveModel() string {
    return "store_product"
}

// AfterCreate 商品创建后 — 同步ES、记录日志
func (o *ProductObserver) AfterCreate(tx *gorm.DB, model interface{}) error {
    if product, ok := model.(*models.StoreProduct); ok {
        log.Printf("[ProductObserver] 商品创建: %s (ID: %d)", product.StoreName, product.Id)
        // TODO: 同步到 Elasticsearch
        // TODO: 发送新品通知
    }
    return nil
}

// AfterUpdate 商品更新后 — 更新ES索引
func (o *ProductObserver) AfterUpdate(tx *gorm.DB, model interface{}) error {
    if product, ok := model.(*models.StoreProduct); ok {
        log.Printf("[ProductObserver] 商品更新: ID=%d", product.Id)
        // TODO: 更新 ES 索引
        // TODO: 如果价格变动，通知关注用户
    }
    return nil
}

// AfterDelete 商品删除后 — 清理ES索引
func (o *ProductObserver) AfterDelete(tx *gorm.DB, model interface{}) error {
    log.Printf("[ProductObserver] 商品删除, 清理ES索引...")
    // TODO: 从 ES 删除索引
    return nil
}
```

#### OrderObserver — 订单观察者

```go
package observers

import (
    "gorm.io/gorm"
    "log"
    "shop/internal/models"
)

type OrderObserver struct{}

func (o *OrderObserver) ObserveModel() string {
    return "store_order"
}

// AfterCreate 订单创建后 — 记录状态、发通知
func (o *OrderObserver) AfterCreate(tx *gorm.DB, model interface{}) error {
    if order, ok := model.(*models.StoreOrder); ok {
        log.Printf("[OrderObserver] 订单创建: %s, 用户: %d", order.OrderId, order.Uid)
        // TODO: 记录订单状态变更
        // TODO: 发送下单成功通知
        // TODO: 启动超时未支付自动取消任务
    }
    return nil
}

// AfterUpdate 订单更新后 — 状态变更处理
func (o *OrderObserver) AfterUpdate(tx *gorm.DB, model interface{}) error {
    if order, ok := model.(*models.StoreOrder); ok {
        log.Printf("[OrderObserver] 订单更新: %s, 状态: %d", order.OrderId, order.Status)
        // TODO: 根据状态变化触发不同逻辑
        // 比如：已支付 → 通知商家；已发货 → 通知买家
    }
    return nil
}
```

### 4.6 启动注册 — 类似 Laravel ServiceProvider::boot()

```go
package observers

import "gorm.io/gorm"
import "shop/internal/observer"

// RegisterAll 注册所有观察者 — 在应用启动时调用
func RegisterAll(db *gorm.DB) error {
    registry := observer.NewRegistry()

    // 注册所有观察者
    registry.Register(
        &ProductObserver{},
        &OrderObserver{},
        // 未来新增 Observer 只需在这里加一行
    )

    // 安装 GORM 插件
    plugin := observer.NewPlugin(registry)
    return db.Use(plugin)
}
```

在 main.go 或数据库初始化处调用：

```go
// 初始化数据库后
observers.RegisterAll(global.Db)
```

## 五、推荐目录结构

```
shop-main/
├── internal/
│   ├── models/            # 模型层 — 保持纯净，只定义结构体和表名
│   │   ├── base_model.go
│   │   ├── store_product.go
│   │   ├── store_order.go
│   │   └── ...
│   ├── observer/          # 【新增】观察者框架核心 — 可跨项目复用
│   │   ├── interfaces.go      # Observer 接口定义
│   │   ├── registry.go        # 注册中心
│   │   └── plugin.go          # GORM Plugin 实现
│   ├── observers/         # 【新增】具体观察者实现 — 业务相关
│   │   ├── product_observer.go
│   │   ├── order_observer.go
│   │   ├── user_observer.go
│   │   └── register.go        # 统一注册入口
│   ├── service/           # Service 层 — 无需改动
│   └── controllers/       # Controller 层 — 无需改动
```

分层原则：
- `internal/observer/` — **框架层**，定义接口和机制，纯粹的设计模式实现，可抽离为独立包
- `internal/observers/` — **业务层**，具体的观察者实现，包含业务逻辑
- `internal/models/` — **模型层**，保持干净，不写业务逻辑

## 六、与现有项目集成指南

### 6.1 零风险渐进式迁移

1. 先创建 `internal/observer/` 框架代码
2. 创建 `internal/observers/` 并编写第一个 Observer（建议从 ProductObserver 开始）
3. 在数据库初始化后调用 `observers.RegisterAll(global.Db)`
4. Observer 中先只做日志打印，验证事件触发正确
5. 逐步将 service 层中的「副作用逻辑」（日志、通知、同步等）迁移到 Observer
6. 确认稳定后，清理 service 层中已迁移的代码

### 6.2 注意事项

- **事务安全**：Before 系列的 Observer 在事务内执行，返回 error 会回滚事务
- **异步处理**：耗时操作（发邮件、推送通知）建议在 Observer 中投递到队列，而非同步执行
- **性能影响**：Observer 在每次数据库操作时触发，确保逻辑轻量
- **测试方式**：Observer 可以独立创建 mock 的 gorm.DB 进行单元测试

## 七、功能对比总结

| 对比点 | 直接用 GORM Hook | Observer 模式 |
|--------|-----------------|--------------|
| 代码位置 | 散落在各个 Model 文件 | 集中在 observers 目录 |
| 一个事件多个处理 | 不支持 | 支持多个 Observer |
| 可测试性 | 难，依赖 GORM | Observer 可独立单测 |
| 开关控制 | 不行 | 注册时控制 |
| 新增监听 | 改 Model 文件 | 新增 Observer + 注册 |
| 模型纯净度 | 模型混入业务逻辑 | 模型保持纯粹 |

## 八、Observer 中 model 数据完整性问题（关键）

> **这是实际落地中最容易踩的坑。** 不同的 GORM 调用方式，Observer 中拿到的 model 数据完整度截然不同。类比 PHP 框架：Laravel Observer 拿到的是完整模型实例，而 ThinkPHP 老版本的模型事件只能拿到更新的部分字段。GORM 的行为取决于你的调用方式。

### 8.1 各操作场景下 model 的数据情况

#### Create — 完整模型（等同 Laravel）

```go
// 项目中现有写法 (store_product.go)
func AddProduct(m *StoreProduct) error {
    return global.Db.Create(m).Error
}
```

Observer 中 `tx.Statement.Model` 拿到的是 **完整的 `*StoreProduct` 实例**，`AfterCreate` 时 GORM 已经把自增 ID 回填。所有字段都可用，这和 Laravel 完全一致。

#### Update（链式调用）— 只有部分数据（类似 TP 老版本）

```go
// 项目中现有写法 (store_product.go)
func UpdateByProduct(id int64, m *StoreProduct) error {
    return global.Db.Model(&StoreProduct{}).Where("id = ?", id).Updates(m).Error
}
```

这种链式调用下：
- `tx.Statement.Model` → `&StoreProduct{}`，**空结构体！** 没有 ID，没有任何业务数据
- `tx.Statement.Dest` → 传入的 `m`，**只包含要更新的字段**，不是完整记录
- 你在 Observer 里想拿订单号、用户ID 等关键字段来做业务联动，**直接用 model 是拿不到的**

#### Update（Save 方式）— 完整模型

```go
// 如果用 Save 方式
var product StoreProduct
global.Db.First(&product, id)
product.Price = 99.9
global.Db.Save(&product)
```

`Save` 方式下 `tx.Statement.Model` 是完整模型，所有字段可用。

#### Delete — 基本是空的

```go
// 项目中现有写法 (store_product.go)
func DelByProduct(ids []int64) error {
    return global.Db.Where("id in (?)", ids).Delete(&StoreProduct{}).Error
}
```

`tx.Statement.Model` 是 `&StoreProduct{}`，空结构体。连删的是哪条记录都不知道，只能从 `tx.Statement.Vars` 中解析条件参数。

### 8.2 数据完整性对照表

| 操作方式 | `tx.Statement.Model` | `tx.Statement.Dest` | Observer 可用性 | 类比 PHP |
|---------|----------------------|---------------------|----------------|---------|
| `db.Create(&product)` | 完整模型（含回填ID） | 同 Model | 直接可用 | Laravel |
| `db.Save(&product)` | 完整模型 | 同 Model | 直接可用 | Laravel |
| `db.Model(&T{}).Where(...).Updates(m)` | 空结构体 | 部分字段 | **需补全** | TP 老版本 |
| `db.Where(...).Delete(&T{})` | 空结构体 | 空 | **需补全** | TP 老版本 |

### 8.3 解决方案

#### 方案 A：Observer 中按需查询补全（推荐，改动最小）

适用场景：不想大改现有 Model 层代码，先让 Observer 跑起来。

```go
// order_observer.go
func (o *OrderObserver) AfterUpdate(tx *gorm.DB, model interface{}) error {
    var order models.StoreOrder

    // 优先从 Dest 获取 ID
    if dest, ok := tx.Statement.Dest.(*models.StoreOrder); ok && dest.Id > 0 {
        // 用新 Session 查询完整数据（避免影响当前事务链）
        if err := tx.Session(&gorm.Session{NewDB: true}).First(&order, dest.Id).Error; err != nil {
            return nil // 查不到就跳过，不影响主流程
        }
    } else {
        return nil // 无法获取 ID，跳过
    }

    // 现在 order 是完整数据
    log.Printf("[OrderObserver] 订单 %s 已更新, 用户: %d, 状态: %d",
        order.OrderId, order.Uid, order.Status)
    return nil
}
```

注意事项：
- 使用 `tx.Session(&gorm.Session{NewDB: true})` 创建新 Session，避免污染当前操作的查询条件
- 补全查询会多一次 DB 请求，对于高频操作需评估性能影响
- 建议只对需要完整数据的 Observer 做补全，简单的日志记录不需要

#### 方案 B：改造 Model 层调用方式（推荐，长期方案）

核心思路：将「链式 Update」改为「先查后改」，让 Observer 天然拿到完整数据。

```go
// 改造前（Observer 拿不到完整数据）
func UpdateByProduct(id int64, m *StoreProduct) error {
    return global.Db.Model(&StoreProduct{}).Where("id = ?", id).Updates(m).Error
}

// 改造后（Observer 可以拿到完整模型）
func UpdateByProduct(id int64, updates map[string]interface{}) error {
    var product StoreProduct
    if err := global.Db.First(&product, id).Error; err != nil {
        return err
    }
    // 此时 tx.Statement.Model = &product（完整数据）
    return global.Db.Model(&product).Updates(updates).Error
}
```

对于 Delete 操作，同理先查再删：

```go
// 改造前
func DelByProduct(ids []int64) error {
    return global.Db.Where("id in (?)", ids).Delete(&StoreProduct{}).Error
}

// 改造后（Observer 可以拿到被删记录的完整数据）
func DelByProduct(ids []int64) error {
    var products []StoreProduct
    global.Db.Where("id in (?)", ids).Find(&products)
    if len(products) == 0 {
        return nil
    }
    return global.Db.Delete(&products).Error
}
```

#### 方案 C：在 Plugin 层统一封装数据补全

在 Observer 框架的 Plugin 层提供统一的数据获取辅助方法，对 Observer 开发者透明：

```go
// observer/helpers.go

// FetchFullModel 从 tx 中获取完整模型数据
// 如果 Model 已包含主键，直接返回；否则尝试从 Dest 或条件中获取主键后查询
func FetchFullModel(tx *gorm.DB, dest interface{}) error {
    if tx.Statement.Schema == nil {
        return fmt.Errorf("schema not found")
    }

    // 尝试从 Dest 中获取主键
    primaryField := tx.Statement.Schema.PrioritizedPrimaryField
    if primaryField == nil {
        return fmt.Errorf("primary field not found")
    }

    // 从 Dest 中获取主键值
    if tx.Statement.Dest != nil {
        destValue := reflect.ValueOf(tx.Statement.Dest)
        if destValue.Kind() == reflect.Ptr {
            destValue = destValue.Elem()
        }
        if destValue.Kind() == reflect.Struct {
            field := destValue.FieldByName(primaryField.Name)
            if field.IsValid() && !field.IsZero() {
                return tx.Session(&gorm.Session{NewDB: true}).First(dest, field.Interface()).Error
            }
        }
    }

    return fmt.Errorf("cannot determine primary key from context")
}
```

使用方式：
```go
func (o *ProductObserver) AfterUpdate(tx *gorm.DB, model interface{}) error {
    var product models.StoreProduct
    if err := observer.FetchFullModel(tx, &product); err != nil {
        return nil // 无法补全，跳过
    }
    // product 现在是完整数据
    log.Printf("[ProductObserver] 商品 %s 更新", product.StoreName)
    return nil
}
```

### 8.4 推荐策略：A + B 结合的渐进式方案

1. **第一阶段**（快速上线）：使用方案 A，在需要完整数据的 Observer 中做查询补全
2. **第二阶段**（逐步改造）：对核心模型（Order、Product）的 Model 层改为「先查后改」方式（方案 B）
3. **第三阶段**（框架完善）：将方案 C 的辅助方法集成到框架层，降低 Observer 开发者的心智负担

这样既不需要一步到位大改所有代码，又能保证 Observer 的数据可靠性。

---

## 九、常见问题

**Q1: Observer 中可以访问旧数据吗（类似 Laravel 的 getOriginal）？**
GORM 的 `tx.Statement.Changed()` 可以检测字段是否变化，但不像 Laravel 那样直接提供旧值。如需旧值，可以在 BeforeUpdate 中先查询一次。

**Q2: 如何临时禁用 Observer？**
可以通过 Session 的方式：`db.Session(&gorm.Session{SkipHooks: true})` 跳过所有回调。

**Q3: Observer 的执行顺序？**
按注册顺序执行。先注册的 Observer 先执行。

**Q4: 批量操作会触发 Observer 吗？**
GORM 的批量操作（如 `db.Create(&products)` 批量创建）会触发回调，但传入的 model 是切片类型，Observer 中需要做类型判断处理。

**Q5: 性能影响大吗？**
Observer 本身的分发开销极小（就是 map 查找 + 接口断言）。性能瓶颈在于 Observer 内部的业务逻辑，建议耗时操作异步化。

---

*本文档基于项目现有架构设计。理解吃透后按照第六章的集成指南渐进式落地，第八章的数据完整性方案是实施中的关键点，务必重点关注。*
*后续重构时可直接基于本文档的代码方案开始实现。*
