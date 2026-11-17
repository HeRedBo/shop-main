package workers

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"shop/internal/models"
	"shop/internal/worker"
	orderEnum "shop/pkg/enums/order"

	"github.com/IBM/sarama"
)

// fakeSyncProducer 记录发送调用并可注入返回错误，替代真实 Kafka。
type fakeSyncProducer struct {
	sent     []*sarama.ProducerMessage
	sendErr  error
	callSend int
}

func (f *fakeSyncProducer) Send(msg *sarama.ProducerMessage) (int32, int64, error) {
	f.callSend++
	if f.sendErr != nil {
		return 0, 0, f.sendErr
	}
	f.sent = append(f.sent, msg)
	return 0, 0, nil
}

// stubs 保存三个可注入接缝的原始实现，配合 t.Cleanup 还原，避免测试间相互污染。
func stubs(t *testing.T,
	producerFn func() orderSyncProducer,
	processedFn func(string) (bool, error),
	markFn func(models.ProcessedEvent) error,
) {
	t.Helper()
	oldP, oldC, oldM := orderSyncProducerFn, orderEventProcessedFn, markOrderProcessedFn
	if producerFn != nil {
		orderSyncProducerFn = producerFn
	}
	if processedFn != nil {
		orderEventProcessedFn = processedFn
	}
	if markFn != nil {
		markOrderProcessedFn = markFn
	}
	t.Cleanup(func() {
		orderSyncProducerFn, orderEventProcessedFn, markOrderProcessedFn = oldP, oldC, oldM
	})
}

func orderMsg(eventID, orderID string, extra map[string]interface{}) *worker.Message {
	payload := map[string]interface{}{"uid": float64(1), "order_id": orderID}
	for k, v := range extra {
		payload[k] = v
	}
	body, _ := json.Marshal(payload)
	return &worker.Message{
		Topic:     "shop-order-events",
		Headers:   map[string]string{"event_id": eventID, "aggregate_id": orderID},
		EventType: "order.created",
		Value:     body,
		RawOffset: int64(0),
	}
}

// ② 订单对外投递 topic 常量必须是 shop-order（外部 order-consumer 订阅此主题）。
func TestOrderTopicConstant(t *testing.T) {
	if orderEnum.Topic != "shop-order" {
		t.Fatalf("订单对外投递 topic 应为 shop-order，实际=%q", orderEnum.Topic)
	}
}

// ① publishToKafka：底层 Send 失败时，error 必须向上传播（不能被吞成 nil）。
func TestPublishToKafka_PropagatesSendError(t *testing.T) {
	sendErr := errors.New("boom-kafka")
	fp := &fakeSyncProducer{sendErr: sendErr}
	stubs(t, func() orderSyncProducer { return fp }, nil, nil)

	h := NewOrderHandler()
	err := h.publishToKafka(map[string]interface{}{"uid": float64(123), "order_id": "ORD1"}, orderEnum.OperationCreate)
	if err == nil {
		t.Fatal("期望 publishToKafka 在 Send 失败时返回 error，实际返回 nil（错误被静默吞掉）")
	}
	if !errors.Is(err, sendErr) {
		t.Fatalf("返回的 error 应包装底层 Send 错误，实际=%v", err)
	}
}

// ① publishToKafka：缺少 uid 字段应返回 error 且不得触发 Send。
func TestPublishToKafka_MissingUIDReturnsError(t *testing.T) {
	fp := &fakeSyncProducer{}
	stubs(t, func() orderSyncProducer { return fp }, nil, nil)

	h := NewOrderHandler()
	err := h.publishToKafka(map[string]interface{}{"order_id": "ORD1"}, orderEnum.OperationCreate)
	if err == nil {
		t.Fatal("缺少 uid 时应返回 error")
	}
	if fp.callSend != 0 {
		t.Fatalf("缺少 uid 不应触发 Send，实际=%d", fp.callSend)
	}
}

// ① publishToKafka：Producer 未初始化（nil）应返回 error 而非静默跳过。
func TestPublishToKafka_NilProducerReturnsError(t *testing.T) {
	stubs(t, func() orderSyncProducer { return nil }, nil, nil)

	h := NewOrderHandler()
	err := h.publishToKafka(map[string]interface{}{"uid": float64(1), "order_id": "ORD1"}, orderEnum.OperationCreate)
	if err == nil {
		t.Fatal("Producer 未初始化时应返回 error 而非静默跳过")
	}
}

// ③ Handle：幂等命中（processed_events 已存在）时直接返回 nil，绝不重复投递。
func TestHandle_IdempotentHitSkipsDelivery(t *testing.T) {
	fp := &fakeSyncProducer{}
	marked := 0
	stubs(t,
		func() orderSyncProducer { return fp },
		func(string) (bool, error) { return true, nil }, // 已处理
		func(models.ProcessedEvent) error { marked++; return nil },
	)

	h := NewOrderHandler()
	if err := h.Handle(context.Background(), orderMsg("evt-hit", "ORD1", nil)); err != nil {
		t.Fatalf("幂等命中应返回 nil，实际=%v", err)
	}
	if fp.callSend != 0 {
		t.Fatalf("幂等命中不应重复投递，Send 被调用 %d 次", fp.callSend)
	}
	if marked != 0 {
		t.Fatalf("幂等命中不应再写标记，marked=%d", marked)
	}
}

// 关键顺序保证：转发失败时 Handle 返回 error 且不写幂等标记（否则重试因命中幂等而永久丢投递）。
func TestHandle_PublishFailureNoIdempotentMark(t *testing.T) {
	fp := &fakeSyncProducer{sendErr: errors.New("kafka down")}
	marked := 0
	stubs(t,
		func() orderSyncProducer { return fp },
		func(string) (bool, error) { return false, nil },
		func(models.ProcessedEvent) error { marked++; return nil },
	)

	h := NewOrderHandler()
	err := h.Handle(context.Background(), orderMsg("evt-fail", "ORD1", nil))
	if err == nil {
		t.Fatal("转发失败时 Handle 应返回 error（触发重试/死信），实际返回 nil")
	}
	if marked != 0 {
		t.Fatalf("转发失败不应写幂等标记（否则永久丢投递），marked=%d", marked)
	}
}

// 转发成功时：投递一次、写入幂等标记一次，且投递目标 topic = shop-order。
func TestHandle_PublishSuccessMarksIdempotent(t *testing.T) {
	fp := &fakeSyncProducer{}
	marked := 0
	stubs(t,
		func() orderSyncProducer { return fp },
		func(string) (bool, error) { return false, nil },
		func(models.ProcessedEvent) error { marked++; return nil },
	)

	h := NewOrderHandler()
	if err := h.Handle(context.Background(), orderMsg("evt-ok", "ORD9", map[string]interface{}{"uid": float64(7)})); err != nil {
		t.Fatalf("转发成功应返回 nil，实际=%v", err)
	}
	if fp.callSend != 1 {
		t.Fatalf("成功应投递一次，实际=%d", fp.callSend)
	}
	if marked != 1 {
		t.Fatalf("成功后应写幂等标记一次，实际=%d", marked)
	}
	if got := fp.sent[0].Topic; got != "shop-order" {
		t.Fatalf("投递 topic 应为 shop-order，实际=%q", got)
	}
}
