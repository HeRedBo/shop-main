package workers

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"shop/internal/models"
	"shop/internal/worker"
	"shop/pkg/global"
)

// OrderHandler 订单事件处理器
// 处理 order.created / order.paid / order.shipped / order.completed / order.canceled / order.updated 事件
// 当前阶段：幂等检查 + 日志记录（后续可扩展为库存扣减、通知发货、积分计算等）
// 注意：event_id / event_type / aggregate_id 由 Relay 写入 Kafka Headers，必定存在
type OrderHandler struct{}

// NewOrderHandler 创建 OrderHandler 实例
func NewOrderHandler() *OrderHandler {
	return &OrderHandler{}
}

// Handle 处理订单事件
// 流程：提取元数据 → 幂等检查 → 业务逻辑 → 记录幂等
func (h *OrderHandler) Handle(ctx context.Context, event *worker.Event) error {
	// 1. 从 Kafka Headers 中提取元数据（Relay 投递时必定设置）
	eventId := event.Headers["event_id"]
	eventType := event.EventType // consumer 已从 headers["event_type"] 中提取
	aggregateId := event.Headers["aggregate_id"]

	if eventId == "" {
		global.LOG.Warnf("[OrderHandler] 消息缺少 event_id header，跳过: topic=%s offset=%d", event.Topic, event.Offset)
		return nil
	}

	global.LOG.Infof("[OrderHandler] 收到事件: event_id=%s event_type=%s aggregate_id=%s offset=%d",
		eventId, eventType, aggregateId, event.Offset)

	// 2. 幂等检查：查询 processed_events 表是否已处理该 event_id
	var count int64
	if err := global.Db.Model(&models.ProcessedEvent{}).Where("event_id = ?", eventId).Count(&count).Error; err != nil {
		return fmt.Errorf("查询幂等记录失败: %w", err)
	}
	if count > 0 {
		global.LOG.Infof("[OrderHandler] 事件已处理，跳过: event_id=%s", eventId)
		return nil
	}

	// 3. 解析 payload（订单业务数据）
	var payload map[string]interface{}
	if err := json.Unmarshal(event.Value, &payload); err != nil {
		global.LOG.Errorf("[OrderHandler] 解析 payload 失败: %v, 原文: %s", err, truncate(string(event.Value), 200))
		return fmt.Errorf("解析 payload 失败: %w", err)
	}

	// 4. 根据 event_type 执行业务逻辑
	switch eventType {
	case "order.created":
		global.LOG.Infof("[OrderHandler] 订单创建事件: order_id=%s, payload_keys=%v", aggregateId, mapKeys(payload))

	case "order.paid":
		global.LOG.Infof("[OrderHandler] 订单支付事件: order_id=%s, payload_keys=%v", aggregateId, mapKeys(payload))

	case "order.shipped":
		global.LOG.Infof("[OrderHandler] 订单发货事件: order_id=%s", aggregateId)

	case "order.completed":
		global.LOG.Infof("[OrderHandler] 订单完成事件: order_id=%s", aggregateId)

	case "order.canceled":
		global.LOG.Infof("[OrderHandler] 订单取消事件: order_id=%s", aggregateId)

	case "order.updated":
		global.LOG.Infof("[OrderHandler] 订单更新事件: order_id=%s, payload_keys=%v", aggregateId, mapKeys(payload))

	default:
		global.LOG.Warnf("[OrderHandler] 未知事件类型: event_type=%s, event_id=%s", eventType, eventId)
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
			global.LOG.Infof("[OrderHandler] 幂等记录已存在（并发写入），跳过: event_id=%s", eventId)
			return nil
		}
		return fmt.Errorf("写入幂等记录失败: %w", err)
	}

	global.LOG.Infof("[OrderHandler] 事件处理完成: event_id=%s event_type=%s", eventId, eventType)
	return nil
}
