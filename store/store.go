// Package store 是业务数据访问层（最小实现）。
//
// 说明：本项目的媒体面与会话状态全部驻留进程内存（ADR D8），数据库只保存
// 房间元数据这类"可以丢"的数据，因此数据库不可用时服务仍可正常开会。
package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	Pool *pgxpool.Pool
}

// Open 创建连接池并做一次 Ping 探活。
func Open(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("解析数据库配置失败: %w", err)
	}
	cfg.MaxConns = 10
	cfg.MinConns = 1
	cfg.MaxConnLifetime = time.Hour

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("创建连接池失败: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("数据库探活失败: %w", err)
	}
	return &Store{Pool: pool}, nil
}

func (s *Store) Close() {
	if s != nil && s.Pool != nil {
		s.Pool.Close()
	}
}

// UpsertRoom 记录房间元数据（房间创建时调用）。
func (s *Store) UpsertRoom(ctx context.Context, roomID, title string) error {
	if s == nil || s.Pool == nil {
		return nil
	}
	_, err := s.Pool.Exec(ctx,
		`INSERT INTO rooms (id, title) VALUES ($1, $2)
		 ON CONFLICT (id) DO UPDATE SET title = EXCLUDED.title, updated_at = now()`,
		roomID, title)
	return err
}

// ListRooms 返回最近的房间列表（GraphQL 的第二个接口会用）。
func (s *Store) ListRooms(ctx context.Context, limit int) ([]RoomRow, error) {
	if s == nil || s.Pool == nil {
		return nil, nil
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.Pool.Query(ctx,
		`SELECT id, title, created_at FROM rooms ORDER BY created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []RoomRow
	for rows.Next() {
		var r RoomRow
		if err := rows.Scan(&r.ID, &r.Title, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

type RoomRow struct {
	ID        string
	Title     string
	CreatedAt time.Time
}
