package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/HeRedBo/pkg/mq"
	"github.com/IBM/sarama"
	"github.com/go-redis/redis/v7"
	"gorm.io/gorm"

	"shop/internal/models"
	"shop/pkg/global"
)

// WorkerRelayConfig Relay 配置（从 global.CONFIG.Worker 提取）
type WorkerRelayConfig struct {
	PollInterval   time.Duration
	BatchSize      int
	LeaseTimeout   time.Duration
	MaxRetries     int
	RetryBaseDelay time.Duration
	CleanupDays    int // 清理多少天前的 SENT 记录
}

// outboxNotifyChannel Redis Pub/Sub 频道名，WriteOutbox 写入后 PUBLISH，Relay SUBSCRIBE 监听
const outboxNotifyChannel = "outbox:new"

// Relay 事务发件箱投递器
// 定时扫描 event_outbox 表中 status=WAIT 的记录，投递到 Kafka
type Relay struct {
	ctx    context.Context
	cancel context.CancelFunc
	config WorkerRelayConfig
	notify chan struct{} // 内部通知 channel，收到 Redis 通知后触发立即扫描
}

// 编译期断言：Relay 实现 Runnable 接口
var _ Runnable = (*Relay)(nil)

// NewRelay 创建 Relay 实例
func NewRelay(config WorkerRelayConfig) *Relay {
	return &Relay{
		config: config,
		notify: make(chan struct{}, 1), // 缓冲为 1，多余信号丢弃
	}
}

// Name 返回任务名称，实现 Runnable 接口
func (r *Relay) Name() string {
	return "relay"
}

// Start 启动 Relay 循环，实现 Runnable 接口
// 通知驱动 + Ticker 兜底：
// 1. Redis SUBSCRIBE 监听 outbox 通知，收到信号立即扫描
// 2. Ticker 兜底（间隔 10s），保证最终一致
// 3. 租约回收：扫描超时的 SENDING 记录（间隔 30s）
// 4. 定期清理：删除过期的 SENT 记录（每小时）
func (r *Relay) Start(ctx context.Context) error {
	// 从传入的 ctx 派生子 context，用于控制内部 goroutine 生命周期
	r.ctx, r.cancel = context.WithCancel(ctx)

	Logger().Info("[relay] Relay 已启动（通知驱动 + Ticker 兜底模式）")

	// 1. 启动 Redis SUBSCRIBE 监听
	go r.subscribeNotify()

	// 2. 主循环：Ticker 兜底（PollInterval）+ 通知触发
	go r.scanLoop()

	// 3. 租约回收：扫描超时的 SENDING 记录（间隔 30s）
	go r.loop("reclaim-lease", 30*time.Second, r.reclaimLease)

	// 4. 定期清理：删除过期的 SENT 记录（每小时）
	go r.loop("cleanup-sent", 1*time.Hour, r.cleanupSent)

	return nil
}

// Stop 停止 Relay，实现 Runnable 接口
func (r *Relay) Stop() error {
	if r.cancel != nil {
		r.cancel()
	}
	Logger().Info("[relay] Relay 已停止")
	return nil
}

// subscribeNotify 监听 Redis Pub/Sub 通知，收到 outbox:new 频道消息后触发内部 notify channel
// go-redis v7 断线后自动重连
func (r *Relay) subscribeNotify() {
	if global.RedisClient == nil {
		Logger().Warn("[relay] Redis 客户端未初始化，仅使用 Ticker 兜底")
		return
	}

	pubsub := global.RedisClient.Subscribe(outboxNotifyChannel)
	defer pubsub.Close()

	ch := pubsub.Channel()
	Logger().Infof("[relay] 已订阅 Redis 频道: %s", outboxNotifyChannel)

	for {
		select {
		case <-r.ctx.Done():
			Logger().Info("[relay] subscribeNotify 已退出")
			return
		case msg, ok := <-ch:
			if !ok {
				// channel 被关闭（pubsub.Close() 触发）
				return
			}
			Logger().Debugf("[relay] 收到 outbox 通知: %s", msg.Payload)
			// 非阻塞通知内部 channel
			select {
			case r.notify <- struct{}{}:
			default:
				// channel 已满，说明已有待处理的扫描任务，丢弃多余信号
			}
		}
	}
}

// scanLoop 主扫描循环：Ticker 兜底 + 通知触发
func (r *Relay) scanLoop() {
	interval := r.config.PollInterval
	if interval <= 0 {
		interval = 10 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	Logger().Infof("[relay] scanLoop 已启动（Ticker=%v + 通知触发）", interval)

	for {
		select {
		case <-r.ctx.Done():
			Logger().Info("[relay] scanLoop 已退出")
			return
		case <-r.notify:
			// 收到通知，立即扫描
			func() {
				defer func() {
					if rec := recover(); rec != nil {
						Logger().Errorf("[relay] scanLoop 通知触发扫描 panic recovered: %v", rec)
					}
				}()
				r.scanAndDeliver()
			}()
		case <-ticker.C:
			// 兜底扫描
			func() {
				defer func() {
					if rec := recover(); rec != nil {
						Logger().Errorf("[relay] scanLoop 兜底扫描 panic recovered: %v", rec)
					}
				}()
				r.scanAndDeliver()
			}()
		}
	}
}

// loop 通用定时循环，按 interval 间隔执行 fn，避免 CPU 空转
func (r *Relay) loop(name string, interval time.Duration, fn func()) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	Logger().Infof("[relay] 定时任务 [%s] 已启动，间隔=%v", name, interval)

	for {
		select {
		case <-r.ctx.Done():
			Logger().Infof("[relay] 定时任务 [%s] 已退出", name)
			return
		case <-ticker.C:
			func() {
				defer func() {
					if rec := recover(); rec != nil {
						Logger().Errorf("[relay] 定时任务 [%s] panic recovered: %v", name, rec)
					}
				}()
				fn()
			}()
		}
	}
}

// scanAndDeliver 扫描 WAIT 记录并按 queue_type 分发投递
func (r *Relay) scanAndDeliver() {
	// 1. 扫描即时消息（Kafka）：queue_type=kafka 且 execute_at IS NULL
	r.scanAndDeliverByType(models.QueueTypeKafka, true)

	// 2. 扫描 Redis 消息：queue_type=redis 且（即时消息 execute_at IS NULL OR 延迟消息 execute_at <= NOW()）
	r.scanAndDeliverByType(models.QueueTypeRedis, false)
}

// scanAndDeliverByType 按队列类型扫描并投递
func (r *Relay) scanAndDeliverByType(queueType string, immediateOnly bool) {
	var outboxes []models.EventOutbox

	query := global.Db.Where("status = ? AND queue_type = ?", models.OutboxStatusWait, queueType)

	if immediateOnly {
		// Kafka 即时消息：execute_at IS NULL
		query = query.Where("execute_at IS NULL")
	} else {
		// Redis 消息：即时消息（execute_at IS NULL）+ 到期的延迟消息（execute_at <= NOW）
		query = query.Where("(execute_at IS NULL OR execute_at <= ?)", time.Now())
	}

	err := query.Order("created_at ASC").
		Limit(r.config.BatchSize).
		Find(&outboxes).Error
	if err != nil {
		Logger().Errorf("[relay] 查询 %s WAIT 记录失败: %v", queueType, err)
		return
	}

	if len(outboxes) == 0 {
		return
	}

	Logger().Debugf("[relay] 扫描到 %d 条 %s WAIT 记录", len(outboxes), queueType)

	for i := range outboxes {
		select {
		case <-r.ctx.Done():
			return
		default:
		}
		r.deliverOne(&outboxes[i])
	}
}

// deliverOne 投递单条 outbox 记录（根据 queue_type 分发到 Kafka 或 Redis）
func (r *Relay) deliverOne(outbox *models.EventOutbox) {
	// 先原子抢占：UPDATE status=SENDING, sending_at=NOW() WHERE id=? AND status=WAIT
	result := global.Db.Model(&models.EventOutbox{}).
		Where("id = ? AND status = ?", outbox.Id, models.OutboxStatusWait).
		Updates(map[string]interface{}{
			"status":     models.OutboxStatusSending,
			"sending_at": time.Now(),
		})
	if result.Error != nil {
		Logger().Errorf("[relay] 抢占记录 id=%d 失败: %v", outbox.Id, result.Error)
		return
	}
	if result.RowsAffected == 0 {
		// 已被其他实例抢占，跳过
		return
	}

	// 根据 queue_type 选择投递方式
	var err error
	switch outbox.QueueType {
	case models.QueueTypeRedis:
		err = r.deliverToRedis(outbox)
	default: // kafka
		err = r.deliverToKafka(outbox)
	}

	if err != nil {
		Logger().Errorf("[relay] %s 投递失败 event_id=%s: %v", outbox.QueueType, outbox.EventId, err)
		r.handleDeliveryFailure(outbox, err.Error())
		return
	}

	// 投递成功：UPDATE status=SENT, sending_at=NULL
	err = global.Db.Model(&models.EventOutbox{}).
		Where("id = ? AND status = ?", outbox.Id, models.OutboxStatusSending).
		Updates(map[string]interface{}{
			"status":     models.OutboxStatusSent,
			"sending_at": nil,
			"error_msg":  "",
		}).Error
	if err != nil {
		Logger().Errorf("[relay] 更新 SENT 状态失败 id=%d: %v", outbox.Id, err)
	} else {
		Logger().Debugf("[relay] 投递成功 event_id=%s, queue_type=%s, topic=%s", outbox.EventId, outbox.QueueType, outbox.Topic)
	}
}

// deliverToKafka 投递到 Kafka
func (r *Relay) deliverToKafka(outbox *models.EventOutbox) error {
	producer := mq.GetKafkaSyncProducer(mq.DefaultKafkaSyncProducer)
	if producer == nil {
		return fmt.Errorf("kafka producer not initialized")
	}

	msg := &sarama.ProducerMessage{
		Topic: outbox.Topic,
		Key:   mq.KafkaMsgValueStrEncoder(outbox.PartitionKey),
		Value: mq.KafkaMsgValueEncoder([]byte(outbox.Payload)),
		Headers: []sarama.RecordHeader{
			{Key: []byte("event_id"), Value: []byte(outbox.EventId)},
			{Key: []byte("event_type"), Value: []byte(outbox.EventType)},
			{Key: []byte("aggregate_id"), Value: []byte(outbox.AggregateId)},
		},
	}

	_, _, err := producer.Send(msg)
	return err
}

// deliverToRedis 投递到 Redis Stream
func (r *Relay) deliverToRedis(outbox *models.EventOutbox) error {
	if global.RedisClient == nil {
		return fmt.Errorf("redis client not initialized")
	}

	// headers 序列化为 JSON 字符串（Redis Stream field value 不支持嵌套 map）
	headersJSON, _ := json.Marshal(map[string]string{
		"event_id":     outbox.EventId,
		"event_type":   outbox.EventType,
		"aggregate_id": outbox.AggregateId,
	})

	// XADD: 写入 Redis Stream
	// 使用 Stream key = outbox.Topic（如 "delay-order-timeout"）
	result, err := global.RedisClient.XAdd(&redis.XAddArgs{
		Stream: outbox.Topic,
		Values: map[string]interface{}{
			"event_id":     outbox.EventId,
			"event_type":   outbox.EventType,
			"aggregate_id": outbox.AggregateId,
			"key":          outbox.PartitionKey,
			"value":        outbox.Payload,
			"headers":      string(headersJSON),
		},
	}).Result()

	if err != nil {
		return fmt.Errorf("XADD 失败: %w", err)
	}

	// 投递成功后，清理 Sorted Set 中的调度记录（防止 DB Relay 兜底投递后残留）
	global.RedisClient.ZRem(delayScheduleKey, outbox.EventId)

	Logger().Debugf("[relay] Redis 投递成功 stream=%s, id=%s", outbox.Topic, result)
	return nil
}

// handleDeliveryFailure 处理投递失败
func (r *Relay) handleDeliveryFailure(outbox *models.EventOutbox, errMsg string) {
	newRetryCount := outbox.RetryCount + 1

	if newRetryCount >= outbox.MaxRetries {
		// 超过最大重试次数 → 移入死信表
		Logger().Warnf("[relay] event_id=%s 超过最大重试次数(%d)，移入死信表", outbox.EventId, outbox.MaxRetries)
		r.moveToDeadLetter(outbox, errMsg)
		return
	}

	// 指数退避：retryBaseDelay * 2^retryCount
	backoff := r.config.RetryBaseDelay * (1 << outbox.RetryCount)
	if backoff > 30*time.Minute {
		backoff = 30 * time.Minute
	}

	// 保持 WAIT 状态，retry_count+1，记录错误信息
	err := global.Db.Model(&models.EventOutbox{}).
		Where("id = ? AND status = ?", outbox.Id, models.OutboxStatusSending).
		Updates(map[string]interface{}{
			"status":      models.OutboxStatusWait,
			"sending_at":  nil,
			"retry_count": newRetryCount,
			"error_msg":   errMsg,
		}).Error
	if err != nil {
		Logger().Errorf("[relay] 更新重试状态失败 id=%d: %v", outbox.Id, err)
	} else {
		Logger().Debugf("[relay] event_id=%s 投递失败，将在 %v 后重试（第 %d 次）",
			outbox.EventId, backoff, newRetryCount)
	}
}

// moveToDeadLetter 将失败记录移入死信表（在同一事务中）
func (r *Relay) moveToDeadLetter(outbox *models.EventOutbox, errMsg string) {
	err := global.Db.Transaction(func(tx *gorm.DB) error {
		// 1. 写入死信表
		deadLetter := models.EventDeadLetter{
			EventId:      outbox.EventId,
			AggregateId:  outbox.AggregateId,
			EventType:    outbox.EventType,
			Topic:        outbox.Topic,
			PartitionKey: outbox.PartitionKey,
			Payload:      outbox.Payload,
			ErrorMsg:     errMsg,
			RetryCount:   outbox.RetryCount,
			Status:       models.DeadLetterStatusPending,
			CreatedAt:    time.Now(),
			UpdatedAt:    time.Now(),
		}
		if err := tx.Create(&deadLetter).Error; err != nil {
			return fmt.Errorf("写入死信表失败: %w", err)
		}

		// 2. 更新 outbox 状态为 DEAD
		if err := tx.Model(&models.EventOutbox{}).
			Where("id = ?", outbox.Id).
			Updates(map[string]interface{}{
				"status":     models.OutboxStatusDead,
				"sending_at": nil,
				"error_msg":  errMsg,
			}).Error; err != nil {
			return fmt.Errorf("更新 outbox 状态为 DEAD 失败: %w", err)
		}

		return nil
	})

	if err != nil {
		Logger().Errorf("[relay] 移入死信表失败 event_id=%s: %v", outbox.EventId, err)
	}
}

// reclaimLease 租约回收
// 扫描 status=SENDING 且 sending_at 超时的记录，重置为 WAIT
func (r *Relay) reclaimLease() {
	threshold := time.Now().Add(-r.config.LeaseTimeout)

	result := global.Db.Model(&models.EventOutbox{}).
		Where("status = ? AND sending_at < ?", models.OutboxStatusSending, threshold).
		Updates(map[string]interface{}{
			"status":     models.OutboxStatusWait,
			"sending_at": nil,
		})

	if result.Error != nil {
		Logger().Errorf("[relay] 租约回收失败: %v", result.Error)
		return
	}

	if result.RowsAffected > 0 {
		Logger().Warnf("[relay] 租约回收：重置了 %d 条超时的 SENDING 记录", result.RowsAffected)
	}
}

// cleanupSent 清理已发送的记录
// 删除 SENT 状态超过 CleanupDays 天的记录，分批删除（每次 1000 条），避免锁表
func (r *Relay) cleanupSent() {
	if r.config.CleanupDays <= 0 {
		return
	}

	threshold := time.Now().AddDate(0, 0, -r.config.CleanupDays)
	totalDeleted := int64(0)

	for {
		select {
		case <-r.ctx.Done():
			if totalDeleted > 0 {
				Logger().Infof("[relay] 清理中断，已删除 %d 条 SENT 记录", totalDeleted)
			}
			return
		default:
		}

		// 分批删除，每次 1000 条
		result := global.Db.Where("status = ? AND created_at < ?", models.OutboxStatusSent, threshold).
			Limit(1000).
			Delete(&models.EventOutbox{})

		if result.Error != nil {
			Logger().Errorf("[relay] 清理 SENT 记录失败: %v", result.Error)
			return
		}

		totalDeleted += result.RowsAffected

		if result.RowsAffected == 0 {
			break
		}
	}

	if totalDeleted > 0 {
		Logger().Infof("[relay] 清理完成，共删除 %d 条过期 SENT 记录", totalDeleted)
	}
}
