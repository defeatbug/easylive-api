package mediarec

import (
	"testing"

	"github.com/pion/rtp"
)

// TestActiveDropsClosedWriters 钉住"active 表不清理"的缺陷。
//
// 旧实现只有 StopSession / CloseAll 会清 m.active；但真实路径里写入器是被
// Pump 的 defer（轨道结束）或 detachRecorders（停止录制）逐个 Close 的。
// 于是 active 里长期堆着已关闭的 Writer：
//   - Active() 虚高（观测/调试口径失真）；
//   - 这些 Writer 被 map 引用，文件句柄统计与内存都回收不掉。
func TestActiveDropsClosedWriters(t *testing.T) {
	m, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.StartSession("room-active", "tester"); err != nil {
		t.Fatal(err)
	}

	w1, err := m.Open("room-active", "p-1", "camera", Codec{Name: "VP8", ClockRate: 90000})
	if err != nil {
		t.Fatal(err)
	}
	w2, err := m.Open("room-active", "p-1", "microphone", Codec{Name: "opus", ClockRate: 48000})
	if err != nil {
		t.Fatal(err)
	}
	// 写一点数据，确保写协程真的在跑（关闭路径覆盖"有积压包"的情形）
	w1.Write(rtpPacket(vp8Keyframe(320, 180, 32), 0, true))
	w2.Write(&rtp.Packet{Header: rtp.Header{Timestamp: 0}, Payload: []byte{0xfc, 0x01}})

	if got := m.Active(); got != 2 {
		t.Fatalf("两条轨道应计为 2 个活动写入器，实际 %d", got)
	}

	// 模拟"轨道结束"：Pump 的 defer 直接 Close，不经过 StopSession
	if err := w1.Close(); err != nil {
		t.Fatal(err)
	}
	if got := m.Active(); got != 1 {
		t.Fatalf("关闭一条后 Active 应为 1，实际 %d（说明 active 表没清理）", got)
	}

	if err := w2.Close(); err != nil {
		t.Fatal(err)
	}
	if got := m.Active(); got != 0 {
		t.Fatalf("全部关闭后 Active 应为 0，实际 %d", got)
	}

	// Close 幂等：重复关闭不能把计数打负，也不能 panic
	if err := w1.Close(); err != nil {
		t.Fatal(err)
	}
	if got := m.Active(); got != 0 {
		t.Fatalf("重复 Close 后 Active 仍应为 0，实际 %d", got)
	}

	if sess := m.StopSession("room-active"); sess == nil {
		t.Fatal("会话应能正常收尾")
	}
}
