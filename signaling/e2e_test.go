// 端到端冒烟测试：不需要浏览器，用两个 Pion 客户端验证
//  1. 信令全链路：join / 事件广播 / ping-pong / chat
//  2. 媒体转发：一个客户端 publish，另一个 subscribe，确认 RTP 真的流过来了
//
// 运行：
//
//	go test ./signaling/ -v -timeout 180s
//	SKIP_MEDIA_E2E=1 go test ./signaling/ -v        # 只跑信令
package signaling_test

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	ws "github.com/fasthttp/websocket"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"easylive-api/config"
	"easylive-api/protocol"
	"easylive-api/server"
	"easylive-api/sfu"
	"easylive-api/signaling"
)

func freeUDPPort(t *testing.T) int {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("申请 UDP 端口失败: %v", err)
	}
	port := conn.LocalAddr().(*net.UDPAddr).Port
	_ = conn.Close()
	return port
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("申请 TCP 端口失败: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

// testServer 是测试用服务器句柄（URL + 配置，鉴权测试需要拿配置签名令牌）。
type testServer struct {
	url string
	cfg *config.Config
}

// startServer 启动测试服务器（ResumeGrace=0：连接断开即离会，便于测试"立即清理"语义）。
func startServer(t *testing.T) string {
	t.Helper()
	return startServerFull(t, 0, "", "", "test").url
}

// startServerWithGrace 同上，但可指定断线恢复宽限期（M4 测试用）。
func startServerWithGrace(t *testing.T, grace time.Duration) string {
	t.Helper()
	return startServerFull(t, grace, "", "", "test").url
}

// startServerFull 允许指定鉴权参数（authMode/jwtSecret/appEnv）。
// testAPIKey 是测试用的运维密钥。
//
// /graphql、/recordings、/token 在**非 development** 环境都要求 X-API-Key
// （这是刻意加的访问控制：以前 GraphQL 完全裸挂、录制接口在没配密钥时还放行）。
// 因此测试服务器必须配置密钥，客户端必须带请求头——否则测的就不是真实配置。
const testAPIKey = "test-ops-key"

// apiKeyForEnv 按环境决定测试服务器是否配置运维密钥。
// production 用例留空，用于验证"未配置密钥"时的失败即拒绝（403）。
func apiKeyForEnv(appEnv string) string {
	if appEnv == "production" {
		return ""
	}
	return testAPIKey
}

func startServerFull(t *testing.T, grace time.Duration, authMode, jwtSecret, appEnv string) *testServer {
	t.Helper()
	port := freeTCPPort(t)
	cfg := &config.Config{
		AppName: "easylive-api-test",
		// 只有非 production 的用例才配密钥：production 用例需要能构造
		// "服务端漏配 TOKEN_API_KEY"这个场景（应当 403，而不是 401 或被放行）。
		TokenAPIKey:  apiKeyForEnv(appEnv),
		AppPort:      fmt.Sprint(port),
		MediaUDPPort: freeUDPPort(t),
		// 刻意开启 ICE-TCP：这条路径曾经因为日志工厂尚未赋值而必然 panic，
		// 只测 UDP 路径是发现不了的（ICETCPPort=0 会直接跳过整段代码）
		ICETCPPort:          freeTCPPort(t),
		AdvertiseIP:         "127.0.0.1",
		IncludeLoopback:     true,
		WSMaxConnections:    32,
		RoomMaxParticipants: 8,
		WSHeartbeat:         30 * time.Second,
		HTTPRateLimitPerIP:  1000,
		WebOrigins:          []string{"http://localhost:5173"},
		FiberConcurrency:    4096,
		EnableDebugEndpoint: true,
		ResumeGrace:         grace,
		AuthMode:            authMode,
		JWTSecret:           jwtSecret,
		TokenTTL:            time.Hour,
		AppEnv:              appEnv,
	}

	engine, err := sfu.NewEngine(cfg)
	if err != nil {
		t.Fatalf("初始化引擎失败: %v", err)
	}
	t.Cleanup(func() { _ = engine.Close() })

	manager := sfu.NewManager(engine, cfg.RoomMaxParticipants, cfg.ResumeGrace)
	hub := signaling.NewHub(manager, cfg)
	srv := server.New(cfg, manager, hub, nil) // nil store：测试不依赖数据库

	go func() { _ = srv.App().Listen("127.0.0.1:" + cfg.AppPort) }()
	t.Cleanup(func() { _ = srv.App().Shutdown() })

	url := "ws://127.0.0.1:" + cfg.AppPort + "/ws"
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c, _, err := ws.DefaultDialer.Dial(url, nil)
		if err == nil {
			_ = c.Close()
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	return &testServer{url: url, cfg: cfg}
}

// peer 是测试用客户端：一条 WS + 两条 PeerConnection（与真实前端同构）
type peer struct {
	t    *testing.T
	conn *ws.Conn
	name string

	pubPC *webrtc.PeerConnection
	subPC *webrtc.PeerConnection

	events chan *protocol.Message
	// done 在读循环退出（连接关闭/出错）时关闭，用于断言"服务端把连接关掉了"
	done chan struct{}

	mu          sync.Mutex
	pendingCand []*protocol.Message
	participant string
}

func dial(t *testing.T, url, name string) *peer {
	t.Helper()
	conn, _, err := ws.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("[%s] 连接信令失败: %v", name, err)
	}
	p := &peer{t: t, conn: conn, name: name, events: make(chan *protocol.Message, 256), done: make(chan struct{})}
	go p.readLoop()
	return p
}

func toInit(c *protocol.Candidate) webrtc.ICECandidateInit {
	return webrtc.ICECandidateInit{Candidate: c.Candidate, SDPMid: c.SDPMid, SDPMLineIndex: c.SDPMLineIndex}
}

func (p *peer) readLoop() {
	defer close(p.done)
	for {
		_, data, err := p.conn.ReadMessage()
		if err != nil {
			return
		}
		var msg protocol.Message
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}

		// ICE 候选：对端描述已就绪就直接加，否则先缓存（SetRemoteDescription 之后 flush）
		if msg.Type == protocol.TypeCandidate {
			p.mu.Lock()
			pc := p.pubPC
			if msg.Target == protocol.TargetSub {
				pc = p.subPC
			}
			remoteSet := pc != nil && pc.RemoteDescription() != nil
			if !remoteSet {
				p.pendingCand = append(p.pendingCand, &msg)
			}
			p.mu.Unlock()
			if remoteSet && msg.Candidate != nil {
				_ = pc.AddICECandidate(toInit(msg.Candidate))
			}
			continue
		}

		if msg.Type == protocol.TypeJoined {
			p.mu.Lock()
			p.participant = msg.ParticipantID
			p.mu.Unlock()
		}
		select {
		case p.events <- &msg:
		default:
		}
	}
}

func (p *peer) flushCandidates() {
	p.mu.Lock()
	cands := p.pendingCand
	p.pendingCand = nil
	p.mu.Unlock()

	for _, m := range cands {
		if m.Candidate == nil {
			continue
		}
		pc := p.pubPC
		if m.Target == protocol.TargetSub {
			pc = p.subPC
		}
		if pc != nil {
			_ = pc.AddICECandidate(toInit(m.Candidate))
		}
	}
}

func (p *peer) send(msg *protocol.Message) {
	p.t.Helper()
	if err := p.conn.WriteJSON(msg); err != nil {
		p.t.Fatalf("[%s] 发送 %s 失败: %v", p.name, msg.Type, err)
	}
}

// waitType 等待指定类型的消息，忽略其他事件；收到 error 消息直接失败。
func (p *peer) waitType(typ protocol.MsgType, timeout time.Duration) *protocol.Message {
	p.t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case m := <-p.events:
			if m.Type == protocol.TypeError {
				p.t.Fatalf("[%s] 服务端返回错误: %s %s", p.name, m.Code, m.Reason)
			}
			if m.Type == typ {
				return m
			}
		case <-deadline:
			p.t.Fatalf("[%s] 等待 %s 超时", p.name, typ)
			return nil
		}
	}
}

func (p *peer) join(roomID, displayName string) *protocol.Message {
	p.t.Helper()
	p.send(&protocol.Message{Type: protocol.TypeJoin, RoomID: roomID, DisplayName: displayName})
	return p.waitType(protocol.TypeJoined, 10*time.Second)
}

func (p *peer) close() {
	if p.pubPC != nil {
		_ = p.pubPC.Close()
	}
	if p.subPC != nil {
		_ = p.subPC.Close()
	}
	_ = p.conn.Close()
}

// ---------- 测试 1：信令链路 ----------

func TestE2ESignaling(t *testing.T) {
	url := startServer(t)

	alice := dial(t, url, "alice")
	defer alice.close()
	bob := dial(t, url, "bob")
	defer bob.close()

	joinedA := alice.join("room-1", "Alice")
	if joinedA.SessionID == "" || joinedA.ParticipantID == "" {
		t.Fatalf("joined 缺少 sessionId/participantId: %+v", joinedA)
	}
	if joinedA.Room == nil || joinedA.Room.RoomID != "room-1" || len(joinedA.Room.Participants) != 1 {
		t.Fatalf("joined 未带回正确的房间快照: %+v", joinedA.Room)
	}

	joinedB := bob.join("room-1", "Bob")
	if joinedB.Room == nil || len(joinedB.Room.Participants) != 2 {
		t.Fatalf("bob 入会后房间快照应有 2 人，实际: %+v", joinedB.Room)
	}

	ev := alice.waitType(protocol.TypeParticipantJoin, 5*time.Second)
	if ev.Participant == nil || ev.Participant.Name != "Bob" {
		t.Fatalf("期望 participant_joined(Bob)，实际: %+v", ev.Participant)
	}

	// ping / pong（同时验证请求响应会回带幂等键 id）
	alice.send(&protocol.Message{Type: protocol.TypePing, ID: "ping-1"})
	pong := alice.waitType(protocol.TypePong, 5*time.Second)
	if pong.ID != "ping-1" {
		t.Fatalf("pong 未回带请求 id: %+v", pong)
	}

	// 聊天广播
	alice.send(&protocol.Message{Type: protocol.TypeChat, Text: "hello"})
	chat := bob.waitType(protocol.TypeChatMessage, 5*time.Second)
	if chat.ChatMessage == nil || chat.ChatMessage.Text != "hello" || chat.ChatMessage.FromName != "Alice" {
		t.Fatalf("chat_message 内容不正确: %+v", chat.ChatMessage)
	}
}

// ---------- 测试 2：媒体转发（真正的 RTP 端到端） ----------

func TestE2EMediaForwarding(t *testing.T) {
	if os.Getenv("SKIP_MEDIA_E2E") != "" {
		t.Skip("SKIP_MEDIA_E2E 已设置")
	}
	url := startServer(t)

	pub := dial(t, url, "publisher")
	defer pub.close()
	sub := dial(t, url, "subscriber")
	defer sub.close()

	pub.join("room-media", "Pub")
	sub.join("room-media", "Sub")

	// --- 发布端：pub 连接 + 一条音频轨 + offer ---
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
	pub.send(&protocol.Message{Type: protocol.TypePublish, SDP: pc.LocalDescription().SDP})

	answer := pub.waitType(protocol.TypePublishAnswer, 20*time.Second)
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer.SDP}); err != nil {
		t.Fatalf("发布端设置 answer 失败: %v", err)
	}
	pub.flushCandidates()

	// 持续写入音频采样，保证有 RTP 包可被转发
	go func() {
		frame := make([]byte, 320)
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for range ticker.C {
			if pc.ConnectionState() == webrtc.PeerConnectionStateClosed {
				return
			}
			_ = track.WriteSample(media.Sample{Data: frame, Duration: 20 * time.Millisecond})
		}
	}()

	// --- 订阅端：等 track_published → subscribe → 收 RTP ---
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

	recv := make(chan int, 1)
	subPC.OnTrack(func(remote *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		count := 0
		for {
			if _, _, err := remote.ReadRTP(); err != nil {
				return
			}
			count++
			if count == 5 {
				select {
				case recv <- count:
				default:
				}
			}
		}
	})
	subPC.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		init := c.ToJSON()
		sub.send(&protocol.Message{Type: protocol.TypeCandidate, Target: protocol.TargetSub,
			Candidate: &protocol.Candidate{Candidate: init.Candidate, SDPMid: init.SDPMid, SDPMLineIndex: init.SDPMLineIndex}})
	})

	sub.send(&protocol.Message{Type: protocol.TypeSubscribe, TrackID: trackID})
	srvOffer := sub.waitType(protocol.TypeSubscribeOffer, 20*time.Second)
	if err := subPC.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: srvOffer.SDP}); err != nil {
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

	select {
	case n := <-recv:
		t.Logf("✅ 媒体转发成功：订阅端已收到 %d 个 RTP 包", n)
	case <-time.After(25 * time.Second):
		t.Fatal("超时：订阅端没有收到 RTP 包（检查 ICE 候选、SFU_ADVERTISE_IP、UDP mux 端口）")
	}
}
