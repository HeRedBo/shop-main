package cron

import (
	"fmt"
	"sync"

	"shop/pkg/global"

	cronlib "github.com/robfig/cron/v3"
)

// JobEntry 已注册的定时任务条目
type JobEntry struct {
	Name string // 任务名称
	Spec string // cron 表达式
	Cmd  func() // 任务执行函数
}

// Registry 定时任务注册表，管理所有定时任务的注册与调度
type Registry struct {
	mu      sync.Mutex
	jobs    []JobEntry
	cron    *cronlib.Cron
	running bool
}

// NewRegistry 创建一个新的定时任务注册表
func NewRegistry() *Registry {
	return &Registry{
		jobs: make([]JobEntry, 0),
	}
}

// Register 注册一个定时任务
// name: 任务名称（用于日志标识）
// spec: cron 表达式，支持秒级精度（如 "0 30 * * * *" 表示每小时第30分钟）
// cmd: 任务执行函数
func (r *Registry) Register(name string, spec string, cmd func()) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.jobs = append(r.jobs, JobEntry{
		Name: name,
		Spec: spec,
		Cmd:  cmd,
	})
}

// Start 启动调度器，将所有已注册的任务添加到 cron 调度器并启动
// 启动失败（如 cron 表达式错误）会打印错误日志但不会 panic
func (r *Registry) Start() {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.running {
		global.LOG.Warn("[cron] 调度器已在运行中，跳过重复启动")
		return
	}

	// 使用 WithSeconds 支持秒级精度的 cron 表达式
	r.cron = cronlib.New(cronlib.WithSeconds())

	for _, job := range r.jobs {
		_, err := r.cron.AddFunc(job.Spec, job.Cmd)
		if err != nil {
			global.LOG.Errorf("[cron] 注册任务失败: name=%s, spec=%s, error=%v", job.Name, job.Spec, err)
			continue
		}
		global.LOG.Infof("[cron] 已注册任务: name=%s, spec=%s", job.Name, job.Spec)
	}

	r.cron.Start()
	r.running = true

	global.LOG.Infof("[cron] 调度器已启动，共 %d 个任务", len(r.jobs))
}

// Stop 停止调度器，等待所有正在执行的任务完成
func (r *Registry) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.running || r.cron == nil {
		return
	}

	ctx := r.cron.Stop()
	<-ctx.Done()
	r.running = false

	global.LOG.Info("[cron] 调度器已停止")
}

// Jobs 返回所有已注册的任务列表（只读副本）
func (r *Registry) Jobs() []JobEntry {
	r.mu.Lock()
	defer r.mu.Unlock()

	result := make([]JobEntry, len(r.jobs))
	copy(result, r.jobs)
	return result
}

// IsRunning 返回调度器是否正在运行
func (r *Registry) IsRunning() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running
}

// String 返回注册表的可读描述
func (r *Registry) String() string {
	return fmt.Sprintf("CronRegistry{jobs=%d, running=%v}", len(r.jobs), r.running)
}
