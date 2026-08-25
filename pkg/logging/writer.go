package logging

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"
	"go.uber.org/zap/zapcore"
)

// dailyWriter 按天 + 按大小双重切割的日志写入器
// 文件名格式:
//   正常:       {module}_20260823.log
//   同一天超限: {module}_20260823.1.log, {module}_20260823.2.log ...
// 每天自动创建新文件，同一天内文件超过 maxSize 后自动切割出带序号的新文件
type dailyWriter struct {
	mu      sync.Mutex
	dir     string
	module  string
	ext     string
	maxSize int64  // 单文件最大字节数，0 表示不限制
	current *os.File
	curDate string // "20060102" 格式
	curSeq  int    // 当前文件序号，0 表示无序号
	curSize int64  // 当前文件已写入字节数
}

// newDailyWriter 创建按天 + 按大小双重切割的写入器
func newDailyWriter(cfg LogConfig, module string) zapcore.WriteSyncer {
	dir := filepath.Join(cfg.LogFilepath, module)
	// 确保目录存在
	_ = os.MkdirAll(dir, 0755)

	w := &dailyWriter{
		dir:     dir,
		module:  module,
		ext:     cfg.LogFileExt,
		maxSize: int64(cfg.MaxSize) * 1024 * 1024, // MB → 字节
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
	n, err = w.current.Write(p)
	w.curSize += int64(n)
	return n, err
}

func (w *dailyWriter) Sync() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.current != nil {
		return w.current.Sync()
	}
	return nil
}

// rotateIfNeeded 检查是否需要切割
// 触发条件：跨天 OR 当前文件超过 maxSize
func (w *dailyWriter) rotateIfNeeded() {
	today := time.Now().Format("20060102")
	dateChanged := today != w.curDate
	sizeExceeded := w.maxSize > 0 && w.curSize >= w.maxSize

	// 既没跨天也没超限，直接返回
	if !dateChanged && !sizeExceeded && w.current != nil {
		return
	}

	// 关闭旧文件
	if w.current != nil {
		_ = w.current.Close()
	}

	var filename string
	if dateChanged {
		// 跨天：序号归零，文件名不带序号
		filename = filepath.Join(w.dir, fmt.Sprintf("%s_%s.%s", w.module, today, w.ext))
		w.curDate = today
		w.curSeq = 0
	} else {
		// 同一天超限：序号递增
		w.curSeq++
		filename = filepath.Join(w.dir, fmt.Sprintf("%s_%s.%d.%s", w.module, today, w.curSeq, w.ext))
	}

	f, err := os.OpenFile(filename, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return
	}
	w.current = f

	// 获取已有文件大小（应用重启时，今天的文件可能已经很大）
	info, err := f.Stat()
	if err == nil {
		w.curSize = info.Size()
	} else {
		w.curSize = 0
	}

	// 重启时扫描已有序号文件，避免序号冲突
	// 例如已存在 order_20260823.1.log 和 .2.log，则 curSeq 从 2 开始
	w.curSeq = w.findMaxSeq(today)
}

// findMaxSeq 扫描目录中当天已有的最大序号文件
// 文件名格式: {module}_{date}.{seq}.{ext}
func (w *dailyWriter) findMaxSeq(date string) int {
	maxSeq := 0
	prefix := fmt.Sprintf("%s_%s.", w.module, date)
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		return 0
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		// 提取序号部分: {module}_{date}.{seq}.{ext}
		rest := name[len(prefix):]
		dotIdx := strings.Index(rest, ".")
		if dotIdx < 0 {
			continue
		}
		seqStr := rest[:dotIdx]
		seq, err := strconv.Atoi(seqStr)
		if err == nil && seq > maxSeq {
			maxSeq = seq
		}
	}
	return maxSeq
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
