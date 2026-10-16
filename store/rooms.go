package store

import (
	"context"
	"errors"
	"fmt"

	"easylive-api/ent"
	"easylive-api/ent/room"
)

// ErrRoomExists 表示房间号已被占用（生成编码时撞了）。
var ErrRoomExists = errors.New("房间号已存在")

// CreateRoom 创建会议并记录归属用户。
//
// 与 UpsertRoom 的关键区别：**冲突不覆盖**。
// UpsertRoom 用于"进房即建记录"的路径（同一个房间号重复进来是正常的），
// 而这里用于"用户主动建会"：房间号是服务端随机生成的，一旦撞码就换个码重试，
// 绝不能把别人已有的会议覆盖掉——那等于连主持人一起换掉。
func (s *Store) CreateRoom(ctx context.Context, id, title, createdBy string, ownerID int64) error {
	if s.Ent == nil {
		return errors.New("数据层不可用")
	}
	exists, err := s.Ent.Room.Query().Where(room.ID(id)).Exist(ctx)
	if err != nil {
		return fmt.Errorf("检查房间号失败: %w", err)
	}
	if exists {
		return ErrRoomExists
	}
	_, err = s.Ent.Room.Create().
		SetID(id).
		SetTitle(title).
		SetCreatedBy(createdBy).
		SetOwnerID(ownerID).
		Save(ctx)
	if err != nil {
		// 并发下仍可能撞唯一键：把它归一到 ErrRoomExists，让调用方换码重试
		if ent.IsConstraintError(err) {
			return ErrRoomExists
		}
		return fmt.Errorf("创建会议失败: %w", err)
	}
	return nil
}

// RoomOwner 返回房间的归属用户 ID；0 表示没有归属（演示模式创建的，或旧数据）。
//
// 不存在房间时返回 ent.NotFoundError——调用方据此判断"这个会议号不存在"。
func (s *Store) RoomOwner(ctx context.Context, id string) (int64, error) {
	r, err := s.Ent.Room.Get(ctx, id)
	if err != nil {
		return 0, err
	}
	if r.OwnerID == nil {
		return 0, nil
	}
	return *r.OwnerID, nil
}

// RoomsByOwner 列出某人创建的会议（新的在前）。
func (s *Store) RoomsByOwner(ctx context.Context, ownerID int64, limit int) ([]*ent.Room, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	return s.Ent.Room.Query().
		Where(room.OwnerID(ownerID)).
		Order(ent.Desc(room.FieldCreatedAt)).
		Limit(limit).
		All(ctx)
}
