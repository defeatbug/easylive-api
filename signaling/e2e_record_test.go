// 录制开关的权限回归测试。
//
// 为什么单独一条：曾经为了"主持人不在线也能录"把服务端的 roomAdmin 校验去掉了，
// 但前端按钮只对主持人显示、前端注释与 README 都还写着"服务端会独立校验"——
// 三处口径互相矛盾，非主持人手工发一条 record 就能录，还能停掉别的房间的录制。
// 现在权限由服务端兜住，这里把它钉死。
package signaling_test

import (
	"strings"
	"testing"
	"time"

	"easylive-api/auth"
	"easylive-api/mediarec"
	"easylive-api/protocol"
	"easylive-api/sfu"
)

func TestE2ERecordRequiresRoomAdmin(t *testing.T) {
	ts := startServerFull(t, 0, "required", testSecret, "development")
	const room = "room-record"

	adminTok := issueToken(t, ts.cfg, "u-admin", "主持人", auth.VideoGrant{
		RoomJoin: true, Room: room, CanPublish: true, CanSubscribe: true, CanPublishData: true, RoomAdmin: true,
	})
	memberTok := issueToken(t, ts.cfg, "u-member", "成员", auth.VideoGrant{
		RoomJoin: true, Room: room, CanPublish: true, CanSubscribe: true, CanPublishData: true,
	})

	admin := dial(t, ts.url, "admin")
	defer admin.close()
	joinWithToken(admin, room, "主持人", adminTok)

	member := dial(t, ts.url, "member")
	defer member.close()
	joinWithToken(member, room, "成员", memberTok)

	// 1) 普通成员：服务端必须拒绝（前端隐藏按钮只是体验，不是安全边界）
	member.send(&protocol.Message{Type: protocol.TypeRecord, Action: protocol.ActionRecordStart})
	if ev := waitError(t, member, protocol.ErrForbidden, 5*time.Second); !strings.Contains(ev.Reason, "录制") {
		t.Fatalf("拒绝原因应说明是录制权限，实际 %q", ev.Reason)
	}

	// 2) 主持人：权限通过，但本测试服务器没有启用录制能力 → 报"未开启录制"。
	// 这条区分的是"权限被拒"（forbidden）与"能力未启用"（internal_error）。
	admin.send(&protocol.Message{Type: protocol.TypeRecord, Action: protocol.ActionRecordStart})
	if ev := waitError(t, admin, protocol.ErrInternal, 5*time.Second); !strings.Contains(ev.Reason, "未开启录制") {
		t.Fatalf("主持人应因未启用录制而失败，实际 %q", ev.Reason)
	}
}

// TestE2ERecordingRoomScoped 是"录制按房间隔离"的端到端护栏。
//
// 它同时盯住三件曾经出过问题的事：
//  1. 主持人开录 → 本房间全员看到红点（成员看得见，但自己开不了）；
//  2. 另一个房间开录 → **拿到属于自己的会话**，两边互不影响；
//  3. 另一个房间停掉自己的录制，也不影响本房间的录制与入会快照。
//
// 旧的单例实现里，第 2 步必须明确报错（ErrRoomBusy）；改成 map[roomID]*Session 后，
// 每个房间独立录制，这条护栏改成验证"独立会话、独立状态、独立收尾"。
func TestE2ERecordingRoomScoped(t *testing.T) {
	rec, err := mediarec.New(t.TempDir())
	if err != nil {
		t.Fatalf("创建录制管理器失败: %v", err)
	}
	t.Cleanup(func() { rec.CloseAll() })
	ts := startServerFull(t, 0, "required", testSecret, "development", func(m *sfu.Manager) {
		m.EnableRecording(rec)
	})

	grant := func(room string, admin bool) auth.VideoGrant {
		return auth.VideoGrant{
			RoomJoin: true, Room: room, CanPublish: true, CanSubscribe: true,
			CanPublishData: true, RoomAdmin: admin,
		}
	}
	hostTok := issueToken(t, ts.cfg, "u-host1", "主持人1", grant("room-r1", true))
	memberTok := issueToken(t, ts.cfg, "u-member1", "成员1", grant("room-r1", false))
	host2Tok := issueToken(t, ts.cfg, "u-host2", "主持人2", grant("room-r2", true))

	host := dial(t, ts.url, "host1")
	defer host.close()
	joinWithToken(host, "room-r1", "主持人1", hostTok)

	member := dial(t, ts.url, "member1")
	defer member.close()
	joinWithToken(member, "room-r1", "成员1", memberTok)

	host2 := dial(t, ts.url, "host2")
	defer host2.close()
	joinWithToken(host2, "room-r2", "主持人2", host2Tok)

	// 1) 主持人开录：本房间全员（含普通成员）都该看到红点
	host.send(&protocol.Message{Type: protocol.TypeRecord, Action: protocol.ActionRecordStart})
	var sessionR1 string
	for _, p := range []*peer{host, member} {
		st := p.waitType(protocol.TypeRecordingState, 5*time.Second)
		if st.Recording == nil || !st.Recording.Active {
			t.Fatalf("[%s] 应收到录制中状态，实际 %+v", p.name, st.Recording)
		}
		sessionR1 = st.Recording.SessionID
	}
	if sessionR1 == "" {
		t.Fatal("room-r1 的录制状态应带会话 ID")
	}

	// 2) 普通成员自己开不了（前端隐藏按钮只是体验）
	member.send(&protocol.Message{Type: protocol.TypeRecord, Action: protocol.ActionRecordStart})
	if ev := waitError(t, member, protocol.ErrForbidden, 5*time.Second); !strings.Contains(ev.Reason, "录制") {
		t.Fatalf("成员被拒原因应说明是录制权限，实际 %q", ev.Reason)
	}

	// 3) 另一个房间开录：拿到属于自己的会话，与 room-r1 的会话不同
	host2.send(&protocol.Message{Type: protocol.TypeRecord, Action: protocol.ActionRecordStart})
	st2 := host2.waitType(protocol.TypeRecordingState, 5*time.Second)
	if st2.Recording == nil || !st2.Recording.Active {
		t.Fatalf("room-r2 开录后主持人2应收到录制中状态，实际 %+v", st2.Recording)
	}
	if st2.Recording.SessionID == "" || st2.Recording.SessionID == sessionR1 {
		t.Fatalf("room-r2 应拿到独立的会话 ID，room-r1=%q room-r2=%q", sessionR1, st2.Recording.SessionID)
	}

	// 4) 另一个房间停掉自己的录制：只影响自己，room-r1 必须继续录
	host2.send(&protocol.Message{Type: protocol.TypeRecord, Action: protocol.ActionRecordStop})
	if st := host2.waitType(protocol.TypeRecordingState, 5*time.Second); st.Recording == nil || st.Recording.Active {
		t.Fatalf("room-r2 停止后应为未录制状态，实际 %+v", st.Recording)
	}
	lateR1Tok := issueToken(t, ts.cfg, "u-late1", "迟到者1", grant("room-r1", true))
	lateR1 := dial(t, ts.url, "late1")
	defer lateR1.close()
	joinedR1 := joinWithToken(lateR1, "room-r1", "迟到者1", lateR1Tok)
	if joinedR1.Room == nil || joinedR1.Room.Recording == nil || !joinedR1.Room.Recording.Active {
		t.Fatalf("room-r1 的录制不应被 room-r2 的停止影响，入会快照应为录制中，实际 %+v", joinedR1.Room)
	}

	// 5) room-r2 的新入会者，快照里不该带"录制中"红点（room-r2 已经停了）
	lateTok := issueToken(t, ts.cfg, "u-late", "迟到者", grant("room-r2", true))
	late := dial(t, ts.url, "late")
	defer late.close()
	joined := joinWithToken(late, "room-r2", "迟到者", lateTok)
	if joined.Room == nil || joined.Room.Recording == nil {
		t.Fatalf("joined 缺少录制状态字段: %+v", joined.Room)
	}
	if joined.Room.Recording.Active {
		t.Fatal("room-r2 已停止录制，新入会者不应看到录制中红点")
	}

	// 6) 归属房间自己停：全员回到未录制
	host.send(&protocol.Message{Type: protocol.TypeRecord, Action: protocol.ActionRecordStop})
	if st := member.waitType(protocol.TypeRecordingState, 5*time.Second); st.Recording == nil || st.Recording.Active {
		t.Fatalf("停止后应为未录制状态，实际 %+v", st.Recording)
	}
}
