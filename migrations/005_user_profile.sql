-- 用户资料：头像与资料更新时间。
--
-- 为什么头像直接存进数据库（BYTEA）而不是文件系统：
--  1. 少一个要配置、要备份、要清理的目录，也不需要额外挂载卷；
--  2. 项目现在只有这一处二进制内容，放库里最省事。
-- 代价是库会变大——所以上传接口有 512KB 上限，且只接受 png/jpeg/webp。
-- 真要上规模再迁到对象存储，接口形状（GET /auth/avatar/:id）不用动。
ALTER TABLE users ADD COLUMN IF NOT EXISTS avatar_mime       TEXT;
ALTER TABLE users ADD COLUMN IF NOT EXISTS avatar_bytes      BYTEA;
ALTER TABLE users ADD COLUMN IF NOT EXISTS avatar_updated_at TIMESTAMPTZ;
