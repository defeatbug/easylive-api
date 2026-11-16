package sfu

import (
	"testing"

	"easylive-api/mediarec"
)

// TestRecordingIsPerRoom 钉住两个真实缺陷（旧实现是全局单会话）：
//  1. 两个房间同时录制时，B 房开始会拿到 A 房的会话（文件/清单串台）；
//  2. 停止录制会摘掉**所有**房间的写入器，于是 A 房停止会连带停掉 B 房。
//
// 这里不依赖真实 Pion：StartRecording/StopRecording/RoomRecordingState 只碰
// Manager.recorder 与房间表，直接用零值 Manager + 临时目录即可（测试属于 sfu 包内）。
func TestRecordingIsPerRoom(t *testing.T) {
	rec, err := mediarec.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := &Manager{recorder: rec} // rooms 为空：本例只验证录制状态机，不建真实房间

	a1, err := m.StartRecording("room-a", "p-a")
	if err != nil {
		t.Fatalf("room-a 开始录制失败: %v", err)
	}
	// 幂等：同房间重复开始返回同一会话
	a2, err := m.StartRecording("room-a", "p-a")
	if err != nil {
		t.Fatal(err)
	}
	if a1.ID != a2.ID {
		t.Fatalf("重复开始应返回同一会话，实际 %s vs %s", a1.ID, a2.ID)
	}

	if st := m.RoomRecordingState("room-a"); !st.Active {
		t.Fatal("room-a 应在录制中")
	}
	// 关键断言：另一个房间不能被显示成"录制中"（旧实现取全局会话，这里必红）
	if st := m.RoomRecordingState("room-b"); st.Active {
		t.Fatalf("room-b 不应显示录制中: %+v", st)
	}

	if _, err := m.StartRecording("room-b", "p-b"); err != nil {
		t.Fatalf("room-b 开始录制失败: %v", err)
	}
	if st := m.RoomRecordingState("room-b"); !st.Active {
		t.Fatal("room-b 开始后应在录制中")
	}

	// 停止 room-a 不能影响 room-b
	stopped := m.StopRecording("room-a")
	if stopped == nil || stopped.RoomID != "room-a" {
		t.Fatalf("停止 room-a 应返回 room-a 的会话，实际 %+v", stopped)
	}
	if st := m.RoomRecordingState("room-a"); st.Active {
		t.Fatal("room-a 停止后不应仍在录制")
	}
	if st := m.RoomRecordingState("room-b"); !st.Active {
		t.Fatalf("停止 room-a 不应影响 room-b: %+v", st)
	}

	// 全局状态（HTTP /recordings 用）：仍有房间在录，应 active
	if st := m.RecordingState(); !st.Active {
		t.Fatalf("room-b 仍在录制，全局状态应为 active: %+v", st)
	}

	m.StopRecording("room-b")
	if st := m.RoomRecordingState("room-b"); st.Active {
		t.Fatal("room-b 停止后不应仍在录制")
	}
	// 全部停止后应为 inactive
	if st := m.RecordingState(); st.Active {
		t.Fatalf("全部停止后全局录制状态应为 inactive: %+v", st)
	}
}

// TestRecordingStateGlobalPicksLatest 验证全局状态在多个房间同时录制时不会随机跳变：
// /recordings 列表只有"一个" recording 字段，取最近开始的会话，结果必须是确定的。
func TestRecordingStateGlobalPicksLatest(t *testing.T) {
	rec, err := mediarec.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := &Manager{recorder: rec}

	if _, err := rec.StartSession("room-old", "tester"); err != nil {
		t.Fatal(err)
	}
	if _, err := rec.StartSession("room-new", "tester"); err != nil {
		t.Fatal(err)
	}
	st := m.RecordingState()
	if !st.Active || st.SessionID == "" {
		t.Fatalf("应报告 active 且带回会话 ID: %+v", st)
	}

	rec.StopSession("room-old")
	if st := m.RoomRecordingState("room-new"); !st.Active {
		t.Fatal("停止较早的会话不应影响较晚的会话")
	}
	if st := m.RecordingState(); !st.Active || st.SessionID == "" {
		t.Fatalf("仍应报告 active: %+v", st)
	}
	rec.CloseAll()
}
