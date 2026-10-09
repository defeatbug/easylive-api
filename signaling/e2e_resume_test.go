// M4 断线重连的端到端测试。
//
// 验证的核心契约（也是"重连"这件事真正难的地方）：
//  1. 连接意外断开时，房间内其他人只看到 participant_state=reconnecting，
//     **不会**看到 participant_left，参与者的轨道与订阅原地保留；
//  2. 带上 resumeToken 重连 → 复用同一个 participantId（resumed=true），
//     其他人看到 participant_state=active，房间快照里此人不会重复出现；
//  3. 令牌是一次性的：恢复成功后再用同一个令牌重连，只能得到全新参与者；
//  4. 宽限期内没有回来 → 此时才真正离会（participant_left）；
//  5. 客户端主动 leave 不走宽限期（立即离会）。
package signaling_test

import (
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"easylive-api/protocol"
)

func TestE2EReconnectResume(t *testing.T) {
	const grace = 3 * time.Second
	url := startServerWithGrace(t, grace)

	alice := dial(t, url, "alice")
	defer alice.close()
	joinedA := alice.join("room-r", "Alice")
	_ = joinedA

	bob := dial(t, url, "bob")
	joinedB := bob.join("room-r", "Bob")
	if joinedB.ResumeToken == "" {
		t.Fatalf("joined 未下发 resumeToken: %+v", joinedB)
	}
	if joinedB.Resumed {
		t.Fatalf("首次入会不应标记为 resumed: %+v", joinedB)
	}
	if ev := alice.waitType(protocol.TypeParticipantJoin, 5*time.Second); ev.Participant.ID != joinedB.ParticipantID {
		t.Fatalf("alice 未收到 Bob 入会事件: %+v", ev.Participant)
	}

	// ---- 1) 模拟网络中断：直接断开 WebSocket（不发 leave）----
	_ = bob.conn.Close()

	ev := alice.waitType(protocol.TypeParticipantState, 5*time.Second)
	if ev.ParticipantID != joinedB.ParticipantID || ev.State != protocol.StateReconnecting {
		t.Fatalf("期望 Bob 进入 reconnecting，实际: %+v", ev)
	}
	// 宽限期内不应出现离会/轨道下线
	assertNoEvent(t, alice, protocol.TypeParticipantLeave, 400*time.Millisecond)
	assertNoEvent(t, alice, protocol.TypeTrackUnpublished, 100*time.Millisecond)

	// ---- 2) 带 resumeToken 重连：复用同一身份 ----
	bob2 := dial(t, url, "bob-again")
	defer bob2.close()
	bob2.send(&protocol.Message{
		Type:        protocol.TypeJoin,
		RoomID:      "room-r",
		DisplayName: "Bob",
		ResumeToken: joinedB.ResumeToken,
	})
	rejoined := bob2.waitType(protocol.TypeJoined, 5*time.Second)
	if !rejoined.Resumed {
		t.Fatalf("重连未被识别为恢复: %+v", rejoined)
	}
	if rejoined.ParticipantID != joinedB.ParticipantID {
		t.Fatalf("恢复后 participantId 变了：%s -> %s", joinedB.ParticipantID, rejoined.ParticipantID)
	}
	if rejoined.Room == nil || len(rejoined.Room.Participants) != 2 {
		t.Fatalf("恢复后的房间快照应仍是 2 人（不能重复计入）: %+v", rejoined.Room)
	}

	ev = alice.waitType(protocol.TypeParticipantState, 5*time.Second)
	if ev.ParticipantID != joinedB.ParticipantID || ev.State != protocol.StateActive {
		t.Fatalf("期望 Bob 恢复为 active，实际: %+v", ev)
	}

	// ---- 3) 令牌一次性：再用同一个令牌只能得到新参与者 ----
	bob3 := dial(t, url, "bob-third")
	defer bob3.close()
	bob3.send(&protocol.Message{
		Type:        protocol.TypeJoin,
		RoomID:      "room-r",
		DisplayName: "Bob3",
		ResumeToken: joinedB.ResumeToken,
	})
	third := bob3.waitType(protocol.TypeJoined, 5*time.Second)
	if third.Resumed || third.ParticipantID == joinedB.ParticipantID {
		t.Fatalf("已消费的令牌不应能再次恢复: %+v", third)
	}

	// ---- 4) 宽限期耗尽 → 真正离会 ----
	carol := dial(t, url, "carol")
	defer carol.close()
	joinedC := carol.join("room-r", "Carol")
	_ = alice.waitType(protocol.TypeParticipantJoin, 5*time.Second)

	// 断开 Carol 且**不重连**：应先看到 reconnecting，宽限期耗尽后才真正离会
	_ = carol.conn.Close()
	if ev := alice.waitType(protocol.TypeParticipantState, 5*time.Second); ev.ParticipantID != joinedC.ParticipantID || ev.State != protocol.StateReconnecting {
		t.Fatalf("Carol 断线后应进入 reconnecting: %+v", ev)
	}
	left := waitLeave(t, alice, joinedC.ParticipantID, grace+5*time.Second)
	if left.ParticipantID != joinedC.ParticipantID {
		t.Fatalf("宽限期后应收到 Carol 的 participant_left，实际: %+v", left)
	}
}

func TestE2ELeaveIsImmediate(t *testing.T) {
	url := startServerWithGrace(t, 5*time.Second) // 宽限期很长，但主动 leave 必须立即生效

	alice := dial(t, url, "alice")
	defer alice.close()
	alice.join("room-l", "Alice")

	bob := dial(t, url, "bob")
	defer bob.close()
	joinedB := bob.join("room-l", "Bob")
	_ = alice.waitType(protocol.TypeParticipantJoin, 5*time.Second)

	bob.send(&protocol.Message{Type: protocol.TypeLeave})
	left := waitLeave(t, alice, joinedB.ParticipantID, 3*time.Second)
	if left.ParticipantID != joinedB.ParticipantID {
		t.Fatalf("主动离会应立即广播 participant_left: %+v", left)
	}
	assertNoEvent(t, alice, protocol.TypeParticipantState, 200*time.Millisecond)
}

// ---------- 小工具 ----------

// waitLeave 等待某个参与者的离会事件
func waitLeave(t *testing.T, p *peer, participantID string, timeout time.Duration) *protocol.Message {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case m := <-p.events:
			if m.Type == protocol.TypeParticipantLeave && m.ParticipantID == participantID {
				return m
			}
		case <-deadline:
			t.Fatalf("[%s] 等待 %s 的 participant_left 超时", p.name, participantID)
			return nil
		}
	}
}

// assertNoEvent 断言在窗口期内没有出现某类事件
func assertNoEvent(t *testing.T, p *peer, typ protocol.MsgType, window time.Duration) {
	t.Helper()
	deadline := time.After(window)
	for {
		select {
		case m := <-p.events:
			if m.Type == typ {
				t.Fatalf("[%s] 不应收到 %s: %+v", p.name, typ, m)
			}
		case <-deadline:
			return
		}
	}
}

// TestE2EResumeResetMedia 覆盖"信令恢复了、但媒体面已经死了"的重连路径。
//
// 这是最容易踩坑的一条：客户端重建 PeerConnection 会产生**新的 SSRC**，
// 如果服务端不清掉旧轨道，同一路视频就会变成两条轨道（界面上多出一张卡片）。
// 契约：resetMedia=true 的重连必须 ①保留 participantId ②清理旧轨道并广播 track_unpublished
//
//	③之后重新发布得到的是新 trackId，且发布者身份不变。
func TestE2EResumeResetMedia(t *testing.T) {
	const grace = 5 * time.Second
	url := startServerWithGrace(t, grace)

	alice := dial(t, url, "alice")
	defer alice.close()
	alice.join("room-m", "Alice")

	bob := dial(t, url, "bob")
	joinedB := bob.join("room-m", "Bob")
	_ = alice.waitType(protocol.TypeParticipantJoin, 5*time.Second)

	// ---- 1) Bob 发布一路视频 ----
	pc, err := publishVP8(t, bob, "camera")
	if err != nil {
		t.Fatalf("发布失败: %v", err)
	}
	tp := alice.waitType(protocol.TypeTrackPublished, 20*time.Second)
	if tp.Track == nil || tp.Track.PublisherID != joinedB.ParticipantID {
		t.Fatalf("Alice 应看到 Bob 的轨道: %+v", tp.Track)
	}
	oldTrackID := tp.Track.ID

	// ---- 2) 信令断开（媒体面在服务端仍然活着）----
	//
	// 这里刻意**不关**发布连接：真实场景是"客户端这边媒体面已经死了（网络切换/进程挂起），
	// 但服务端还没来得及感知"。这正是 resetMedia 存在的意义——否则服务端会一直留着这条旧轨道。
	// （如果服务端已经自行清理，resetMedia 就是幂等的空操作，也不会有重复轨道。）
	_ = bob.conn.Close()
	_ = pc // 保持连接存活，模拟服务端侧轨道仍在

	if ev := alice.waitType(protocol.TypeParticipantState, 5*time.Second); ev.State != protocol.StateReconnecting {
		t.Fatalf("Bob 应进入 reconnecting: %+v", ev)
	}

	// ---- 3) 带 resetMedia 重连 ----
	bob2 := dial(t, url, "bob-again")
	defer bob2.close()
	bob2.send(&protocol.Message{
		Type:        protocol.TypeJoin,
		RoomID:      "room-m",
		DisplayName: "Bob",
		ResumeToken: joinedB.ResumeToken,
		ResetMedia:  true,
	})
	re := bob2.waitType(protocol.TypeJoined, 10*time.Second)
	if !re.Resumed {
		t.Fatalf("身份应被恢复（resumed=true），实际: %+v", re)
	}
	if re.ParticipantID != joinedB.ParticipantID {
		t.Fatalf("resetMedia 不应改变 participantId：%s -> %s", joinedB.ParticipantID, re.ParticipantID)
	}
	if re.Room == nil || len(re.Room.Participants) != 2 {
		t.Fatalf("房间快照应仍是 2 人: %+v", re.Room)
	}

	// 旧轨道必须被清理并广播（否则界面会残留一张永远停在最后一帧的卡片）
	unpub := alice.waitType(protocol.TypeTrackUnpublished, 10*time.Second)
	if unpub.TrackID != oldTrackID {
		t.Fatalf("应清理旧轨道 %s，实际清理 %s", oldTrackID, unpub.TrackID)
	}

	// ---- 4) 重新发布：新 trackId、同一发布者 ----
	if _, err := publishVP8(t, bob2, "camera"); err != nil {
		t.Fatalf("重新发布失败: %v", err)
	}
	tp2 := alice.waitType(protocol.TypeTrackPublished, 20*time.Second)
	if tp2.Track == nil {
		t.Fatalf("重新发布后未收到 track_published")
	}
	if tp2.Track.ID == oldTrackID {
		t.Fatalf("重新发布应产生新的 trackId（旧的是 %s）", oldTrackID)
	}
	if tp2.Track.PublisherID != joinedB.ParticipantID {
		t.Fatalf("重新发布后发布者身份应不变：期望 %s，实际 %s", joinedB.ParticipantID, tp2.Track.PublisherID)
	}
}

// publishVP8 在一条新的 PeerConnection 上发布一路 VP8 视频轨，并等到服务端确认建轨。
// 注意：必须等连接 established 之后再写首帧，否则包会被未就绪的 SRTP 丢掉，
// 而本函数只写一帧——丢了服务端就永远不会触发 OnTrack。
func publishVP8(t *testing.T, p *peer, source string) (*webrtc.PeerConnection, error) {
	t.Helper()
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, err
	}
	p.pubPC = pc

	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000}, "video", "cam")
	if err != nil {
		return nil, err
	}
	if _, err := pc.AddTrack(track); err != nil {
		return nil, err
	}
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		init := c.ToJSON()
		p.send(&protocol.Message{
			Type:   protocol.TypeCandidate,
			Target: protocol.TargetPub,
			Candidate: &protocol.Candidate{
				Candidate: init.Candidate, SDPMid: init.SDPMid, SDPMLineIndex: init.SDPMLineIndex,
			},
		})
	})

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return nil, err
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		return nil, err
	}
	<-webrtc.GatheringCompletePromise(pc)
	p.send(&protocol.Message{Type: protocol.TypePublish, SDP: pc.LocalDescription().SDP, Source: source})

	answer := p.waitType(protocol.TypePublishAnswer, 20*time.Second)
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer.SDP}); err != nil {
		return nil, err
	}
	p.flushCandidates()

	if !waitPCConnected(pc, 15*time.Second) {
		t.Fatalf("[%s] 发布连接未建立，当前状态 %s", p.name, pc.ConnectionState())
	}
	if err := track.WriteSample(media.Sample{Data: vp8Frame(1200), Duration: 33 * time.Millisecond}); err != nil {
		return nil, err
	}
	return pc, nil
}
