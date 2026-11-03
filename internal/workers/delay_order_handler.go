package workers

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"shop/internal/models"
	"shop/internal/worker"
	orderEnum "shop/pkg/enums/order"
	"shop/pkg/global"
)

// DelayOrderHandler 延迟订单事件处理器
// 处理 Redis Stream 中的延迟订单消息（如订单超时取消、自动确认收货）
type DelayOrderHandler struct{}

// NewDelayOrderHandler 创建 DelayOrderHandler 实例
func NewDelayOrderHandler() *DelayOrderHandler {
	return &DelayOrderHandler{}
}

// DelayOrderPayload 延迟订单消息体
// Deprecated: 使用 worker.OrderEventPayload 代替，保留类型别名以兼容旧代码
type DelayOrderPayload = worker.OrderEventPayload

// Handle 处理延迟订单事件
func (h *DelayOrderHandler) Handle(ctx context.Context, msg *worker.Message) error {
	// 1. 从 Headers 中提取元数据
	eventId := msg.Headers["event_id"]
	eventType := msg.EventType
	aggregateId := msg.Headers["aggregate_id"]

	if eventId == "" {
		Logger().Warnf("[DelayOrderHandler] 消息缺少 event_id header，跳过: topic=%s", msg.Topic)
		return nil
	}

	Logger().Infof("[DelayOrderHandler] 收到延迟事件: event_id=%s event_type=%s aggregate_id=%s source=%s",
		eventId, eventType, aggregateId, msg.Source)

	// 2. 幂等检查
	var count int64
	if err := global.Db.Model(&models.ProcessedEvent{}).Where("event_id = ?", eventId).Count(&count).Error; err != nil {
		return fmt.Errorf("查询幂等记录失败: %w", err)
	}
	if count > 0 {
		Logger().Infof("[DelayOrderHandler] 事件已处理，跳过: event_id=%s", eventId)
		return nil
	}

	// 3. 解析 payload
	var payload worker.OrderEventPayload
	if err := json.Unmarshal(msg.Value, &payload); err != nil {
		Logger().Errorf("[DelayOrderHandler] 解析 payload 失败: %v", err)
		return fmt.Errorf("解析 payload 失败: %w", err)
	}

	// 4. 根据 eventType 执行对应业务逻辑
	switch eventType {
	case "order.timeout":
		if err := h.handleOrderTimeout(ctx, &payload); err != nil {
			return err
		}
	case "order.auto_confirm":
		if err := h.handleAutoConfirm(ctx, &payload); err != nil {
			return err
		}
	default:
		Logger().Warnf("[DelayOrderHandler] 未处理的延迟事件类型: event_type=%s", eventType)
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
		if isDuplicateKeyError(err) {
			Logger().Infof("[DelayOrderHandler] 幂等记录已存在（并发写入），跳过: event_id=%s", eventId)
			return nil
		}
		return fmt.Errorf("写入幂等记录失败: %w", err)
	}

	Logger().Infof("[DelayOrderHandler] 延迟事件处理完成: event_id=%s event_type=%s order_id=%s",
		eventId, eventType, payload.OrderId)
	return nil
}

// handleOrderTimeout 处理订单超时取消
// 仅未支付（Paid=0）且未发货（Status=0）的订单才需要超时取消
func (h *DelayOrderHandler) handleOrderTimeout(ctx context.Context, payload *worker.OrderEventPayload) error {
	Logger().Infof("[DelayOrderHandler] 处理订单超时: order_id=%s", payload.OrderId)

	// 查询订单
	var order models.StoreOrder
	if err := global.Db.Where("order_id = ?", payload.OrderId).First(&order).Error; err != nil {
		if err.Error() == "record not found" {
			Logger().Warnf("[DelayOrderHandler] 订单不存在: order_id=%s", payload.OrderId)
			return nil // 订单不存在，忽略
		}
		return fmt.Errorf("查询订单失败: %w", err)
	}

	// 仅未支付的订单才需要超时取消（Paid=0 且 Status=0 表示初始未支付状态）
	if order.Paid == orderEnum.PAY_STATUS_0 && order.Status == orderEnum.StatusUnpaid {
		now := time.Now()
		result := global.Db.Model(&models.StoreOrder{}).
			Where("order_id = ? AND paid = 0 AND status = 0", payload.OrderId).
			Updates(map[string]interface{}{
				"status":     orderEnum.StatusCancelled,
				"updated_at": now,
			})
		if result.Error != nil {
			return fmt.Errorf("取消订单失败: %w", result.Error)
		}
		if result.RowsAffected == 0 {
			Logger().Infof("[DelayOrderHandler] 订单状态已变更（并发处理），跳过: order_id=%s", payload.OrderId)
			return nil
		}
		Logger().Infof("[DelayOrderHandler] 订单超时取消成功: order_id=%s", payload.OrderId)
	} else {
		Logger().Infof("[DelayOrderHandler] 订单已支付或状态不符，跳过超时取消: order_id=%s, paid=%d, status=%d",
			payload.OrderId, order.Paid, order.Status)
	}

	return nil
}

// handleAutoConfirm 处理自动确认收货
// 仅已支付（Paid=1）且待收货（Status=1）的订单才自动确认
func (h *DelayOrderHandler) handleAutoConfirm(ctx context.Context, payload *worker.OrderEventPayload) error {
	Logger().Infof("[DelayOrderHandler] 处理自动确认收货: order_id=%s", payload.OrderId)

	// 查询订单
	var order models.StoreOrder
	if err := global.Db.Where("order_id = ?", payload.OrderId).First(&order).Error; err != nil {
		if err.Error() == "record not found" {
			Logger().Warnf("[DelayOrderHandler] 订单不存在: order_id=%s", payload.OrderId)
			return nil
		}
		return fmt.Errorf("查询订单失败: %w", err)
	}

	// 仅待收货状态的订单才自动确认（Paid=1 且 Status=1）
	if order.Paid == orderEnum.PAY_STATUS_1 && order.Status == orderEnum.StatusDelivered {
		now := time.Now()
		result := global.Db.Model(&models.StoreOrder{}).
			Where("order_id = ? AND paid = 1 AND status = 1", payload.OrderId).
			Updates(map[string]interface{}{
				"status":     orderEnum.StatusCompleted,
				"updated_at": now,
			})
		if result.Error != nil {
			return fmt.Errorf("自动确认收货失败: %w", result.Error)
		}
		if result.RowsAffected == 0 {
			Logger().Infof("[DelayOrderHandler] 订单状态已变更，跳过自动确认: order_id=%s", payload.OrderId)
			return nil
		}
		Logger().Infof("[DelayOrderHandler] 自动确认收货成功: order_id=%s", payload.OrderId)
	} else {
		Logger().Infof("[DelayOrderHandler] 订单状态不符自动确认条件，跳过: order_id=%s, paid=%d, status=%d",
			payload.OrderId, order.Paid, order.Status)
	}

	return nil
}
