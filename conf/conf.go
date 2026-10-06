package conf

import "time"

type Config struct {
	App      App      `mapstructure:"app" yaml:"app"`
	Api      Api      `mapstructure:"api" yaml:"api"`
	Database Database `mapstructure:"database" yaml:"database"`
	Redis    Redis    `mapstructure:"redis" yaml:"redis"`
	Kafka    Kafka    `mapstructure:"kafka" yaml:"kafka"`
	Worker   WorkerConfig `mapstructure:"worker" yaml:"worker"`
	Server   Server   `mapstructure:"server" yaml:"server"`
	Zap      Zap      `mapstructure:"zap" yaml:"zap"`
	Wechat   Wechat   `mapstructure:"wechat" yaml:"wechat"`
	Express  Express  `mapstructure:"express" yaml:"express"`
}

type App struct {
	Domain    string `mapstructure:"domain" yaml:"domain"`
	JwtSecret string `mapstructure:"jwt-secret" yaml:"jwt-secret"`
	PageSize  int    `mapstructure:"page-size" yaml:"page-size"`
	PrefixUrl string `mapstructure:"prefix-url" yaml:"prefix-url"`

	RuntimeRootPath string `mapstructure:"runtime-root-path" yaml:"runtime-root-path"`

	ImageSavePath  string   `mapstructure:"image-save-path" yaml:"image-save-path"`
	ImageMaxSize   int      `mapstructure:"image-max-size" yaml:"image-max-size"`
	ImageAllowExts []string `mapstructure:"image-allow-exts" yaml:"image-allow-exts"`

	ExportSavePath string `mapstructure:"export-save-path" yaml:"export-save-path"`
	QrCodeSavePath string `mapstructure:"qrcode-save-path" yaml:"qrcode-save-path"`
	FontSavePath   string `mapstructure:"font-save-path" yaml:"font-save-path"`

	LogSavePath string `mapstructure:"log-save-path" yaml:"log-save-path"`
	LogSaveName string `mapstructure:"log-save-name" yaml:"log-save-name"`
	LogFileExt  string `mapstructure:"log-file-ext" yaml:"log-file-ext"`
	TimeFormat  string `mapstructure:"time-format" yaml:"time-format"`
}

type Api struct {
	SearchProductAK string `mapstructure:"search-product-ak" yaml:"search-product-ak"`
	SearchProductSK string `mapstructure:"search-product-sk" yaml:"search-product-sk"`
}

type Database struct {
	Type        string `mapstructure:"type" yaml:"type"`
	User        string `mapstructure:"user" yaml:"user"`
	Password    string `mapstructure:"password" yaml:"password"`
	Host        string `mapstructure:"host" yaml:"host"`
	Name        string `mapstructure:"name" yaml:"name"`
	TablePrefix string `mapstructure:"table-prefix" yaml:"table-prefix"`
}

type Redis struct {
	Host        string        `mapstructure:"host" yaml:"host"`
	Password    string        `mapstructure:"password" yaml:"password"`
	IdleTimeout time.Duration `mapstructure:"idle-timeout" yaml:"idle-timeout"`
}

type Server struct {
	RunMode      string        `mapstructure:"run-mode" yaml:"run-mode"`
	HttpPort     int           `mapstructure:"http-port" yaml:"http-port"`
	ReadTimeout  time.Duration `mapstructure:"read-timeout" yaml:"read-timeout"`
	WriteTimeout time.Duration `mapstructure:"write-timeout" yaml:"write-timeout"`
}

type Zap struct {
	LogLevel    string   `mapstructure:"log-level" yaml:"log-level"`           // debug / info / warn / error
	LogMode     string   `mapstructure:"log-mode" yaml:"log-mode"`             // console / json
	LogOutput   string   `mapstructure:"log-output" yaml:"log-output"`         // stdout / file / both
	LogFilepath string   `mapstructure:"log-filepath" yaml:"log-filepath"`     // 日志文件根目录
	LogFileExt  string   `mapstructure:"log-file-ext" yaml:"log-file-ext"`     // 文件扩展名
	MaxSize     int      `mapstructure:"max-size" yaml:"max-size"`             // 单文件最大 MB
	MaxAge      int      `mapstructure:"max-age" yaml:"max-age"`               // 保留天数
	MaxBackups  int      `mapstructure:"max-backups" yaml:"max-backups"`       // 保留文件数
	Compress    bool     `mapstructure:"compress" yaml:"compress"`             // 压缩旧文件
	Modules     []string `mapstructure:"modules" yaml:"modules"`               // 业务模块列表
}

type Kafka struct {
	Hosts []string `mapstructure:"hosts" yaml:"hosts"`
}

// HandlerConfig 单个 Handler 的配置
type HandlerConfig struct {
	Topic       string `mapstructure:"topic" yaml:"topic"`              // 监听的 Topic
	Concurrency int    `mapstructure:"concurrency" yaml:"concurrency"`   // 该 Handler 的 Worker Pool 并发数
	Ordered     bool   `mapstructure:"ordered" yaml:"ordered"`           // 是否有序消费（同 key 同 worker，预留字段）
	BufferSize  int    `mapstructure:"buffer-size" yaml:"buffer-size"`   // tasks channel 缓冲大小
}

type WorkerConfig struct {
	Enabled           bool     `mapstructure:"enabled" yaml:"enabled"`
	GroupId           string   `mapstructure:"group-id" yaml:"group-id"`
	Topics            []string `mapstructure:"topics" yaml:"topics"`
	Concurrency       int      `mapstructure:"concurrency" yaml:"concurrency"`
	PollInterval      string   `mapstructure:"poll-interval" yaml:"poll-interval"`
	MaxRetries        int      `mapstructure:"max-retries" yaml:"max-retries"`
	RetryBaseDelay    string   `mapstructure:"retry-base-delay" yaml:"retry-base-delay"`
	RelayBatchSize    int      `mapstructure:"relay-batch-size" yaml:"relay-batch-size"`
	RelayLeaseTimeout string   `mapstructure:"relay-lease-timeout" yaml:"relay-lease-timeout"`
	RelayCleanupDays  int      `mapstructure:"relay-cleanup-days" yaml:"relay-cleanup-days"` // 清理多少天前的 SENT 记录

	// per-Handler 独立配置（推荐方式）
	Handlers []HandlerConfig `mapstructure:"handlers" yaml:"handlers"`

	// 消费超时时间（默认 30s）
	HandlerTimeout string `mapstructure:"handler-timeout" yaml:"handler-timeout"`

	// 全局默认值（handlers 中未指定的字段使用此默认值）
	DefaultConcurrency int `mapstructure:"default-concurrency" yaml:"default-concurrency"`
	DefaultBufferSize  int `mapstructure:"default-buffer-size" yaml:"default-buffer-size"`
}

type Wechat struct {
	AppID          string `mapstructure:"app_id" yaml:"app_id"`                     //appid
	AppSecret      string `mapstructure:"app_secret" yaml:"app_secret"`             //app_secret
	Token          string `mapstructure:"token" yaml:"token"`                       //token
	EncodingAESKey string `mapstructure:"encoding_aes_key" yaml:"encoding_aes_key"` //EncodingAESKey
}

type Express struct {
	EBusinessId string `eBusinessId:"host" yaml:"eBusinessId"`
	AppKey      string `mapstructure:"appKey" yaml:"appKey"`
}
