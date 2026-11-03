package worker

import (
	"go.uber.org/zap"
	"shop/pkg/global"
)

// Logger 返回 worker 模块的独立 logger
func Logger() *zap.SugaredLogger {
	return global.GetLogger("worker")
}
