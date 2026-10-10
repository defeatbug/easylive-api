package loadtest

import (
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"easylive-api/protocol"
	"easylive-api/sfu"
)

// Options 是一路压测客户端的配置。
type Options struct {
	WSURL       string
	RoomID      string
	Identity    string // 身份前缀，实际身份为 <prefix>-<index>
	Token       string
	VideoKbps   int // 每路视频目标码率，0 表示不发布视频
	VideoFPS    int
	AudioKbps   int // 每路音频目标码率，0 表示不发布音频
	Subscribe   bool
	SampleEvery time.Duration
	Timeout     time.Duration
	ICE         []iceServerJSON // 可在多路之间共享（避免每路都请求一次 /ice）
	// Debug 打开后会打印协商细节（payload type、扩展 ID、extmap 行），
	// 用于排查"连接正常但收不到媒体/测不到延迟"这类问题
	Debug bool
}

func (o Options) withDefaults() Options {
	if o.VideoFPS <= 0 {
		o.VideoFPS = 30
	}
	if o.SampleEvery <= 0 {
		o.SampleEvery = time.Second
	}
	if o.Timeout <= 0 {
		o.Timeout = 15 * time.Second
	}
	return o
}

// Sample 是一路客户端在一个采样周期内的观测值。
type Sample struct {
	At            time.Time `json:"at"`
	Lane          int       `json:"lane"`
	Identity      string    `json:"identity"`
	ParticipantID string    `json:"participantId"`
	Connected     bool      `json:"connected"`
	Error         string    `json:"error,omitempty"`

	// 链路（来自 RTCPeerConnection.GetStats）
	RTTMs       float64 `json:"rttMs"`
	JitterMs    float64 `json:"jitterMs"`
	LossRate    float64 `json:"lossRate"`
	PacketsLost int64   `json:"packetsLost"`

	// 流量
	InKbps  float64 `json:"inKbps"`
	OutKbps float64 `json:"outKbps"`

	// 单向延迟（毫秒）：由自定义头扩展携带发送时刻，端到端测量（同机时钟一致）
	LatencyP50     float64 `json:"latencyP50Ms"`
	LatencyP95     float64 `json:"latencyP95Ms"`
	LatencyMax     float64 `json:"latencyMaxMs"`
	LatencySamples int     `json:"latencySamples"`

	PublishTracks   int `json:"publishTracks"`
	SubscribeTracks int `json:"subscribeTracks"`
}

// RunLane 运行一路压测客户端，直到 ctx 取消。它不会 panic，所有错误都通过 Sample.Error 上报。
func RunLane(ctx context.Context, opts Options, index int, sink func(Sample)) {
	o := opts.withDefaults()
	identity := fmt.Sprintf("%s-%02d", o.Identity, index)
	out := Sample{Lane: index, Identity: identity}

	fail := func(err error) {
		out.At = time.Now()
		out.Error = err.Error()
		sink(out)
	}

	wsc, err := dialWS(ctx, o.WSURL)
	if err != nil {
		fail(err)
		return
	}
	defer wsc.close()

	// ---- 入会 ----
	joinMsg := &protocol.Message{Type: protocol.TypeJoin, RoomID: o.RoomID, DisplayName: identity}
	if o.Token != "" {
		joinMsg.Token = o.Token
	}
	joined, err := wsc.request(ctx, joinMsg, protocol.TypeJoined, o.Timeout)
	if err != nil {
		fail(fmt.Errorf("入会失败: %w", err))
		return
	}
	out.ParticipantID = joined.ParticipantID
	defer func() {
		_ = wsc.send(&protocol.Message{Type: protocol.TypeLeave})
	}()

	// ---- 媒体引擎：注册延迟测量扩展（与浏览器一样用默认拦截器）----
	api, err := newLoadAPI()
	if err != nil {
		fail(err)
		return
	}

	cfg := webrtc.Configuration{}
	for _, s := range o.ICE {
		cfg.ICEServers = append(cfg.ICEServers, webrtc.ICEServer{
			URLs: s.URLs, Username: s.Username, Credential: s.Credential,
			CredentialType: webrtc.ICECredentialTypePassword,
		})
	}

	lane := &laneState{
		opts:     o,
		identity: identity,
		ws:       wsc,
		pending:  map[string][]webrtc.ICECandidateInit{},
		latency:  &latencyWindow{},
	}

	// ---- 发布连接（客户端是 offerer）----
	pubPC, err := api.NewPeerConnection(cfg)
	if err != nil {
		fail(fmt.Errorf("创建发布连接失败: %w", err))
		return
	}
	defer func() { _ = pubPC.Close() }()
	lane.pubPC = pubPC
	pubPC.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		init := c.ToJSON()
		_ = wsc.send(&protocol.Message{Type: protocol.TypeCandidate, Target: protocol.TargetPub,
			Candidate: &protocol.Candidate{Candidate: init.Candidate, SDPMid: init.SDPMid, SDPMLineIndex: init.SDPMLineIndex}})
	})

	var senders []*rtpSender
	if o.VideoKbps > 0 {
		s, serr := lane.addVideoTrack(pubPC, api)
		if serr != nil {
			fail(serr)
			return
		}
		senders = append(senders, s)
		lane.senders = append(lane.senders, s)
	}
	if o.AudioKbps > 0 {
		s, serr := lane.addAudioTrack(pubPC, api)
		if serr != nil {
			fail(serr)
			return
		}
		senders = append(senders, s)
		lane.senders = append(lane.senders, s)
	}
	out.PublishTracks = len(senders)

	if err := lane.publish(ctx, pubPC); err != nil {
		fail(err)
		return
	}

	// ---- 订阅连接（服务端是 offerer）----
	subPC, err := api.NewPeerConnection(cfg)
	if err != nil {
		fail(fmt.Errorf("创建订阅连接失败: %w", err))
		return
	}
	defer func() { _ = subPC.Close() }()
	lane.subPC = subPC
	subPC.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		init := c.ToJSON()
		_ = wsc.send(&protocol.Message{Type: protocol.TypeCandidate, Target: protocol.TargetSub,
			Candidate: &protocol.Candidate{Candidate: init.Candidate, SDPMid: init.SDPMid, SDPMLineIndex: init.SDPMLineIndex}})
	})
	subPC.OnTrack(func(remote *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		go lane.readRemote(remote)
	})

	// 订阅协商必须串行：服务端在 sub 连接上是 offerer，并发会撞 LocalDescription 状态
	lane.subQueue = make(chan struct{}, 1)
	lane.subQueue <- struct{}{}

	if o.Subscribe {
		for _, t := range joined.Room.Tracks {
			if t.PublisherID != joined.ParticipantID {
				lane.subscribe(ctx, t.ID)
			}
		}
	}

	// ---- 事件循环（候选人、后来者的轨道、连接状态）----
	go lane.handleEvents(ctx, joined.ParticipantID)

	// ---- 发送合成媒体 ----
	sendCtx, stopSend := context.WithCancel(ctx)
	defer stopSend()
	for _, s := range senders {
		s.debug = o.Debug
		if o.Debug {
			id := lane.identity
			s.debugf = func(format string, args ...any) {
				log.Printf("loadtest["+id+"]: "+format, args...)
			}
		}
		go s.run(sendCtx)
	}

	// ---- 采样循环 ----
	ticker := time.NewTicker(o.SampleEvery)
	defer ticker.Stop()
	acc := &rateAccumulator{}
	var lastSentBytes uint64
	lastAt := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			out.At = time.Now()
			out.Connected = pubPC.ConnectionState() == webrtc.PeerConnectionStateConnected
			if o.Debug && !lane.loggedPairs {
				lane.loggedPairs = true
				log.Printf("loadtest[%s]: candidate-pairs %v", identity, describePairs(pubPC, subPC))
			}
			acc.fill(pubPC, subPC, &out)
			lane.latency.take(&out)

			// 上行码率用发送端自计数（GetStats 不提供 outbound-rtp）
			var sentPackets, sentBytes uint64
			for _, sd := range senders {
				sentPackets += sd.sentPackets.Load()
				sentBytes += sd.sentBytes.Load()
			}
			if dt := out.At.Sub(lastAt).Seconds(); dt > 0 {
				out.OutKbps = float64(sentBytes-lastSentBytes) * 8 / 1000 / dt
			}
			lastAt, lastSentBytes = out.At, sentBytes
			out.PublishTracks = len(senders)
			if sentPackets == 0 && len(senders) > 0 && acc.initialized {
				var fails uint64
				for _, sd := range senders {
					fails += sd.writeFails.Load()
				}
				if fails > 0 {
					out.Error = fmt.Sprintf("媒体写出失败 %d 次（协商未完成或编解码不匹配）", fails)
				}
			}
			sink(out)
		}
	}
}

// ---------- lane 内部状态 ----------

type laneState struct {
	opts     Options
	identity string
	ws       *wsConn

	mu       sync.Mutex
	pubPC    *webrtc.PeerConnection
	subPC    *webrtc.PeerConnection
	pending  map[string][]webrtc.ICECandidateInit // target -> 候选（远端描述未就绪时缓存）
	subQueue chan struct{}
	subCount int
	// pubExtID 是发布连接协商出来的延迟扩展 ID；subExtID 是订阅连接的。
	// 两者不一致时无法可靠解析延迟，会关闭延迟测量（如实记录而不是给假数据）。
	pubExtID uint8
	subExtID uint8
	warned   bool

	// senders 保存本路发布的发送器，协商完成后用它回填 payload type
	senders []*rtpSender
	// loggedPairs 保证候选对详情只打印一次
	loggedPairs bool

	latency *latencyWindow
}

// rtpSender 封装"按目标码率持续发送合成 RTP"所需的全部参数。
type rtpSender struct {
	track      *webrtc.TrackLocalStaticRTP
	payloadTyp uint8
	ssrc       uint32
	clockRate  uint32
	// payloadBytes 是每个包的载荷大小（按码率反推）
	payloadBytes int
	// packets 是每帧拆成的包数；lastPacketBytes 是末包载荷长度（用于精确对齐目标码率）
	interval        time.Duration
	packets         int
	lastPacketBytes int
	isVideo         bool
	extID           uint8
	startedAt       time.Time

	seq       uint16
	timestamp uint32

	// 自己计数而不是读 GetStats：**Pion 的 GetStats 不产出 outbound-rtp 统计**
	// （只收集 ICE/DTLS/SCTP/DataChannel/PeerConnection 与接收端入向统计），
	// 所以上行码率必须由发送方自己统计——好在我们本来就是逐包写出的，最准确。
	sentPackets atomic.Uint64
	sentBytes   atomic.Uint64
	writeFails  atomic.Uint64
	extErr      atomic.Uint64

	debug  bool
	debugf func(format string, args ...any)
}

func (s *rtpSender) run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	payload := make([]byte, s.payloadBytes)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for i := 0; i < s.packets; i++ {
				// 末包按剩余字节补齐：只用固定 1200 字节的整包会让实际码率明显低于目标
				// （目标 800kbps@30fps 需要 3333 字节/帧，两个整包只有 2400，实测就成了 576kbps）
				body := payload
				if s.isVideo && i == s.packets-1 && s.lastPacketBytes > 0 && s.lastPacketBytes <= len(payload) {
					body = payload[:s.lastPacketBytes]
				}
				pkt := &rtp.Packet{
					Header: rtp.Header{
						Version:        2,
						PayloadType:    s.payloadTyp,
						SequenceNumber: s.seq,
						Timestamp:      s.timestamp,
						SSRC:           s.ssrc,
						Marker:         s.isVideo && i == s.packets-1,
					},
					Payload: body,
				}
				if s.isVideo && s.extID > 0 {
					// 发送时刻（毫秒，截断到 32 位）：订阅端用"收到时间 - 这个值"算单向延迟。
					// 必须与接收端共用**进程级**原点（latencyBase），否则会有常量偏差甚至负数。
					var ts [4]byte
					binary.BigEndian.PutUint32(ts[:], uint32(time.Since(latencyBase).Milliseconds()))
					if err := pkt.Header.SetExtensionWithProfile(s.extID, ts[:], 0xBEDE); err != nil {
						s.extErr.Add(1) // 不再静默：写扩展失败会让延迟测量全空
					}
					if s.debug && s.sentPackets.Load() < 3 {
						s.debugf("发送包 ext=%v extIDs=%v extID=%d len1=%d",
							pkt.Header.Extension, pkt.Header.GetExtensionIDs(), s.extID, len(pkt.Header.GetExtension(s.extID)))
					}
				}
				if werr := s.track.WriteRTP(pkt); werr != nil {
					// 通常是"尚未绑定发送端"（协商未完成）或编解码不匹配。
					// 静默丢包在这里很危险：表现为"连接正常但对方收不到"，所以要计数。
					s.writeFails.Add(1)
				} else {
					s.sentPackets.Add(1)
					s.sentBytes.Add(uint64(len(pkt.Payload)))
				}
				s.seq++
			}
			// 一个 tick 内发完一帧（视频）或多个音频包
			s.timestamp += uint32(float64(s.clockRate) * s.interval.Seconds())
		}
	}
}

// ---------- 建轨与协商 ----------

func newLoadAPI() (*webrtc.API, error) {
	m := &webrtc.MediaEngine{}
	if err := m.RegisterHeaderExtension(
		webrtc.RTPHeaderExtensionCapability{URI: sfu.LatencyExtensionURI},
		webrtc.RTPCodecTypeVideo,
	); err != nil {
		return nil, fmt.Errorf("注册延迟扩展失败: %w", err)
	}
	if err := m.RegisterDefaultCodecs(); err != nil {
		return nil, err
	}
	ir := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(m, ir); err != nil {
		return nil, err
	}
	return webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(ir)), nil
}

func (l *laneState) addVideoTrack(pc *webrtc.PeerConnection, _ *webrtc.API) (*rtpSender, error) {
	track, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000}, "video", "loadtest")
	if err != nil {
		return nil, fmt.Errorf("创建视频轨失败: %w", err)
	}
	sender, err := pc.AddTrack(track)
	if err != nil {
		return nil, fmt.Errorf("添加视频轨失败: %w", err)
	}
	// 1200 字节是常见 MTU 内的安全值（留足 IP/UDP/SRTP 头空间）
	const videoPayloadBytes = 1200
	frameBytes := l.opts.VideoKbps * 1000 / 8 / l.opts.VideoFPS
	// 必须**向上取整**：3333 字节的帧用 1200 字节的包要 3 个（2 整 + 1 残），
	// 向下取整只能装 2400 字节，实测码率会比目标低 25%（640 vs 800 kbps）。
	packets := (frameBytes + videoPayloadBytes - 1) / videoPayloadBytes
	if packets < 1 {
		packets = 1
	}
	last := frameBytes - (packets-1)*videoPayloadBytes
	if last <= 0 {
		last = videoPayloadBytes
	}
	if last > videoPayloadBytes {
		last = videoPayloadBytes
	}
	return &rtpSender{
		track:           track,
		clockRate:       90000,
		isVideo:         true,
		interval:        time.Second / time.Duration(l.opts.VideoFPS),
		packets:         packets,
		lastPacketBytes: last,
		payloadBytes:    videoPayloadBytes,
		startedAt:       time.Now(),
		ssrc:            ssrcOf(sender),
	}, nil
}

func (l *laneState) addAudioTrack(pc *webrtc.PeerConnection, _ *webrtc.API) (*rtpSender, error) {
	track, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, "audio", "loadtest")
	if err != nil {
		return nil, fmt.Errorf("创建音频轨失败: %w", err)
	}
	sender, err := pc.AddTrack(track)
	if err != nil {
		return nil, fmt.Errorf("添加音频轨失败: %w", err)
	}
	// 每个 20ms 包按码率反推载荷大小：kbps * 1000 / 8 / 50 字节
	bytesPerPacket := l.opts.AudioKbps * 1000 / 8 / 50
	if bytesPerPacket < 20 {
		bytesPerPacket = 20
	}
	return &rtpSender{
		track:        track,
		clockRate:    48000,
		interval:     20 * time.Millisecond,
		packets:      1,
		payloadBytes: bytesPerPacket,
		startedAt:    time.Now(),
		ssrc:         ssrcOf(sender),
	}, nil
}

// packetsPerFrame 按目标码率与帧率反推一帧需要拆成几个 RTP 包。
func packetsPerFrame(kbps, fps int) int {
	if fps <= 0 {
		fps = 30
	}
	frameBytes := kbps * 1000 / 8 / fps
	n := frameBytes / 1200
	if n < 1 {
		n = 1
	}
	return n
}

func ssrcOf(sender *webrtc.RTPSender) uint32 {
	if p := sender.GetParameters(); len(p.Encodings) > 0 {
		return uint32(p.Encodings[0].SSRC)
	}
	return 0
}

// publish 完成发布协商（客户端 offer / 服务端 answer）。
func (l *laneState) publish(ctx context.Context, pc *webrtc.PeerConnection) error {
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return fmt.Errorf("创建 offer 失败: %w", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		return fmt.Errorf("设置本地描述失败: %w", err)
	}
	<-webrtc.GatheringCompletePromise(pc)

	l.pubExtID = extIDFromSDP(pc.LocalDescription().SDP, sfu.LatencyExtensionURI)

	resp, err := l.ws.request(ctx, &protocol.Message{
		Type: protocol.TypePublish, SDP: pc.LocalDescription().SDP, Source: "camera",
	}, protocol.TypePublishAnswer, l.opts.Timeout)
	if err != nil {
		return fmt.Errorf("发布失败: %w", err)
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: resp.SDP}); err != nil {
		return fmt.Errorf("设置远端 answer 失败: %w", err)
	}
	l.flushCandidates(protocol.TargetPub)

	// 协商完成后才能确定各轨的 payload type
	l.fillPayloadTypes(pc.LocalDescription().SDP)
	if l.opts.Debug {
		local := pc.LocalDescription().SDP
		log.Printf("loadtest[%s]: 发布协商完成 延迟扩展ID=%d extmap=%v payloadTypes=%v",
			l.identity, l.pubExtID, extmapLines(local), trackTypes(l.senders))
	}
	return nil
}

// extmapLines 提取 SDP 里的 a=extmap 行（排查扩展没协商上时最有用）。
func extmapLines(sdpText string) []string {
	out := []string{}
	for _, line := range strings.Split(sdpText, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "a=extmap:") {
			out = append(out, line)
		}
	}
	return out
}

// trackTypes 返回各发送器最终使用的 payload type（0 表示没解析到，会静默丢包）。
func trackTypes(senders []*rtpSender) map[string]uint8 {
	out := map[string]uint8{}
	for _, s := range senders {
		kind := "audio"
		if s.isVideo {
			kind = "video"
		}
		out[kind] = s.payloadTyp
	}
	return out
}

// fillPayloadTypes 从已协商的 SDP 里取 VP8 / Opus 的 payload type 并写回各发送器。
//
// 必须从 SDP 取而不是硬编码：payload type 是协商结果（不同浏览器/服务端可能不同），
// 而我们发的是手写 RTP 包——用错 PT 会被对端直接丢弃，表现为"连接正常但收不到媒体"。
func (l *laneState) fillPayloadTypes(sdpText string) {
	// 发送器保存在 lane 上（简化为按序匹配：视频在前、音频在后）
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.senders == nil {
		return
	}
	for _, s := range l.senders {
		name := "opus"
		if s.isVideo {
			name = "VP8"
			// 视频轨才带延迟测量扩展（音频包太小，且延迟主要由视频体现）。
			// 之前漏了这一行：协商到的 ID 只存在 lane 上，发送器仍然是 0，
			// 于是"设置了扩展"的分支从未执行，接收端一个延迟样本都拿不到。
			s.extID = l.pubExtID
		}
		if pt, ok := payloadTypeFromSDP(sdpText, name); ok {
			s.payloadTyp = pt
		}
	}
}

func (l *laneState) subscribe(ctx context.Context, trackID string) {
	select {
	case <-l.subQueue:
	case <-ctx.Done():
		return
	}
	defer func() { l.subQueue <- struct{}{} }()

	pc := l.subPC
	if pc == nil {
		return
	}
	resp, err := l.ws.request(ctx, &protocol.Message{Type: protocol.TypeSubscribe, TrackID: trackID},
		protocol.TypeSubscribeOffer, l.opts.Timeout)
	if err != nil {
		return
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: resp.SDP}); err != nil {
		return
	}
	l.flushCandidates(protocol.TargetSub)
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return
	}
	if err := pc.SetLocalDescription(answer); err != nil {
		return
	}
	if l.subExtID == 0 {
		l.subExtID = extIDFromSDP(pc.LocalDescription().SDP, sfu.LatencyExtensionURI)
	}
	_ = l.ws.send(&protocol.Message{Type: protocol.TypeAnswer, Target: protocol.TargetSub, ID: resp.ID, SDP: pc.LocalDescription().SDP})

	l.mu.Lock()
	l.subCount++
	l.mu.Unlock()
}

// handleEvents 处理服务端事件：ICE 候选、后来者的新轨道。
func (l *laneState) handleEvents(ctx context.Context, selfID string) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-l.ws.events:
			if !ok {
				return
			}
			switch msg.Type {
			case protocol.TypeCandidate:
				if msg.Candidate == nil {
					continue
				}
				target := msg.Target
				l.mu.Lock()
				pc := l.pubPC
				if target == protocol.TargetSub {
					pc = l.subPC
				}
				ready := pc != nil && pc.RemoteDescription() != nil
				if !ready {
					init := webrtc.ICECandidateInit{Candidate: msg.Candidate.Candidate, SDPMid: msg.Candidate.SDPMid, SDPMLineIndex: msg.Candidate.SDPMLineIndex}
					l.pending[target] = append(l.pending[target], init)
				}
				l.mu.Unlock()
				if ready && pc != nil {
					_ = pc.AddICECandidate(webrtc.ICECandidateInit{
						Candidate: msg.Candidate.Candidate, SDPMid: msg.Candidate.SDPMid, SDPMLineIndex: msg.Candidate.SDPMLineIndex,
					})
				}
			case protocol.TypeTrackPublished:
				if msg.Track == nil || msg.Track.PublisherID == selfID || !l.opts.Subscribe {
					continue
				}
				go l.subscribe(ctx, msg.Track.ID)
			}
		}
	}
}

func (l *laneState) flushCandidates(target string) {
	l.mu.Lock()
	list := l.pending[target]
	l.pending[target] = nil
	pc := l.pubPC
	if target == protocol.TargetSub {
		pc = l.subPC
	}
	l.mu.Unlock()
	if pc == nil {
		return
	}
	for _, c := range list {
		_ = pc.AddICECandidate(c)
	}
}

// readRemote 持续读取订阅到的 RTP，既保持接收路径通畅，也顺带解析延迟扩展。
func (l *laneState) readRemote(remote *webrtc.TrackRemote) {
	seen := 0
	for {
		pkt, _, err := remote.ReadRTP()
		if err != nil {
			return
		}
		if l.opts.Debug && seen < 3 {
			seen++
			log.Printf("loadtest[%s]: 收到远端包 kind=%s hasExt=%v extIDs=%v ext1=%d ext_sub=%d",
				l.identity, remote.Kind().String(), pkt.Header.Extension, pkt.Header.GetExtensionIDs(),
				len(pkt.Header.GetExtension(l.pubExtID)), len(pkt.Header.GetExtension(l.subExtID)))
		}
		ids := []uint8{l.pubExtID, l.subExtID}
		var raw []byte
		for _, id := range ids {
			if id == 0 {
				continue
			}
			if v := pkt.Header.GetExtension(id); len(v) >= 4 {
				raw = v
				break
			}
		}
		if len(raw) < 4 {
			continue
		}
		sent := time.Duration(binary.BigEndian.Uint32(raw[:4])) * time.Millisecond
		delta := time.Since(latencyBase) - sent
		if delta < 0 || delta > 10*time.Second {
			continue // 明显异常（时钟或解析问题）不计入
		}
		l.latency.add(float64(delta.Milliseconds()))
	}
}

// ---------- 辅助 ----------

// latencyBase 是延迟测量的**进程级**时间原点。
//
// 这台机器上的所有 lane 共享它：发送端写 time.Since(latencyBase)，接收端用同一个基准做减法。
// 踩过的坑：一开始用"每路各自的启动时刻"做原点，发送端与接收端相差几百毫秒，
// 算出来的延迟是负数，被健壮性检查当异常数据丢光——表现为"扩展协商成功却一个延迟样本都没有"。
var latencyBase = time.Now()

var (
	rtpmapRe = regexp.MustCompile(`a=rtpmap:(\d+) ([A-Za-z0-9]+)/`)
	extmapRe = regexp.MustCompile(`a=extmap:(\d+)(?:/[a-z]+)? ([^\r\n]+)`)
)

func payloadTypeFromSDP(sdpText, codecName string) (uint8, bool) {
	for _, m := range rtpmapRe.FindAllStringSubmatch(sdpText, -1) {
		if len(m) < 3 || !equalFold(m[2], codecName) {
			continue
		}
		pt, err := strconv.Atoi(m[1])
		if err == nil && pt >= 0 && pt <= 127 {
			return uint8(pt), true
		}
	}
	return 0, false
}

func extIDFromSDP(sdpText, uri string) uint8 {
	for _, m := range extmapRe.FindAllStringSubmatch(sdpText, -1) {
		if len(m) < 3 {
			continue
		}
		if !containsFold(m[2], uri) {
			continue
		}
		id, err := strconv.Atoi(m[1])
		if err == nil && id > 0 && id <= 14 {
			return uint8(id)
		}
	}
	return 0
}

func equalFold(a, b string) bool {
	return len(a) == len(b) && (a == b || stringsEqualFold(a, b))
}

func containsFold(haystack, needle string) bool {
	if len(needle) == 0 || len(haystack) < len(needle) {
		return false
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if equalFold(haystack[i:i+len(needle)], needle) {
			return true
		}
	}
	return false
}

func stringsEqualFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// ---------- 延迟窗口与速率统计 ----------

// latencyWindow 收集一个采样周期内的延迟样本并给出百分位。
type latencyWindow struct {
	mu      sync.Mutex
	samples []float64
}

func (w *latencyWindow) add(ms float64) {
	w.mu.Lock()
	if len(w.samples) < 20000 {
		w.samples = append(w.samples, ms)
	}
	w.mu.Unlock()
}

func (w *latencyWindow) take(out *Sample) {
	w.mu.Lock()
	s := w.samples
	w.samples = nil
	w.mu.Unlock()
	if len(s) == 0 {
		return
	}
	sort.Float64s(s)
	out.LatencySamples = len(s)
	out.LatencyP50 = percentile(s, 0.50)
	out.LatencyP95 = percentile(s, 0.95)
	out.LatencyMax = s[len(s)-1]
}

// percentile 用最近秩法（nearest-rank）：样本量小的时候比分位插值更保守，
// 不会因为插值把 P95 算得比实际观测到的最小值还小。
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(p*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// rateAccumulator 把 GetStats 的累计值换算成每秒速率。
type rateAccumulator struct {
	lastAt                 time.Time
	lastIn, lastOut        int64
	lastRecvPkts, lastLost int64
	lastSentPkts           int64
	initialized            bool
}

func (a *rateAccumulator) fill(pubPC, subPC *webrtc.PeerConnection, out *Sample) {
	var (
		inBytes, outBytes  int64
		recvPkts, lostPkts int64
		jitter             float64
		rtt                float64
	)
	for _, pc := range []*webrtc.PeerConnection{pubPC, subPC} {
		if pc == nil {
			continue
		}
		for _, st := range pc.GetStats() {
			switch v := st.(type) {
			case webrtc.InboundRTPStreamStats:
				inBytes += int64(v.BytesReceived)
				recvPkts += int64(v.PacketsReceived)
				lostPkts += int64(v.PacketsLost)
				if v.Jitter > jitter {
					jitter = v.Jitter
				}
			case *webrtc.InboundRTPStreamStats:
				inBytes += int64(v.BytesReceived)
				recvPkts += int64(v.PacketsReceived)
				lostPkts += int64(v.PacketsLost)
				if v.Jitter > jitter {
					jitter = v.Jitter
				}
			case webrtc.OutboundRTPStreamStats:
				outBytes += int64(v.BytesSent)
			case *webrtc.OutboundRTPStreamStats:
				outBytes += int64(v.BytesSent)
			// RTT 只取**被提名**的候选对（真正在传媒体的那条）。
			// 踩过的坑：一开始取所有 succeeded 对的最大值，结果 ICE-TCP 那条兜底路径
			// （TCP 小包在 loopback 上受延迟 ACK 影响，RTT 约 90-190ms）把 UDP 的真实值
			// （loopback <1ms）完全盖住，测出来的 RTT 高得离谱。
			case webrtc.ICECandidatePairStats:
				if v.Nominated && v.CurrentRoundTripTime > 0 {
					rtt = v.CurrentRoundTripTime
				}
			case *webrtc.ICECandidatePairStats:
				if v.Nominated && v.CurrentRoundTripTime > 0 {
					rtt = v.CurrentRoundTripTime
				}
			}
		}
	}

	now := time.Now()
	out.RTTMs = rtt * 1000
	out.JitterMs = jitter * 1000
	out.PacketsLost = lostPkts

	if !a.initialized {
		a.initialized = true
		a.lastAt = now
		a.lastIn, a.lastOut = inBytes, outBytes
		a.lastRecvPkts, a.lastLost = recvPkts, lostPkts
		return
	}
	dt := now.Sub(a.lastAt).Seconds()
	if dt <= 0 {
		return
	}
	dIn := inBytes - a.lastIn
	dOut := outBytes - a.lastOut
	dRecv := recvPkts - a.lastRecvPkts
	dLost := lostPkts - a.lastLost
	a.lastAt = now
	a.lastIn, a.lastOut = inBytes, outBytes
	a.lastRecvPkts, a.lastLost = recvPkts, lostPkts

	out.InKbps = float64(dIn) * 8 / 1000 / dt
	out.OutKbps = float64(dOut) * 8 / 1000 / dt
	if dRecv+dLost > 0 {
		out.LossRate = float64(dLost) / float64(dRecv+dLost) * 100
	}
}

// 避免未使用告警（logger 在调试时很有用）
var _ = log.Printf

// describePairs 列出候选对的状态、协议与 RTT。
//
// 排查"RTT 异常高"时这是最关键的一组信息：能直接看出走的是 UDP 还是 ICE-TCP（TCP 兜底路径）、
// 以及本地候选的类型（host / srflx / relay）。
func describePairs(pcs ...*webrtc.PeerConnection) []string {
	out := []string{}
	for _, pc := range pcs {
		if pc == nil {
			continue
		}
		local := map[string]string{}
		for _, st := range pc.GetStats() {
			switch v := st.(type) {
			case webrtc.ICECandidateStats:
				local[v.ID] = candidateTypeName(v.CandidateType) + "/" + v.Protocol
			case *webrtc.ICECandidateStats:
				local[v.ID] = candidateTypeName(v.CandidateType) + "/" + v.Protocol
			}
		}
		for _, st := range pc.GetStats() {
			var state string
			var rtt float64
			var lID string
			var nominated bool
			switch v := st.(type) {
			case webrtc.ICECandidatePairStats:
				state, rtt, lID = string(v.State), v.CurrentRoundTripTime*1000, v.LocalCandidateID
				nominated = v.Nominated
			case *webrtc.ICECandidatePairStats:
				state, rtt, lID = string(v.State), v.CurrentRoundTripTime*1000, v.LocalCandidateID
				nominated = v.Nominated
			default:
				continue
			}
			if state != "succeeded" {
				continue
			}
			loc := local[lID]
			if loc == "" {
				loc = "unknown"
			}
			out = append(out, fmt.Sprintf("%s nominated=%v rtt=%.2fms local=%s", state, nominated, rtt, loc))
		}
	}
	return out
}

// candidateTypeName 把 Pion 的候选类型枚举转成可读字符串。
// 注意它是 int 枚举，直接 string() 会得到一个 rune（而不是 "host"/"srflx"）。
func candidateTypeName(t webrtc.ICECandidateType) string {
	switch t {
	case webrtc.ICECandidateTypeHost:
		return "host"
	case webrtc.ICECandidateTypeSrflx:
		return "srflx"
	case webrtc.ICECandidateTypePrflx:
		return "prflx"
	case webrtc.ICECandidateTypeRelay:
		return "relay"
	default:
		return "unknown"
	}
}
