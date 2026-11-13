package sfu

import (
	"sync"
	"testing"
	"time"

	"easylive-api/protocol"
)

// newTestRoom 构造一个**不启动 speakerLoop 协程**的房间，用于并发/状态机回归测试。
// 不走 newRoom 是为了让测试不依赖后台协程的生命周期（也不用在断言里 close channel）。
func newTestRoom(id string, m *Manager) *Room {
	return &Room{
		ID:            id,
		CreatedAt:     time.Now(),
		manager:       m,
		participants:  make(map[string]*Participant),
		tracks:        make(map[string]*PublishedTrack),
		subscriptions: make(map[string]map[string]bool),
		grace:         make(map[string]*time.Timer),
		speakerStop:   make(chan struct{}),
		hands:         make(map[string]bool),
		quality:       make(map[string]clientQuality),
	}
}

func testManager() *Manager {
	return &Manager{
		maxParticipants: 16,
		resumeGrace:     time.Second,
		rooms:           make(map[string]*Room),
	}
}

// TestExpireDetachedRespectsResume 钉住宽限期里的 TOCTOU：
//
// 旧实现 expireDetached 先取锁确认"参与者还在"，释放锁后再调 Leave。这段窗口里
// 客户端可能刚好带着 resumeToken 重连成功（Resume 把状态改回 active 并停掉计时器），
// 随后 Leave 仍然把这个人踢出房间 —— 现象是"重连说成功了，几秒后又掉线"。
//
// 新实现的判定是"在锁内同时检查 state == reconnecting 并摘除"，所以：
//   - 状态已是 active（Resume 赢了）→ 计时器事件必须被丢弃；
//   - 状态仍是 reconnecting（没人来救）→ 正常离会。
func TestExpireDetachedRespectsResume(t *testing.T) {
	m := testManager()
	r := newTestRoom("room-t", m)

	resumed := NewParticipant("p-resumed", "Resumed", nil, nil)
	resumed.Confirm(r)
	resumed.setState(protocol.StateReconnecting)
	r.participants[resumed.ID] = resumed
	// 模拟 Resume 已经发生：状态回到 active（Resume 会停计时器，但计时器可能已经触发）
	resumed.setState(protocol.StateActive)

	r.expireDetached(resumed.ID)
	if _, ok := r.participants[resumed.ID]; !ok {
		t.Fatal("已经恢复（state=active）的参与者不应被过期的宽限期计时器踢掉")
	}

	// 对照：仍然处于 reconnecting 的参与者，宽限期到了就该离会
	gone := NewParticipant("p-gone", "Gone", nil, nil)
	gone.Confirm(r)
	gone.setState(protocol.StateReconnecting)
	r.participants[gone.ID] = gone

	r.expireDetached(gone.ID)
	if _, ok := r.participants[gone.ID]; ok {
		t.Fatal("宽限期已过且仍在重连中的参与者应当离会")
	}
}

// TestRemoveSkipsRoomRejoinedDuringRace 钉住空房回收的竞态：
//
// Leave 在临界区里判定 emptiness，但判定与 Manager.remove 删表之间还有窗口；
// GetOrCreate 在这段时间仍会返回同一个 Room 对象，于是可能有人 Join 回来。
// 旧实现直接删表，房间就变成"孤儿"：人还在开会，Manager 却不认它。
func TestRemoveSkipsRoomRejoinedDuringRace(t *testing.T) {
	m := testManager()

	// ① 真为空：应当被回收
	emptyRoom := newTestRoom("room-empty", m)
	m.rooms[emptyRoom.ID] = emptyRoom
	m.remove(emptyRoom.ID)
	if _, ok := m.Get(emptyRoom.ID); ok {
		t.Fatal("空房间应当被回收")
	}

	// ② 判定为空之后有人重新加入：必须保留房间
	busyRoom := newTestRoom("room-busy", m)
	p := NewParticipant("p-late", "Late", nil, nil)
	p.Confirm(busyRoom)
	busyRoom.participants[p.ID] = p
	m.rooms[busyRoom.ID] = busyRoom

	m.remove(busyRoom.ID)
	if _, ok := m.Get(busyRoom.ID); !ok {
		t.Fatal("期间有人加入的房间不应被回收（否则房间变成孤儿）")
	}
	select {
	case <-busyRoom.speakerStop:
		t.Fatal("房间保留时不应关闭 speakerStop（采样协程会被误杀）")
	default:
	}
}

// TestParticipantRoomPointerConcurrent 钉住 p.room 的数据竞争。
//
// 写入只发生在 Room.Join，读取发生在 Pion 的回调协程（OnTrack /
// OnConnectionStateChange / 数据通道）。旧实现是普通字段，两者没有共同锁——
// 只有 -race 能抓到。字段改成 atomic.Pointer 后这里必须干净通过。
func TestParticipantRoomPointerConcurrent(t *testing.T) {
	p := NewParticipant("p-race", "Race", nil, nil)
	r := &Room{ID: "room-race"}

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for n := 0; n < 2000; n++ {
				p.Confirm(r)
				_ = p.roomRef()
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := p.roomRef(); got != r {
		t.Fatalf("roomRef 应返回最后写入的房间，实际 %v", got)
	}
}

// TestLeaveEmitsTrackEvent 钉住"离会绕过 removeTrack 导致 AI 转写器摘不掉"的缺陷。
//
// 参与者离会时，removeParticipantLocked 直接把轨道从 map 删掉；随后 p.Close()
// 让 Pump 结束并回调 onTrackEnded → removeTrack，而 removeTrack 发现轨道已不在 map
// 里会提前 return（幂等），于是**永远不会触发轨道事件钩子**。
// AI 转写器正是靠这个钩子 detach 的，不补发就会一直挂在一条已死的轨道上。
func TestLeaveEmitsTrackEvent(t *testing.T) {
	m := testManager()
	r := newTestRoom("room-hook", m)

	p := NewParticipant("p-hook", "Hook", nil, nil)
	p.Confirm(r)
	r.participants[p.ID] = p

	track := &PublishedTrack{info: &protocol.TrackInfo{ID: "t-hook", PublisherID: p.ID, Kind: protocol.KindAudio}}
	r.tracks[track.info.ID] = track

	var mu sync.Mutex
	var events []string
	r.OnTrackEvent(func(t *PublishedTrack, added bool) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, t.info.ID+"="+map[bool]string{true: "added", false: "removed"}[added])
	})

	r.Leave(p.ID)

	mu.Lock()
	defer mu.Unlock()
	if len(events) != 1 || events[0] != "t-hook=removed" {
		t.Fatalf("离会时应为每条下线轨道触发一次 removed 钩子，实际 %v", events)
	}
}
