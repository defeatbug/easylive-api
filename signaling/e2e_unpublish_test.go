// 轨道生命周期回归测试（对应真实故障：屏幕共享结束后，别人仍能看到"最后一帧"，
// 且再次共享会多出一个瓦片）。
//
// 关键约定：
//  1. track_published 也必须发给发布者自己 —— 否则发布者不知道自己轨道的服务端 trackId；
//  2. 发布者可以发 unpublish 撤销轨道，服务端必须广播 track_unpublished；
//  3. 转发热路径结束时（发布者断开等）也要走同一条下线逻辑。
package signaling_test

import (
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"easylive-api/protocol"
)

func TestE2ETrackUnpublish(t *testing.T) {
	url := startServer(t)

	pub := dial(t, url, "unpub-pub")
	defer pub.close()
	sub := dial(t, url, "unpub-sub")
	defer sub.close()

	pub.join("room-unpub", "Pub")
	sub.join("room-unpub", "Sub")

	// ---- 发布端：一条 VP8 视频轨 ----
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("创建发布者连接失败: %v", err)
	}
	pub.pubPC = pc

	capability := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000}
	track, err := webrtc.NewTrackLocalStaticSample(capability, "screen", "screen")
	if err != nil {
		t.Fatalf("创建本地视频轨失败: %v", err)
	}
	if _, err := pc.AddTrack(track); err != nil {
		t.Fatalf("AddTrack 失败: %v", err)
	}
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
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
	pub.send(&protocol.Message{Type: protocol.TypePublish, SDP: pc.LocalDescription().SDP, Source: "screen"})

	answer := pub.waitType(protocol.TypePublishAnswer, 20*time.Second)
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer.SDP}); err != nil {
		t.Fatalf("发布端设置 answer 失败: %v", err)
	}
	pub.flushCandidates()

	if !waitPCConnected(pc, 15*time.Second) {
		t.Fatalf("发布端连接未建立: %s", pc.ConnectionState())
	}
	if err := track.WriteSample(media.Sample{Data: vp8Frame(1200), Duration: 33 * time.Millisecond}); err != nil {
		t.Fatalf("写入首帧失败: %v", err)
	}

	// ---- 断言 1：发布者自己也能收到 track_published（否则它不知道自己的 trackId）----
	selfMsg := pub.waitType(protocol.TypeTrackPublished, 20*time.Second)
	if selfMsg.Track == nil {
		t.Fatal("track_published 缺少 track 信息")
	}
	if selfMsg.Track.PublisherID != pub.participant {
		t.Fatalf("发布者收到的 track_published 不属于自己: %+v（participant=%s）", selfMsg.Track, pub.participant)
	}
	trackID := selfMsg.Track.ID
	t.Logf("✅ 断言 1 通过：发布者获知自己的 trackId=%s", trackID)

	// ---- 断言 2：订阅端同样收到 track_published ----
	subMsg := sub.waitType(protocol.TypeTrackPublished, 20*time.Second)
	if subMsg.Track == nil || subMsg.Track.ID != trackID {
		t.Fatalf("订阅端收到的 track 与发布端不一致: %+v vs %s", subMsg.Track, trackID)
	}

	// ---- 断言 3：发布者 unpublish 后，订阅端收到 track_unpublished ----
	pub.send(&protocol.Message{Type: protocol.TypeUnpublish, TrackID: trackID})
	unpub := sub.waitType(protocol.TypeTrackUnpublished, 10*time.Second)
	if unpub.TrackID != trackID {
		t.Fatalf("track_unpublished 的 trackId 不匹配: 期望 %s 实际 %s", trackID, unpub.TrackID)
	}
	t.Logf("✅ 断言 3 通过：轨道下线已广播给订阅端（陈旧瓦片会被移除）")

	// ---- 断言 4：再次 unpublish 必须幂等（不报错、不重复广播）----
	pub.send(&protocol.Message{Type: protocol.TypeUnpublish, TrackID: trackID})
	select {
	case m := <-sub.events:
		if m.Type == protocol.TypeTrackUnpublished {
			t.Fatal("重复 unpublish 产生了重复的 track_unpublished 广播")
		}
	case <-time.After(800 * time.Millisecond):
		// 没有重复广播 ✓
	}
	t.Log("✅ 断言 4 通过：unpublish 幂等")
}
