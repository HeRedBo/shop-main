package workers

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"shop/internal/models"
	"shop/internal/worker"
	orderEnum "shop/pkg/enums/order"
	"shop/pkg/global"

	"github.com/HeRedBo/pkg/mq"
	"github.com/IBM/sarama"
)

// orderSyncProducer 抽象订单对外投递所需的最小 Kafka 同步生产者能力。
// *mq.SyncProducer 天然满足该接口；单测可用假生产者替换，避免连接真实 Kafka。
type orderSyncProducer interface {
	Send(msg *sarama.ProducerMessage) (int32, int64, error)
}

// 可测试性接缝：默认走真实 mq / global.Db，行为与原内联调用完全一致。
// 单测通过替换这三个变量，即可在无 Kafka/MySQL 环境下确定性验证
// 「转发失败向上报错」与「幂等命中不重复投递」两条关键语义。
var (
	// orderSyncProducerFn 获取对外投递 topic 的 Kafka 同步生产者；未初始化时返回 nil。
	orderSyncProducerFn = func() orderSyncProducer {
		p := mq.GetKafkaSyncProducer(mq.DefaultKafkaSyncProducer)
		if p == nil {
			return nil // 避免把 nil 指针装箱成非 nil 接口，破坏下游 nil 判定
		}
		return p
	}

	// orderEventProcessedFn 幂等判定：查 processed_events 是否已存在该 event_id（语义与原内联查询一致）。
	orderEventProcessedFn = func(eventId string) (bool, error) {
		var count int64
		if err := global.Db.Model(&models.ProcessedEvent{}).Where("event_id = ?", eventId).Count(&count).Error; err != nil {
			return false, err
		}
		return count > 0, nil
	}

	// markOrderProcessedFn 写入幂等记录：仅在转发成功后调用，保证「失败可重试、成功不重复」。
	markOrderProcessedFn = func(record models.ProcessedEvent) error {
		return global.Db.Create(&record).Error
	}
)

// OrderHandler 订单事件处理器
// 处理 order.created / order.updated / order.deleted 事件
// 转发到 Kafka 供下游消费
// 注意：event_id / event_type / aggregate_id 由 Relay 写入 Kafka Headers，必定存在
type OrderHandler struct{}

// NewOrderHandler 创建 OrderHandler 实例
func NewOrderHandler() *OrderHandler {
	return &OrderHandler{}
}

// Handle 处理订单事件
// 流程：提取元数据 → 幂等检查 → 业务逻辑 → 记录幂等
func (h *OrderHandler) Handle(ctx context.Context, msg *worker.Message) error {
	// 1. 从 Kafka Headers 中提取元数据（Relay 投递时必定设置）
	eventId := msg.Headers["event_id"]
	eventType := msg.EventType // consumer 已从 headers["event_type"] 中提取
	aggregateId := msg.Headers["aggregate_id"]

	// 从 RawOffset 中提取 Kafka offset（仅用于日志）
	offset := extractKafkaOffset(msg.RawOffset)

	if eventId == "" {
		Logger().Warnf("[OrderHandler] 消息缺少 event_id header，跳过: topic=%s offset=%d", msg.Topic, offset)
		return nil
	}

	Logger().Infof("[OrderHandler] 收到事件: event_id=%s event_type=%s aggregate_id=%s offset=%d",
		eventId, eventType, aggregateId, offset)

	// 2. 幂等检查：查询 processed_events 表是否已处理该 event_id
	isProcessed, err := orderEventProcessedFn(eventId)
	if err != nil {
		return fmt.Errorf("查询幂等记录失败: %w", err)
	}
	if isProcessed {
		Logger().Infof("[OrderHandler] 事件已处理，跳过: event_id=%s", eventId)
		return nil
	}

	// 3. 解析 payload（订单业务数据）
	var payload map[string]interface{}
	if err := json.Unmarshal(msg.Value, &payload); err != nil {
		Logger().Errorf("[OrderHandler] 解析 payload 失败: %v, 原文: %s", err, truncate(string(msg.Value), 200))
		return fmt.Errorf("解析 payload 失败: %w", err)
	}

	// 4. 根据 event_type 执行业务逻辑
	// 转发失败必须向上返回 error：由 worker 的重试/死信机制接管；
	// 此时不写幂等标记，保证消息可被重新投递而非静默丢失。
	switch eventType {
	case "order.created":
		Logger().Infof("[OrderHandler] 订单创建事件: order_id=%s, payload_keys=%v", aggregateId, mapKeys(payload))
		if err := h.publishToKafka(payload, orderEnum.OperationCreate); err != nil {
			return err
		}

	case "order.updated":
		Logger().Infof("[OrderHandler] 订单更新事件: order_id=%s, payload_keys=%v", aggregateId, mapKeys(payload))
		if err := h.publishToKafka(payload, orderEnum.OperationUpdate); err != nil {
			return err
		}

	case "order.deleted":
		Logger().Infof("[OrderHandler] 订单删除事件: order_id=%s", aggregateId)
		if err := h.publishToKafka(payload, orderEnum.OperationDelete); err != nil {
			return err
		}

	default:
		Logger().Warnf("[OrderHandler] 未知/未处理事件类型: event_type=%s, event_id=%s", eventType, eventId)
	}

	// 5. 写入幂等记录（位于转发成功之后：失败已提前 return，不会落标记，故可重试；成功才落标记，故不重复）
	processed := models.ProcessedEvent{
		EventId:     eventId,
		EventType:   eventType,
		AggregateId: aggregateId,
		ProcessedAt: time.Now(),
		CreatedAt:   time.Now(),
	}
	if err := markOrderProcessedFn(processed); err != nil {
		// 唯一约束冲突 = 已被其他 worker 处理，不算错误
		if isDuplicateKeyError(err) {
			Logger().Infof("[OrderHandler] 幂等记录已存在（并发写入），跳过: event_id=%s", eventId)
			return nil
		}
		return fmt.Errorf("写入幂等记录失败: %w", err)
	}

	Logger().Infof("[OrderHandler] 事件处理完成: event_id=%s event_type=%s", eventId, eventType)
	return nil
}

// publishToKafka 将订单事件转发到对外投递 topic（order.Topic = shop-order），供下游 order-consumer 消费。
// 任意失败都会向上返回 error：由 worker 的重试/死信机制接管，避免转发失败被静默吞掉导致消息丢失。
// 注意：订单 aggregateId 是 OrderId（string），旧代码使用 Uid 作为 Kafka Key。
func (h *OrderHandler) publishToKafka(payload map[string]interface{}, operation string) error {
	// 1. 从 payload 中提取 Uid 作为 Kafka Key（与旧代码行为一致）
	uidVal, ok := payload["uid"]
	if !ok {
		err := fmt.Errorf("payload 中缺少 uid 字段: operation=%s", operation)
		Logger().Errorf("[OrderHandler] %v，终止转发", err)
		return err
	}
	// JSON 反序列化数字为 float64
	uidFloat, ok := uidVal.(float64)
	if !ok {
		err := fmt.Errorf("uid 类型断言失败: %T", uidVal)
		Logger().Errorf("[OrderHandler] %v", err)
		return err
	}
	uid := int64(uidFloat)

	// 2. 将 payload 反序列化为 StoreOrder
	orderData, err := json.Marshal(payload)
	if err != nil {
		Logger().Errorf("[OrderHandler] payload 序列化失败: %v", err)
		return fmt.Errorf("payload 序列化失败: %w", err)
	}

	var storeOrder models.StoreOrder
	if err := json.Unmarshal(orderData, &storeOrder); err != nil {
		Logger().Errorf("[OrderHandler] payload 反序列化为 StoreOrder 失败: %v", err)
		return fmt.Errorf("payload 反序列化为 StoreOrder 失败: %w", err)
	}

	// 3. 构造 OrderMsg
	orderMsg := models.OrderMsg{
		Operation:  operation,
		StoreOrder: &storeOrder,
	}

	msgBytes, err := json.Marshal(orderMsg)
	if err != nil {
		Logger().Errorf("[OrderHandler] OrderMsg 序列化失败: %v", err)
		return fmt.Errorf("OrderMsg 序列化失败: %w", err)
	}

	// 4. 发送到 Kafka（通过可注入接缝获取同步生产者）
	producer := orderSyncProducerFn()
	if producer == nil {
		err := fmt.Errorf("Kafka Producer 未初始化，无法转发订单事件: operation=%s", operation)
		Logger().Errorf("[OrderHandler] %v", err)
		return err
	}

	msg := &sarama.ProducerMessage{
		Topic: orderEnum.Topic,
		Key:   mq.KafkaMsgValueStrEncoder(strconv.FormatInt(uid, 10)),
		Value: mq.KafkaMsgValueEncoder(msgBytes),
	}

	partition, offset, err := producer.Send(msg)
	if err != nil {
		Logger().Errorf("[OrderHandler] 转发到 Kafka 失败: operation=%s, order_id=%s, err=%v", operation, storeOrder.OrderId, err)
		return fmt.Errorf("转发到 Kafka 失败: operation=%s, order_id=%s: %w", operation, storeOrder.OrderId, err)
	}

	Logger().Infof("[OrderHandler] 转发成功: operation=%s, order_id=%s, uid=%d, partition=%d, offset=%d",
		operation, storeOrder.OrderId, uid, partition, offset)
	return nil
}
