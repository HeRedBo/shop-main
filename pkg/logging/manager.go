package logging

import (
	"sync"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// LoggerManager 管理所有业务模块 logger 的全局管理器
type LoggerManager struct {
	loggers sync.Map          // moduleName -> *zap.Logger
	cfg     LogConfig
	encoder zapcore.Encoder
	level   zapcore.Level
}

// manager 全局实例，由 NewManager 初始化
var manager *LoggerManager

// NewManager 创建并初始化全局日志管理器
// 应在 main 函数初始化阶段调用，且仅调用一次
func NewManager(cfg LogConfig) *LoggerManager {
	encoder := newEncoder(cfg.LogMode)
	level := parseLevel(cfg.LogLevel)

	m := &LoggerManager{
		cfg:     cfg,
		encoder: encoder,
		level:   level,
	}

	// 预注册配置中声明的模块
	for _, module := range cfg.Modules {
		m.getOrCreateZapLogger(module)
	}

	manager = m
	return m
}

// GetLogger 获取指定业务模块的 logger
// 如果该模块的 logger 尚未创建，则自动创建并缓存
// 并发安全
func GetLogger(module string) *zap.SugaredLogger {
	if manager == nil {
		// 未初始化时返回空操作 logger，避免 panic
		return zap.NewNop().Sugar()
	}
	return manager.getOrCreateZapLogger(module).Sugar()
}

// GetZapLogger 获取指定模块的原始 *zap.Logger
// 用于传递给外部适配器（如 zapx.New），打通与 logx 的衔接
// 如果 manager 未初始化，返回 zap.NewNop()
func GetZapLogger(module string) *zap.Logger {
	if manager == nil {
		return zap.NewNop()
	}
	return manager.getOrCreateZapLogger(module)
}

// SyncAll 刷新所有已创建 logger 的缓冲
// 应在程序优雅关闭时调用
func SyncAll() {
	if manager == nil {
		return
	}
	manager.loggers.Range(func(key, value interface{}) bool {
		if logger, ok := value.(*zap.Logger); ok {
			_ = logger.Sync()
		}
		return true
	})
}

// getOrCreateZapLogger 内部方法：获取或创建指定模块的 *zap.Logger
func (m *LoggerManager) getOrCreateZapLogger(module string) *zap.Logger {
	// 快速路径：已存在则直接返回
	if val, ok := m.loggers.Load(module); ok {
		return val.(*zap.Logger)
	}

	// 慢路径：创建新 logger
	writer := buildWriter(m.cfg, module)
	core := zapcore.NewCore(m.encoder, writer, m.level)
	baseLogger := zap.New(core, zap.AddCaller())

	// 附带 module 字段，便于 ELK 按模块检索
	logger := baseLogger.With(zap.String("module", module))

	// 存储（并发安全）
	actual, _ := m.loggers.LoadOrStore(module, logger)
	return actual.(*zap.Logger)
}
