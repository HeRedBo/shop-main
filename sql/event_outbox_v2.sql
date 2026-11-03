-- Phase 4: event_outbox 表新增 queue_type 和 execute_at 字段
ALTER TABLE event_outbox 
    ADD COLUMN queue_type VARCHAR(16) NOT NULL DEFAULT 'kafka' COMMENT '目标队列类型: kafka/redis' AFTER payload,
    ADD COLUMN execute_at DATETIME NULL DEFAULT NULL COMMENT '延迟执行时间（Redis 延迟队列专用）' AFTER sending_at;

-- 为延迟消息查询添加索引
ALTER TABLE event_outbox
    ADD INDEX idx_queue_type_status_execute (queue_type, status, execute_at);

-- Phase 5: event_dead_letter 表新增 queue_type 字段
ALTER TABLE event_dead_letter
    ADD COLUMN queue_type VARCHAR(16) NOT NULL DEFAULT 'kafka' COMMENT '来源队列类型: kafka/redis' AFTER payload;
