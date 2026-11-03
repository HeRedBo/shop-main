package workers

import (
	"context"
	"encoding/json"
	"fmt"
	"shop/internal/models"
	"shop/internal/worker"
	"shop/pkg/enums/product"
	"shop/pkg/global"
	"strconv"
	"strings"
	"time"

	"github.com/HeRedBo/pkg/mq"
	"github.com/IBM/sarama"
)

// ProductHandler 商品事件处理器
// 处理 product.created / product.updated / product.deleted / product.on_sale / product.un_sale 事件
// 事件分类职责在 Observer 层：通过 dirty tracking 精确判断 is_show 变化，
// 发出 product.on_sale / product.un_sale / product.updated；
// Handler 中 product.updated 仍保留 resolveUpdateOperation 兜底判断（应对 dirty tracking 不可用的场景）。
// 当前阶段：幂等检查 + 日志记录（后续可扩展为更新 ES 索引、刷新缓存等）
// 注意：event_id / event_type / aggregate_id 由 Relay 写入 Kafka Headers，必定存在
type ProductHandler struct{}

// NewProductHandler 创建 ProductHandler 实例
func NewProductHandler() *ProductHandler {
	return &ProductHandler{}
}

// Handle 处理商品事件
// 流程：提取元数据 → 幂等检查 → 业务逻辑 → 记录幂等
func (h *ProductHandler) Handle(ctx context.Context, msg *worker.Message) error {
	// 1. 从 Kafka Headers 中提取元数据（Relay 投递时必定设置）
	eventId := msg.Headers["event_id"]
	eventType := msg.EventType // consumer 已从 headers["event_type"] 中提取
	aggregateId := msg.Headers["aggregate_id"]

	// 从 RawOffset 中提取 Kafka offset（仅用于日志）
	offset := extractKafkaOffset(msg.RawOffset)

	if eventId == "" {
		Logger().Warnf("[ProductHandler] 消息缺少 event_id header，跳过: topic=%s offset=%d", msg.Topic, offset)
		return nil
	}

	Logger().Infof("[ProductHandler] 收到事件: event_id=%s event_type=%s aggregate_id=%s offset=%d",
		eventId, eventType, aggregateId, offset)

	// 2. 幂等检查：查询 processed_events 表是否已处理该 event_id
	var count int64
	if err := global.Db.Model(&models.ProcessedEvent{}).Where("event_id = ?", eventId).Count(&count).Error; err != nil {
		return fmt.Errorf("查询幂等记录失败: %w", err)
	}
	if count > 0 {
		Logger().Infof("[ProductHandler] 事件已处理，跳过: event_id=%s", eventId)
		return nil
	}

	// 3. 解析 payload（商品业务数据）
	var payload map[string]interface{}
	if err := json.Unmarshal(msg.Value, &payload); err != nil {
		Logger().Errorf("[ProductHandler] 解析 payload 失败: %v, 原文: %s", err, truncate(string(msg.Value), 200))
		return fmt.Errorf("解析 payload 失败: %w", err)
	}

	// 4. 根据 event_type 执行业务逻辑
	switch eventType {
	case "product.created":
		Logger().Infof("[ProductHandler] 商品创建事件: aggregate_id=%s, payload_keys=%v", aggregateId, mapKeys(payload))
		h.publishToKafka(aggregateId, product.OperationCreate, payload)

	case "product.on_sale":
		Logger().Infof("[ProductHandler] 商品上架事件: aggregate_id=%s, payload_keys=%v", aggregateId, mapKeys(payload))
		h.publishToKafka(aggregateId, product.OperationOnSale, payload)

	case "product.un_sale":
		Logger().Infof("[ProductHandler] 商品下架事件: aggregate_id=%s, payload_keys=%v", aggregateId, mapKeys(payload))
		h.publishToKafka(aggregateId, product.OperationUnSale, payload)

	case "product.updated":
		Logger().Infof("[ProductHandler] 商品更新事件: aggregate_id=%s, payload_keys=%v", aggregateId, mapKeys(payload))
		// fallback：Observer 层 dirty tracking 不可用时，仍通过 is_show 值兜底判断
		operation := resolveUpdateOperation(payload)
		h.publishToKafka(aggregateId, operation, payload)

	case "product.deleted":
		Logger().Infof("[ProductHandler] 商品删除事件: aggregate_id=%s", aggregateId)
		h.publishToKafka(aggregateId, product.OperationDelete, payload)

	default:
		Logger().Warnf("[ProductHandler] 未知事件类型: event_type=%s, event_id=%s", eventType, eventId)
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
			Logger().Infof("[ProductHandler] 幂等记录已存在（并发写入），跳过: event_id=%s", eventId)
			return nil
		}
		return fmt.Errorf("写入幂等记录失败: %w", err)
	}

	Logger().Infof("[ProductHandler] 事件处理完成: event_id=%s event_type=%s", eventId, eventType)
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

// extractKafkaOffset 从 Message.RawOffset 中提取 Kafka offset
// RawOffset 是 interface{} 类型，Kafka 消费者中设置为 int64
// 若类型不匹配或为 nil，返回 -1（仅用于日志，不影响业务逻辑）
func extractKafkaOffset(rawOffset interface{}) int64 {
	if rawOffset == nil {
		return -1
	}
	if offset, ok := rawOffset.(int64); ok {
		return offset
	}
	return -1
}

// publishToKafka 将事件转发到 shop-product topic，供下游（如 ES 同步）消费
// 错误不中断主流程，仅记录日志
func (h *ProductHandler) publishToKafka(aggregateId string, operation string, payload map[string]interface{}) {
	id, err := strconv.ParseInt(aggregateId, 10, 64)
	if err != nil {
		Logger().Errorf("[ProductHandler] aggregate_id 转 int64 失败: id=%s, err=%v", aggregateId, err)
		return
	}

	// 构造 ProductMsg：将 payload 反序列化为 StoreProduct，再包装为 ProductMsg
	productData, err := json.Marshal(payload)
	if err != nil {
		Logger().Errorf("[ProductHandler] payload 序列化失败: %v", err)
		return
	}

	var storeProduct models.StoreProduct
	if err := json.Unmarshal(productData, &storeProduct); err != nil {
		Logger().Errorf("[ProductHandler] payload 反序列化为 StoreProduct 失败: %v", err)
		return
	}

	productMsg := models.ProductMsg{
		Operation:    operation,
		StoreProduct: &storeProduct,
	}

	msgBytes, err := json.Marshal(productMsg)
	if err != nil {
		Logger().Errorf("[ProductHandler] ProductMsg 序列化失败: %v", err)
		return
	}

	producer := mq.GetKafkaSyncProducer(mq.DefaultKafkaSyncProducer)
	if producer == nil {
		Logger().Error("[ProductHandler] Kafka Producer 未初始化，跳过转发")
		return
	}

	msg := &sarama.ProducerMessage{
		Topic: product.Topic,
		Key:   mq.KafkaMsgValueStrEncoder(strconv.FormatInt(id, 10)),
		Value: mq.KafkaMsgValueEncoder(msgBytes),
	}

	partition, offset, err := producer.Send(msg)
	if err != nil {
		Logger().Errorf("[ProductHandler] 转发到 Kafka 失败: operation=%s, aggregate_id=%s, err=%v", operation, aggregateId, err)
		return
	}

	Logger().Infof("[ProductHandler] 转发成功: operation=%s, aggregate_id=%s, partition=%d, offset=%d",
		operation, aggregateId, partition, offset)
}

// resolveUpdateOperation 根据 payload 中的 is_show 字段判断更新操作类型（fallback）
// 正常情况下 Observer 层已通过 dirty tracking 精确分类为 product.on_sale / product.un_sale，
// 此函数仅在 product.updated 事件到达 Handler 时作为兜底（如 dirty tracking 不可用的批量更新场景）。
// is_show=1 → on_sale，is_show=0 → un_sale，其他 → update
func resolveUpdateOperation(payload map[string]interface{}) string {
	val, ok := payload["is_show"]
	if !ok || val == nil {
		return product.OperationUpdate
	}

	// JSON 反序列化数字默认为 float64
	showFloat, ok := val.(float64)
	if !ok {
		return product.OperationUpdate
	}

	switch int(showFloat) {
	case 1:
		return product.OperationOnSale
	case 0:
		return product.OperationUnSale
	default:
		return product.OperationUpdate
	}
}

// mapKeys 获取 map 所有 key
func mapKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
