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

const userCols = `id, username, display_name, password_hash, role, disabled, avatar_updated_at`

func scanUser(row pgx.Row) (*User, error) {
	var u User
	if err := row.Scan(&u.ID, &u.Username, &u.DisplayName, &u.PasswordHash, &u.Role, &u.Disabled, &u.AvatarUpdatedAt); err != nil {
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
