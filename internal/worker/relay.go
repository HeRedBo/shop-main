package worker

import (
	"context"
	"fmt"
	"time"

	"github.com/IBM/sarama"
	"github.com/HeRedBo/pkg/mq"
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

// Relay 事务发件箱投递器
// 定时扫描 event_outbox 表中 status=WAIT 的记录，投递到 Kafka
type Relay struct {
	ctx    context.Context
	cancel context.CancelFunc
	config WorkerRelayConfig
}

// NewRelay 创建 Relay 实例
func NewRelay(config WorkerRelayConfig) *Relay {
	ctx, cancel := context.WithCancel(context.Background())
	return &Relay{
		ctx:    ctx,
		cancel: cancel,
		config: config,
	}
}

// Start 启动 Relay 循环
// 启动三个定时任务（各自用 time.NewTicker，避免 CPU 空转）：
// 1. 主循环：扫描 WAIT 记录 → 投递 Kafka
// 2. 租约回收：扫描超时的 SENDING 记录
// 3. 定期清理：删除过期的 SENT 记录
func (r *Relay) Start() {
	global.LOG.Info("[relay] Relay 已启动")

	// 1. 主循环：扫描 WAIT 记录并投递
	go r.loop("scan-deliver", r.config.PollInterval, r.scanAndDeliver)

	// 2. 租约回收：扫描超时的 SENDING 记录（间隔为轮询间隔的 2 倍）
	reclaimInterval := r.config.PollInterval * 2
	if reclaimInterval < 10*time.Second {
		reclaimInterval = 10 * time.Second
	}
	go r.loop("reclaim-lease", reclaimInterval, r.reclaimLease)

	// 3. 定期清理：删除过期的 SENT 记录（每小时清理一次）
	go r.loop("cleanup-sent", 1*time.Hour, r.cleanupSent)
}

// Stop 停止 Relay
func (r *Relay) Stop() {
	r.cancel()
	global.LOG.Info("[relay] Relay 已停止")
}

// loop 通用定时循环，按 interval 间隔执行 fn，避免 CPU 空转
func (r *Relay) loop(name string, interval time.Duration, fn func()) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	global.LOG.Infof("[relay] 定时任务 [%s] 已启动，间隔=%v", name, interval)

	for {
		select {
		case <-r.ctx.Done():
			global.LOG.Infof("[relay] 定时任务 [%s] 已退出", name)
			return
		case <-ticker.C:
			func() {
				defer func() {
					if rec := recover(); rec != nil {
						global.LOG.Errorf("[relay] 定时任务 [%s] panic recovered: %v", name, rec)
					}
				}()
				fn()
			}()
		}
	}
}

// scanAndDeliver 扫描 WAIT 记录并投递到 Kafka
func (r *Relay) scanAndDeliver() {
	var outboxes []models.EventOutbox

	// 1. 查询 status=WAIT 的记录，LIMIT BatchSize
	err := global.Db.Where("status = ?", models.OutboxStatusWait).
		Order("created_at ASC").
		Limit(r.config.BatchSize).
		Find(&outboxes).Error
	if err != nil {
		global.LOG.Errorf("[relay] 查询 WAIT 记录失败: %v", err)
		return
	}

	if len(outboxes) == 0 {
		return
	}

	global.LOG.Debugf("[relay] 扫描到 %d 条 WAIT 记录", len(outboxes))

	// 2. 逐条投递到 Kafka
	for i := range outboxes {
		select {
		case <-r.ctx.Done():
			return
		default:
		}

		r.deliverOne(&outboxes[i])
	}
}

// deliverOne 投递单条 outbox 记录到 Kafka
func (r *Relay) deliverOne(outbox *models.EventOutbox) {
	// 先原子抢占：UPDATE status=SENDING, sending_at=NOW() WHERE id=? AND status=WAIT
	result := global.Db.Model(&models.EventOutbox{}).
		Where("id = ? AND status = ?", outbox.Id, models.OutboxStatusWait).
		Updates(map[string]interface{}{
			"status":    models.OutboxStatusSending,
			"sending_at": time.Now(),
		})
	if result.Error != nil {
		global.LOG.Errorf("[relay] 抢占记录 id=%d 失败: %v", outbox.Id, result.Error)
		return
	}
	if result.RowsAffected == 0 {
		// 已被其他实例抢占，跳过
		return
	}

	// 投递到 Kafka
	producer := mq.GetKafkaSyncProducer(mq.DefaultKafkaSyncProducer)
	if producer == nil {
		global.LOG.Error("[relay] Kafka Producer 未初始化")
		r.handleDeliveryFailure(outbox, "kafka producer not initialized")
		return
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
	if err != nil {
		global.LOG.Errorf("[relay] Kafka 投递失败 event_id=%s: %v", outbox.EventId, err)
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
		global.LOG.Errorf("[relay] 更新 SENT 状态失败 id=%d: %v", outbox.Id, err)
	} else {
		global.LOG.Debugf("[relay] 投递成功 event_id=%s, topic=%s", outbox.EventId, outbox.Topic)
	}
}

// handleDeliveryFailure 处理投递失败
func (r *Relay) handleDeliveryFailure(outbox *models.EventOutbox, errMsg string) {
	newRetryCount := outbox.RetryCount + 1

	if newRetryCount >= outbox.MaxRetries {
		// 超过最大重试次数 → 移入死信表
		global.LOG.Warnf("[relay] event_id=%s 超过最大重试次数(%d)，移入死信表", outbox.EventId, outbox.MaxRetries)
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
		global.LOG.Errorf("[relay] 更新重试状态失败 id=%d: %v", outbox.Id, err)
	} else {
		global.LOG.Debugf("[relay] event_id=%s 投递失败，将在 %v 后重试（第 %d 次）",
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
		global.LOG.Errorf("[relay] 移入死信表失败 event_id=%s: %v", outbox.EventId, err)
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
		global.LOG.Errorf("[relay] 租约回收失败: %v", result.Error)
		return
	}

	if result.RowsAffected > 0 {
		global.LOG.Warnf("[relay] 租约回收：重置了 %d 条超时的 SENDING 记录", result.RowsAffected)
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
				global.LOG.Infof("[relay] 清理中断，已删除 %d 条 SENT 记录", totalDeleted)
			}
			return
		default:
		}

		// 分批删除，每次 1000 条
		result := global.Db.Where("status = ? AND created_at < ?", models.OutboxStatusSent, threshold).
			Limit(1000).
			Delete(&models.EventOutbox{})

		if result.Error != nil {
			global.LOG.Errorf("[relay] 清理 SENT 记录失败: %v", result.Error)
			return
		}

		totalDeleted += result.RowsAffected

		if result.RowsAffected == 0 {
			break
		}
	}

	if totalDeleted > 0 {
		global.LOG.Infof("[relay] 清理完成，共删除 %d 条过期 SENT 记录", totalDeleted)
	}
}
