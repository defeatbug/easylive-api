package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"easylive-api/auth"
)

// User 是登录用户。口令哈希永远不出现在日志、接口响应与快照里。
type User struct {
	ID           int64
	Username     string
	DisplayName  string
	PasswordHash string
	Role         string // host | member
	Disabled     bool
	// AvatarUpdatedAt 非空表示该用户设置过头像。
	// 注意**不把头像字节放进这个结构**：User 会在每次请求里被读取（角色要实时），
	// 顺手带上几百 KB 的图片会让每个请求都白搬一次数据。字节只在头像路由里取。
	AvatarUpdatedAt *time.Time
	// SessionVersion 每次登录/注册自增：旧会话 Cookie 里带的版本会对不上而失效。
	SessionVersion int64
}

// 为什么 users 表走 pgx 原生查询而不是 Ent/sqlc：
// 它是登录路径的**唯一**一张表，查询只有三条（按名取、按 ID 取、插入），
// 为此跑一轮代码生成不值得；而项目真正的铁律是"同一张表只有一个写者"，
// 这一点在这里被严格遵守：users 表的写入只发生在本文件。
// 将来若要扩展（改密、禁用、审计），再迁移到 Ent 也不影响调用方。

// hashForStore 包一层 auth.HashPassword：store 只负责落库，哈希算法归 auth 管。
func hashForStore(password string) (string, error) { return auth.HashPassword(password) }

// ErrUserNotFound 表示用户名不存在。
var ErrUserNotFound = errors.New("用户不存在")

const userCols = `id, username, display_name, password_hash, role, disabled, avatar_updated_at, session_version`

func scanUser(row pgx.Row) (*User, error) {
	var u User
	if err := row.Scan(&u.ID, &u.Username, &u.DisplayName, &u.PasswordHash, &u.Role, &u.Disabled, &u.AvatarUpdatedAt, &u.SessionVersion); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrUserNotFound
		}
		return nil, err
	}
	return &u, nil
}

// UserByUsername 按用户名查找（**不区分大小写**，与唯一索引一致）。
func (s *Store) UserByUsername(ctx context.Context, username string) (*User, error) {
	if s.Pool == nil {
		return nil, errors.New("数据层不可用")
	}
	return scanUser(s.Pool.QueryRow(ctx,
		`SELECT `+userCols+` FROM users WHERE lower(username) = lower($1)`, username))
}

// UserByID 按 ID 查找：每次请求都用它重新读角色，
// 这样改一次用户角色立刻生效，而不是等 Cookie 过期。
func (s *Store) UserByID(ctx context.Context, id int64) (*User, error) {
	if s.Pool == nil {
		return nil, errors.New("数据层不可用")
	}
	return scanUser(s.Pool.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id = $1`, id))
}

// CreateUser 建用户。口令**必须在调用前完成哈希**（本函数不接收明文）。
func (s *Store) CreateUser(ctx context.Context, username, displayName, passwordHash, role string) (int64, error) {
	if s.Pool == nil {
		return 0, errors.New("数据层不可用")
	}
	var id int64
	err := s.Pool.QueryRow(ctx,
		`INSERT INTO users (username, display_name, password_hash, role)
		 VALUES ($1, $2, $3, $4) RETURNING id`,
		username, displayName, passwordHash, role).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("创建用户失败: %w", err)
	}
	return id, nil
}

// EnsureUser 幂等地保证某个用户存在（用于启动时引导管理员账号）。
//
// 行为：用户已存在 → 什么都不做（**不覆盖口令**，避免重启把人工改过的密码打回去）；
// 不存在 → 创建。返回是否新建。
func (s *Store) EnsureUser(ctx context.Context, username, displayName, password, role string) (bool, error) {
	if _, err := s.UserByUsername(ctx, username); err == nil {
		return false, nil
	} else if !errors.Is(err, ErrUserNotFound) {
		return false, err
	}
	hash, err := hashForStore(password)
	if err != nil {
		return false, err
	}
	if _, err := s.CreateUser(ctx, username, displayName, hash, role); err != nil {
		return false, err
	}
	return true, nil
}

// UpdateProfile 修改用户名与显示名。
//
// 用户名是**登录身份**，改它必须同时校验口令（由调用方在路由层做），
// 否则一个被劫持的会话就能把账号"改名带走"。
func (s *Store) UpdateProfile(ctx context.Context, id int64, username, displayName string) error {
	if s.Pool == nil {
		return errors.New("数据层不可用")
	}
	_, err := s.Pool.Exec(ctx,
		`UPDATE users SET username = $2, display_name = $3, updated_at = now() WHERE id = $1`,
		id, username, displayName)
	return err
}

// UsernameTaken 判断用户名是否已被**别人**占用（不区分大小写，与唯一索引一致）。
func (s *Store) UsernameTaken(ctx context.Context, username string, exceptID int64) (bool, error) {
	if s.Pool == nil {
		return false, errors.New("数据层不可用")
	}
	var exists bool
	err := s.Pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM users WHERE lower(username) = lower($1) AND id <> $2)`,
		username, exceptID).Scan(&exists)
	return exists, err
}

// UpdatePassword 写入新的口令哈希（明文哈希在调用方完成）。
func (s *Store) UpdatePassword(ctx context.Context, id int64, passwordHash string) error {
	if s.Pool == nil {
		return errors.New("数据层不可用")
	}
	_, err := s.Pool.Exec(ctx,
		`UPDATE users SET password_hash = $2, updated_at = now() WHERE id = $1`, id, passwordHash)
	return err
}

// SetAvatar 保存头像字节（mime 已由路由层白名单校验、大小已限制）。
func (s *Store) SetAvatar(ctx context.Context, id int64, mime string, data []byte) error {
	if s.Pool == nil {
		return errors.New("数据层不可用")
	}
	_, err := s.Pool.Exec(ctx,
		`UPDATE users SET avatar_mime = $2, avatar_bytes = $3, avatar_updated_at = now(), updated_at = now()
		 WHERE id = $1`, id, mime, data)
	return err
}

// ClearAvatar 恢复默认头像。
func (s *Store) ClearAvatar(ctx context.Context, id int64) error {
	if s.Pool == nil {
		return errors.New("数据层不可用")
	}
	_, err := s.Pool.Exec(ctx,
		`UPDATE users SET avatar_mime = NULL, avatar_bytes = NULL, avatar_updated_at = NULL, updated_at = now()
		 WHERE id = $1`, id)
	return err
}

// Avatar 读取头像字节。未设置头像时返回 ErrUserNotFound（路由层据此返回 404）。
func (s *Store) Avatar(ctx context.Context, id int64) (string, []byte, time.Time, error) {
	if s.Pool == nil {
		return "", nil, time.Time{}, errors.New("数据层不可用")
	}
	var mime *string
	var data []byte
	var at *time.Time
	err := s.Pool.QueryRow(ctx,
		`SELECT avatar_mime, avatar_bytes, avatar_updated_at FROM users WHERE id = $1`, id).
		Scan(&mime, &data, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, time.Time{}, ErrUserNotFound
	}
	if err != nil {
		return "", nil, time.Time{}, err
	}
	if mime == nil || len(data) == 0 || at == nil {
		return "", nil, time.Time{}, ErrUserNotFound
	}
	return *mime, data, *at, nil
}

// BumpSessionVersion 把会话版本 +1 并返回新值。
//
// 每次登录/注册都调用它：之前签发的所有会话 Cookie 立刻失效，
// 从而实现"同一账号只允许一处在线"。用数据库自增而不是应用内存计数，
// 是为了多实例部署时也成立。
func (s *Store) BumpSessionVersion(ctx context.Context, id int64) (int64, error) {
	if s.Pool == nil {
		return 0, errors.New("数据层不可用")
	}
	var version int64
	err := s.Pool.QueryRow(ctx,
		`UPDATE users SET session_version = session_version + 1, updated_at = now()
		 WHERE id = $1 RETURNING session_version`, id).Scan(&version)
	if err != nil {
		return 0, fmt.Errorf("更新会话版本失败: %w", err)
	}
	return version, nil
}

// ListUsers 分页列出用户（管理员查看用）。**绝不返回口令哈希之外的敏感内容**——
// 这里返回的 password_hash 字段由调用方负责不放进响应（路由层用 userJSON 序列化）。
func (s *Store) ListUsers(ctx context.Context, limit, offset int) ([]User, error) {
	if s.Pool == nil {
		return nil, errors.New("数据层不可用")
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.Pool.Query(ctx,
		`SELECT `+userCols+` FROM users ORDER BY id ASC LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]User, 0, limit)
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

// CountUsers 返回用户总数（分页需要）。
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	if s.Pool == nil {
		return 0, errors.New("数据层不可用")
	}
	var n int
	err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n)
	return n, err
}
