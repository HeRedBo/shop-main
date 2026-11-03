package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"shop/internal/models"
	"shop/pkg/global"

	"github.com/go-redis/redis/v7"
)

// delayScheduleKey Redis Sorted Set key，用于延迟消息调度
const delayScheduleKey = "delay:schedule"

// DelayScheduler 基于 Redis Sorted Set 的延迟调度器
// 主调度路径：ZADD → 定时轮询 ZRANGEBYSCORE → XADD Stream → ZREM
// DB 轮询（Relay）作为降级兜底
type DelayScheduler struct {
	ctx          context.Context
	cancel       context.CancelFunc
	client       *redis.Client
	pollInterval time.Duration // 轮询间隔（默认 1s）
	batchSize    int           // 每次最多调度的消息数
}

// 编译期断言：DelayScheduler 实现 Runnable 接口
var _ Runnable = (*DelayScheduler)(nil)

// NewDelayScheduler 创建延迟调度器
func NewDelayScheduler(client *redis.Client) *DelayScheduler {
	return &DelayScheduler{
		client:       client,
		pollInterval: 1 * time.Second,
		batchSize:    50,
	}
}

// Name 返回任务名称，实现 Runnable 接口
func (ds *DelayScheduler) Name() string {
	return "delay-scheduler"
}

// Start 启动调度循环，实现 Runnable 接口
func (ds *DelayScheduler) Start(ctx context.Context) error {
	// 从传入的 ctx 派生子 context
	ds.ctx, ds.cancel = context.WithCancel(ctx)

	Logger().Info("[delay-scheduler] 延迟调度器已启动（Redis Sorted Set 模式）")
	go ds.scheduleLoop()
	return nil
}

// Stop 停止调度器，实现 Runnable 接口
func (ds *DelayScheduler) Stop() error {
	if ds.cancel != nil {
		ds.cancel()
	}
	Logger().Info("[delay-scheduler] 延迟调度器已停止")
	return nil
}

// scheduleLoop 主调度循环：定时扫描 Sorted Set 中到期的延迟消息
func (ds *DelayScheduler) scheduleLoop() {
	ticker := time.NewTicker(ds.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ds.ctx.Done():
			return
		case <-ticker.C:
			func() {
				defer func() {
					if r := recover(); r != nil {
						Logger().Errorf("[delay-scheduler] scheduleLoop panic recovered: %v", r)
					}
				}()
				ds.scheduleDueMessages()
			}()
		}
	}
}

// scheduleDueMessages 调度所有到期的延迟消息
func (ds *DelayScheduler) scheduleDueMessages() {
	now := time.Now().UnixMilli()

	// 1. ZRANGEBYSCORE: 获取 score <= now 的消息（即已到期的）
	results, err := ds.client.ZRangeByScore(delayScheduleKey, &redis.ZRangeBy{
		Min:    "-inf",
		Max:    fmt.Sprintf("%d", now),
		Offset: 0,
		Count:  int64(ds.batchSize),
	}).Result()

	if err != nil {
		if err != redis.Nil {
			Logger().Errorf("[delay-scheduler] ZRANGEBYSCORE 失败: %v", err)
		}
		return
	}

	if len(results) == 0 {
		return
	}

	Logger().Debugf("[delay-scheduler] 扫描到 %d 条到期延迟消息", len(results))

	for _, eventId := range results {
		ds.deliverDelayedMessage(eventId)
	}
}

// deliverDelayedMessage 投递单条到期延迟消息
func (ds *DelayScheduler) deliverDelayedMessage(eventId string) {
	// 1. 查询 event_outbox 获取完整消息
	var outbox models.EventOutbox
	if err := global.Db.Where("event_id = ? AND status = ?", eventId, models.OutboxStatusWait).First(&outbox).Error; err != nil {
		// 记录不存在或已非 WAIT 状态（可能已被 DB Relay 兜底处理），移除 Sorted Set 条目
		ds.client.ZRem(delayScheduleKey, eventId)
		return
	}

	// 2. 原子抢占：UPDATE status=SENDING WHERE id=? AND status=WAIT
	result := global.Db.Model(&models.EventOutbox{}).
		Where("id = ? AND status = ?", outbox.Id, models.OutboxStatusWait).
		Updates(map[string]interface{}{
			"status":     models.OutboxStatusSending,
			"sending_at": time.Now(),
		})
	if result.RowsAffected == 0 {
		// 已被 Relay 兜底抢占，移除 Sorted Set 条目
		ds.client.ZRem(delayScheduleKey, eventId)
		return
	}

	// 3. XADD 到 Redis Stream
	streamPayload := map[string]interface{}{
		"event_id":     outbox.EventId,
		"event_type":   outbox.EventType,
		"aggregate_id": outbox.AggregateId,
		"key":          outbox.PartitionKey,
		"value":        outbox.Payload,
	}
	// headers 需要序列化为 JSON 字符串（Redis Stream field value 不支持嵌套 map）
	headersJSON, _ := json.Marshal(map[string]string{
		"event_id":     outbox.EventId,
		"event_type":   outbox.EventType,
		"aggregate_id": outbox.AggregateId,
	})
	streamPayload["headers"] = string(headersJSON)

	streamID, err := ds.client.XAdd(&redis.XAddArgs{
		Stream: outbox.Topic,
		Values: streamPayload,
	}).Result()

	if err != nil {
		Logger().Errorf("[delay-scheduler] XADD 失败 event_id=%s: %v", eventId, err)
		// 回滚状态为 WAIT
		global.Db.Model(&models.EventOutbox{}).
			Where("id = ? AND status = ?", outbox.Id, models.OutboxStatusSending).
			Updates(map[string]interface{}{
				"status":     models.OutboxStatusWait,
				"sending_at": nil,
			})
		return
	}

	// 4. 更新 event_outbox 状态为 SENT
	global.Db.Model(&models.EventOutbox{}).
		Where("id = ? AND status = ?", outbox.Id, models.OutboxStatusSending).
		Updates(map[string]interface{}{
			"status":     models.OutboxStatusSent,
			"sending_at": nil,
			"error_msg":  "",
		})

	// 5. ZREM 从 Sorted Set 移除
	ds.client.ZRem(delayScheduleKey, eventId)

	Logger().Debugf("[delay-scheduler] 延迟消息投递成功 event_id=%s, stream=%s, stream_id=%s",
		eventId, outbox.Topic, streamID)
}

// ScheduleDelay 将延迟消息注册到 Sorted Set 调度器
// 在 WriteDelayOutbox 后调用，将 event_id 以 execute_at 为 score 写入 Sorted Set
func ScheduleDelay(client *redis.Client, eventId string, executeAt time.Time) error {
	if client == nil {
		return nil // Redis 不可用，依赖 DB Relay 兜底
	}

	score := float64(executeAt.UnixMilli())
	return client.ZAdd(delayScheduleKey, &redis.Z{
		Score:  score,
		Member: eventId,
	}).Err()
}
