package sfu

// 房间回收钩子与锁序的回归测试。
//
// 两件事：
//  1. 房间被回收时要能通知上层（AI 总结会话靠它收尾，否则一直泄漏、同 ID 新房间被旧会话劫持）；
//  2. Stats() 与 Room.State() 互相加锁的顺序不能相反——以前一个持 m.mu 取 r.mu、
//     另一个持 r.mu 取 m.mu，中间只要有一个写者（建房/回收）排队就会 ABBA 互等。

import (
	"sync"
	"testing"
	"time"
)

func TestOnRoomRemovedFires(t *testing.T) {
	m := NewManager(nil, 8, time.Second)
	got := make(chan string, 1)
	m.OnRoomRemoved(func(roomID string) { got <- roomID })

	m.GetOrCreate("room-gone")
	m.remove("room-gone")

	select {
	case id := <-got:
		if id != "room-gone" {
			t.Fatalf("回调参数应为被回收的房间，实际 %q", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("房间回收后未触发 OnRoomRemoved 回调")
	}
	if _, ok := m.Get("room-gone"); ok {
		t.Fatal("房间应已从管理器里移除")
	}

	// 没注册回调时也不能 panic（remove 是内部路径，任何房间离开都会走到）
	m2 := NewManager(nil, 8, time.Second)
	m2.remove("nobody-home")
}

// TestConcurrentStatsAndState 是对锁序的冒烟压测：Stats（manager → room）与
// State（room → manager）并发跑，同时有建房/回收的写者。
//
// 它不能"证明没有死锁"，但在回退成相反锁序后，Go 的写者优先 RWMutex 很容易把它卡住；
// 卡住时 watchdog 会让测试失败而不是一直挂着（CI 里挂死比失败更难查）。
func TestConcurrentStatsAndState(t *testing.T) {
	m := NewManager(nil, 16, time.Second)
	done := make(chan struct{})

	var wg sync.WaitGroup
	run := func(fn func(i int)) {
		for i := 0; i < 3; i++ {
			wg.Add(1)
			go func(worker int) {
				defer wg.Done()
				for j := 0; j < 400; j++ {
					fn(worker*1000 + j)
				}
			}(i)
		}
	}

	// 读：统计 + 快照
	run(func(i int) {
		m.Stats()
		if r, ok := m.Get("room-lock-a"); ok {
			_ = r.State()
		}
	})
	// 写：建房 / 回收（带唯一 ID，避免互相踩同一条 speakerStop）
	run(func(i int) {
		id := "room-lock-w"
		m.GetOrCreate(id)
		m.remove(id)
	})

	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("并发 Stats/State/建房回收 10s 未完成，疑似锁序死锁")
	}

	// 收尾：把仍在的房间回收掉（停采样协程）
	for _, id := range m.RoomIDs() {
		m.remove(id)
	}
	_, _, _ = m.Stats()
}
