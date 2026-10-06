package bootstrap

import (
	"os"
	"path/filepath"
	"shop/internal/observers"
	"shop/pkg/casbin"
	"shop/pkg/global"
	"shop/pkg/jwt"
	"shop/pkg/logging"

	"github.com/HeRedBo/pkg/cache"
	"github.com/HeRedBo/pkg/db"
	"github.com/HeRedBo/pkg/logx/zapx"
	"github.com/HeRedBo/pkg/mq"
	"github.com/IBM/sarama"
	"github.com/go-redis/redis/v7"
)

// DefaultConfigPath 默认配置文件路径
const DefaultConfigPath = "conf/config.yml"

// ResolveConfigPath 解析配置文件路径
// 优先级：
// 1. 用户通过 --config 指定的路径（如果是绝对路径直接返回）
// 2. 基于可执行文件位置推算项目根目录
// 3. 回退到相对路径（go run 开发场景）
func ResolveConfigPath(configPath string) string {
	// 绝对路径直接使用
	if filepath.IsAbs(configPath) {
		return configPath
	}

	// 基于二进制位置推算
	exePath, err := os.Executable()
	if err == nil {
		exeDir := filepath.Dir(exePath)
		projectRoot := filepath.Dir(exeDir) // build/ 的上一级是项目根
		fullPath := filepath.Join(projectRoot, configPath)
		if _, err := os.Stat(fullPath); err == nil {
			return fullPath
		}
	}

	// go run 兜底：cwd 就是项目根
	return configPath
}

// Option 选择性初始化选项
type Option func(*bootstrapConfig)

type bootstrapConfig struct {
	casbin        bool
	observer      bool
	jwt           bool
	kafka         bool
	kafkaConsumer bool
}

// componentFlags 记录实际初始化的组件，Shutdown 仅关闭已初始化的资源
var componentFlags struct {
	redis         bool
	mysql         bool
	kafka         bool
	kafkaConsumer bool
}

// WithCasbin 启用 Casbin 初始化
func WithCasbin() Option {
	return func(c *bootstrapConfig) {
		c.casbin = true
	}
}

// WithObserver 启用模型观察者初始化
func WithObserver() Option {
	return func(c *bootstrapConfig) {
		c.observer = true
	}
}

// WithJWT 启用 JWT 初始化
func WithJWT() Option {
	return func(c *bootstrapConfig) {
		c.jwt = true
	}
}

// WithKafka 启用 Kafka 初始化
func WithKafka() Option {
	return func(c *bootstrapConfig) {
		c.kafka = true
	}
}

// WithKafkaConsumer 启用 Kafka ConsumerGroup 初始化（供 Worker 模块使用）
func WithKafkaConsumer() Option {
	return func(c *bootstrapConfig) {
		c.kafkaConsumer = true
	}
}

// Bootstrap 初始化所有基础组件（配置→日志→Redis→MySQL→Casbin→观察者→JWT→Kafka）
// 适用于 cron 等需要完整初始化环境的入口
func Bootstrap() {
	BootstrapWith(DefaultConfigPath,
		WithCasbin(),
		WithObserver(),
		WithJWT(),
		WithKafka(),
	)
}

// BootstrapWith 支持参数化选项，可选择性初始化组件
// configPath: 配置文件路径，传空字符串则使用默认路径
// opts: 可选组件开关，基础组件（配置/日志/Redis/MySQL）始终初始化
func BootstrapWith(configPath string, opts ...Option) {
	// 默认配置路径
	if configPath == "" {
		configPath = DefaultConfigPath
	}

	// 合并选项
	cfg := &bootstrapConfig{}
	for _, opt := range opts {
		opt(cfg)
	}

	// 解析配置路径
	configPath = ResolveConfigPath(configPath)
	// 1. 加载配置
	global.LoadConfigWithPath(configPath)

	// 2. 初始化日志管理器
	logging.NewManager(logging.NewLogConfig(global.CONFIG.Zap))
	global.LOG = global.GetLogger("app")

	// 3. 初始化 Redis
	initRedis()

	// 4. 初始化 MySQL
	initMySQL()

	// 5. 初始化 Casbin（可选）
	if cfg.casbin {
		casbin.InitCasbin(global.Db)
	}

	// 6. 初始化模型观察者（可选）
	if cfg.observer {
		observers.RegisterAll(global.Db)
	}

	// 7. 初始化 JWT（可选）
	if cfg.jwt {
		jwt.Init()
	}

	// 8. 初始化 Kafka（可选）
	if cfg.kafka {
		initKafka()
	}

	// 9. 初始化 Kafka ConsumerGroup（可选）
	if cfg.kafkaConsumer {
		initKafkaConsumer()
	}
}

// Shutdown 优雅关闭已初始化的基础组件（Kafka→ConsumerGroup→Redis→MySQL→日志刷新）
// 注意：HTTP Server 的关闭由各入口自行管理，不在此处处理
func Shutdown() {
	// 关闭 Kafka ConsumerGroup（仅当已初始化时）
	if componentFlags.kafkaConsumer {
		if global.KafkaConsumerGroup != nil {
			if err := global.KafkaConsumerGroup.Close(); err != nil {
				global.LOG.Error("kafka consumer group close error", err)
			}
		}
	}

	// 关闭 Kafka Producer（仅当已初始化时）
	if componentFlags.kafka {
		if p := mq.GetKafkaSyncProducer(mq.DefaultKafkaSyncProducer); p != nil {
			if err := p.Close(); err != nil {
				global.LOG.Error("kafka close error", err, "client", mq.DefaultKafkaSyncProducer)
			}
		}
	}

	// 关闭 Redis（仅当已初始化时）
	if componentFlags.redis {
		// 关闭 Pub/Sub 专用客户端
		if global.RedisClient != nil {
			if err := global.RedisClient.Close(); err != nil {
				global.LOG.Errorf("redis pub/sub client close error: %v", err)
			}
		}
		// 关闭 cache 封装层客户端
		if r := cache.GetRedisClient(cache.DefaultRedisClient); r != nil {
			if err := r.Close(); err != nil {
				global.LOG.Error("redis close error", err, "client", cache.DefaultRedisClient)
			}
		}
	}

	// 关闭 MySQL（仅当已初始化时）
	if componentFlags.mysql {
		if err := db.CloseMysqlClient(db.DefaultClient); err != nil {
			global.LOG.Error("CloseMysqlClient error", err, "client", db.DefaultClient)
		}
	}

	// 刷新所有 logger 缓冲
	logging.SyncAll()
}

// initRedis 初始化 Redis 连接
func initRedis() {
	err := cache.InitRedis(cache.DefaultRedisClient, &redis.Options{
		Addr:        global.CONFIG.Redis.Host,
		Password:    global.CONFIG.Redis.Password,
		IdleTimeout: global.CONFIG.Redis.IdleTimeout,
	})
	if err != nil {
		global.LOG.Error("InitRedis error", err, "client", cache.DefaultRedisClient)
		panic(err)
	}
	componentFlags.redis = true

	// 创建独立 Redis 客户端用于 Pub/Sub（cache 封装层不支持 Subscribe/Publish）
	global.RedisClient = redis.NewClient(&redis.Options{
		Addr:     global.CONFIG.Redis.Host,
		Password: global.CONFIG.Redis.Password,
	})
	if err := global.RedisClient.Ping().Err(); err != nil {
		global.LOG.Errorf("[bootstrap] Redis Pub/Sub 客户端连接失败: %v", err)
	} else {
		global.LOG.Info("[bootstrap] Redis Pub/Sub 客户端已连接")
	}
}

// initMySQL 初始化 MySQL 连接（含独立 SQL 日志）
func initMySQL() {
	mysqlLogger := zapx.New(logging.GetZapLogger("mysql"))
	mysqlQueryLogger := zapx.New(logging.GetZapLogger("mysql_query"))

	err := db.InitMysqlClientWithOptions(db.DefaultClient,
		global.CONFIG.Database.User,
		global.CONFIG.Database.Password,
		global.CONFIG.Database.Host,
		global.CONFIG.Database.Name,
		db.WithLogger(mysqlLogger),
		db.WithSQLLogger(mysqlQueryLogger),
		db.WithEnableSqlLog(true),
	)
	if err != nil {
		global.LOG.Error("InitMysqlClient error", err, "client", db.DefaultClient)
		panic(err)
	}
	componentFlags.mysql = true
	global.Db = db.GetMysqlClient(db.DefaultClient).DB
}

// initKafka 初始化 Kafka 生产者（含独立 MQ 日志）
func initKafka() {
	mqLogger := zapx.New(logging.GetZapLogger("mq"))
	mq.SetLogger(mqLogger)

	err := mq.InitSyncKafkaProducer(mq.DefaultKafkaSyncProducer, global.CONFIG.Kafka.Hosts, nil)
	if err != nil {
		global.LOG.Error("InitSyncKafkaProducer err", err, "client", mq.DefaultKafkaSyncProducer)
		panic(err)
	}
	componentFlags.kafka = true
}

// initKafkaConsumer 初始化 Kafka ConsumerGroup（供 Worker 模块消费消息）
func initKafkaConsumer() {
	config := sarama.NewConfig()
	config.Consumer.Return.Errors = true

	groupId := global.CONFIG.Worker.GroupId
	brokers := global.CONFIG.Kafka.Hosts

	cg, err := sarama.NewConsumerGroup(brokers, groupId, config)
	if err != nil {
		global.LOG.Error("initKafkaConsumer error", err, "groupId", groupId, "brokers", brokers)
		panic(err)
	}
	global.KafkaConsumerGroup = cg
	componentFlags.kafkaConsumer = true

	global.LOG.Info("kafka consumer group initialized", "groupId", groupId, "brokers", brokers)
}
