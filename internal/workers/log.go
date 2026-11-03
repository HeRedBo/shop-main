package workers

import (
	"shop/pkg/global"

	"go.uber.org/zap"
)

// Logger 返回 worker 模块的独立 logger
func Logger() *zap.SugaredLogger {
	return global.GetLogger("worker")
}
