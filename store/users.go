package store

import (
	"context"
	"errors"
	"fmt"

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

const userCols = `id, username, display_name, password_hash, role, disabled`

func scanUser(row pgx.Row) (*User, error) {
	var u User
	if err := row.Scan(&u.ID, &u.Username, &u.DisplayName, &u.PasswordHash, &u.Role, &u.Disabled); err != nil {
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
