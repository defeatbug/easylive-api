-- 会话版本号：用来实现"同一账号只允许一处在线"。
--
-- 为什么需要它：会话是**签名 Cookie**（无服务端会话表），所以服务端默认无法吊销某个会话。
-- 加一个递增的版本号并把版本写进 Cookie，就能做到"再登录一次，之前的会话全部失效"：
--   登录/注册成功 → session_version += 1 → 旧 Cookie 里的版本对不上 → 全部作废。
-- 这是"单点登录"最省事的实现，也不需要给会话建表。
ALTER TABLE users ADD COLUMN IF NOT EXISTS session_version BIGINT NOT NULL DEFAULT 1;
