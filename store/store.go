// Package store 是数据访问层。
//
// 这里采用 **Ent + sqlc 的混合方案**，边界是刻意划的（不是历史包袱）：
//
//   - **Ent 管业务实体**（rooms / meetings）：有实体关系、需要校验与级联，
//     写路径集中于此；将来若要 GraphQL 的自动分页/过滤，也能直接从 schema 受益。
//   - **sqlc 管指标时序**（metric_samples，下一步接入）：高写入 + 强聚合
//     （按时间桶 GROUP BY、百分位），用显式 SQL 表达最直接，也能避免 ORM 的
//     懒加载意外发生在采样协程里。
//   - 两者**共用同一个 pgxpool**：Ent 通过 stdlib.OpenDBFromPool 拿到 database/sql
//     视图，sqlc 直接用 pool。连接数不会翻倍，也不会出现"两个池各占一半"的坑。
//
// 一条铁律：**同一张表只有一个写者**。rooms/meetings 只由 Ent 写，
// metric_samples 只由 sqlc 写；读可以交叉，但写绝不交叉。
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"

	"easylive-api/ent"
	"easylive-api/ent/meeting"
	"easylive-api/ent/room"
	"easylive-api/store/sqlcgen"
)

// Store 持有共享连接池与两个访问层。
type Store struct {
	// Pool 交给 sqlc（它生成的 DBTX 接口由 *pgxpool.Pool 直接满足）
	Pool *pgxpool.Pool
	// Ent 是业务实体的访问层
	Ent *ent.Client
	// Q 是 sqlc 生成的查询集：指标时序的写入与聚合读全走它
	Q *sqlcgen.Queries

	sqlDB *sql.DB
}

// Open 建立连接池并装配两个访问层。
//
// 注意：数据库是**可选依赖**——媒体面与会话状态都在内存里（ADR D8），
// 连不上数据库时调用方（main）只告警并继续提供服务。
func Open(ctx context.Context, dsn string) (*Store, error) {
	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("解析数据库 DSN 失败: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("创建连接池失败: %w", err)
	}
	// 启动即探活：连不上就立刻失败，让 main 在启动阶段决定降级，
	// 而不是等第一个请求进来才发现数据库不可用
	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("数据库探活失败: %w", err)
	}

	// 关键：把**同一个 pool** 包成 database/sql 视图给 Ent，而不是再开一个池
	sqlDB := stdlib.OpenDBFromPool(pool)
	client := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, sqlDB)))

	// sqlc 直接用 pool（它生成的 DBTX 接口由 *pgxpool.Pool 满足），
	// 于是两套访问层共享同一个连接池：连接数不会翻倍
	return &Store{Pool: pool, Ent: client, Q: sqlcgen.New(pool), sqlDB: sqlDB}, nil
}

// Close 释放资源。顺序有讲究：先关 Ent 的 database/sql 视图，再关底层 pool，
// 避免"视图还在用、池已经关了"的竞态。
func (s *Store) Close() error {
	var firstErr error
	if s.Ent != nil {
		if err := s.Ent.Close(); err != nil {
			firstErr = err
		}
	}
	if s.Pool != nil {
		s.Pool.Close()
	}
	return firstErr
}

// AutoMigrate 按 Ent schema 建表/加列。
//
// 仅用于**本地开发与测试**：生产以 migrations/*.sql 为准（sqlc 也读它，
// 因此必须有一份稳定的 DDL 事实源，不能由运行时隐式变更）。
func (s *Store) AutoMigrate(ctx context.Context) error {
	return s.Ent.Schema.Create(ctx)
}

// ---------- 房间（Ent 写路径）----------

// UpsertRoom 按房间号插入或更新标题/创建者。
//
// 用 OnConflict 而不是"先查再写"：并发创建同一房间号时前者是原子的，
// 后者会有竞态（两个请求都查不到 → 都插入 → 一个报唯一键冲突）。
func (s *Store) UpsertRoom(ctx context.Context, id, title, createdBy string) (*ent.Room, error) {
	err := s.Ent.Room.Create().
		SetID(id).
		SetTitle(title).
		SetCreatedBy(createdBy).
		OnConflict(
			entsql.ConflictColumns(room.FieldID),
			entsql.ResolveWithNewValues(),
			// created_at 是 Immutable 的：冲突时保持首次创建时间，不要被覆盖
			entsql.ResolveWith(func(u *entsql.UpdateSet) {
				u.SetIgnore(room.FieldCreatedAt)
			}),
		).
		Exec(ctx)
	if err != nil {
		return nil, fmt.Errorf("写入房间失败: %w", err)
	}
	return s.Ent.Room.Get(ctx, id)
}

// GetRoom 按房间号查询；不存在时返回 ent.NotFoundError。
func (s *Store) GetRoom(ctx context.Context, id string) (*ent.Room, error) {
	return s.Ent.Room.Get(ctx, id)
}

// ListRooms 按创建时间倒序列出房间。
func (s *Store) ListRooms(ctx context.Context, limit, offset int) ([]*ent.Room, error) {
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	return s.Ent.Room.Query().
		Order(ent.Desc(room.FieldCreatedAt)).
		Limit(limit).
		Offset(offset).
		All(ctx)
}

// CloseRoom 标记房间关闭，并把该房间**尚未结束**的会议记录一并收尾。
func (s *Store) CloseRoom(ctx context.Context, id string) (*ent.Room, error) {
	now := time.Now()
	if err := s.Ent.Room.UpdateOneID(id).SetClosedAt(now).Exec(ctx); err != nil {
		return nil, fmt.Errorf("关闭房间失败: %w", err)
	}
	if _, err := s.EndOpenMeetings(ctx, id); err != nil {
		return nil, err
	}
	return s.Ent.Room.Get(ctx, id)
}

// ---------- 供采样器使用的薄包装 ----------
//
// 这三个方法只是把 Ent 的返回值丢掉。存在的意义是**让 recorder 的 Store 接口
// 不出现 ent 类型**，从而可以用一个内存假实现给采样逻辑写单测（本机没有 Postgres）。

// EnsureRoom 保证房间行存在（房间号是业务主键，重复调用是幂等的）。
func (s *Store) EnsureRoom(ctx context.Context, roomID, createdBy string) error {
	_, err := s.UpsertRoom(ctx, roomID, "", createdBy)
	return err
}

// RecordMeetingStart 开始一条会议记录。
func (s *Store) RecordMeetingStart(ctx context.Context, roomID, meetingID string) error {
	_, err := s.StartMeeting(ctx, roomID, meetingID)
	return err
}

// RecordMeetingEnd 收尾会议记录（写结束时间、时长与两个峰值）。
func (s *Store) RecordMeetingEnd(ctx context.Context, meetingID string, peakParticipants, peakTracks int) error {
	_, err := s.EndMeeting(ctx, meetingID, peakParticipants, peakTracks)
	return err
}

// ---------- 会议记录（Ent 写路径）----------

// StartMeeting 建一条会议记录。
func (s *Store) StartMeeting(ctx context.Context, roomID, meetingID string) (*ent.Meeting, error) {
	m, err := s.Ent.Meeting.Create().
		SetID(meetingID).
		SetRoomID(roomID).
		Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("创建会议记录失败: %w", err)
	}
	return m, nil
}

// EndMeeting 收尾会议记录：写结束时间、时长与两个峰值。
func (s *Store) EndMeeting(ctx context.Context, meetingID string, peakParticipants, peakTracks int) (*ent.Meeting, error) {
	m, err := s.Ent.Meeting.Get(ctx, meetingID)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	// 时长由服务端计算，避免依赖数据库时区设置
	duration := int(now.Sub(m.StartedAt).Seconds())
	if duration < 0 {
		duration = 0
	}
	return s.Ent.Meeting.UpdateOneID(meetingID).
		SetEndedAt(now).
		SetDurationSec(duration).
		SetPeakParticipants(peakParticipants).
		SetPeakTracks(peakTracks).
		Save(ctx)
}

// maxMeetingField 求某个整型字段在房间内的最大值。
//
// 一次只查一个聚合是刻意的：多个 MAX 会撞名（见 RoomMeetingStats 里的说明）。
func (s *Store) maxMeetingField(ctx context.Context, roomID, field string) (int64, error) {
	var rows []struct {
		Max int64 `json:"max"`
	}
	if err := s.Ent.Meeting.Query().
		Where(meeting.RoomID(roomID)).
		Aggregate(ent.Max(field)).
		Scan(ctx, &rows); err != nil {
		return 0, fmt.Errorf("聚合 %s 失败: %w", field, err)
	}
	if len(rows) == 0 {
		return 0, nil
	}
	return rows[0].Max, nil
}

// EndOpenMeetings 把所有"还没写结束时间"的会议记录收尾（用于房间关闭/服务重启后的清理）。
func (s *Store) EndOpenMeetings(ctx context.Context, roomID string) (int, error) {
	now := time.Now()
	n, err := s.Ent.Meeting.Update().
		Where(meeting.RoomID(roomID), meeting.EndedAtIsNil()).
		SetEndedAt(now).
		Save(ctx)
	if err != nil {
		return 0, fmt.Errorf("收尾未结束的会议记录失败: %w", err)
	}
	return n, nil
}

// ListMeetings 列出某房间的会议记录（最近的在前）。
func (s *Store) ListMeetings(ctx context.Context, roomID string, limit int) ([]*ent.Meeting, error) {
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	return s.Ent.Meeting.Query().
		Where(meeting.RoomID(roomID)).
		Order(ent.Desc(meeting.FieldStartedAt)).
		Limit(limit).
		All(ctx)
}

// RoomMeetingStats 是房间级会议统计（用 Ent 的聚合能力，不必写 SQL）。
type RoomMeetingStats struct {
	Meetings         int   `json:"meetings"`
	TotalDurationSec int64 `json:"totalDurationSec"`
	PeakParticipants int64 `json:"peakParticipants"`
	PeakTracks       int64 `json:"peakTracks"`
}

// RoomMeetingStats 聚合某房间的历史会议数据。
func (s *Store) RoomMeetingStats(ctx context.Context, roomID string) (*RoomMeetingStats, error) {
	// 这里曾经把 count / sum / 两个 max 放在**一次**聚合查询里，结果生产环境上
	// "峰值轨道数"永远是 0：Ent 为每个聚合生成的 SQL 别名都取自函数名，
	// 两个 MAX 于是都映射到同一个键（"max"）上互相覆盖，扫描时后者覆盖前者、
	// 而 max_tracks 这个键根本不存在。同一份内存计数写进 metric_samples 是 5、
	// 写进 meetings 读出来却是 0 —— 两个数据源对不上才暴露出来。
	//
	// 现在只把别名天然不同的 count / sum 放在一起，两个 MAX 各自单独查：
	// 多一次往返，但结果不依赖生成 SQL 的别名规则。
	out := &RoomMeetingStats{}

	var basic []struct {
		Count int   `json:"count"`
		Sum   int64 `json:"sum"`
	}
	if err := s.Ent.Meeting.Query().
		Where(meeting.RoomID(roomID)).
		Aggregate(ent.Count(), ent.Sum(meeting.FieldDurationSec)).
		Scan(ctx, &basic); err != nil {
		return nil, fmt.Errorf("聚合会议统计失败: %w", err)
	}
	if len(basic) > 0 {
		out.Meetings = basic[0].Count
		out.TotalDurationSec = basic[0].Sum
	}

	peakParts, err := s.maxMeetingField(ctx, roomID, meeting.FieldPeakParticipants)
	if err != nil {
		return nil, err
	}
	peakTracks, err := s.maxMeetingField(ctx, roomID, meeting.FieldPeakTracks)
	if err != nil {
		return nil, err
	}
	out.PeakParticipants = peakParts
	out.PeakTracks = peakTracks
	return out, nil
}

// ---------- 指标时序（sqlc 写路径 + 聚合读）----------
//
// 上层（GraphQL resolver、后台采样协程）只看到本文件里的普通 Go 类型，
// 不直接依赖 sqlc 生成的结构体：这样以后加字段/换查询不必改动调用方。

// MetricSample 是一条待写入的指标样本。
type MetricSample struct {
	RoomID       string
	Participants int
	Tracks       int
	Packets      int64
	Bytes        int64
	RTTMs        int
	LossRate     float64
	OutKbps      int
	InKbps       int
}

// RecordMetricSample 写入一条指标样本（sqlc 路径）。
//
// 这个时间序列表只有 sqlc 写、Ent 不碰——"一张表只有一个写者"是混合方案的铁律。
func (s *Store) RecordMetricSample(ctx context.Context, m MetricSample) error {
	_, err := s.Q.InsertMetricSample(ctx, sqlcgen.InsertMetricSampleParams{
		RoomID:       m.RoomID,
		Participants: int32(m.Participants),
		Tracks:       int32(m.Tracks),
		Packets:      m.Packets,
		Bytes:        m.Bytes,
		RttMs:        int32(m.RTTMs),
		LossRate:     float32(m.LossRate),
		OutKbps:      int32(m.OutKbps),
		InKbps:       int32(m.InKbps),
	})
	if err != nil {
		return fmt.Errorf("写入指标样本失败: %w", err)
	}
	return nil
}

// RoomMetrics 是某房间在时间窗内的聚合结果（评测章节直接可用）。
type RoomMetrics struct {
	SampleCount      int64   `json:"sampleCount"`
	AvgParticipants  float64 `json:"avgParticipants"`
	PeakParticipants int64   `json:"peakParticipants"`
	AvgTracks        float64 `json:"avgTracks"`
	PeakTracks       int64   `json:"peakTracks"`
	AvgRTTMs         float64 `json:"avgRttMs"`
	AvgLossRate      float64 `json:"avgLossRate"`
	TotalBytes       int64   `json:"totalBytes"`
	AvgInKbps        float64 `json:"avgInKbps"`
	AvgOutKbps       float64 `json:"avgOutKbps"`
}

// RoomMetrics 聚合某房间自 since 起的指标。
func (s *Store) RoomMetrics(ctx context.Context, roomID string, since time.Time) (*RoomMetrics, error) {
	row, err := s.Q.RoomMetricsWindow(ctx, sqlcgen.RoomMetricsWindowParams{
		RoomID: roomID,
		Since:  pgtype.Timestamptz{Time: since, Valid: true},
	})
	if err != nil {
		return nil, fmt.Errorf("聚合房间指标失败: %w", err)
	}
	return &RoomMetrics{
		SampleCount:      row.SampleCount,
		AvgParticipants:  row.AvgParticipants,
		PeakParticipants: row.PeakParticipants,
		AvgTracks:        row.AvgTracks,
		PeakTracks:       row.PeakTracks,
		AvgRTTMs:         row.AvgRttMs,
		AvgLossRate:      row.AvgLossRate,
		TotalBytes:       row.TotalBytes,
		AvgInKbps:        row.AvgInKbps,
		AvgOutKbps:       row.AvgOutKbps,
	}, nil
}

// SampleBucket 是折叠后的一个时间桶（画曲线用）。
type SampleBucket struct {
	Bucket          time.Time `json:"bucket"`
	AvgParticipants float64   `json:"avgParticipants"`
	AvgTracks       float64   `json:"avgTracks"`
	AvgRTTMs        float64   `json:"avgRttMs"`
	AvgLossRate     float64   `json:"avgLossRate"`
	TotalBytes      int64     `json:"totalBytes"`
}

// SampleBuckets 按固定时间桶聚合某房间的指标曲线。
func (s *Store) SampleBuckets(ctx context.Context, roomID string, bucket time.Duration, since time.Time) ([]SampleBucket, error) {
	if bucket <= 0 {
		bucket = time.Minute
	}
	rows, err := s.Q.ListSampleBuckets(ctx, sqlcgen.ListSampleBucketsParams{
		RoomID:        roomID,
		BucketSeconds: bucket.Seconds(),
		Since:         pgtype.Timestamptz{Time: since, Valid: true},
	})
	if err != nil {
		return nil, fmt.Errorf("聚合指标曲线失败: %w", err)
	}
	out := make([]SampleBucket, 0, len(rows))
	for _, r := range rows {
		item := SampleBucket{
			AvgParticipants: r.AvgParticipants,
			AvgTracks:       r.AvgTracks,
			AvgRTTMs:        r.AvgRttMs,
			AvgLossRate:     r.AvgLossRate,
			TotalBytes:      r.TotalBytes,
		}
		if r.Bucket.Valid {
			item.Bucket = r.Bucket.Time
		}
		out = append(out, item)
	}
	return out, nil
}

// GlobalMetrics 返回全服务器视角的聚合（跨房间），用于"单节点总容量"结论。
type GlobalMetrics struct {
	SampleCount      int64 `json:"sampleCount"`
	PeakParticipants int64 `json:"peakParticipants"`
	PeakTracks       int64 `json:"peakTracks"`
	TotalBytes       int64 `json:"totalBytes"`
}

// GlobalMetrics 聚合自 since 起的全部指标样本。
func (s *Store) GlobalMetrics(ctx context.Context, since time.Time) (*GlobalMetrics, error) {
	row, err := s.Q.GlobalMetricsWindow(ctx, pgtype.Timestamptz{Time: since, Valid: true})
	if err != nil {
		return nil, fmt.Errorf("聚合全局指标失败: %w", err)
	}
	return &GlobalMetrics{
		SampleCount:      row.SampleCount,
		PeakParticipants: row.PeakParticipants,
		PeakTracks:       row.PeakTracks,
		TotalBytes:       row.TotalBytes,
	}, nil
}

// PruneMetrics 删除 before 之前的样本，返回删除行数（保留策略）。
func (s *Store) PruneMetrics(ctx context.Context, before time.Time) (int64, error) {
	n, err := s.Q.DeleteSamplesBefore(ctx, pgtype.Timestamptz{Time: before, Valid: true})
	if err != nil {
		return 0, fmt.Errorf("清理过期指标失败: %w", err)
	}
	return n, nil
}
