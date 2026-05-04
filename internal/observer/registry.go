package observer

import "sync"

// ObserverRegistry 观察者注册中心
// 管理 ModelObserver 的注册与查询，类似 Laravel 的 $model->observe()
type ObserverRegistry struct {
	mu        sync.RWMutex
	observers map[string][]ModelObserver // key: 表名, value: 观察者列表（有序）
}

// NewRegistry 创建新的注册中心
func NewRegistry() *ObserverRegistry {
	return &ObserverRegistry{
		observers: make(map[string][]ModelObserver),
	}
}

// Register 注册观察者 — 类似 Laravel 的 $model->observe()
// 按 ObserveModel() 返回的表名分组存储，注册顺序即为执行顺序
func (r *ObserverRegistry) Register(obs ...ModelObserver) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, o := range obs {
		model := o.ObserveModel()
		r.observers[model] = append(r.observers[model], o)
	}
}

// GetObservers 获取某个模型（表名）的所有观察者
func (r *ObserverRegistry) GetObservers(tableName string) []ModelObserver {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.observers[tableName]
}

// HasObservers 检查某模型是否有观察者
func (r *ObserverRegistry) HasObservers(tableName string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.observers[tableName]) > 0
}
