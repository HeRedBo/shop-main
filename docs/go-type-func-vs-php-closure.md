# Go `type func` 自定义函数类型 与 PHP 闭包 深度对比扫盲

## 一、从 `type` 关键字说起

### 1.1 `type` 的本质

在 Go 中，`type` 关键字用于**定义新的类型**。它不只是定义结构体，还可以给任何已有类型创建一个"别名"或"新类型"：

```go
type int         // 基本类型
type string      // 基本类型
type MyInt int   // 基于 int 创建新类型
type func(int) string  // 基于函数签名创建新类型 ← 这就是 type func
```

**核心认知：函数在 Go 中是一等公民（first-class），它和 int、string 一样，可以是一种类型。**

### 1.2 为什么函数需要成为"类型"

在 PHP 中，你习惯了变量可以存函数（匿名函数/闭包），但 PHP 的函数类型是隐式的、松散的。Go 是强类型语言，**任何值要有明确的类型才能被使用**：

```go
// 你不能直接写：
var f = func(x int) string { ... }  // ✗ 编译错误，必须声明类型

// 你必须写：
var f func(x int) string = func(x int) string { ... }  // ✓ 显式声明函数类型
```

这就引出了一个问题：如果同一个函数签名在多处使用，每次都写一长串 `func(x int) string` 太冗余了。`type func` 就是为了解决这个问题——**给函数签名起个名字**。

---

## 二、`type func` 的定义与基本用法

### 2.1 语法

```go
type 类型名 func(参数列表) 返回值列表
```

### 2.2 最简单的例子

```go
// 定义一个函数类型
type Handler func(int, string) bool

// 使用：任何符合签名的函数都是 Handler 类型
var h Handler = func(id int, name string) bool {
    return id > 0 && name != ""
}

// 也可以是一个具名函数
func myHandler(id int, name string) bool {
    return id > 0
}

var h2 Handler = myHandler  // 具名函数也可以赋值
```

### 2.3 在项目中实际看到的例子

以本项目中的选项模式（Functional Options）为例：

```go
// 定义一个"选项"函数类型
type Option func(*Config)

// 各种选项函数
func WithTimeout(t time.Duration) Option {
    return func(c *Config) {
        c.Timeout = t
    }
}

func WithRetry(n int) Option {
    return func(c *Config) {
        c.Retry = n
    }
}

// 使用：可变参数接收一堆 Option 函数
func NewConfig(opts ...Option) *Config {
    c := &Config{}
    for _, opt := range opts {
        opt(c)  // 依次执行每个选项函数
    }
    return c
}

// 调用
cfg := NewConfig(
    WithTimeout(5 * time.Second),
    WithRetry(3),
)
```

---

## 三、核心使用场景

### 3.1 场景一：HTTP Handler（最经典的用法）

```go
// Go 标准库的定义
type HandlerFunc func(ResponseWriter, *Request)

// 你的处理函数直接实现这个类型
http.HandleFunc("/hello", func(w http.ResponseWriter, r *http.Request) {
    w.Write([]byte("Hello"))
})
```

**解决的问题**：让普通函数直接变成 Handler 接口，不需要额外包装。

### 3.2 场景二：Functional Options 模式（函数式选项）

```go
// 定义选项类型
type ServiceOption func(*Service)

// 各种配置选项
func WithPort(port int) ServiceOption {
    return func(s *Service) { s.Port = port }
}

func WithName(name string) ServiceOption {
    return func(s *Service) { s.Name = name }
}

// 创建服务，灵活组合选项
func NewService(opts ...ServiceOption) *Service {
    s := &Service{}
    for _, opt := range opts {
        opt(s)
    }
    return s
}

svc := NewService(WithPort(8080), WithName("my-service"))
```

**解决的问题**：替代冗长的构造函数参数列表，提供灵活、可扩展的配置方式。对比 PHP：

```php
// PHP 中你可能这样做
$service = new Service([
    'port' => 8080,
    'name' => 'my-service',
]);

// 或者用 Builder 模式
$service = (new ServiceBuilder())
    ->setPort(8080)
    ->setName('my-service')
    ->build();
```

Go 的 Functional Options 比数组配置类型安全，比 Builder 模式更简洁。

### 3.3 场景三：中间件链（Middleware）

```go
type Middleware func(http.Handler) http.Handler

func LoggingMiddleware(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        log.Println("request received")
        next.ServeHTTP(w, r)
    })
}

func AuthMiddleware(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        // 验证 token...
        next.ServeHTTP(w, r)
    })
}

// 链式组合
func Chain(middlewares ...Middleware) Middleware {
    return func(final http.Handler) http.Handler {
        for i := len(middlewares) - 1; i >= 0; i-- {
            final = middlewares[i](final)
        }
        return final
    }
}
```

**解决的问题**：将横切关注点（日志、鉴权、CORS）以函数组合的方式叠加，职责清晰。

### 3.4 场景四：回调函数 / 事件钩子

```go
type OnSuccess func(result string)
type OnError func(err error)

func DoWork(success OnSuccess, failure OnError) {
    result, err := process()
    if err != nil {
        failure(err)
        return
    }
    success(result)
}
```

### 3.5 场景五：策略模式（替代接口）

```go
// 传统方式：定义接口 + 多个实现
type SortStrategy interface {
    Sort([]int) []int
}

// 用 type func 简化：
type SortFunc func([]int) []int

func BubbleSort(arr []int) []int { /* ... */ }
func QuickSort(arr []int) []int  { /* ... */ }

// 直接使用函数作为策略
func Sorter(data []int, strategy SortFunc) []int {
    return strategy(data)
}

Sorter(data, BubbleSort)
Sorter(data, QuickSort)
```

**解决的问题**：当策略只有一个方法时，用接口太重了，一个函数类型就够了。

---

## 四、Go `type func` vs PHP 闭包 深度对比

### 4.1 本质差异

| 维度 | Go `type func` | PHP 闭包 (Closure) |
|------|----------------|-------------------|
| **类型系统** | 编译期强类型，签名必须严格匹配 | 运行时弱类型，签名不强制 |
| **本质** | 一种**类型定义**，定义函数的"形状" | 一种**运行时对象**（Closure 类的实例） |
| **函数签名** | 必须完全匹配参数和返回值 | 不检查签名，调用时可能报错 |
| **性能** | 编译期确定，零开销抽象 | 运行时创建对象，有额外开销 |
| **闭包捕获** | 通过词法作用域捕获外部变量 | 通过 `use` 关键字显式捕获 |
| **方法调用** | 函数类型本身可以有方法 | Closure 对象有 bind/call 等方法 |

### 4.2 代码对比

**Go：类型安全的函数类型**

```go
// 定义类型
type Calculator func(int, int) int

// 必须严格匹配签名
var add Calculator = func(a, b int) int {
    return a + b
}

// var wrong Calculator = func(a string) int { ... }  // ✗ 编译错误！

// 函数类型可以有自己的方法
func (c Calculator) String() string {
    return "I'm a calculator function"
}
```

**PHP：灵活的闭包**

```php
// 没有"函数类型"的概念，Closure 就是 Closure
$add = function(int $a, int $b): int {
    return $a + $b;
};

// PHP 不会在编译期检查签名是否匹配某个"类型"
// 你可以随意赋值，运行时才知道对不对
$wrong = function(string $a): int {  // PHP 不会阻止你
    return strlen($a);
};

// PHP 8 引入了联合类型和更严格的类型，但本质仍是弱类型检查
```

### 4.3 闭包捕获变量的差异

**Go：自动词法捕获**

```go
func outer() func() int {
    count := 0
    return func() int {
        count++  // 自动捕获外部变量
        return count
    }
}

f := outer()
fmt.Println(f())  // 1
fmt.Println(f())  // 2
```

**PHP：必须显式 `use`**

```php
function outer() {
    $count = 0;
    return function() use (&$count) {  // 必须显式声明捕获
        $count++;
        return $count;
    };
}

$f = outer();
echo $f();  // 1
echo $f();  // 2
```

**关键差异**：
- Go 自动捕获（词法闭包），更简洁
- PHP 必须用 `use` 显式声明，更明确但更啰嗦
- PHP 传值捕获需要 `&` 引用符号，Go 天然引用捕获

### 4.4 函数作为参数

**Go：必须声明函数类型**

```go
type Filter func(int) bool

func FilterSlice(slice []int, f Filter) []int {
    var result []int
    for _, v := range slice {
        if f(v) {
            result = append(result, v)
        }
    }
    return result
}

// 调用
FilterSlice([]int{1, 2, 3, 4}, func(n int) bool {
    return n > 2
})
```

**PHP：直接用 callable 类型提示**

```php
function filterSlice(array $slice, callable $f): array {
    $result = [];
    foreach ($slice as $v) {
        if ($f($v)) {
            $result[] = $v;
        }
    }
    return $result;
}

// 调用 — 更随意
filterSlice([1, 2, 3, 4], function($n) {
    return $n > 2;
});

// 甚至可以传字符串（函数名）
filterSlice([1, 2, 3, 4], 'strlen');
```

### 4.5 函数类型能否有方法

**Go：可以！这是强大的特性**

```go
type Handler func(int) string

// 给函数类型添加方法
func (h Handler) Serve() {
    result := h(42)
    fmt.Println(result)
}

// 这让函数类型可以实现接口！
type Servicer interface {
    Serve()
}

// Handler 自动实现了 Servicer 接口
var _ Servicer = Handler(func(n int) string {
    return fmt.Sprintf("handled %d", n)
})
```

**PHP：Closure 是对象，有内置方法，但你不能自定义**

```php
// PHP 的 Closure 自带一些方法
$closure = function() { echo "hello"; };
$closure->bindTo($newThis);   // 绑定 $this
$closure->call($obj);          // 绑定到对象并调用
Closure::fromCallable('strlen');  // 从可调用创建闭包
```

---

## 五、`type func` 解决了什么问题

### 5.1 类型安全 + 代码复用

没有 `type func`：

```go
// 到处重复写函数签名
func RegisterHandler(h func(ctx *gin.Context) error) { ... }
func WrapHandler(h func(ctx *gin.Context) error) { ... }
func ChainHandler(h func(ctx *gin.Context) error) { ... }
```

有了 `type func`：

```go
type HandlerFunc func(ctx *gin.Context) error

func RegisterHandler(h HandlerFunc) { ... }
func WrapHandler(h HandlerFunc) { ... }
func ChainHandler(h HandlerFunc) { ... }
```

### 5.2 让函数实现接口

Go 的接口是隐式实现的。`type func` + 方法可以让函数类型实现接口：

```go
// 标准库的经典设计
type Handler interface {
    ServeHTTP(ResponseWriter, *Request)
}

type HandlerFunc func(ResponseWriter, *Request)

// 让 HandlerFunc 实现 Handler 接口
func (f HandlerFunc) ServeHTTP(w ResponseWriter, r *Request) {
    f(w, r)
}

// 现在任何 func(ResponseWriter, *Request) 都是 Handler！
http.Handle("/", http.HandlerFunc(myHandler))
```

这在 PHP 中没有对应概念。PHP 的接口必须用类来实现。

### 5.3 函数式选项模式（替代多参数构造函数）

```go
// 没有 type func：构造函数参数爆炸
func NewServer(host string, port int, timeout time.Duration, retry int, debug bool) *Server {
    // ...
}

// 有 type func：优雅的可扩展配置
type ServerOption func(*Server)

func NewServer(opts ...ServerOption) *Server {
    s := &Server{/* defaults */}
    for _, opt := range opts {
        opt(s)
    }
    return s
}

NewServer(WithPort(8080), WithDebug(true))  // 清晰、可扩展
```

### 5.4 中间件 / 装饰器模式

```go
type Middleware func(http.Handler) http.Handler

// 函数组合，链式调用
func Chain(mws ...Middleware) Middleware {
    return func(h http.Handler) http.Handler {
        for _, mw := range mws {
            h = mw(h)
        }
        return h
    }
}
```

PHP 中你通常用类的管道模式或者 Laravel 的中间件栈来实现类似效果。

---

## 六、PHP 开发者转 Go 的认知映射

| PHP 中的做法 | Go 中的对应 |
|-------------|------------|
| `$fn = function() {}` | `var fn := func() {}` |
| `callable` 类型提示 | `type MyFunc func(...)` 自定义函数类型 |
| `use ($var)` 捕获变量 | 自动词法捕获 |
| `Closure::bindTo($this)` | 不需要，Go 用结构体方法代替 |
| 接口 + 单方法实现策略 | `type func` 直接定义策略类型 |
| 数组传配置项 | Functional Options 模式 |
| `$middleware = function($req, $next) {}` | `type Middleware func(Handler) Handler` |
| 事件监听器 `$emitter->on('event', fn)` | `type Listener func(event Event)` |

---

## 七、速查表

```go
// 1. 基本定义
type MyFunc func(int) string

// 2. 带多返回值
type Handler func(ctx Context) (Result, error)

// 3. 无参数无返回值
type Callback func()

// 4. 可变参数
type Reducer func(items ...int) int

// 5. 给函数类型加方法
func (f MyFunc) Execute(input int) string {
    return f(input)
}

// 6. 函数类型实现接口
type Runnable interface { Run() }
func (f Callback) Run() { f() }

// 7. 作为结构体字段
type Server struct {
    onStart func()        // 直接用匿名函数类型
    onStop  ShutdownFunc  // 用自定义函数类型
}

// 8. 作为函数参数
func Register(f func(int) bool) { ... }

// 9. 返回函数
func MakeHandler() func(http.ResponseWriter, *http.Request) {
    return func(w http.ResponseWriter, r *http.Request) {
        w.Write([]byte("OK"))
    }
}

// 10. 函数类型嵌套
type Pipeline func(func(int) int) func(int) int
```

---

## 八、总结

### `type func` 的本质
- 它不是"新语法"，只是 `type` 关键字 + `func` 函数签名 的组合
- 它让函数从"匿名类型"变成"具名类型"，可以被复用、传递、实现接口

### 和 PHP 闭包的根本区别
- **PHP 闭包** = 运行时的匿名函数对象，灵活但松散
- **Go type func** = 编译期的函数类型定义，严格但安全

### 什么时候用 `type func`
1. 同一个函数签名在多处使用 → 提取为命名类型
2. 需要函数实现接口 → 给函数类型加方法
3. 函数式选项模式 → `type Option func(*Config)`
4. 中间件/装饰器 → `type Middleware func(Handler) Handler`
5. 策略模式只有一个方法时 → 用函数类型替代接口

### 心智模型
> 把 Go 的 `type func` 想象成 PHP 的 `Closure` 类，但它是**编译期的、类型安全的、可以有自定义方法的**。
> 它不是语法糖，而是 Go 类型系统的一等组成部分。
