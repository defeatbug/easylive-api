-- 权限模型调整：全站**唯一管理员** + 每个会议各有自己的主持人。
--
-- 旧模型是全局的 host/member：谁的角色是 host，就在**所有**房间里都是主持人。
-- 新模型把"主持人"变成**会议内**的概念：谁创建会议，谁就是那个会议的主持人；
-- 全站只有一个管理员（admin），它在任何会议里都是主持人，并且能查看所有用户。

-- 1) 角色收敛为 admin / member（老数据里的 host 就是管理员）
UPDATE users SET role = 'admin'  WHERE role = 'host';
UPDATE users SET role = 'member' WHERE role IS NULL OR role NOT IN ('admin', 'member');
ALTER TABLE users ALTER COLUMN role SET DEFAULT 'member';

-- 2) 管理员**只能有一个**：由数据库唯一索引兜底，而不是靠应用层自觉。
--    部分唯一索引（WHERE 子句）在 PostgreSQL 里正好能表达"这一列只能有一行取某个值"。
CREATE UNIQUE INDEX IF NOT EXISTS idx_users_single_admin ON users (role) WHERE role = 'admin';

-- 3) 会议归属：owner_id 指向 users.id。
--    为什么用用户 ID 而不是用户名：用户名可以改，改完不该把会议的主持权弄丢。
ALTER TABLE rooms ADD COLUMN IF NOT EXISTS owner_id BIGINT;
CREATE INDEX IF NOT EXISTS idx_rooms_owner_id ON rooms (owner_id);
