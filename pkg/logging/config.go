package logging

import "shop/conf"

// LogConfig 日志配置，从 conf.Zap 映射
type LogConfig struct {
	LogLevel   string   // debug / info / warn / error
	LogMode    string   // console / json
	LogOutput  string   // stdout / file / both
	LogFilepath string  // 日志文件根目录
	LogFileExt  string  // 文件扩展名
	MaxSize     int     // 单文件最大 MB
	MaxAge      int     // 保留天数
	MaxBackups  int     // 保留文件数
	Compress    bool    // 压缩旧文件
	Modules     []string // 预注册的业务模块列表
}

// NewLogConfig 从配置文件结构体创建日志配置
func NewLogConfig(cfg conf.Zap) LogConfig {
	return LogConfig{
		LogLevel:    cfg.LogLevel,
		LogMode:     cfg.LogMode,
		LogOutput:   cfg.LogOutput,
		LogFilepath: cfg.LogFilepath,
		LogFileExt:  cfg.LogFileExt,
		MaxSize:     cfg.MaxSize,
		MaxAge:      cfg.MaxAge,
		MaxBackups:  cfg.MaxBackups,
		Compress:    cfg.Compress,
		Modules:     cfg.Modules,
	}
}
