package mediarec

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestWriterConcurrentWriteClose 钉住一个曾经存在的真实缺陷：
//
// 旧实现用 close(w.frames) 收尾，而 Write 在媒体热路径上、与 Close 之间没有同步点。
// 于是"停止录制"时，热路径的 send 可能撞上已关闭的 channel 而 panic；热路径跑在
// safeGo 里，panic 被兜住后这条轨道就再也不会转发——表现为"停止录制之后，
// 某个人的画面/声音对所有人永久消失"。
//
// 这个测试在 -race 下运行（make test 已改为 go test -race）：
//   - 旧实现会 panic 或报 data race；
//   - 新实现（stop channel + 克隆包）必须安静通过。
func TestWriterConcurrentWriteClose(t *testing.T) {
	m, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.StartSession("room-race", "p-admin"); err != nil {
		t.Fatal(err)
	}
	w, err := m.Open("room-race", "p-1", "camera", Codec{Name: "VP8", ClockRate: 90000})
	if err != nil {
		t.Fatal(err)
	}

	// 多个写协程模拟多条热路径同时写同一个写入器（真实场景里一条轨道只有一个 Pump，
	// 这里刻意加压，让 Write/Close 的重叠窗口更容易被 race detector 抓到）。
	const writers = 4
	const packetsPerWriter = 4000
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			<-start
			for n := 0; n < packetsPerWriter; n++ {
				frame := vp8Keyframe(320, 180, 32)
				// 每个 writer 用不同的时间戳区间，避免被 IVF 当成重复包/时间戳倒退整帧丢弃
				ts := uint32(seed*1_000_000 + n*3000)
				w.Write(rtpPacket(frame, ts, true))
			}
		}(i)
	}
	close(start)
	// 在写入进行中关闭：这正是"主持人点停止录制"的时序。
	if err := w.Close(); err != nil {
		t.Fatalf("Close 返回错误: %v", err)
	}
	wg.Wait() // 关闭后的 Write 必须安全返回（丢弃），不能 panic

	// Close 幂等：停止录制与轨道结束时都会调一次。
	if err := w.Close(); err != nil {
		t.Fatalf("重复 Close 应幂等，实际: %v", err)
	}
	if sess := m.StopSession("room-race"); sess == nil {
		t.Fatal("会话应当能被正常收尾")
	}
}

// TestMultiRoomSessionsIndependent 钉住另一个真实缺陷：
//
// 旧实现把所有房间的录制状态塞进 Manager 的**单个** session/sessionDir 字段，
// 而开关 API 是房间级的。后果是：B 房开始录制会拿到 A 房的会话（文件写进 A 目录、
// 清单记在 A 上），A 房停止录制会把 B 房的写入器一起摘掉，A 房空房回收还会停掉 B 房。
//
// 这个测试要求：两个房间各自开、各自写、停一个不影响另一个，清单各归各。
func TestMultiRoomSessionsIndependent(t *testing.T) {
	dir := t.TempDir()
	m, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}

	sa, err := m.StartSession("room-a", "p-a")
	if err != nil {
		t.Fatal(err)
	}
	sb, err := m.StartSession("room-b", "p-b")
	if err != nil {
		t.Fatal(err)
	}
	if sa.ID == sb.ID {
		t.Fatalf("两个房间的会话 ID 不应相同: %s", sa.ID)
	}

	// 各自在"自己的会话目录"里开一条轨道
	wa, err := m.Open("room-a", "p-a", "camera", Codec{Name: "VP8", ClockRate: 90000})
	if err != nil {
		t.Fatal(err)
	}
	wb, err := m.Open("room-b", "p-b", "camera", Codec{Name: "VP8", ClockRate: 90000})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(wa.Path()) != filepath.Join(dir, sa.ID) {
		t.Fatalf("room-a 的文件应落在自己的会话目录: %s", wa.Path())
	}
	if filepath.Dir(wb.Path()) != filepath.Join(dir, sb.ID) {
		t.Fatalf("room-b 的文件应落在自己的会话目录: %s", wb.Path())
	}

	wa.Write(rtpPacket(vp8Keyframe(320, 180, 32), 0, true))
	wb.Write(rtpPacket(vp8Keyframe(320, 180, 32), 0, true))

	// 只停 room-a
	stoppedA := m.StopSession("room-a")
	if stoppedA == nil || stoppedA.RoomID != "room-a" {
		t.Fatalf("停止 room-a 应返回 room-a 的会话，实际 %+v", stoppedA)
	}
	if m.Recording("room-a") {
		t.Fatal("room-a 停止后不应仍在录制")
	}
	if !m.Recording("room-b") {
		t.Fatal("停止 room-a 不应影响 room-b：room-b 必须仍在录制")
	}

	// room-b 必须还能继续写（写入器没有被 A 的收尾摘掉）
	if _, err := os.Stat(wb.Path()); err != nil {
		t.Fatalf("room-b 的写入器不应被关闭: %v", err)
	}
	wb.Write(rtpPacket(vp8Keyframe(320, 180, 32), 3000, true))
	wb.Write(rtpPacket(vp8Keyframe(320, 180, 32), 6000, true))

	stoppedB := m.StopSession("room-b")
	if stoppedB == nil || len(stoppedB.Files) != 1 {
		t.Fatalf("room-b 收尾应带回 1 个文件，实际 %+v", stoppedB)
	}
	if stoppedB.Files[0].Frames < 2 {
		t.Fatalf("room-b 停止前写入的帧应当被计入清单，实际 frames=%d", stoppedB.Files[0].Frames)
	}

	// 两份清单都要在磁盘上，且 roomID 正确（不能串台）
	for _, want := range []struct {
		sess   *Session
		roomID string
	}{{stoppedA, "room-a"}, {stoppedB, "room-b"}} {
		raw, rerr := os.ReadFile(filepath.Join(dir, want.sess.ID, "manifest.json"))
		if rerr != nil {
			t.Fatalf("%s 的清单未落盘: %v", want.roomID, rerr)
		}
		var got Session
		if uerr := json.Unmarshal(raw, &got); uerr != nil {
			t.Fatalf("%s 的清单不可解析: %v", want.roomID, uerr)
		}
		if got.RoomID != want.roomID {
			t.Fatalf("清单串台：期望 roomID=%s，实际 %s", want.roomID, got.RoomID)
		}
		if len(got.Files) != 1 {
			t.Fatalf("%s 清单应有 1 个文件，实际 %d", want.roomID, len(got.Files))
		}
	}

	// 全部停止后 CloseAll 不应 panic
	m.CloseAll()
	if len(m.RecordingRooms()) != 0 {
		t.Fatalf("CloseAll 后不应还有录制中的房间: %v", m.RecordingRooms())
	}
}
