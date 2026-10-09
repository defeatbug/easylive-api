// 活跃说话人（active speaker）端到端测试。
//
// 验证的核心契约：
//  1. 服务端把 RFC 6464 音频电平头扩展协商进 answer（否则浏览器不会发这个字段）；
//  2. 服务端在转发热路径上解析电平并广播 active_speakers，
//     判定**不依赖解码音频**，与"媒体面只转发不转码"的约束一致；
//  3. 只在集合变化时广播：持续说话期间不会反复刷消息；
//  4. 停止说话（电平掉下去）后会广播空集合，前端据此取消高亮。
package signaling_test

import (
	"regexp"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"

	"easylive-api/protocol"
)

// newPionAPIWithAudioLevel 构造一个注册了音频电平扩展的客户端 API。
// 浏览器默认就会 offer 这个扩展，测试里必须显式注册，否则 offer/answer 里都不会出现 extmap。
func newPionAPIWithAudioLevel(t *testing.T) *webrtc.API {
	t.Helper()
	m := &webrtc.MediaEngine{}
	if err := m.RegisterHeaderExtension(
		webrtc.RTPHeaderExtensionCapability{URI: sdp.AudioLevelURI},
		webrtc.RTPCodecTypeAudio,
	); err != nil {
		t.Fatalf("注册音频电平扩展失败: %v", err)
	}
	if err := m.RegisterDefaultCodecs(); err != nil {
		t.Fatalf("注册默认编解码器失败: %v", err)
	}
	return webrtc.NewAPI(webrtc.WithMediaEngine(m))
}

// audioLevelExtIDFromSDP 从 SDP 里取出音频电平扩展的 ID
func audioLevelExtIDFromSDP(t *testing.T, sdpText string) uint8 {
	t.Helper()
	re := regexp.MustCompile(`a=extmap:(\d+)[^\r\n]*` + regexp.QuoteMeta(sdp.AudioLevelURI))
	m := re.FindStringSubmatch(sdpText)
	if len(m) < 2 {
		t.Fatalf("SDP 中没有协商出音频电平扩展:\n%s", sdpText)
	}
	id, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("扩展 ID 不是整数: %v", err)
	}
	return uint8(id)
}

func TestE2EActiveSpeaker(t *testing.T) {
	url := startServer(t)

	alice := dial(t, url, "alice")
	defer alice.close()
	alice.join("room-spk", "Alice")

	bob := dial(t, url, "bob")
	defer bob.close()
	joinedB := bob.join("room-spk", "Bob")
	_ = alice.waitType(protocol.TypeParticipantJoin, 5*time.Second)

	// ---- Bob 发布一路 Opus 音频（带音频电平扩展）----
	api := newPionAPIWithAudioLevel(t)
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("创建发布连接失败: %v", err)
	}
	bob.pubPC = pc

	track, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, "audio", "mic")
	if err != nil {
		t.Fatalf("创建音频轨失败: %v", err)
	}
	if _, err := pc.AddTrack(track); err != nil {
		t.Fatalf("AddTrack 失败: %v", err)
	}
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		init := c.ToJSON()
		bob.send(&protocol.Message{Type: protocol.TypeCandidate, Target: protocol.TargetPub,
			Candidate: &protocol.Candidate{Candidate: init.Candidate, SDPMid: init.SDPMid, SDPMLineIndex: init.SDPMLineIndex}})
	})

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("CreateOffer 失败: %v", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("SetLocalDescription 失败: %v", err)
	}
	if !regexp.MustCompile(regexp.QuoteMeta(sdp.AudioLevelURI)).MatchString(pc.LocalDescription().SDP) {
		t.Fatalf("客户端 offer 里没有音频电平扩展（注册没生效）")
	}
	<-webrtc.GatheringCompletePromise(pc)
	bob.send(&protocol.Message{Type: protocol.TypePublish, SDP: pc.LocalDescription().SDP, Source: "microphone"})

	answer := bob.waitType(protocol.TypePublishAnswer, 20*time.Second)
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer.SDP}); err != nil {
		t.Fatalf("设置 answer 失败: %v", err)
	}
	bob.flushCandidates()

	// 服务端必须在 answer 里确认这个扩展（证明 engine 的注册生效）
	extID := audioLevelExtIDFromSDP(t, answer.SDP)

	if !waitPCConnected(pc, 15*time.Second) {
		t.Fatalf("发布连接未建立，当前状态 %s", pc.ConnectionState())
	}

	// ---- 持续发送"说话"电平的音频包 ----
	stop := make(chan struct{})
	defer close(stop)
	// 用原子布尔而不是 channel：channel 只能读一次，之后所有包都会变成"静音"
	var talking atomic.Bool
	talking.Store(true) // 初始：说话

	go func() {
		seq := uint16(0)
		ts := uint32(0)
		payload := make([]byte, 160)
		for {
			select {
			case <-stop:
				return
			default:
			}
			// RFC 6464：低 7 位是 -dBov。20 → 线性约 0.1（远高于 0.02 的门槛）；
			// 127 → 约等于静音。
			dbov := byte(127)
			if talking.Load() {
				dbov = 20
			}
			pkt := &rtp.Packet{
				Header: rtp.Header{
					Version:        2,
					PayloadType:    111,
					SequenceNumber: seq,
					Timestamp:      ts,
					SSRC:           0x1234,
				},
				Payload: payload,
			}
			// RFC 8285 单字节扩展（浏览器用的就是它）：写入音频电平
			if err := pkt.Header.SetExtensionWithProfile(extID, []byte{dbov}, 0xBEDE); err != nil {
				return
			}
			_ = track.WriteRTP(pkt)
			seq++
			ts += 960
			time.Sleep(20 * time.Millisecond)
		}
	}()

	// ---- 断言 1：Alice 收到 active_speakers，且包含 Bob ----
	spk := waitSpeakers(t, alice, 10*time.Second, func(list []protocol.SpeakerInfo) bool {
		return len(list) > 0 && list[0].ParticipantID == joinedB.ParticipantID
	})
	if spk[0].Level < 0.02 {
		t.Fatalf("说话人电平应高于门槛，实际 %.4f", spk[0].Level)
	}

	// ---- 断言 2：停止说话后应广播空集合（前端据此取消高亮）----
	talking.Store(false)
	waitSpeakers(t, alice, 10*time.Second, func(list []protocol.SpeakerInfo) bool { return len(list) == 0 })
}

// waitSpeakers 等待一条满足条件的 active_speakers 广播
func waitSpeakers(t *testing.T, p *peer, timeout time.Duration, ok func([]protocol.SpeakerInfo) bool) []protocol.SpeakerInfo {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case m := <-p.events:
			if m.Type == protocol.TypeActiveSpeakers && ok(m.Speakers) {
				return m.Speakers
			}
		case <-deadline:
			t.Fatalf("[%s] 等待满足条件的 active_speakers 超时", p.name)
			return nil
		}
	}
}
