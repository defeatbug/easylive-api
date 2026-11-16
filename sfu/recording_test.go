package sfu

// 录制与虚拟轨道、房间归属的回归测试。
//
// 背景：房间里的 AI 助手虚拟轨没有远端（t.remote == nil），而"开始录制"会遍历
// 房间里所有已发布轨道去取 PayloadType/SSRC——曾经这里没有 nil 判断，结果是
// 房间用过一次 @AI 之后，任何人点"开始录制"都会 nil 解引用 panic、自己的连接被打断。

import (
	"testing"
	"time"

	"easylive-api/mediarec"
)

// newTestManagerWithRecording 造一个开了录制能力的 Manager（不需要真实的 Pion 引擎）。
func newTestManagerWithRecording(t *testing.T) (*Manager, *mediarec.Manager) {
	t.Helper()
	rec, err := mediarec.New(t.TempDir())
	if err != nil {
		t.Fatalf("创建录制管理器失败: %v", err)
	}
	t.Cleanup(func() { rec.CloseAll() })
	m := NewManager(nil, 16, 30*time.Second)
	m.EnableRecording(rec)
	return m, rec
}

// TestAttachRecordersSkipsVirtualTrack 回归：虚拟轨不能让 attachRecorders panic。
func TestAttachRecordersSkipsVirtualTrack(t *testing.T) {
	m, rec := newTestManagerWithRecording(t)
	r := m.GetOrCreate("room-agent-rec")
	defer close(r.speakerStop) // 回收说话人采样协程

	track, err := r.PublishAgentTrack()
	if err != nil {
		t.Fatalf("发布虚拟轨失败: %v", err)
	}
	if _, err := rec.StartSession(r.ID, "p-1"); err != nil {
		t.Fatalf("开始录制失败: %v", err)
	}

	// 这里以前会 nil 解引用 panic（测试进程会直接崩）
	r.attachRecorders()

	if track.recording() {
		t.Fatal("虚拟轨没有远端 RTP 输入，不应该挂录制写入器")
	}
	if rec.Active() != 0 {
		t.Fatalf("虚拟轨不应产生录制文件，实际活跃写入器 %d", rec.Active())
	}
}

// TestRecordingIsolatedPerRoom 回归：录制按房间隔离——
// 每个房间有自己的会话，别的房间开始/停止录制都互不影响，
// 录制状态也按房间取：A 在录，B 的新入会者不该看到"录制中"红点。
func TestRecordingIsolatedPerRoom(t *testing.T) {
	m, _ := newTestManagerWithRecording(t)
	a := m.GetOrCreate("room-rec-a")
	defer close(a.speakerStop)
	b := m.GetOrCreate("room-rec-b")
	defer close(b.speakerStop)

	if _, err := m.StartRecording("room-rec-a", "p-a"); err != nil {
		t.Fatalf("room-rec-a 开始录制失败: %v", err)
	}
	// 同房间重复开启：幂等
	if _, err := m.StartRecording("room-rec-a", "p-a"); err != nil {
		t.Fatalf("同房间重复开始录制应幂等成功: %v", err)
	}
	// 别的房间：独立会话，不再被拒绝
	sessB, err := m.StartRecording("room-rec-b", "p-b")
	if err != nil {
		t.Fatalf("room-rec-b 开始录制失败: %v", err)
	}
	if sessB.RoomID != "room-rec-b" {
		t.Fatalf("room-rec-b 应拿到自己的会话，实际 %+v", sessB)
	}
	// 停掉 room-rec-b 不能停掉 room-rec-a 的录制
	if sess := m.StopRecording("room-rec-b"); sess == nil || sess.RoomID != "room-rec-b" {
		t.Fatalf("停止 room-rec-b 应返回它自己的会话，实际 %+v", sess)
	}
	if sess := m.RecordingSession("room-rec-a"); sess == nil || sess.RoomID != "room-rec-a" {
		t.Fatalf("room-rec-a 的会话不应被停掉，实际 %+v", sess)
	}
	// 录制状态也要按房间隔离：A 在录，B 的新入会者不该看到"录制中"红点
	if st := m.RoomRecordingState("room-rec-a"); !st.Active {
		t.Fatal("room-rec-a 应看到录制中")
	}
	if st := m.RoomRecordingState("room-rec-b"); st.Active {
		t.Fatalf("room-rec-b 不应看到录制中，实际 %+v", st)
	}
	if b.State().Recording.Active {
		t.Fatal("room-rec-b 的入会快照不应带录制中红点")
	}
	// 归属房间自己停：正常结束
	if sess := m.StopRecording("room-rec-a"); sess == nil {
		t.Fatal("room-rec-a 停止录制失败：应返回会话")
	}
	if m.RecordingSession("room-rec-a") != nil {
		t.Fatal("停止后不应还有会话")
	}
}
