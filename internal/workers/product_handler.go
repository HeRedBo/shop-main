package workers

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"shop/internal/models"
	"shop/internal/worker"
	"shop/pkg/global"
)

// ProductHandler 商品事件处理器
// 处理 product.created / product.updated / product.deleted 事件
// 当前阶段：幂等检查 + 日志记录（后续可扩展为更新 ES 索引、刷新缓存等）
// 注意：event_id / event_type / aggregate_id 由 Relay 写入 Kafka Headers，必定存在
type ProductHandler struct{}

// NewProductHandler 创建 ProductHandler 实例
func NewProductHandler() *ProductHandler {
	return &ProductHandler{}
}

// Handle 处理商品事件
// 流程：提取元数据 → 幂等检查 → 业务逻辑 → 记录幂等
func (h *ProductHandler) Handle(ctx context.Context, event *worker.Event) error {
	// 1. 从 Kafka Headers 中提取元数据（Relay 投递时必定设置）
	eventId := event.Headers["event_id"]
	eventType := event.EventType // consumer 已从 headers["event_type"] 中提取
	aggregateId := event.Headers["aggregate_id"]

	if eventId == "" {
		global.LOG.Warnf("[ProductHandler] 消息缺少 event_id header，跳过: topic=%s offset=%d", event.Topic, event.Offset)
		return nil
	}

	global.LOG.Infof("[ProductHandler] 收到事件: event_id=%s event_type=%s aggregate_id=%s offset=%d",
		eventId, eventType, aggregateId, event.Offset)

	// 2. 幂等检查：查询 processed_events 表是否已处理该 event_id
	var count int64
	if err := global.Db.Model(&models.ProcessedEvent{}).Where("event_id = ?", eventId).Count(&count).Error; err != nil {
		return fmt.Errorf("查询幂等记录失败: %w", err)
	}
	if count > 0 {
		global.LOG.Infof("[ProductHandler] 事件已处理，跳过: event_id=%s", eventId)
		return nil
	}

	// 3. 解析 payload（商品业务数据）
	var payload map[string]interface{}
	if err := json.Unmarshal(event.Value, &payload); err != nil {
		global.LOG.Errorf("[ProductHandler] 解析 payload 失败: %v, 原文: %s", err, truncate(string(event.Value), 200))
		return fmt.Errorf("解析 payload 失败: %w", err)
	}

	// 4. 根据 event_type 执行业务逻辑
	switch eventType {
	case "product.created":
		global.LOG.Infof("[ProductHandler] 商品创建事件: aggregate_id=%s, payload_keys=%v", aggregateId, mapKeys(payload))

	case "product.updated":
		global.LOG.Infof("[ProductHandler] 商品更新事件: aggregate_id=%s, payload_keys=%v", aggregateId, mapKeys(payload))

	case "product.deleted":
		global.LOG.Infof("[ProductHandler] 商品删除事件: aggregate_id=%s", aggregateId)

	default:
		global.LOG.Warnf("[ProductHandler] 未知事件类型: event_type=%s, event_id=%s", eventType, eventId)
	}

	// 5. 写入幂等记录
	processed := models.ProcessedEvent{
		EventId:     eventId,
		EventType:   eventType,
		AggregateId: aggregateId,
		ProcessedAt: time.Now(),
		CreatedAt:   time.Now(),
	}
	if err := global.Db.Create(&processed).Error; err != nil {
		// 唯一约束冲突 = 已被其他 worker 处理，不算错误
		if isDuplicateKeyError(err) {
			global.LOG.Infof("[ProductHandler] 幂等记录已存在（并发写入），跳过: event_id=%s", eventId)
			return nil
		}
		return fmt.Errorf("写入幂等记录失败: %w", err)
	}

	global.LOG.Infof("[ProductHandler] 事件处理完成: event_id=%s event_type=%s", eventId, eventType)
	return nil
}

// isDuplicateKeyError 判断是否为 MySQL 唯一约束冲突错误
// MySQL error code: 1062 (Duplicate entry)
func isDuplicateKeyError(err error) bool {
	if err == nil {
		return false
	}
	errMsg := err.Error()
	return strings.Contains(errMsg, "1062") || strings.Contains(errMsg, "Duplicate entry")
}

// truncate 截取字符串，超长部分用 ... 代替
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// mapKeys 获取 map 的所有 key
func mapKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
