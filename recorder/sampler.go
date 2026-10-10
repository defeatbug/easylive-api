// Package recorder 把运行时的房间状态按周期落库，供历史查询与评测章节使用。
//
// 一条底线：**数据库不出现在媒体热路径上**（ADR D8）。因此这里用"轮询内存快照"
// 而不是在转发链路上埋点；任何一次写库失败只记日志、下个周期继续——
// 宁可少几条样本，也绝不能让写库阻塞转发。
//
// 落库分工沿用数据层的边界：
//   - 房间与会议记录 → Ent（业务实体）
//   - 指标样本       → sqlc（高写入 + 后续强聚合）
package recorder

import (
	"context"
	"log"
	"sync"
	"time"

	"easylive-api/sfu"
	"easylive-api/store"
)

// Store 是采样器需要的持久化能力。
//
// 用接口而不是直接依赖 *store.Store：采样逻辑（峰值统计、会议开始/收尾、空置宽限、
// 失败重试）可以完全用内存假实现做单元测试——本机没有 Postgres，这一点很重要。
type Store interface {
	EnsureRoom(ctx context.Context, roomID, createdBy string) error
	RecordMeetingStart(ctx context.Context, roomID, meetingID string) error
	RecordMeetingEnd(ctx context.Context, meetingID string, peakParticipants, peakTracks int) error
	RecordMetricSample(ctx context.Context, m store.MetricSample) error
	PruneMetrics(ctx context.Context, before time.Time) (int64, error)
}

// Rooms 提供房间快照（由 *sfu.Manager 实现）。
type Rooms interface {
	RoomSnapshots() []sfu.RoomSnapshot
}

// Config 是采样器配置。
type Config struct {
	// Interval 采样周期（默认 5s：足够画出曲线，又不会把库写爆）
	Interval time.Duration
	// Retention 指标保留时长；<=0 表示不清理
	Retention time.Duration
	// EmptyGrace 房间消失后多久才收尾会议记录。
	// 存在的意义：断线重连、最后一个人短暂离开都可能让房间瞬间空掉，
	// 没有宽限期会把一次会议切成好几条记录。
	EmptyGrace time.Duration
	// Timeout 单次写库超时（写库慢不能拖住采样循环）
	Timeout time.Duration
	// Clock 便于测试注入时间
	Clock func() time.Time
}

func (c Config) withDefaults() Config {
	if c.Interval <= 0 {
		c.Interval = 5 * time.Second
	}
	if c.EmptyGrace <= 0 {
		c.EmptyGrace = 2 * c.Interval
	}
	if c.Timeout <= 0 {
		c.Timeout = 3 * time.Second
	}
	if c.Clock == nil {
		c.Clock = time.Now
	}
	return c
}

// meeting 是一次进行中的会议（内存态，落库在开始与结束时各一次）。
type meeting struct {
	id               string
	startedAt        time.Time
	peakParticipants int
	peakTracks       int
	// missingSince 记录"房间从快照里消失"的时刻，用于空置宽限
	missingSince time.Time
	// persisted 表示"开始记录"是否已经写库成功。
	//
	// 为什么要这个标记：如果第一次写库失败（数据库抖动），下个周期房间已经在 active 里，
	// 只按 exists 判断就会永远跳过 RecordMeetingStart——结果这场会议在库里根本不存在，
	// 而进程日志看起来一切正常。这是"失败后必须能自愈"的典型场景。
	persisted bool
}

// Sampler 周期性地把房间状态落库。
type Sampler struct {
	store Store
	rooms Rooms
	cfg   Config

	mu        sync.Mutex
	active    map[string]*meeting
	lastPrune time.Time
	// failures 用于给写库失败做日志限流（数据库长时间不可用时不要刷屏）
	failures int64
}

func New(st Store, rooms Rooms, cfg Config) *Sampler {
	return &Sampler{store: st, rooms: rooms, cfg: cfg.withDefaults(), active: map[string]*meeting{}}
}

// Run 阻塞运行采样循环，直到 ctx 取消；退出前会收尾所有进行中的会议记录。
func (s *Sampler) Run(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.Interval)
	defer ticker.Stop()
	log.Printf("recorder: 指标采样已启动（周期 %s，保留 %s，空置宽限 %s）",
		s.cfg.Interval, s.cfg.Retention, s.cfg.EmptyGrace)

	for {
		select {
		case <-ctx.Done():
			// 用 WithoutCancel：此时 ctx 已经取消，但收尾写入仍要能发出，
			// 否则重启后会留下一批 ended_at 为空的会议记录
			s.shutdown(context.WithoutCancel(ctx))
			return
		case <-ticker.C:
			s.Tick(ctx)
		}
	}
}

// ActiveMeetings 返回进行中的会议数（观测与测试用）。
func (s *Sampler) ActiveMeetings() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.active)
}

// Tick 执行一次采样。单独暴露出来是为了让单元测试直接驱动它，不必等真实时钟。
func (s *Sampler) Tick(ctx context.Context) {
	now := s.cfg.Clock()
	snaps := s.rooms.RoomSnapshots()

	seen := make(map[string]bool, len(snaps))
	for _, snap := range snaps {
		seen[snap.ID] = true

		s.mu.Lock()
		m, exists := s.active[snap.ID]
		if !exists {
			m = &meeting{id: meetingID(snap.ID, now), startedAt: now}
			s.active[snap.ID] = m
		}
		m.missingSince = time.Time{}
		if snap.Participants > m.peakParticipants {
			m.peakParticipants = snap.Participants
		}
		if snap.Tracks > m.peakTracks {
			m.peakTracks = snap.Tracks
		}
		mid := m.id
		needPersist := !m.persisted
		s.mu.Unlock()

		if needPersist {
			if err := s.call(ctx, "确保房间存在", func(c context.Context) error {
				return s.store.EnsureRoom(c, snap.ID, "")
			}); err != nil {
				continue // 房间行没写成功就先不记会议，下个周期继续重试
			}
			if err := s.call(ctx, "创建会议记录", func(c context.Context) error {
				return s.store.RecordMeetingStart(c, snap.ID, mid)
			}); err != nil {
				continue
			}
			s.mu.Lock()
			// 期间可能已被 closeMissing 清掉，重新取一次再标记
			if cur, ok := s.active[snap.ID]; ok {
				cur.persisted = true
			}
			s.mu.Unlock()
		}

		// 指标样本走 sqlc；数值直接取内存计数（不调用 GetStats，避免干扰转发）
		_ = s.call(ctx, "写入指标样本", func(c context.Context) error {
			return s.store.RecordMetricSample(c, store.MetricSample{
				RoomID:       snap.ID,
				Participants: snap.Participants,
				Tracks:       snap.Tracks,
				Packets:      int64(snap.Packets),
				Bytes:        int64(snap.Bytes),
			})
		})
	}

	s.closeMissing(ctx, seen, now)
	s.maybePrune(ctx, now)
}

// closeMissing 收尾"已从快照消失且超过空置宽限"的会议记录。
func (s *Sampler) closeMissing(ctx context.Context, seen map[string]bool, now time.Time) {
	type pending struct {
		id               string
		peakParticipants int
		peakTracks       int
	}

	var toClose []pending
	s.mu.Lock()
	for id, m := range s.active {
		if seen[id] {
			continue
		}
		if m.missingSince.IsZero() {
			m.missingSince = now // 第一次发现消失：开始计时，不立刻收尾
			continue
		}
		if now.Sub(m.missingSince) < s.cfg.EmptyGrace {
			continue
		}
		toClose = append(toClose, pending{
			id: m.id, peakParticipants: m.peakParticipants, peakTracks: m.peakTracks,
		})
	}
	s.mu.Unlock()

	for _, p := range toClose {
		if err := s.call(ctx, "收尾会议记录", func(c context.Context) error {
			return s.store.RecordMeetingEnd(c, p.id, p.peakParticipants, p.peakTracks)
		}); err != nil {
			continue // 留到下一个周期重试
		}
		s.mu.Lock()
		// 用会议 ID 匹配删除，避免把期间新开的会议误删
		for roomID, m := range s.active {
			if m.id == p.id {
				delete(s.active, roomID)
			}
		}
		s.mu.Unlock()
		log.Printf("recorder: 会议结束 room=%s 峰值人数=%d 峰值轨道=%d", p.id, p.peakParticipants, p.peakTracks)
	}
}

// maybePrune 执行保留策略（最多每分钟一次，避免每个周期都扫一遍表）。
func (s *Sampler) maybePrune(ctx context.Context, now time.Time) {
	if s.cfg.Retention <= 0 {
		return
	}
	s.mu.Lock()
	if !s.lastPrune.IsZero() && now.Sub(s.lastPrune) < time.Minute {
		s.mu.Unlock()
		return
	}
	s.lastPrune = now
	s.mu.Unlock()

	_ = s.call(ctx, "清理过期样本", func(c context.Context) error {
		n, err := s.store.PruneMetrics(c, now.Add(-s.cfg.Retention))
		if err != nil {
			return err
		}
		if n > 0 {
			log.Printf("recorder: 清理 %d 条过期指标样本（保留 %s）", n, s.cfg.Retention)
		}
		return nil
	})
}

// shutdown 收尾所有进行中的会议（进程退出前调用）。
func (s *Sampler) shutdown(ctx context.Context) {
	s.mu.Lock()
	pending := make([]*meeting, 0, len(s.active))
	for _, m := range s.active {
		pending = append(pending, m)
	}
	s.active = map[string]*meeting{}
	s.mu.Unlock()

	if len(pending) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for _, m := range pending {
		if err := s.store.RecordMeetingEnd(ctx, m.id, m.peakParticipants, m.peakTracks); err != nil {
			log.Printf("recorder: 退出时收尾会议失败 meeting=%s: %v", m.id, err)
			continue
		}
	}
	log.Printf("recorder: 退出前已收尾 %d 条进行中的会议记录", len(pending))
}

// call 给每次写库套上超时与日志限流：数据库不可用时不能刷屏，也不能拖住采样循环。
func (s *Sampler) call(ctx context.Context, what string, fn func(context.Context) error) error {
	cctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()
	if err := fn(cctx); err != nil {
		s.mu.Lock()
		s.failures++
		n := s.failures
		s.mu.Unlock()
		// 只报第 1 次与之后每 20 次：数据库长时间不可用时日志仍然可读
		if n == 1 || n%20 == 0 {
			log.Printf("recorder: %s 失败（累计 %d 次，采样继续）: %v", what, n, err)
		}
		return err
	}
	return nil
}

// meetingID 生成会议 ID：房间号 + 开始时间，便于在库里直接看出"哪次"会议。
func meetingID(roomID string, at time.Time) string {
	return roomID + "-" + at.UTC().Format("20060102T150405")
}
