-- 用户表：登录系统的唯一事实源（与 rooms/meetings 一样，DDL 只写在这里）。
--
-- 为什么单独一张表而不是复用别的东西：登录要能回答"这个用户名对应的口令哈希是什么、
-- 这个人是不是主持人"。把它塞进配置或内存，就没法加第二个用户、也没法撤权。
--
-- 设计要点：
--  1. 口令**只存哈希**（argon2id，见 auth/password.go），任何日志、接口、快照都不出现明文；
--  2. username 用 lower() 建唯一索引：登录不该区分大小写，否则 Alice/alice 会变成两个人；
--  3. role 只有 host/member 两种：主持人权限由**服务端**据此判定，
--     而不是让前端在申请令牌时自报（那等于谁都能当主持人）。
CREATE TABLE IF NOT EXISTS users (
    id            BIGSERIAL   PRIMARY KEY,
    username      TEXT        NOT NULL,
    display_name  TEXT        NOT NULL DEFAULT '',
    password_hash TEXT        NOT NULL,
    role          TEXT        NOT NULL DEFAULT 'member',
    disabled      BOOLEAN     NOT NULL DEFAULT false,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_users_username_lower ON users (lower(username));
