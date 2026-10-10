// 采样器的单元测试：全部用内存假实现，不需要数据库。
//
// 覆盖的是**只有逻辑没有 SQL** 的那部分：峰值统计、会议开始/收尾、空置宽限、
// 写库失败后的恢复、保留清理、退出前收尾。这些正是最容易写错、
// 又最难在连库集成测试里复现的地方。
package recorder

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"easylive-api/sfu"
	"easylive-api/store"
)

// ---------- 测试替身 ----------

type endCall struct {
	ID string
	P  int
	T  int
}

type fakeStore struct {
	mu      sync.Mutex
	rooms   []string
	starts  []string
	ends    []endCall
	samples []store.MetricSample
	prunes  []time.Time

	failEnsure bool
	failStart  bool
	failSample bool
}

func (f *fakeStore) EnsureRoom(_ context.Context, roomID, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failEnsure {
		return errors.New("ensure failed")
	}
	f.rooms = append(f.rooms, roomID)
	return nil
}

func (f *fakeStore) RecordMeetingStart(_ context.Context, _, meetingID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failStart {
		return errors.New("start failed")
	}
	f.starts = append(f.starts, meetingID)
	return nil
}

func (f *fakeStore) RecordMeetingEnd(_ context.Context, meetingID string, p, t int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ends = append(f.ends, endCall{ID: meetingID, P: p, T: t})
	return nil
}

func (f *fakeStore) RecordMetricSample(_ context.Context, m store.MetricSample) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failSample {
		return errors.New("sample failed")
	}
	f.samples = append(f.samples, m)
	return nil
}

func (f *fakeStore) PruneMetrics(_ context.Context, before time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prunes = append(f.prunes, before)
	return 7, nil
}

func (f *fakeStore) snapshot() (rooms, starts []string, ends []endCall, samples []store.MetricSample, prunes int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.rooms...), append([]string{}, f.starts...),
		append([]endCall{}, f.ends...), append([]store.MetricSample{}, f.samples...), len(f.prunes)
}

type fakeRooms struct {
	mu    sync.Mutex
	snaps []sfu.RoomSnapshot
}

func (f *fakeRooms) RoomSnapshots() []sfu.RoomSnapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sfu.RoomSnapshot{}, f.snaps...)
}

func (f *fakeRooms) set(snaps ...sfu.RoomSnapshot) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snaps = snaps
}

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newTestSampler(st Store, rooms Rooms, clk *testClock, retention time.Duration) *Sampler {
	return New(st, rooms, Config{
		Interval:   5 * time.Second,
		Retention:  retention,
		EmptyGrace: 10 * time.Second,
		Timeout:    time.Second,
		Clock:      clk.now,
	})
}

// ---------- 用例 ----------

func TestTickStartsMeetingAndKeepsPeaks(t *testing.T) {
	ctx := context.Background()
	st := &fakeStore{}
	rooms := &fakeRooms{}
	clk := &testClock{t: time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)}
	s := newTestSampler(st, rooms, clk, 0)

	// 第一次采样：房间刚出现 → upsert 房间 + 开始会议 + 写一条样本
	rooms.set(sfu.RoomSnapshot{ID: "room-a", Participants: 2, Tracks: 3, Packets: 100, Bytes: 1000})
	s.Tick(ctx)

	r, starts, ends, samples, _ := st.snapshot()
	if len(r) != 1 || r[0] != "room-a" {
		t.Fatalf("应 upsert 房间 room-a，实际 %v", r)
	}
	if len(starts) != 1 || !strings.HasPrefix(starts[0], "room-a-") {
		t.Fatalf("应创建一条 room-a 的会议记录，实际 %v", starts)
	}
	if len(samples) != 1 || samples[0].Participants != 2 || samples[0].Tracks != 3 {
		t.Fatalf("应写入一条样本（2 人 3 轨），实际 %+v", samples)
	}
	if len(ends) != 0 {
		t.Fatalf("房间还在，不应结束会议")
	}

	// 第二次采样：人数/轨道上升到峰值；且不应重复 upsert/重复开会
	clk.advance(5 * time.Second)
	rooms.set(sfu.RoomSnapshot{ID: "room-a", Participants: 5, Tracks: 4, Packets: 200, Bytes: 2000})
	s.Tick(ctx)
	r, starts, _, samples, _ = st.snapshot()
	if len(r) != 1 || len(starts) != 1 {
		t.Fatalf("房间与会议记录都应只写一次，实际 rooms=%v starts=%v", r, starts)
	}
	if len(samples) != 2 {
		t.Fatalf("应累计 2 条样本，实际 %d", len(samples))
	}

	// 第三次：房间消失，但还在空置宽限期内 → 不能立刻收尾
	clk.advance(5 * time.Second)
	rooms.set()
	s.Tick(ctx)
	if _, _, ends, _, _ = st.snapshot(); len(ends) != 0 {
		t.Fatalf("空置宽限期内不应结束会议，实际 %+v", ends)
	}
	if s.ActiveMeetings() != 1 {
		t.Fatalf("会议应仍在进行中，实际 %d", s.ActiveMeetings())
	}

	// 第四次：超过宽限期 → 收尾，且峰值必须保留下来（这是会议记录的核心价值）
	clk.advance(11 * time.Second)
	s.Tick(ctx)
	_, _, ends, _, _ = st.snapshot()
	if len(ends) != 1 {
		t.Fatalf("超过宽限期应收尾会议，实际 %+v", ends)
	}
	if ends[0].P != 5 || ends[0].T != 4 {
		t.Fatalf("收尾时应带上峰值（5 人 4 轨），实际 %+v", ends[0])
	}
	if s.ActiveMeetings() != 0 {
		t.Fatalf("收尾后不应再有进行中的会议")
	}
}

func TestTickRecoversFromStoreFailures(t *testing.T) {
	ctx := context.Background()
	st := &fakeStore{failEnsure: true, failStart: true, failSample: true}
	rooms := &fakeRooms{}
	clk := &testClock{t: time.Now()}
	s := newTestSampler(st, rooms, clk, 0)

	rooms.set(sfu.RoomSnapshot{ID: "room-b", Participants: 1, Tracks: 1})

	// 数据库不可用：不能 panic，也不能把房间从 active 里丢掉
	s.Tick(ctx)
	if r, starts, _, _, _ := st.snapshot(); len(r) != 0 || len(starts) != 0 {
		t.Fatalf("写入失败时不应记录成功，实际 rooms=%v starts=%v", r, starts)
	}
	if s.ActiveMeetings() != 1 {
		t.Fatalf("失败后仍应记住这个房间，等待重试")
	}

	// 数据库恢复：**必须补上会议记录**（这正是 persisted 标记存在的意义）
	st.mu.Lock()
	st.failEnsure, st.failStart, st.failSample = false, false, false
	st.mu.Unlock()

	clk.advance(5 * time.Second)
	s.Tick(ctx)
	r, starts, _, samples, _ := st.snapshot()
	if len(r) != 1 || len(starts) != 1 {
		t.Fatalf("恢复后应补写房间与会议记录，实际 rooms=%v starts=%v", r, starts)
	}
	if len(samples) != 1 {
		t.Fatalf("恢复后应写入样本，实际 %d", len(samples))
	}
}

func TestTickPrunesRetentionAtMostOncePerMinute(t *testing.T) {
	ctx := context.Background()
	st := &fakeStore{}
	rooms := &fakeRooms{}
	clk := &testClock{t: time.Now()}
	s := newTestSampler(st, rooms, clk, 24*time.Hour)
	rooms.set(sfu.RoomSnapshot{ID: "room-c", Participants: 1, Tracks: 1})

	s.Tick(ctx) // 首次：允许清理
	if _, _, _, _, prunes := st.snapshot(); prunes != 1 {
		t.Fatalf("首次采样应执行一次保留清理，实际 %d", prunes)
	}

	clk.advance(5 * time.Second)
	s.Tick(ctx)
	if _, _, _, _, prunes := st.snapshot(); prunes != 1 {
		t.Fatalf("一分钟内不应重复清理，实际 %d", prunes)
	}

	clk.advance(2 * time.Minute)
	s.Tick(ctx)
	if _, _, _, _, prunes := st.snapshot(); prunes != 2 {
		t.Fatalf("超过一分钟后应再次清理，实际 %d", prunes)
	}

	// 关闭保留策略后不再清理
	s2 := newTestSampler(st, rooms, clk, 0)
	s2.Tick(ctx)
	if _, _, _, _, prunes := st.snapshot(); prunes != 2 {
		t.Fatalf("Retention<=0 时不应清理，实际 %d", prunes)
	}
}

func TestRunShutdownEndsActiveMeetings(t *testing.T) {
	st := &fakeStore{}
	rooms := &fakeRooms{}
	clk := &testClock{t: time.Now()}
	rooms.set(sfu.RoomSnapshot{ID: "room-d", Participants: 3, Tracks: 2})

	s := New(st, rooms, Config{Interval: 10 * time.Millisecond, Clock: clk.now})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(done)
	}()

	// 等到会议被记录
	deadline := time.After(3 * time.Second)
	for s.ActiveMeetings() == 0 {
		select {
		case <-deadline:
			t.Fatalf("采样器未在预期时间内记录会议")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("Run 未在取消后退出")
	}

	// 退出前必须收尾：否则库里会留下一批 ended_at 为空的会议记录
	_, _, ends, _, _ := st.snapshot()
	if len(ends) != 1 {
		t.Fatalf("退出时应收尾进行中的会议，实际 %+v", ends)
	}
	if ends[0].P != 3 || ends[0].T != 2 {
		t.Fatalf("退出收尾也应带上峰值，实际 %+v", ends[0])
	}
}
