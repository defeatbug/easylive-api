-- 初始表结构（最小）：只保存房间元数据。
-- 媒体与会话状态驻留进程内存，不入库（ADR D8）。
CREATE TABLE IF NOT EXISTS rooms (
    id         TEXT PRIMARY KEY,
    title      TEXT        NOT NULL DEFAULT '',
    created_by TEXT        NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_rooms_created_at ON rooms (created_at DESC);
