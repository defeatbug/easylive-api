-- 002：把 rooms 扩成完整业务实体，并加入会议记录表（Ent schema 的 SQL 映射）。
--
-- 为什么用 ALTER 而不是重写 001：演示环境/已部署的库里已经有 001 建出的 rooms 表，
-- 重写会丢数据；ADD COLUMN IF NOT EXISTS 对"已有库"和"全新库"都安全。
--
-- ⚠️ 本文件既是 sqlc 的 schema 输入，也是生产的权威 DDL。
--    Ent schema（ent/schema/*.go）必须与它一致，由 store/schema_drift_test.go 自动校验。
--    在有可用 Postgres 的机器上，更推荐用 `atlas migrate diff` 从 Ent schema 重新生成。

ALTER TABLE rooms ADD COLUMN IF NOT EXISTS closed_at TIMESTAMPTZ;

CREATE TABLE IF NOT EXISTS meetings (
    id                TEXT        PRIMARY KEY,
    started_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    ended_at          TIMESTAMPTZ,
    duration_sec      BIGINT      NOT NULL DEFAULT 0,
    peak_participants BIGINT      NOT NULL DEFAULT 0,
    peak_tracks       BIGINT      NOT NULL DEFAULT 0,
    room_id           TEXT        NOT NULL REFERENCES rooms (id) ON DELETE CASCADE
);

-- 评测与历史查询都是"某房间最近的会议"，按 (room_id, started_at DESC) 建联合索引
CREATE INDEX IF NOT EXISTS idx_meetings_room_id_started_at ON meetings (room_id, started_at DESC);
