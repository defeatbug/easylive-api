-- 指标时序的查询定义（sqlc 生成类型安全的 Go 函数）。
--
-- 两条约定，都是为了生成结果好用：
--  1. 参数一律用 sqlc.arg(名字)：否则生成的结构体里会出现 Column2/Column3 这种字段名；
--  2. 聚合结果显式加类型转换（::float8 / ::bigint）：不做的话 sqlc 只能推断出 interface{}，
--     调用方就得满屏类型断言。
--
-- 命名约定：<动作><对象>，动词开头便于一眼看出读还是写。

-- name: InsertMetricSample :one
INSERT INTO metric_samples (
    room_id, participants, tracks, packets, bytes, rtt_ms, loss_rate, out_kbps, in_kbps
) VALUES (
    sqlc.arg(room_id),
    sqlc.arg(participants),
    sqlc.arg(tracks),
    sqlc.arg(packets),
    sqlc.arg(bytes),
    sqlc.arg(rtt_ms),
    sqlc.arg(loss_rate),
    sqlc.arg(out_kbps),
    sqlc.arg(in_kbps)
)
RETURNING *;

-- name: ListRecentSamples :many
SELECT * FROM metric_samples
WHERE room_id = sqlc.arg(room_id)
ORDER BY sampled_at DESC
LIMIT sqlc.arg(row_limit);

-- name: RoomMetricsWindow :one
-- 一个时间窗内的房间级聚合：评测章节要的"平均在线人数/轨道数、平均 RTT 与丢包、累计流量"。
-- COALESCE 保证"窗口内没有样本"时返回 0 而不是 NULL，调用方不必处理空值。
SELECT
    COUNT(*)                                  AS sample_count,
    COALESCE(AVG(participants), 0)::float8    AS avg_participants,
    COALESCE(MAX(participants), 0)::bigint    AS peak_participants,
    COALESCE(AVG(tracks), 0)::float8          AS avg_tracks,
    COALESCE(MAX(tracks), 0)::bigint          AS peak_tracks,
    COALESCE(AVG(rtt_ms), 0)::float8          AS avg_rtt_ms,
    COALESCE(AVG(loss_rate), 0)::float8       AS avg_loss_rate,
    COALESCE(SUM(bytes), 0)::bigint           AS total_bytes,
    COALESCE(AVG(in_kbps), 0)::float8         AS avg_in_kbps,
    COALESCE(AVG(out_kbps), 0)::float8        AS avg_out_kbps
FROM metric_samples
WHERE room_id = sqlc.arg(room_id) AND sampled_at >= sqlc.arg(since);

-- name: GlobalMetricsWindow :one
-- 全服务器视角（跨房间）：支撑"单节点总容量"这类结论。
SELECT
    COUNT(*)                                AS sample_count,
    COALESCE(MAX(participants), 0)::bigint  AS peak_participants,
    COALESCE(MAX(tracks), 0)::bigint        AS peak_tracks,
    COALESCE(SUM(bytes), 0)::bigint         AS total_bytes
FROM metric_samples
WHERE sampled_at >= sqlc.arg(since);

-- name: ListSampleBuckets :many
-- 按时间桶聚合：把细粒度样本折叠成画图用的曲线。
--
-- 注意这里**不用** TimescaleDB 的 time_bucket()——那要装扩展，而我们的部署约束是
-- "一台普通 Postgres"。改成 epoch 取整，语义等价且零扩展依赖：
--   bucket = floor(epoch / 桶宽) * 桶宽
SELECT
    -- 末尾必须显式 ::timestamptz：不加的话 sqlc 推断不出类型，会生成 interface{}
    (to_timestamp(floor(extract(epoch FROM sampled_at) / sqlc.arg(bucket_seconds)::float8)
                  * sqlc.arg(bucket_seconds)::float8))::timestamptz AS bucket,
    AVG(participants)::float8                               AS avg_participants,
    AVG(tracks)::float8                                     AS avg_tracks,
    AVG(rtt_ms)::float8                                     AS avg_rtt_ms,
    AVG(loss_rate)::float8                                  AS avg_loss_rate,
    SUM(bytes)::bigint                                      AS total_bytes
FROM metric_samples
WHERE room_id = sqlc.arg(room_id) AND sampled_at >= sqlc.arg(since)
GROUP BY bucket
ORDER BY bucket ASC;

-- name: DeleteSamplesBefore :execrows
-- 保留策略：删除过期样本（评测跑完清理，避免表无限增长）。
DELETE FROM metric_samples WHERE sampled_at < sqlc.arg(before);
