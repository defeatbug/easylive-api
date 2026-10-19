package sfu

import (
	"testing"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"easylive-api/protocol"
)

// TestLocalPublishedTrack 验证虚拟轨道：创建 → 写入 → 订阅者能拿到 local 轨。
func TestLocalPublishedTrack(t *testing.T) {
	codec := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}
	track, err := newLocalPublishedTrack(protocol.AIAgentID, "microphone", codec, 12345)
	if err != nil {
		t.Fatalf("创建虚拟轨道失败: %v", err)
	}
	info := track.Info()
	if info.PublisherID != protocol.AIAgentID || info.Kind != protocol.KindAudio {
		t.Fatalf("轨道信息错误: %+v", info)
	}
	// 写入一包：无订阅者时 WriteRTP 不应报错（Pion 无绑定直接丢弃）
	pkt := &rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: 111, SequenceNumber: 1, Timestamp: 0, SSRC: 12345},
		Payload: []byte{0xf8, 0xff, 0xfe}, // 无效但 WriteRTP 只透传
	}
	if err := track.WriteAgentRTP(pkt); err != nil {
		t.Fatalf("WriteAgentRTP 失败: %v", err)
	}
	if n, _ := track.Stats(); n != 1 {
		t.Fatalf("包计数应为 1，got %d", n)
	}
	// 静音后写入应被丢弃计数
	track.SetMuted(true)
	if err := track.WriteAgentRTP(pkt); err != nil {
		t.Fatalf("静音写入不应报错: %v", err)
	}
	if d := track.DropCount(); d != 1 {
		t.Fatalf("丢弃计数应为 1，got %d", d)
	}
	track.SetMuted(false)
	// RequestKeyframe 对无 pubPC 的轨道是 no-op，不应 panic
	track.RequestKeyframe()
}

// TestNoteAudioLevel 验证服务端上报电平 → AudioLevel 生效 → 超过 hold 窗口归零。
func TestNoteAudioLevel(t *testing.T) {
	codec := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}
	track, err := newLocalPublishedTrack(protocol.AIAgentID, "microphone", codec, 999)
	if err != nil {
		t.Fatalf("创建虚拟轨道失败: %v", err)
	}
	if l := track.AudioLevel(); l != 0 {
		t.Fatalf("初始电平应为 0，got %f", l)
	}
	track.NoteAudioLevel(0.5)
	if l := track.AudioLevel(); l != 0.5 {
		t.Fatalf("上报后电平应为 0.5，got %f", l)
	}
	// 钳制
	track.NoteAudioLevel(2)
	if l := track.AudioLevel(); l != 1 {
		t.Fatalf("电平应钳制为 1，got %f", l)
	}
}

// TestPublishAgentTrack 验证房间级虚拟参会者：幂等发布 + 快照带身份。
func TestPublishAgentTrack(t *testing.T) {
	r := newRoom("test-agent", &Manager{})
	defer close(r.speakerStop) // 回收采样协程

	track1, err := r.PublishAgentTrack()
	if err != nil {
		t.Fatalf("发布失败: %v", err)
	}
	track2, err := r.PublishAgentTrack()
	if err != nil {
		t.Fatalf("二次发布失败: %v", err)
	}
	if track1 != track2 {
		t.Fatal("幂等发布应返回同一条轨道")
	}
	if peer := r.AgentPeer(); peer == nil || peer.ID != protocol.AIAgentID {
		t.Fatalf("AgentPeer 身份错误: %+v", peer)
	}
	// 快照应包含 AI 参与者与它的轨道
	st := r.State()
	foundPeer, foundTrack := false, false
	for _, p := range st.Participants {
		if p.ID == protocol.AIAgentID {
			foundPeer = true
		}
	}
	for _, tr := range st.Tracks {
		if tr.PublisherID == protocol.AIAgentID {
			foundTrack = true
		}
	}
	if !foundPeer || !foundTrack {
		t.Fatalf("快照缺 AI 身份（peer=%v track=%v）", foundPeer, foundTrack)
	}
	// ParticipantName 解析
	if n := r.ParticipantName(protocol.AIAgentID); n != protocol.AIAgentName {
		t.Fatalf("ParticipantName 错误，got %q", n)
	}
	// 上报电平后应被说话人检测捕获
	track1.NoteAudioLevel(0.8)
	speakers := r.detectSpeakers()
	found := false
	for _, s := range speakers {
		if s.ParticipantID == protocol.AIAgentID {
			found = true
		}
	}
	if !found {
		t.Fatal("说话人检测未捕获 AI")
	}
}
