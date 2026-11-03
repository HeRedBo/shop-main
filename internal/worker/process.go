package worker

import "fmt"

// ProcessStatus 进程状态
type ProcessStatus int

const (
	ProcessStatusCreated ProcessStatus = iota // 已创建
	ProcessStatusRunning                      // 运行中
	ProcessStatusStopped                      // 已停止
	ProcessStatusFailed                       // 异常退出
)

// String 返回状态的可读字符串
func (s ProcessStatus) String() string {
	switch s {
	case ProcessStatusCreated:
		return "CREATED"
	case ProcessStatusRunning:
		return "RUNNING"
	case ProcessStatusStopped:
		return "STOPPED"
	case ProcessStatusFailed:
		return "FAILED"
	default:
		return "UNKNOWN"
	}
}

// Process 封装单个守护任务单元
// 每个 Process 对应一个独立的 Runnable 实例（Kafka/Redis 消费者、监控任务等）
type Process struct {
	Name     string        // 任务名称（如 "kafka-order-consumer"、"health-checker"）
	TaskType string        // 任务类型（"kafka" / "redis" / "cron" / "monitor" 等）
	Runner   Runnable      // 任务实例
	Status   ProcessStatus // 运行状态
	Err      error         // 最后一次错误（Status=FAILED 时有值）
}

// String 返回 Process 的可读描述
func (p *Process) String() string {
	return fmt.Sprintf("Process{name=%s, type=%s, status=%s}", p.Name, p.TaskType, p.Status)
}

// NewProcess 创建一个新的 Process
func NewProcess(name, taskType string, runner Runnable) *Process {
	return &Process{
		Name:     name,
		TaskType: taskType,
		Runner:   runner,
		Status:   ProcessStatusCreated,
	}
}
