package mediarec

// 录制写入器的并发/归属回归测试。
//
// 这两类问题都不是"编造出来的边界"：停止录制是用户在会中随时会点的操作，
// 而它和媒体转发热路径是并发的。

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
)

// TestWriteAfterCloseIsIgnored 是回归测试：写入器被关闭后，转发协程可能还拿着
// 旧指针再写一次（`if w := t.rec.Load(); w != nil { w.Write(pkt) }` 的窗口）。
//
// 没有保护时这里是 send on closed channel → panic。panic 会被 sfu.safeGo 兜住，
// 进程不崩，但那条轨道的 Pump 就此永久退出——房间里所有人从此收不到这个人的媒体。
func TestWriteAfterCloseIsIgnored(t *testing.T) {
	m, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := m.Open("room-1", "p-1", "camera", Codec{
		Name: "VP8", ClockRate: 90000, PayloadType: 96, SSRC: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	loaded := w // 模拟转发协程在 Close 之前 Load 到的指针
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Close 之后 Write 仍然 panic（回归）：%v", r)
		}
	}()
	loaded.Write(&rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 96, SSRC: 1}})
}

// TestWriteConcurrentWithClose 把"边转发边停录"跑起来：-race 下还能覆盖数据竞争。
func TestWriteConcurrentWithClose(t *testing.T) {
	m, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pkt := &rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 96, SSRC: 1}}
	for i := 0; i < 20; i++ {
		w, err := m.Open("room-1", "p-1", "camera", Codec{
			Name: "VP8", ClockRate: 90000, PayloadType: 96, SSRC: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		stop := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					w.Write(pkt)
				}
			}
		}()
		time.Sleep(time.Millisecond)
		if err := w.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		close(stop)
		wg.Wait()
	}
}

// TestStartSessionRejectsOtherRoom 锁定"录制会话是进程级单例"这条现状的**错误行为**：
// 同一个房间重复开启是幂等的，但**别的房间**必须明确失败。
//
// 早先这里直接返回已有会话，于是 B 房间显示"已开始录制"却一个字节都不落盘。
func TestStartSessionRejectsOtherRoom(t *testing.T) {
	m, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, err := m.StartSession("room-A", "p-1")
	if err != nil {
		t.Fatalf("room-A 开始录制失败: %v", err)
	}
	if _, err := m.StartSession("room-B", "p-2"); !errors.Is(err, ErrRoomBusy) {
		t.Fatalf("第二个房间应被拒绝（ErrRoomBusy），实际 err=%v", err)
	}
	// 同一房间重复开启：幂等，拿到同一个会话
	again, err := m.StartSession("room-A", "p-1")
	if err != nil {
		t.Fatalf("同房间重复开启应成功: %v", err)
	}
	if again.ID != a.ID {
		t.Fatalf("同房间应复用会话 %s，实际 %s", a.ID, again.ID)
	}
	// RecordingFor 是"新轨道要不要挂写入器"的判断依据，必须只认归属房间
	if !m.RecordingFor("room-A") {
		t.Fatal("room-A 正在录制，RecordingFor 应为 true")
	}
	if m.RecordingFor("room-B") {
		t.Fatal("room-B 没有录制会话，RecordingFor 应为 false")
	}
	m.StopSession()
	if m.RecordingFor("room-A") {
		t.Fatal("停止后 RecordingFor 应为 false")
	}
}
