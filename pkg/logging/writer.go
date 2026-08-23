package logging

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"
	"go.uber.org/zap/zapcore"
)

// dailyWriter 按天切割的日志写入器
// 文件名格式: {module}_20260823.log
// 每天自动创建新文件，旧文件保留不删除（由清理策略处理）
type dailyWriter struct {
	mu       sync.Mutex
	dir      string
	module   string
	ext      string
	current  *os.File
	curDate  string // "20060102" 格式
}

// newDailyWriter 创建按天切割的写入器
func newDailyWriter(cfg LogConfig, module string) zapcore.WriteSyncer {
	dir := filepath.Join(cfg.LogFilepath, module)
	// 确保目录存在
	_ = os.MkdirAll(dir, 0755)

	w := &dailyWriter{
		dir:    dir,
		module: module,
		ext:    cfg.LogFileExt,
	}
	// 初始化时打开今天的文件
	w.rotateIfNeeded()
	return zapcore.AddSync(w)
}

func (w *dailyWriter) Write(p []byte) (n int, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.rotateIfNeeded()
	if w.current == nil {
		return 0, fmt.Errorf("dailyWriter: no log file open")
	}
	return w.current.Write(p)
}

func (w *dailyWriter) Sync() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.current != nil {
		return w.current.Sync()
	}
	return nil
}

// rotateIfNeeded 检查是否需要切割（跨天时触发）
func (w *dailyWriter) rotateIfNeeded() {
	today := time.Now().Format("20060102")
	if today == w.curDate && w.current != nil {
		return
	}
	// 关闭旧文件
	if w.current != nil {
		_ = w.current.Close()
	}
	// 打开新文件
	filename := filepath.Join(w.dir, fmt.Sprintf("%s_%s.%s", w.module, today, w.ext))
	f, err := os.OpenFile(filename, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return
	}
	w.current = f
	w.curDate = today
}

// newLumberjackWriter 为指定业务模块创建 lumberjack 日志写入器（按大小切割）
func newLumberjackWriter(cfg LogConfig, module string) zapcore.WriteSyncer {
	dir := filepath.Join(cfg.LogFilepath, module)
	filename := filepath.Join(dir, fmt.Sprintf("%s.%s", module, cfg.LogFileExt))

	w := &lumberjack.Logger{
		Filename:   filename,
		MaxSize:    cfg.MaxSize,    // MB
		MaxAge:     cfg.MaxAge,     // 天
		MaxBackups: cfg.MaxBackups, // 文件数
		Compress:   cfg.Compress,
		LocalTime:  true,
	}

	return zapcore.AddSync(w)
}

// newStdoutWriter 创建标准输出写入器
func newStdoutWriter() zapcore.WriteSyncer {
	return zapcore.AddSync(os.Stdout)
}

// newLogWriter 根据配置选择日志写入策略
// 默认使用按天切割（dailyWriter），方便本地/测试环境按日期查看日志
func newLogWriter(cfg LogConfig, module string) zapcore.WriteSyncer {
	return newDailyWriter(cfg, module)
}

// buildWriter 根据配置构建 writer
// stdout: 仅输出到终端
// file:   仅输出到文件（按模块分文件）
// both:   同时输出到终端和文件
func buildWriter(cfg LogConfig, module string) zapcore.WriteSyncer {
	switch cfg.LogOutput {
	case "stdout":
		return newStdoutWriter()
	case "file":
		return newLogWriter(cfg, module)
	case "both":
		// 同时输出到 stdout 和文件
		stdout := newStdoutWriter()
		fileWriter := newLogWriter(cfg, module)
		return zapcore.NewMultiWriteSyncer(stdout, fileWriter)
	default:
		return newStdoutWriter()
	}
}
