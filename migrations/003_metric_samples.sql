-- 003：指标时序表（**由 sqlc 负责的读写，刻意不放进 Ent**）。
--
-- 为什么不给 Ent 管：
--  1. 这是高写入 + 强聚合的数据（每房间每 N 秒一行；查询是"按时间桶 GROUP BY、取均值/百分位"），
--     显式 SQL 比 ORM 的查询 DSL 更直接，也更容易在论文里解释清楚；
--  2. 采样发生在后台协程里，我们不希望任何 ORM 的懒加载/预加载意外把连接拖在那里；
--  3. 边界清晰：同一张表只有一个写者（Ent 写 rooms/meetings，sqlc 写 metric_samples）。
--
-- 注意：这张表**不参与媒体热路径**，它只承接 /healthz 与 /debug/stats 已经在算的聚合值。
CREATE TABLE IF NOT EXISTS metric_samples (
    id           BIGSERIAL   PRIMARY KEY,
    room_id      TEXT        NOT NULL,
    sampled_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    participants INTEGER     NOT NULL DEFAULT 0,
    tracks       INTEGER     NOT NULL DEFAULT 0,
    packets      BIGINT      NOT NULL DEFAULT 0,
    bytes        BIGINT      NOT NULL DEFAULT 0,
    rtt_ms       INTEGER     NOT NULL DEFAULT 0,
    loss_rate    REAL        NOT NULL DEFAULT 0,
    out_kbps     INTEGER     NOT NULL DEFAULT 0,
    in_kbps      INTEGER     NOT NULL DEFAULT 0
);

-- 主查询形态：某房间某时间窗内的样本（按时间倒序）
CREATE INDEX IF NOT EXISTS idx_metric_samples_room_time
    ON metric_samples (room_id, sampled_at DESC);

-- 全局清理/按时间扫描（保留策略）
CREATE INDEX IF NOT EXISTS idx_metric_samples_time
    ON metric_samples (sampled_at DESC);
