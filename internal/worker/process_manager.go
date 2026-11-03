package worker

import (
	"context"
	"fmt"
	"sync"

)

// ProcessManager 管理多个 Runnable 任务（Process）的生命周期
// 类似 Hyperf 的 ProcessManager，但使用 goroutine 而非 fork 进程
type ProcessManager struct {
	processes []*Process
	registry  *Registry
	ctx       context.Context
	cancel    context.CancelFunc
	mu        sync.Mutex
	started   bool
}

// NewProcessManager 创建 ProcessManager
func NewProcessManager(registry *Registry) *ProcessManager {
	ctx, cancel := context.WithCancel(context.Background())
	return &ProcessManager{
		processes: make([]*Process, 0),
		registry:  registry,
		ctx:       ctx,
		cancel:    cancel,
	}
}

// GetRegistry 返回 Handler 注册表
func (pm *ProcessManager) GetRegistry() *Registry {
	return pm.registry
}

// AddProcess 注册一个 Process 到管理器
// 如果 p 为 nil 则跳过（方便工厂函数在条件不满足时直接返回 nil）
func (pm *ProcessManager) AddProcess(p *Process) {
	if p == nil {
		return
	}
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.processes = append(pm.processes, p)
	Logger().Infof("[process-manager] 已注册 Process: %s", p)
}

// AddProcesses 批量添加 Process，自动过滤 nil
// 方便工厂函数在条件不满足时返回 nil
func (pm *ProcessManager) AddProcesses(processes ...*Process) {
	for _, p := range processes {
		if p != nil {
			pm.AddProcess(p)
		}
	}
}

// Start 启动所有 Process
// 串行启动（避免并发创建 ConsumerGroup 时的竞争），每个 Process 在独立 goroutine 中运行
func (pm *ProcessManager) Start() error {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if pm.started {
		return fmt.Errorf("process manager already started")
	}

	if len(pm.processes) == 0 {
		Logger().Warn("[process-manager] 没有注册任何 Process，跳过启动")
		return nil
	}

	// 串行启动所有 Process
	for _, p := range pm.processes {
		Logger().Infof("[process-manager] 启动 Process: %s", p)
		if err := p.Runner.Start(pm.ctx); err != nil {
			// 启动失败：回滚已启动的 Process
			pm.stopStartedProcesses(p)
			return fmt.Errorf("启动 Process[%s] 失败: %w", p.Name, err)
		}
		p.Status = ProcessStatusRunning
	}

	pm.started = true
	Logger().Infof("[process-manager] 所有 Process 已启动，共 %d 个", len(pm.processes))
	return nil
}

// Stop 优雅关闭所有 Process（逆序关闭）
func (pm *ProcessManager) Stop() error {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if !pm.started {
		return nil
	}

	Logger().Info("[process-manager] 开始关闭所有 Process...")

	// 逆序关闭
	for i := len(pm.processes) - 1; i >= 0; i-- {
		p := pm.processes[i]
		Logger().Infof("[process-manager] 关闭 Process: %s", p)
		if err := p.Runner.Stop(); err != nil {
			Logger().Errorf("[process-manager] Process[%s] 关闭失败: %v", p.Name, err)
			p.Status = ProcessStatusFailed
			p.Err = err
		} else {
			p.Status = ProcessStatusStopped
		}
	}

	// 取消 context
	pm.cancel()
	pm.started = false

	Logger().Info("[process-manager] 所有 Process 已关闭")
	return nil
}

// stopStartedProcesses 回滚已启动的 Process（启动失败时使用）
func (pm *ProcessManager) stopStartedProcesses(failedProcess *Process) {
	for _, p := range pm.processes {
		if p == failedProcess {
			break
		}
		if p.Status == ProcessStatusRunning {
			if err := p.Runner.Stop(); err != nil {
				Logger().Errorf("[process-manager] 回滚 Process[%s] 失败: %v", p.Name, err)
			}
			p.Status = ProcessStatusStopped
		}
	}
}

// GetProcesses 返回所有已注册的 Process（只读快照）
func (pm *ProcessManager) GetProcesses() []*Process {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	result := make([]*Process, len(pm.processes))
	copy(result, pm.processes)
	return result
}
