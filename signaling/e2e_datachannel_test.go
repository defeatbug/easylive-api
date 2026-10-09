// 数据通道（SCTP 数据面）端到端测试。
//
// 验证的契约：
//  1. 客户端在 pub 连接上创建的通道，服务端能收到（OnDataChannel）；
//  2. 服务端在同一条 SCTP 关联上**反向**开通道即可推送，不需要重新协商
//     （sub 连接上的 offer 流程很敏感，能不动就不动）；
//  3. 瞬时事件（表情）走数据面中继：服务端不保存，只转发给其他人；
//  4. 状态变更（举手）走信令广播并**进房间快照**，新入会者也能看到谁举了手。
package signaling_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"easylive-api/protocol"
)

// dcPair 是一条数据通道的两个方向
type dcPair struct {
	send *webrtc.DataChannel
	recv chan []byte
}

// openDataChannel 让一个测试客户端建立媒体连接并开通数据通道。
// 数据通道依赖 DTLS/SCTP，所以必须先有一条真正连上的 PeerConnection——
// 这里用一路"不写样本的音频轨"把连接拉起来（不需要真的发媒体）。
func openDataChannel(t *testing.T, p *peer) *dcPair {
	t.Helper()

	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("[%s] 创建连接失败: %v", p.name, err)
	}
	p.pubPC = pc

	// 服务端反向开的通道会通过 OnDataChannel 到达客户端
	recv := make(chan []byte, 32)
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		dc.OnMessage(func(m webrtc.DataChannelMessage) {
			buf := make([]byte, len(m.Data))
			copy(buf, m.Data)
			select {
			case recv <- buf:
			default:
			}
		})
	})

	dc, err := pc.CreateDataChannel("easylive", nil)
	if err != nil {
		t.Fatalf("[%s] 创建数据通道失败: %v", p.name, err)
	}
	opened := make(chan struct{}, 1)
	dc.OnOpen(func() {
		select {
		case opened <- struct{}{}:
		default:
		}
	})

	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, "audio", "mic")
	if err != nil {
		t.Fatalf("[%s] 创建音频轨失败: %v", p.name, err)
	}
	if _, err := pc.AddTrack(track); err != nil {
		t.Fatalf("[%s] AddTrack 失败: %v", p.name, err)
	}
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		init := c.ToJSON()
		p.send(&protocol.Message{Type: protocol.TypeCandidate, Target: protocol.TargetPub,
			Candidate: &protocol.Candidate{Candidate: init.Candidate, SDPMid: init.SDPMid, SDPMLineIndex: init.SDPMLineIndex}})
	})

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("[%s] CreateOffer 失败: %v", p.name, err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("[%s] SetLocalDescription 失败: %v", p.name, err)
	}
	<-webrtc.GatheringCompletePromise(pc)
	p.send(&protocol.Message{Type: protocol.TypePublish, SDP: pc.LocalDescription().SDP, Source: "microphone"})

	answer := p.waitType(protocol.TypePublishAnswer, 20*time.Second)
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer.SDP}); err != nil {
		t.Fatalf("[%s] 设置 answer 失败: %v", p.name, err)
	}
	p.flushCandidates()

	if !waitPCConnected(pc, 15*time.Second) {
		t.Fatalf("[%s] 连接未建立，当前状态 %s", p.name, pc.ConnectionState())
	}
	select {
	case <-opened:
	case <-time.After(15 * time.Second):
		t.Fatalf("[%s] 数据通道在 15s 内未打开", p.name)
	}
	return &dcPair{send: dc, recv: recv}
}

func TestE2EDataChannel(t *testing.T) {
	url := startServer(t)

	alice := dial(t, url, "alice")
	defer alice.close()
	alice.join("room-dc", "Alice")

	bob := dial(t, url, "bob")
	defer bob.close()
	joinedB := bob.join("room-dc", "Bob")
	_ = alice.waitType(protocol.TypeParticipantJoin, 5*time.Second)

	aliceDC := openDataChannel(t, alice)
	bobDC := openDataChannel(t, bob)

	// ---- 1) 举手：数据面上报 → 信令面广播 ----
	if err := bobDC.send.SendText(`{"type":"raise_hand","raised":true}`); err != nil {
		t.Fatalf("发送举手上报失败: %v", err)
	}
	ev := alice.waitType(protocol.TypeHandState, 10*time.Second)
	if ev.ParticipantID != joinedB.ParticipantID {
		t.Fatalf("举手事件应来自 Bob，实际: %+v", ev)
	}
	if ev.Raised == nil || !*ev.Raised {
		t.Fatalf("举手事件应标记 raised=true: %+v", ev)
	}

	// ---- 2) 表情：数据面中继给其他人（服务端不保存）----
	if err := bobDC.send.SendText(`{"type":"reaction","emoji":"👍"}`); err != nil {
		t.Fatalf("发送表情失败: %v", err)
	}
	select {
	case raw := <-aliceDC.recv:
		var m protocol.DataMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("中继内容不是合法 JSON: %s", raw)
		}
		if m.Type != protocol.DataReaction || m.Emoji != "👍" {
			t.Fatalf("中继内容不正确: %+v", m)
		}
		// 身份由服务端填充，客户端无法伪造
		if m.FromID != joinedB.ParticipantID {
			t.Fatalf("中继应带上服务端填充的 FromID=%s，实际 %q", joinedB.ParticipantID, m.FromID)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("Alice 未在数据通道上收到表情中继（反向通道不可用？）")
	}

	// ---- 3) 举手是状态：新入会者的房间快照里应能看到 ----
	carol := dial(t, url, "carol")
	defer carol.close()
	joinedC := carol.join("room-dc", "Carol")
	if joinedC.Room == nil {
		t.Fatalf("joined 未带回房间快照")
	}
	found := false
	for _, id := range joinedC.Room.Hands {
		if id == joinedB.ParticipantID {
			found = true
		}
	}
	if !found {
		t.Fatalf("房间快照应包含已举手的 Bob(%s)，实际 hands=%v", joinedB.ParticipantID, joinedC.Room.Hands)
	}

	// ---- 4) 取消举手 ----
	if err := bobDC.send.SendText(`{"type":"raise_hand","raised":false}`); err != nil {
		t.Fatalf("发送取消举手失败: %v", err)
	}
	deadline := time.After(10 * time.Second)
	for {
		select {
		case m := <-alice.events:
			if m.Type == protocol.TypeHandState && m.ParticipantID == joinedB.ParticipantID && m.Raised != nil && !*m.Raised {
				return // 通过
			}
		case <-deadline:
			t.Fatalf("未收到取消举手事件")
		}
	}
}
