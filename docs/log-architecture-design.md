# 商城系统日志架构设计文档

> 面向初学者的图解指南，带你理解日志系统改造的每一个设计决策。

---

## 目录

- [第一部分：项目背景与问题](#第一部分项目背景与问题)
- [第二部分：整体架构设计](#第二部分整体架构设计)
- [第三部分：使用的设计模式详解](#第三部分使用的设计模式详解)
- [第四部分：日志文件切割策略](#第四部分日志文件切割策略)
- [第五部分：通用改造模式](#第五部分通用改造模式)
- [第六部分：与业界标准对比](#第六部分与业界标准对比)
- [第七部分：关键代码索引](#第七部分关键代码索引)

---

## 第一部分：项目背景与问题

### 1.1 改造前：多套日志体系并存的困境

想象一下：你开了一家大商场，每个店铺用自己的收银系统——有的用现金、有的用刷卡、有的用支付宝、有的甚至用记账本。作为商场经理，你想统计今天的总营业额，得一家一家去对账，痛苦不堪。

我们的项目在改造前就面临同样的问题：

```
改造前的日志现状（混乱）
═══════════════════════════════════════════════════════

  业务代码                日志输出去哪里？
  ─────────              ──────────────────
  订单模块    ──→  混在一个大文件里，分不清谁是谁
  商品模块    ──→  混在一个大文件里
  用户模块    ──→  混在一个大文件里
  MySQL 查询  ──→  没有独立日志，出问题无法追踪
  Kafka MQ   ──→  没有独立日志

  结果：一个 log 文件几百 MB，打开就卡死
       出了问题像大海捞针，无法快速定位
```

**具体痛点：**

| 痛点 | 描述 | 影响 |
|------|------|------|
| 🔴 日志混杂 | 所有模块写同一个文件 | 无法按模块排查问题 |
| 🔴 文件过大 | 没有合理的切割策略 | 打开文件就要等很久 |
| 🔴 接口不统一 | 不同包用不同的日志库 | 无法统一管理日志级别、格式 |
| 🔴 外部包无法注入 | db/mq 等包使用默认日志 | 生产环境无法收集日志 |

### 1.2 改造目标

```
改造后的理想状态
═══════════════════════════════════════════════════════

  runtime/logs/
  ├── app/          ← 全局应用日志
  ├── order/        ← 订单模块独立日志
  ├── product/      ← 商品模块独立日志
  ├── user/         ← 用户模块独立日志
  ├── auth/         ← 鉴权日志
  ├── http/         ← HTTP 请求日志
  ├── mysql/        ← MySQL 业务日志
  ├── mysql_query/  ← SQL 查询日志（与 mysql 分离）
  └── mq/           ← Kafka 消息队列日志

  效果：
  ✅ 每个模块独立文件，按日期自动切割
  ✅ 统一接口，一处改配置全局生效
  ✅ 外部包（db/mq）日志也纳入管理
  ✅ JSON 格式输出，ELK 可直接采集
```

**三大核心目标：**

1. **统一日志接口** — 所有模块用同一套 API 写日志
2. **支持自定义日志文件注入** — 外部包（db、mq）也能写入我们指定的文件
3. **按模块分离** — 每个业务模块有独立的日志目录和文件

---

## 第二部分：整体架构设计

### 2.1 日志模块三层架构图

整个日志系统分为三层，每层各司其职：

```
┌─────────────────────────────────────────────────────────────┐
│                    业务代码调用层                              │
│                                                             │
│   GetLogger("order")  ──→  *zap.SugaredLogger（方便用）      │
│   GetZapLogger("mysql") ──→ *zap.Logger（给适配器用）        │
│                                                             │
│   💡 业务代码只需关心"我要往哪个模块写日志"                     │
├─────────────────────────────────────────────────────────────┤
│                  LoggerManager 管理层                        │
│                                                             │
│   ┌──────────────────────────────────────────────────┐      │
│   │  sync.Map<moduleName, *zap.Logger>               │      │
│   │                                                  │      │
│   │  "app"     → *zap.Logger (带 caller)             │      │
│   │  "order"   → *zap.Logger (带 caller)             │      │
│   │  "mysql"   → *zap.Logger (带 caller)             │      │
│   │  "mq"      → *zap.Logger (带 caller)             │      │
│   │  ...                                             │      │
│   └──────────────────────────────────────────────────┘      │
│                                                             │
│   💡 并发安全、按需创建、启动时预注册                          │
├─────────────────────────────────────────────────────────────┤
│                    Writer 写入层                             │
│                                                             │
│   dailyWriter: 按天 + 按大小 双重切割                         │
│   文件命名: {module}_{YYYYMMDD}.log                          │
│   超限命名: {module}_{YYYYMMDD}.{seq}.log                    │
│                                                             │
│   💡 自动管理文件的生命周期，不需要人工干预                      │
└─────────────────────────────────────────────────────────────┘
```

**各层职责说明：**

| 层级 | 职责 | 类比 |
|------|------|------|
| 调用层 | 提供简单易用的 API 给业务代码 | 商场里各店铺的收银台 |
| 管理层 | 管理所有模块的 Logger 实例，并发安全 | 商场的统一收银系统 |
| 写入层 | 负责日志文件的创建、切割、写入 | 商场的账本 storage |

### 2.2 外部包日志注入流程图

这是本次改造最关键的设计——如何让外部包（db、mq）的日志写入我们指定的文件：

```
                    项目 logging 模块                          外部 pkg 包
              ┌───────────────────────┐                ┌──────────────────────┐
              │                       │                │                      │
              │  GetZapLogger("mysql")│                │      db 包            │
              │         │             │                │                      │
              │         ▼             │                │  InitMysqlClient     │
              │   *zap.Logger         │                │  WithOptions()       │
              │         │             │                │                      │
              │         │             │   ┌────────┐   │  ┌────────────────┐  │
              │         └─────────────┼──→│zapx.New│──→├──│  mysqlLogger   │  │
              │                       │   │(适配器) │   │  │ (logx.Logger)  │  │
              │                       │   └────────┘   │  └────────┬───────┘  │
              │                       │                │           │          │
              │                       │                │  ┌────────▼───────┐  │
              │                       │                │  │  gormLogger    │  │
              │                       │                │  │ (GORM 适配器)   │  │
              │                       │                │  └────────────────┘  │
              │                       │                │                      │
              │                       │                │  日志写入:            │
              │                       │                │  runtime/logs/mysql/ │
              └───────────────────────┘                └──────────────────────┘


              ┌───────────────────────┐                ┌──────────────────────┐
              │                       │                │                      │
              │  GetZapLogger("mq")   │                │      mq 包            │
              │         │             │                │                      │
              │         ▼             │   ┌────────┐   │  InitSyncKafka      │
              │   *zap.Logger         │──→│zapx.New│   │  Producer()         │
              │         │             │   └────┬───┘   │                      │
              │         └─────────────┼────────┘       │  ┌────────────────┐  │
              │                       │                │  │  kafka producer │  │
              │         │             │                │  │  kafka consumer │  │
              │         └─────────────┼──→mq.SetLogger()→│  sarama logger  │  │
              │                       │                │  └────────────────┘  │
              │                       │                │                      │
              │                       │                │  日志写入:            │
              │                       │                │  runtime/logs/mq/    │
              └───────────────────────┘                └──────────────────────┘
```

**注入链路解读（以 db 为例）：**

```
步骤拆解：

  ① logging.GetZapLogger("mysql")
     └→ 从 LoggerManager 获取 mysql 模块的 *zap.Logger
        这个 Logger 写入 runtime/logs/mysql/ 目录

  ② zapx.New(zapLogger)
     └→ 把 *zap.Logger 包装成 logx.Logger 接口
        db 包只认 logx.Logger，不关心底层是什么

  ③ db.WithLogger(mysqlLogger)
     └→ 通过函数式选项注入到 db 实例
        db 内部所有日志都走这个 Logger

  结果：db 的日志自动写入 runtime/logs/mysql/ 目录 ✅
```

### 2.3 适配器模式转换图

不同外部包使用不同的日志接口类型，我们需要"翻译官"来桥接：

```
┌──────────────────────────────────────────────────────────────────────┐
│                        类型适配全景图                                 │
│                                                                      │
│   logging 模块              适配器              外部包接口              │
│   (项目侧提供)            (翻译官)            (外部包要求)             │
│                                                                      │
│   ┌──────────────┐                              ┌──────────────┐     │
│   │ *zap.Logger  │─────→ zapx.New() ────→       │ db 包         │     │
│   │              │     (zap → logx)     │logx.Logger│            │     │
│   └──────────────┘                        │     │ WithLogger() │     │
│          │                                │     └──────────────┘     │
│          │                                │                          │
│          │           ┌──────────────────────────────────────┐        │
│          │           │         logx.Logger 接口              │        │
│          │           │                                      │        │
│          │           │  Info(msg, fields...)                │        │
│          │           │  Warn(msg, fields...)                │        │
│          │           │  Error(msg, fields...)               │        │
│          │           │  Debug(msg, fields...)               │        │
│          │           │  WithFields(fields...) Logger        │        │
│          │           │                                      │        │
│          │           │  💡 框架无关，不绑定 zap/logrus/标准库 │        │
│          │           └──────────────────────────────────────┘        │
│          │                                │                          │
│          │                                │     ┌──────────────┐     │
│          └───────────────────────────────→│logx.Logger│ mq 包    │     │
│                                           │     │ SetLogger()  │     │
│                                           │     └──────────────┘     │
│                                           │                          │
└──────────────────────────────────────────────────────────────────────┘
```

---

## 第三部分：使用的设计模式详解

### 3.1 适配器模式 (Adapter Pattern)

**一句话解释：** 就像电源转换插头——不同国家的插头形状不同，但用了转换头就能插到中国的插座上。

**在本项目中的体现：**

| 适配器 | 把什么 | 转成什么 | 用在哪里 |
|--------|--------|---------|---------|
| `zapx.New()` | `*zap.Logger` | `logx.Logger` | db 包、mq 包的日志注入 |
| `gormLogger` | `gorm.logger.Interface` | `logx.Logger` | GORM 框架的日志桥接 |
| `saramaLogger` | `sarama.StdLogger` | `logx.Logger` | Kafka Sarama 的日志桥接 |

**图解：**

```
  现实世界的类比：

  ┌──────────┐     ┌──────────────┐     ┌──────────────┐
  │ 美国插头  │────→│  转换适配器  │────→│  中国插座    │
  │ (两脚扁)  │     │              │     │ (三脚扁)    │
  └──────────┘     └──────────────┘     └──────────────┘

  代码世界的对应：

  ┌──────────┐     ┌──────────────┐     ┌──────────────┐
  │*zap.Logger│────→│  zapx.New()  │────→│ logx.Logger  │
  │(zap 库)   │     │  (适配器)     │     │ (统一接口)   │
  └──────────┘     └──────────────┘     └──────────────┘

  为什么需要适配？
  因为 db 包的 WithLogger() 只接受 logx.Logger 类型
  而我们的 logging 模块产生的是 *zap.Logger 类型
  zapx.New() 就是那个"转换插头"
```

**为什么用这个模式？**

- **解耦**：业务代码不直接依赖具体的日志库，换日志库只改适配器
- **复用**：logx.Logger 接口已经定义好了，不需要为每个外部包重新实现
- **扩展**：将来接入新的外部包，只需要写一个新的适配器

---

### 3.2 依赖注入 (Dependency Injection)

**一句话解释：** 不是自己造水，而是接上外面的水管——你不需要自己打井，市政供水帮你搞定。

**在本项目中的体现：**

```
  没有依赖注入（自己造水）：         有依赖注入（接水管）：

  ┌──────────────┐                 ┌──────────────┐
  │   db 包      │                 │   db 包      │
  │              │                 │              │
  │ 内部自己创建  │                 │ 等待外部注入  │
  │ 默认 Logger  │                 │ ↓            │
  │ → 写到默认   │                 │ WithLogger() │
  │   控制台     │                 │ → 写到我们    │
  │              │                 │   指定的文件  │
  └──────────────┘                 └──────────────┘

  问题：日志散落在控制台，         效果：日志统一写入
  生产环境无法收集                  runtime/logs/mysql/
```

**两种注入方式对比：**

```
  方式 A：构造时注入（db 包）              方式 B：全局注入（mq 包）

  db.InitMysqlClientWithOptions(          mq.SetLogger(mqLogger)
      db.DefaultClient,                   // 设置一次，所有 MQ 实例
      ...,                                   自动使用这个 Logger
      db.WithLogger(mysqlLogger),
      db.WithSQLLogger(mysqlQueryLogger),  特点：简单粗暴，全局生效
  )                                       缺点：无法按实例区分

  特点：精确控制每个实例的 Logger
  优点：不同 DB 可以用不同日志
```

**为什么用这个模式？**

- **灵活性**：可以在运行时决定用哪个 Logger
- **可测试性**：测试时可以注入一个假 Logger
- **可控性**：项目侧完全掌控日志的输出目标

---

### 3.3 函数式选项模式 (Functional Options Pattern)

**一句话解释：** 像点菜一样——你不需要告诉服务员"我不要葱、不要蒜、多加辣"，只需要一个个勾选选项就行。

**在本项目中的体现：**

```
  去餐厅点菜（类比）：                代码中的用法：

  ┌────────────────────┐             ┌──────────────────────────────┐
  │  基础菜品：牛排     │             │  基础初始化：DB 连接          │
  │                    │             │                              │
  │  + 不要葱 (选项1)  │             │  + WithLogger(logger)        │
  │  + 不要蒜 (选项2)  │             │  + WithSQLLogger(qLogger)    │
  │  + 多加辣 (选项3)  │             │  + WithEnableSqlLog(true)    │
  │                    │             │                              │
  │  每个选项独立可选   │             │  每个配置独立、可选、可扩展    │
  └────────────────────┘             └──────────────────────────────┘
```

**代码示例：**

```go
// 函数式选项的本质：一个函数，接收配置指针并修改它
type Option func(*option)

// 每个 WithXxx 返回一个 Option
func WithLogger(l logx.Logger) Option {
    return func(o *option) {
        o.logger = l       // 修改配置中的 logger 字段
    }
}

func WithSQLLogger(l logx.Logger) Option {
    return func(o *option) {
        o.sqlLogger = l    // 修改配置中的 sqlLogger 字段
    }
}

// 使用：像搭积木一样按需组合
db.InitMysqlClientWithOptions(
    db.DefaultClient,
    user, password, host, name,
    db.WithLogger(mysqlLogger),         // 想要自定义日志？加上这个
    db.WithSQLLogger(mysqlQueryLogger), // 想要 SQL 日志分离？加上这个
    db.WithEnableSqlLog(true),          // 想要开启 SQL 日志？加上这个
)
```

**为什么用这个模式？**

- **可扩展**：新增选项不需要改函数签名（加一个 `WithXxx` 就行）
- **可读性好**：`WithLogger(xxx)` 一看就知道在干什么
- **向后兼容**：老代码不传选项也能正常工作（有默认值兜底）

---

### 3.4 接口隔离原则 (Interface Segregation)

**一句话解释：** USB 接口标准——不管你是罗技的鼠标还是戴尔的键盘，只要符合 USB 标准就能插上用。

**在本项目中的体现：**

```
  logx.Logger 接口定义（只有 5 个方法）：

  ┌────────────────────────────────────────────┐
  │           logx.Logger 接口                  │
  │                                            │
  │  Info(msg string, fields ...LogField)      │
  │  Warn(msg string, fields ...LogField)      │
  │  Error(msg string, fields ...LogField)     │
  │  Debug(msg string, fields ...LogField)     │
  │  WithFields(fields ...LogField) Logger     │
  │                                            │
  │  💡 只有 5 个方法，非常精简                  │
  │  💡 不依赖 zap.Field，用自己定义的 LogField  │
  │  💡 任何日志库都能实现这个接口                │
  └────────────────────────────────────────────┘
```

**对比：如果接口设计得不好（反面教材）：**

```
  ❌ 坏设计（绑定了具体实现）：          ✅ 好设计（logx 的做法）：

  type Logger interface {              type Logger interface {
      ZapLogger() *zap.Logger             Info(msg, fields...)
      SetLevel(zapcore.Level)             Warn(msg, fields...)
      AddHook(zapcore.Core)               Error(msg, fields...)
      // 这些方法只有 zap               Debug(msg, fields...)
      // 其他日志库根本无法实现            WithFields(fields...) Logger
  }                                  }

  问题：接口绑定了 zap 库             效果：任何日志库都能实现
  换日志库就要改接口                   换日志库只改适配器
```

**为什么用这个原则？**

- **通用性**：不绑定任何具体日志库，将来可以换
- **简洁性**：只定义必要的方法，实现起来简单
- **稳定性**：接口小意味着变化少，不容易破坏兼容性

---

### 3.5 类型别名委托 (Type Alias Delegation)

**一句话解释：** 给一个人起了个外号——他身份证号没变，只是大家叫他外号更方便。

**在本项目中的体现：**

```go
// mq 包中的类型别名定义
type Logger = logx.Logger      // Logger 就是 logx.Logger，完全等价
type LogField = logx.LogField  // LogField 就是 logx.LogField

// 这意味着：
// - mq.Logger 和 logx.Logger 是同一个类型（不是新类型）
// - 任何实现了 logx.Logger 的值都可以直接赋给 mq.Logger
// - 保持了 mq 包的 API 向后兼容（老代码用 mq.Logger 的不用改）
```

**图解：**

```
  类型别名 vs 类型定义的区别：

  类型别名（=）：               类型定义（没有 =）：

  type Logger = logx.Logger    type Logger logx.Logger
       │                            │
       ▼                            ▼
  完全等价，可以互换             全新的类型，需要转换
  mq.Logger ≡ logx.Logger      mq.Logger ≠ logx.Logger

  ✅ 老代码 mq.Logger 继续生效   ❌ 老代码需要改类型
  ✅ 新代码用 logx.Logger 也行   ❌ 需要手动转换类型
```

**为什么用这个模式？**

- **向后兼容**：已有的 `mq.Logger` 类型引用不需要修改
- **统一底层**：虽然名字不同，但底层都是 `logx.Logger`
- **零成本**：类型别名在编译后完全消失，没有运行时开销

---

### 3.6 三级优先级兜底策略

**一句话解释：** 像机场安检通道——VIP 通道优先，没有就走普通通道，再没有就走默认通道。总之你一定能过安检。

**在本项目中的体现：**

```
  ┌──────────────────────────────────────────────────────────────┐
  │                  三级优先级兜底策略                             │
  │                                                              │
  │   优先级 1（最高）：Option 注入                                │
  │   ┌────────────────────────────────────────────────┐         │
  │   │ db.WithLogger(自定义Logger)                    │         │
  │   │ → 用你传入的 Logger                            │         │
  │   └──────────────────────┬─────────────────────────┘         │
  │                          │ 没传？                             │
  │                          ▼                                   │
  │   优先级 2（中等）：全局 SetLogger                             │
  │   ┌────────────────────────────────────────────────┐         │
  │   │ logx.SetLogger(全局Logger)                     │         │
  │   │ → 用全局设置的 Logger                          │         │
  │   └──────────────────────┬─────────────────────────┘         │
  │                          │ 也没设？                           │
  │                          ▼                                   │
  │   优先级 3（兜底）：默认控制台                                 │
  │   ┌────────────────────────────────────────────────┐         │
  │   │ defaultLogger                                  │         │
  │   │ → 输出到控制台，开发环境够用                     │         │
  │   └────────────────────────────────────────────────┘         │
  │                                                              │
  │   💡 无论如何，日志都不会丢失                                  │
  └──────────────────────────────────────────────────────────────┘
```

**实际场景举例：**

```
  场景 1：生产环境（Option 注入生效）
  ─────────────────────────────────
  main.go 中：
    db.WithLogger(mysqlLogger)    ← 传入了自定义 Logger
    → 日志写入 runtime/logs/mysql/

  场景 2：开发环境（全局 SetLogger 生效）
  ─────────────────────────────────
  main.go 中：
    logx.SetLogger(zapx.New(projectLogger))
    → 所有没传 Option 的包都用这个全局 Logger

  场景 3：测试/忘记配置（默认控制台兜底）
  ─────────────────────────────────
  什么都没配置
    → 日志输出到控制台，不会丢日志
```

---

## 第四部分：日志文件切割策略

### 4.1 双重切割策略流程图

我们的 `dailyWriter` 实现了"按天 + 按大小"的双重切割策略：

```
                    写入一条日志
                         │
                         ▼
              ┌─────────────────────┐
              │  今天的日期变了吗？    │
              │  (today != curDate)  │
              └─────────┬───────────┘
                    ┌───┴───┐
                   是       否
                    │        │
                    ▼        ▼
         ┌──────────────┐  ┌─────────────────────┐
         │ 创建新日期文件  │  │  当前文件超过 maxSize？│
         │ 序号归零       │  │  (curSize >= maxSize) │
         │               │  └─────────┬───────────┘
         │ {module}_     │       ┌───┴───┐
         │  20260823.log │      是       否
         │               │       │        │
         │ curSeq = 0    │       ▼        ▼
         └───────┬───────┘  ┌──────────┐  ┌──────────┐
                 │          │ 创建切割文件│  │ 直接写入  │
                 │          │ 序号+1    │  │ 当前文件  │
                 │          │           │  │          │
                 │          │{module}_  │  │          │
                 │          │ 20260823  │  │          │
                 │          │  .1.log   │  │          │
                 │          └─────┬─────┘  └────┬─────┘
                 │                │              │
                 └────────────────┼──────────────┘
                                  ▼
                          更新 curSize
                          继续等待下一条
```

### 4.2 文件命名规则

```
  runtime/logs/mysql/
  │
  ├── mysql_20260821.log          ← 8月21日的主文件（第一份）
  ├── mysql_20260821.1.log        ← 8月21日的第1次切割（文件超过100MB）
  ├── mysql_20260821.2.log        ← 8月21日的第2次切割
  │
  ├── mysql_20260822.log          ← 8月22日的主文件（新的一天，序号归零）
  ├── mysql_20260822.1.log        ← 8月22日的第1次切割
  │
  └── mysql_20260823.log          ← 8月23日（今天，正在写入）
```

### 4.3 与纯 lumberjack 方案的对比

```
  纯 lumberjack 方案：                dailyWriter 双切割方案：

  ┌────────────────────┐             ┌────────────────────────┐
  │ mysql.mysql.log    │             │ mysql_20260823.log     │
  │ mysql.mysql.log.1  │             │ mysql_20260823.1.log   │
  │ mysql.mysql.log.2  │             │ mysql_20260822.log     │
  │                    │             │ mysql_20260822.1.log   │
  │ 问题：              │             │                        │
  │ - 文件名没有时间信息 │             │ 优势：                  │
  │ - 不知道哪个是今天  │             │ - 按日期一目了然          │
  │ - 清理旧文件不方便  │             │ - 按日期删除很方便        │
  │ - Filebeat 难匹配  │             │ - Filebeat 按日期采集    │
  └────────────────────┘             └────────────────────────┘
```

### 4.4 重启序号恢复机制

应用重启时，`dailyWriter` 会扫描目录中已有的文件，避免序号冲突：

```
  场景：应用在 20260823 已经切割了 .1 和 .2 后重启

  重启前目录状态：
  ├── mysql_20260823.log
  ├── mysql_20260823.1.log    ← 已存在
  └── mysql_20260823.2.log    ← 已存在

  重启后 findMaxSeq() 扫描结果：
  maxSeq = 2

  下次切割时：
  └── mysql_20260823.3.log    ← 从 3 开始，不会覆盖已有文件 ✅
```

---

## 第五部分：通用改造模式

### 5.1 决策树：选择合适的改造方式

当你需要为 pkg 下的某个包接入自定义日志时，按以下决策树选择方案：

```
                    pkg 包使用什么日志体系？
                              │
            ┌─────────────────┼─────────────────────┐
            │                 │                      │
            ▼                 ▼                      ▼
     使用 logx.Logger    使用 *zap.Logger       使用 *log.Logger
     作为日志接口        硬编码在代码里           导出变量可替换
            │                 │                      │
            ▼                 ▼                      ▼
      ┌──────────┐     ┌──────────┐           ┌──────────┐
      │ 方式 A   │     │ 方式 B   │           │ 方式 C   │
      │ zapx.New │     │ 直接传入 │           │ Writer   │
      │ 适配器   │     │ *zap.L.  │           │ 适配器   │
      └──────────┘     └──────────┘           └──────────┘

            │
            │  如果以上都不行
            ▼
     使用 log.Printf
     或 fmt.Println
     完全硬编码
            │
            ▼
      ┌──────────┐
      │ 方式 D   │
      │ 修改源码 │
      │ 接入logx │
      └──────────┘
```

### 5.2 方式 A：logx.Logger 接口包（最推荐）

**适用对象：** 已经使用 `logx.Logger` 接口的包（如 db、mq）

**改造成本：** 零成本，直接使用

```go
// 步骤 1：获取模块专属的 *zap.Logger
mysqlZapLogger := logging.GetZapLogger("mysql")

// 步骤 2：用 zapx 适配为 logx.Logger
mysqlLogxLogger := zapx.New(mysqlZapLogger)

// 步骤 3：通过 Option 注入
err = db.InitMysqlClientWithOptions(
    db.DefaultClient,
    user, password, host, name,
    db.WithLogger(mysqlLogxLogger),
    db.WithSQLLogger(mysqlLogxLogger),
    db.WithEnableSqlLog(true),
)
```

**适用场景：** db 包、mq 包，以及任何已经接入 logx 的包

---

### 5.3 方式 B：*zap.Logger 硬编码包

**适用对象：** 直接使用 `*zap.Logger` 作为参数的包（如 httpclient、trace）

**改造成本：** 低成本，直接传入

```go
// 步骤 1：获取模块专属的 *zap.Logger
httpZapLogger := logging.GetZapLogger("http")

// 步骤 2：直接传给包的 Option（不需要适配）
client := httpclient.New(
    httpclient.WithTimeout(10*time.Second),
    httpclient.WithLogger(httpZapLogger),  // 直接传 *zap.Logger
)
```

**适用场景：** httpclient、trace 等直接使用 zap 的包

---

### 5.4 方式 C：*log.Logger 导出变量包

**适用对象：** 使用标准库 `log.New` 创建 stdLogger 的包（如 cache、redis、es）

**改造成本：** 中等成本，需要写 io.Writer 适配器

```go
// 步骤 1：创建一个 io.Writer，把写入的内容转发到 zap
type zapWriter struct {
    logger *zap.Logger
    level  zapcore.Level
}

func (w *zapWriter) Write(p []byte) (n int, err error) {
    w.logger.Log(w.level, strings.TrimSpace(string(p)))
    return len(p), nil
}

// 步骤 2：替换包内的 stdLogger
cacheLogger := logging.GetZapLogger("cache")
cache.stdLogger = log.New(
    &zapWriter{logger: cacheLogger, level: zapcore.InfoLevel},
    "", 0,
)
```

**适用场景：** cache、redis、es、nosql 等使用 stdLogger 的包

---

### 5.5 方式 D：标准库硬编码包

**适用对象：** 直接使用 `log.Printf` / `fmt.Println` 的包（如 errors、kafka）

**改造成本：** 高成本，需要修改源码

```go
// 改造前（硬编码）：
func handleError(err error) {
    log.Printf("error occurred: %v", err)  // 无法注入
}

// 改造后（接入 logx）：
var logger logx.Logger = logx.GetLogger()  // 兜底用全局

func SetLogger(l logx.Logger) {
    logger = l                              // 支持注入
}

func handleError(err error) {
    logger.Error("error occurred", logx.LogField{Key: "error", Value: err})
}
```

**适用场景：** errors、kafka 等完全硬编码的包，需要改源码

---

## 第六部分：与业界标准对比

### 6.1 对比总览

| 维度 | 业界标准 | 本项目实现 | 对比 |
|------|---------|-----------|------|
| 日志接口抽象 | SLF4J (Java) / log15 (Go) | `logx.Logger` 接口 | ✅ 一致 |
| 适配器模式 | slf4j-log4j bridge | `zapx.New()` / `zapAdapter` | ✅ 一致 |
| 依赖注入 | Spring DI / Google Wire | Functional Options 注入 | ✅ 一致 |
| 日志轮转 | logrotate / lumberjack | `dailyWriter` (自研双切割) | ✅ 增强 |
| 模块化日志 | Logger per module | `LoggerManager` + `sync.Map` | ✅ 一致 |
| 结构化日志 | JSON structured logging | zap JSON encoder | ✅ 一致 |
| ELK 兼容 | Filebeat + Elasticsearch | 按天文件 + JSON 格式 | ✅ 兼容 |

### 6.2 与 Java SLF4J 的对比

```
  Java 世界的做法：                    Go 本项目的做法：

  ┌──────────────┐                  ┌──────────────┐
  │  SLF4J 接口  │                  │ logx.Logger  │
  │  (日志门面)   │                  │  (日志门面)   │
  └──────┬───────┘                  └──────┬───────┘
         │                                 │
  ┌──────┴───────┐                  ┌──────┴───────┐
  │ slf4j-log4j  │                  │ zapx.New()   │
  │ (适配器)      │                  │ (适配器)      │
  └──────┬───────┘                  └──────┴───────┘
         │                                 │
  ┌──────┴───────┐                  ┌──────┴───────┐
  │  Log4j/      │                  │ *zap.Logger  │
  │  Logback     │                  │ (具体实现)    │
  │  (具体实现)   │                  │              │
  └──────────────┘                  └──────────────┘

  核心思想完全一致：
  定义统一接口 → 适配器桥接 → 具体实现可替换
```

### 6.3 dailyWriter vs lumberjack 的增强点

```
  lumberjack 的切割策略：              dailyWriter 的切割策略：

  只按大小切割                         按天 + 按大小 双重切割

  app.log                            mysql_20260823.log
  app.log.1                          mysql_20260823.1.log
  app.log.2                          mysql_20260822.log
  app.log.3                          mysql_20260822.1.log
  app.log.4
  app.log.5                          优势：
                                     ✅ 文件名含日期，一目了然
  问题：                              ✅ 按天清理旧文件很方便
  ❌ 文件名无时间信息                   ✅ 按天删除很方便
  ❌ 不知道哪个文件是哪天的             ✅ Filebeat 按日期模式匹配
  ❌ 清理旧文件需要额外逻辑             ✅ 比纯 lumberjack 更适合
  ❌ 对 Filebeat 不友好                   本地排查和 ELK 采集
```

### 6.4 ELK 兼容性说明

```
  日志采集链路：

  ┌──────────────┐     ┌──────────┐     ┌──────────────┐
  │ dailyWriter  │────→│ Filebeat │────→│ Elasticsearch│
  │              │     │          │     │              │
  │ 按天生成文件  │     │ 按通配符  │     │ 按 module    │
  │ JSON 格式    │     │ 采集日志  │     │ 字段索引     │
  │ 含 module    │     │          │     │              │
  │ 字段         │     │ *.log    │     │              │
  └──────────────┘     └──────────┘     └──────────────┘

  JSON 输出示例：
  {
    "level": "INFO",
    "ts": "2026-08-23T10:30:00.000+0800",
    "caller": "order/service.go:42",
    "msg": "order created",
    "module": "order",
    "orderId": "12345"
  }
```

---

## 第七部分：关键代码索引

### 7.1 项目日志模块（logging 包）

| 文件路径 | 核心职责 |
|---------|---------|
| `pkg/logging/config.go` | 日志配置结构 `LogConfig`，从 `conf.Zap` 映射 |
| `pkg/logging/encoder.go` | 编码器工厂 `newEncoder()`，支持 JSON/Console 两种模式 |
| `pkg/logging/manager.go` | `LoggerManager` 核心管理器，`GetLogger()` / `GetZapLogger()` 对外 API |
| `pkg/logging/writer.go` | `dailyWriter` 按天+按大小双切割写入器，`buildWriter()` 构建策略 |

### 7.2 外部 pkg 包（github.com/HeRedBo/pkg）

| 包 | 关键文件 | 日志相关职责 |
|---|---------|------------|
| `logx` | `logger.go` | 统一日志接口定义 `Logger` + 全局注入 `SetLogger/GetLogger` |
| `logx` | `default.go` | 默认 Logger 实现（三级优先级兜底） |
| `logx` | `config.go` | 配置驱动初始化 `InitLogger(LogConfig)` |
| `logx/zapx` | `zap_adapter.go` | zap 适配器 `zapx.New(*zap.Logger) logx.Logger` |
| `db` | `mysql.go` | `WithLogger()` / `WithSQLLogger()` 函数式选项 |
| `db` | `gorm_adapter.go` | GORM 日志适配器 `gormLogger` |
| `mq` | `logger.go` | 类型别名 `type Logger = logx.Logger` + `SetLogger()` |
| `mq` | `sarama_logger.go` | Sarama Kafka 日志适配器 |

### 7.3 项目配置与入口

| 文件路径 | 日志相关职责 |
|---------|------------|
| `conf/config.yml` | `zap` 配置节：日志级别、模式、模块列表 |
| `conf/conf.go` | `Zap` 配置结构体定义 |
| `main.go` | 初始化 `LoggerManager`，注入 db/mq 日志 |

### 7.4 日志目录结构

```
runtime/logs/
├── app/                    ← GetLogger("app") 写入
│   └── app_20260823.log
├── order/                  ← GetLogger("order") 写入
│   └── order_20260823.log
├── product/                ← GetLogger("product") 写入
│   └── product_20260823.log
├── user/                   ← GetLogger("user") 写入
│   └── user_20260823.log
├── auth/                   ← GetLogger("auth") 写入
│   └── auth_20260823.log
├── http/                   ← GetLogger("http") 写入
│   └── http_20260823.log
├── menu/                   ← GetLogger("menu") 写入
│   └── menu_20260823.log
├── role/                   ← GetLogger("role") 写入
│   └── role_20260823.log
├── upload/                 ← GetLogger("upload") 写入
│   └── upload_20260823.log
├── mysql/                  ← GetZapLogger("mysql") → zapx.New() → db.WithLogger()
│   └── mysql_20260823.log
├── mysql_query/            ← GetZapLogger("mysql_query") → zapx.New() → db.WithSQLLogger()
│   └── mysql_query_20260823.log
└── mq/                     ← GetZapLogger("mq") → zapx.New() → mq.SetLogger()
    └── mq_20260823.log
```

---

> **文档版本**：v1.0
> **最后更新**：2026-08-23
