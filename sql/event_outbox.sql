-- =============================================
-- Transactional Outbox 事务发件箱表
-- =============================================
CREATE TABLE IF NOT EXISTS `event_outbox` (
    `id`           BIGINT       NOT NULL AUTO_INCREMENT COMMENT '主键',
    `event_id`     VARCHAR(36)  NOT NULL COMMENT '事件唯一ID（UUID），消费端幂等键',
    `aggregate_id` VARCHAR(64)  NOT NULL COMMENT '业务实体ID（如订单号、商品ID）',
    `event_type`   VARCHAR(64)  NOT NULL COMMENT '事件类型（如 order.created, product.updated）',
    `topic`        VARCHAR(128) NOT NULL COMMENT 'Kafka Topic',
    `partition_key` VARCHAR(64) NOT NULL DEFAULT '' COMMENT 'Kafka 分区键（保证同一业务实体消息有序）',
    `payload`      JSON         NOT NULL COMMENT '事件数据（JSON 格式）',
    `status`       VARCHAR(16)  NOT NULL DEFAULT 'WAIT' COMMENT '状态：WAIT-待投递 SENDING-投递中 SENT-已投递 DEAD-死信',
    `retry_count`  INT          NOT NULL DEFAULT 0 COMMENT '已重试次数',
    `max_retries`  INT          NOT NULL DEFAULT 5 COMMENT '最大重试次数',
    `error_msg`    TEXT                  DEFAULT NULL COMMENT '最近一次错误信息',
    `sending_at`   DATETIME              DEFAULT NULL COMMENT '开始投递时间（租约控制）',
    `created_at`   DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at`   DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_event_id` (`event_id`),
    KEY `idx_status_created` (`status`, `created_at`),
    KEY `idx_sending_at` (`sending_at`),
    KEY `idx_aggregate_id` (`aggregate_id`),
    KEY `idx_event_type` (`event_type`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Transactional Outbox 事务发件箱';

-- =============================================
-- 死信表：超过最大重试次数的事件
-- =============================================
CREATE TABLE IF NOT EXISTS `event_dead_letter` (
    `id`           BIGINT       NOT NULL AUTO_INCREMENT COMMENT '主键',
    `event_id`     VARCHAR(36)  NOT NULL COMMENT '事件唯一ID',
    `aggregate_id` VARCHAR(64)  NOT NULL COMMENT '业务实体ID',
    `event_type`   VARCHAR(64)  NOT NULL COMMENT '事件类型',
    `topic`        VARCHAR(128) NOT NULL COMMENT 'Kafka Topic',
    `partition_key` VARCHAR(64) NOT NULL DEFAULT '' COMMENT 'Kafka 分区键',
    `payload`      JSON         NOT NULL COMMENT '事件数据',
    `error_msg`    TEXT         NOT NULL COMMENT '最终错误信息',
    `retry_count`  INT          NOT NULL DEFAULT 0 COMMENT '总重试次数',
    `status`       VARCHAR(16)  NOT NULL DEFAULT 'PENDING' COMMENT '状态：PENDING-待处理 RESOLVED-已解决',
    `resolved_at`  DATETIME              DEFAULT NULL COMMENT '解决时间',
    `created_at`   DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updated_at`   DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_event_id` (`event_id`),
    KEY `idx_status_created` (`status`, `created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='事件死信表';

-- =============================================
-- 幂等消费记录表
-- =============================================
CREATE TABLE IF NOT EXISTS `processed_events` (
    `id`           BIGINT       NOT NULL AUTO_INCREMENT COMMENT '主键',
    `event_id`     VARCHAR(36)  NOT NULL COMMENT '事件唯一ID',
    `event_type`   VARCHAR(64)  NOT NULL COMMENT '事件类型',
    `aggregate_id` VARCHAR(64)  NOT NULL COMMENT '业务实体ID',
    `processed_at` DATETIME     NOT NULL COMMENT '处理时间',
    `created_at`   DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_event_id` (`event_id`),
    KEY `idx_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='幂等消费记录表';
