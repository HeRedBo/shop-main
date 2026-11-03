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
	var count int64
	if err := global.Db.Model(&models.ProcessedEvent{}).Where("event_id = ?", eventId).Count(&count).Error; err != nil {
		return fmt.Errorf("查询幂等记录失败: %w", err)
	}
	if count > 0 {
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
	switch eventType {
	case "order.created":
		Logger().Infof("[OrderHandler] 订单创建事件: order_id=%s, payload_keys=%v", aggregateId, mapKeys(payload))
		h.publishToKafka(payload, orderEnum.OperationCreate)

	case "order.updated":
		Logger().Infof("[OrderHandler] 订单更新事件: order_id=%s, payload_keys=%v", aggregateId, mapKeys(payload))
		h.publishToKafka(payload, orderEnum.OperationUpdate)

	case "order.deleted":
		Logger().Infof("[OrderHandler] 订单删除事件: order_id=%s", aggregateId)
		h.publishToKafka(payload, orderEnum.OperationDelete)

	default:
		Logger().Warnf("[OrderHandler] 未知/未处理事件类型: event_type=%s, event_id=%s", eventType, eventId)
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
			Logger().Infof("[OrderHandler] 幂等记录已存在（并发写入），跳过: event_id=%s", eventId)
			return nil
		}
		return fmt.Errorf("写入幂等记录失败: %w", err)
	}

	Logger().Infof("[OrderHandler] 事件处理完成: event_id=%s event_type=%s", eventId, eventType)
	return nil
}

// publishToKafka 将事件转发到 shop-product topic，供下游消费
// 注意：订单 aggregateId 是 OrderId（string），旧代码使用 Uid 作为 Kafka Key
// 错误不中断主流程，仅记录日志
func (h *OrderHandler) publishToKafka(payload map[string]interface{}, operation string) {
	// 1. 从 payload 中提取 Uid 作为 Kafka Key（与旧代码行为一致）
	uidVal, ok := payload["uid"]
	if !ok {
		Logger().Errorf("[OrderHandler] payload 中缺少 uid 字段，跳过转发: operation=%s", operation)
		return
	}
	// JSON 反序列化数字为 float64
	uidFloat, ok := uidVal.(float64)
	if !ok {
		Logger().Errorf("[OrderHandler] uid 类型断言失败: %T", uidVal)
		return
	}
	uid := int64(uidFloat)

	// 2. 将 payload 反序列化为 StoreOrder
	orderData, err := json.Marshal(payload)
	if err != nil {
		Logger().Errorf("[OrderHandler] payload 序列化失败: %v", err)
		return
	}

	var storeOrder models.StoreOrder
	if err := json.Unmarshal(orderData, &storeOrder); err != nil {
		Logger().Errorf("[OrderHandler] payload 反序列化为 StoreOrder 失败: %v", err)
		return
	}

	// 3. 构造 OrderMsg
	orderMsg := models.OrderMsg{
		Operation:  operation,
		StoreOrder: &storeOrder,
	}

	msgBytes, err := json.Marshal(orderMsg)
	if err != nil {
		Logger().Errorf("[OrderHandler] OrderMsg 序列化失败: %v", err)
		return
	}

	// 4. 发送到 Kafka
	producer := mq.GetKafkaSyncProducer(mq.DefaultKafkaSyncProducer)
	if producer == nil {
		Logger().Error("[OrderHandler] Kafka Producer 未初始化，跳过转发")
		return
	}

	msg := &sarama.ProducerMessage{
		Topic: orderEnum.Topic,
		Key:   mq.KafkaMsgValueStrEncoder(strconv.FormatInt(uid, 10)),
		Value: mq.KafkaMsgValueEncoder(msgBytes),
	}

	partition, offset, err := producer.Send(msg)
	if err != nil {
		Logger().Errorf("[OrderHandler] 转发到 Kafka 失败: operation=%s, order_id=%s, err=%v", operation, storeOrder.OrderId, err)
		return
	}

	Logger().Infof("[OrderHandler] 转发成功: operation=%s, order_id=%s, uid=%d, partition=%d, offset=%d",
		operation, storeOrder.OrderId, uid, partition, offset)
}
