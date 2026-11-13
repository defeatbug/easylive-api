package signaling_test

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"easylive-api/protocol"
)

// TestE2ESubscribeIdempotent 钉住"重复订阅会多一条 m-line"的真实缺陷。
//
// 触发场景不是理论问题：入会快照（joined 里的 tracks）与 track_published 可能对
// 同一条轨道各触发一次订阅。旧实现每次都会 AddTrack + 重新协商：
//   - 订阅连接上多出一条 m-line；
//   - subSenders 里的旧 sender 被覆盖，永远无法 RemoveTrack，订阅计数漂移。
//
// 新约定：同一条轨道重复订阅返回 **sdp 为空的 subscribe_offer**（视为 ack，无需协商）。
func TestE2ESubscribeIdempotent(t *testing.T) {
	if os.Getenv("SKIP_MEDIA_E2E") != "" {
		t.Skip("SKIP_MEDIA_E2E 已设置")
	}
	url := startServer(t)

	pub := dial(t, url, "pub")
	defer pub.close()
	sub := dial(t, url, "sub")
	defer sub.close()

	pub.join("room-idem", "Pub")
	sub.join("room-idem", "Sub")

	// --- 发布一条音频轨 ---
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("创建发布者 PeerConnection 失败: %v", err)
	}
	pub.pubPC = pc
	codec := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}
	track, err := webrtc.NewTrackLocalStaticSample(codec, "audio", "camera")
	if err != nil {
		t.Fatalf("创建本地轨道失败: %v", err)
	}
	if _, err := pc.AddTrack(track); err != nil {
		t.Fatalf("AddTrack 失败: %v", err)
	}
	pubPC := pc
	pubPC.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		init := c.ToJSON()
		pub.send(&protocol.Message{Type: protocol.TypeCandidate, Target: protocol.TargetPub,
			Candidate: &protocol.Candidate{Candidate: init.Candidate, SDPMid: init.SDPMid, SDPMLineIndex: init.SDPMLineIndex}})
	})
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("CreateOffer 失败: %v", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("SetLocalDescription 失败: %v", err)
	}
	<-webrtc.GatheringCompletePromise(pc)
	pub.send(&protocol.Message{Type: protocol.TypePublish, SDP: pc.LocalDescription().SDP})
	answer := pub.waitType(protocol.TypePublishAnswer, 20*time.Second)
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer.SDP}); err != nil {
		t.Fatalf("发布端设置 answer 失败: %v", err)
	}
	pub.flushCandidates()

	// 持续写音频，保证订阅连接上真的有包可转发
	stopWriting := make(chan struct{})
	defer close(stopWriting)
	go func() {
		frame := make([]byte, 320)
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopWriting:
				return
			case <-ticker.C:
				_ = track.WriteSample(media.Sample{Data: frame, Duration: 20 * time.Millisecond})
			}
		}
	}()

	// --- 订阅端 ---
	trackMsg := sub.waitType(protocol.TypeTrackPublished, 20*time.Second)
	if trackMsg.Track == nil {
		t.Fatal("track_published 缺少 track 信息")
	}
	trackID := trackMsg.Track.ID

	subPC, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("创建订阅者 PeerConnection 失败: %v", err)
	}
	sub.subPC = subPC
	subPC.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		init := c.ToJSON()
		sub.send(&protocol.Message{Type: protocol.TypeCandidate, Target: protocol.TargetSub,
			Candidate: &protocol.Candidate{Candidate: init.Candidate, SDPMid: init.SDPMid, SDPMLineIndex: init.SDPMLineIndex}})
	})

	// 第一次订阅：正常协商
	sub.send(&protocol.Message{Type: protocol.TypeSubscribe, TrackID: trackID})
	first := sub.waitType(protocol.TypeSubscribeOffer, 20*time.Second)
	if strings.TrimSpace(first.SDP) == "" {
		t.Fatal("第一次订阅应返回非空 SDP")
	}
	if err := subPC.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: first.SDP}); err != nil {
		t.Fatalf("订阅端设置 offer 失败: %v", err)
	}
	subAnswer, err := subPC.CreateAnswer(nil)
	if err != nil {
		t.Fatalf("订阅端 CreateAnswer 失败: %v", err)
	}
	if err := subPC.SetLocalDescription(subAnswer); err != nil {
		t.Fatalf("订阅端 SetLocalDescription 失败: %v", err)
	}
	<-webrtc.GatheringCompletePromise(subPC)
	sub.send(&protocol.Message{Type: protocol.TypeAnswer, Target: protocol.TargetSub, SDP: subPC.LocalDescription().SDP})
	sub.flushCandidates()

	mLineCount := func() int {
		desc := subPC.RemoteDescription()
		if desc == nil {
			return 0
		}
		return strings.Count(desc.SDP, "m=")
	}
	if got := mLineCount(); got != 1 {
		t.Fatalf("第一次订阅后订阅连接应有 1 条 m-line，实际 %d", got)
	}

	// 第二次订阅同一条轨道：必须幂等（空 SDP = ack），不新增 m-line
	sub.send(&protocol.Message{Type: protocol.TypeSubscribe, TrackID: trackID})
	second := sub.waitType(protocol.TypeSubscribeOffer, 20*time.Second)
	if strings.TrimSpace(second.SDP) != "" {
		t.Fatalf("重复订阅应返回空 SDP（ack），实际返回了 %d 字节的 SDP", len(second.SDP))
	}
	if second.Track == nil || second.Track.ID != trackID {
		t.Fatalf("重复订阅的应答应带回同一条轨道，实际 %+v", second.Track)
	}
	if got := mLineCount(); got != 1 {
		t.Fatalf("重复订阅不应新增 m-line，实际 %d 条", got)
	}
}
