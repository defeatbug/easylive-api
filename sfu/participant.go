package sfu

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"

	"easylive-api/mediarec"
	"easylive-api/protocol"
)

// Notifier 由信令层实现，SFU 通过它向客户端推送信令事件。
// 用接口而不是直接依赖 signaling 包，避免 sfu ←→ signaling 的循环依赖。
type Notifier interface {
	Notify(msg *protocol.Message)
}

// subSender 记录一条订阅在 sub 连接上的发送器及其归属轨道。
type subSender struct {
	sender *webrtc.RTPSender
	track  *PublishedTrack // 所属发布轨道（退订时维护 subscribers 计数）
	bound  atomic.Bool     // onBind 是否已触发（计数是否加过 1）
}

// Participant 是一个参会者：两条 PeerConnection + 已发布的轨道集合。
type Participant struct {
	ID       string
	Name     string
	JoinedAt int64

	engine   *Engine
	notifier Notifier
	room     *Room // 由 Room.Join 注入，Participant 不反向持有 manager

	mu        sync.RWMutex
	pubPC     *webrtc.PeerConnection // 发布者连接：客户端 offer / 服务端 answer
	subPC     *webrtc.PeerConnection // 订阅者连接：服务端 offer / 客户端 answer
	published map[string]*PublishedTrack
	// subSenders 记录订阅轨道在 sub 连接上的发送器（trackID -> 发送器），
	// 用于 Unsubscribe 时只移除单条轨道并重协商，而不是关闭整条 sub 连接。
	subSenders    map[string]*subSender
	pendingSource string // 本次 publish 协商声明的媒体来源（见 protocol.Message.Source）
	// pendingCandidates 缓存"远端描述尚未就绪"时收到的 ICE 候选。
	// 浏览器会在 setLocalDescription 后立刻开始 trickle，候选完全可能早于 publish/subscribe 到达；
	// 直接丢弃会导致 ICE 只能靠对端反射候选兜底，表现为"局域网能通、公网偶发失败"。
	pendingCandidates map[string][]protocol.Candidate
	closed            bool
	// 数据通道（SCTP）：pubDC 是客户端开给服务端的（接收），outDC 是服务端反向开的（推送）
	pubDC *webrtc.DataChannel
	outDC *webrtc.DataChannel
	// identity 是令牌声明的稳定身份（无鉴权模式下退化为昵称）
	identity string
	// canPublishData 来自令牌的 canPublishData：数据通道消息（表情/举手）受它约束
	canPublishData bool
	// avatarURL 是解析出来的头像地址（展示用，失败为空）。
	// 用 sync.Once 惰性解析：无论"身份何时写入、房间快照何时构建"都能拿到，
	// 而缓存保证每次广播快照不会都去查一次数据库。
	avatarURL  string
	avatarOnce sync.Once
	// state 见 protocol.StateActive / StateReconnecting
	state string
	// resumeToken 在入会时生成，断线重连时凭它恢复同一身份。
	resumeToken string
}

func NewParticipant(id, name string, engine *Engine, notifier Notifier) *Participant {
	return &Participant{
		ID:                id,
		Name:              name,
		JoinedAt:          time.Now().UnixMilli(),
		engine:            engine,
		notifier:          notifier,
		published:         make(map[string]*PublishedTrack),
		subSenders:        make(map[string]*subSender),
		pendingCandidates: make(map[string][]protocol.Candidate),
		state:             protocol.StateActive,
		canPublishData:    true, // 默认允许；信令层按令牌覆盖
	}
}

// NotifyTo 非阻塞地把事件推给该参与者的信令连接。
// 断线恢复时 notifier 会被换成新连接，所以统一走这里（加读锁），不要在别处直接读 p.notifier。
func (p *Participant) NotifyTo(msg *protocol.Message) {
	p.mu.RLock()
	n := p.notifier
	p.mu.RUnlock()
	if n != nil {
		n.Notify(msg)
	}
}

// SetNotifier 在断线恢复时把参与者重新绑定到新的信令连接。
func (p *Participant) SetNotifier(n Notifier) {
	p.mu.Lock()
	p.notifier = n
	p.mu.Unlock()
}

// State 返回当前连接状态。
func (p *Participant) State() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.state == "" {
		return protocol.StateActive
	}
	return p.state
}

func (p *Participant) setState(state string) {
	p.mu.Lock()
	p.state = state
	p.mu.Unlock()
}

// ResumeToken 返回该参与者的恢复令牌（入会时由信令层写入）。
func (p *Participant) ResumeToken() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.resumeToken
}

// SetResumeToken 写入恢复令牌。
func (p *Participant) SetResumeToken(t string) {
	p.mu.Lock()
	p.resumeToken = t
	p.mu.Unlock()
}

func (p *Participant) Info() *protocol.PeerInfo {
	return &protocol.PeerInfo{
		ID:        p.ID,
		Name:      p.Name,
		Identity:  p.Identity(),
		JoinedAt:  p.JoinedAt,
		State:     p.State(),
		AvatarURL: p.avatar(),
	}
}

// avatar 返回该参与者的头像地址（惰性解析并缓存一次）。
//
// 为什么不用"在 SetIdentity 里解析"：那样依赖"先设身份、后建快照"这个顺序，
// 一旦顺序变了（比如参与者先入房、身份稍后写入），头像就会静默为空。
// 惰性 + Once 让顺序不再重要，代价是第一次调用时查一次库。
func (p *Participant) avatar() string {
	// **身份为空时绝不能缓存**：入会早期快照可能在写入身份之前就调用过这里，
	// sync.Once 会把"空身份 → 空头像"永久记住，之后即使身份写好了也再不会解析。
	// 这个坑很隐蔽：日志、接口、数据库全都正常，只有会议室里头像不显示。
	if p.Identity() == "" {
		return ""
	}
	p.avatarOnce.Do(func() {
		p.avatarURL = resolveAvatar(p.Identity())
	})
	return p.avatarURL
}

// Identity 返回令牌身份。
func (p *Participant) Identity() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.identity == "" {
		return p.Name // 无鉴权模式：身份退化为昵称
	}
	return p.identity
}

// SetIdentity 由信令层在入会时按令牌写入。
func (p *Participant) SetIdentity(identity string) {
	p.mu.Lock()
	p.identity = identity
	p.mu.Unlock()
}

// SetCanPublishData 设置数据面权限（来自令牌）。
func (p *Participant) SetCanPublishData(ok bool) {
	p.mu.Lock()
	p.canPublishData = ok
	p.mu.Unlock()
}

// CanPublishData 返回数据面权限。
func (p *Participant) CanPublishData() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.canPublishData
}

// Confirm 把房间引用交给参与者（在 Room.Join 内部调用）。
func (p *Participant) Confirm(room *Room) { p.room = room }

// ---------- 发布：客户端发起 offer，服务端回 answer ----------

// HandlePublishOffer 处理客户端发布请求（Offer/Answer 方向 1）。
//
// 首次调用会创建 pub 连接；再次调用即为**重协商**（例如开始/停止屏幕共享，
// 客户端在已有连接上 addTrack/removeTrack 后重新发 offer）。
func (p *Participant) HandlePublishOffer(sdp, source string) (string, error) {
	p.mu.Lock()
	pc := p.pubPC
	if source != "" {
		p.pendingSource = source
	}
	p.mu.Unlock()

	if pc == nil {
		var err error
		pc, err = p.createPubPC()
		if err != nil {
			return "", err
		}
	}

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: sdp}); err != nil {
		return "", fmt.Errorf("设置远端 offer 失败: %w", err)
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return "", fmt.Errorf("创建 answer 失败: %w", err)
	}
	if err := pc.SetLocalDescription(answer); err != nil {
		return "", fmt.Errorf("设置本地 answer 失败: %w", err)
	}

	// 这里**刻意不等待** ICE 收集完成（GatheringCompletePromise），而是走 trickle：
	// 本地描述先发出去，其余候选随后通过 candidate 消息补发（客户端会缓存到远端描述就绪）。
	//
	// 原因是踩过的坑：一旦等待收集完成，回答就会被 STUN 服务器的往返时间绑架——
	// 部署里配了公网 STUN 而它慢或不可达时，"发布"会卡住好几秒甚至直到超时，
	// 客户端只看到 publish_answer 迟迟不来。信令路径不该依赖 ICE 收集速度。
	p.flushCandidates(protocol.TargetPub)
	return pc.LocalDescription().SDP, nil
}

// takePendingSource 取走本次协商声明的来源（取走后清空，避免影响后续协商）。
func (p *Participant) takePendingSource() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	src := p.pendingSource
	p.pendingSource = ""
	return src
}

// createPubPC 创建发布者连接并注册回调。
func (p *Participant) createPubPC() (*webrtc.PeerConnection, error) {
	pc, err := p.engine.NewPeerConnection(p.ID)
	if err != nil {
		return nil, err
	}

	pc.OnTrack(func(remote *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		// 必须持续读取接收端 RTCP：Pion 的拦截器链路是"有人读才流动"，
		// 不读的话发布端回来的 RR/SR/NACK 会一直堆在缓冲里（RTT 统计也不会有值）。
		safeGo("rtcp-receiver/"+p.ID, func() { drainReceiverRTCP(receiver) })

		// 来源判定：音频一律 microphone；视频用本次协商声明的 source（camera / screen）
		source := p.takePendingSource()
		if remote.Kind() == webrtc.RTPCodecTypeAudio {
			source = "microphone"
		} else if source == "" {
			source = "camera"
		}
		track, err := p.addPublishedTrack(source, remote, pc)
		if err != nil {
			log.Printf("sfu: 登记轨道失败 participant=%s: %v", p.ID, err)
			return
		}
		if p.room != nil {
			p.room.onTrackPublished(p, track)
		}
		// 唯一的转发热路径：一条轨道一个协程，必须隔离——它 panic 会打掉所有人的会议
		safeGo("pump/"+track.info.ID, track.Pump)
	})

	// 数据通道：客户端在 pub 连接上创建（客户端是 offerer），这里接收。
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		p.mu.Lock()
		p.pubDC = dc
		p.mu.Unlock()
		log.Printf("sfu: 数据通道已建立 participant=%s label=%s", p.ID, dc.Label())

		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			p.handleDataMessage(msg.Data)
		})

		// 反向通道：SCTP 关联本身是双向的，服务端可以直接在同一条关联上再开一条
		// （DCEP OPEN 走带内信令），因此**不需要重新协商**——这一点很重要：
		// sub 连接上的 offer 流程已经很敏感，不动它就不会引入新的竞态。
		out, err := pc.CreateDataChannel("easylive-s2c", nil)
		if err != nil {
			log.Printf("sfu: 创建反向数据通道失败 participant=%s: %v", p.ID, err)
			return
		}
		// 成功也要记一行：数据面没有送达确认，排查"对方收不到"时，
		// 第一件要确认的就是"服务端到底有没有把这条反向通道建起来"。
		log.Printf("sfu: 已创建反向数据通道(推送用) participant=%s label=easylive-s2c", p.ID)
		p.mu.Lock()
		p.outDC = out
		p.mu.Unlock()
	})

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		init := c.ToJSON()
		p.NotifyTo(&protocol.Message{
			Type:   protocol.TypeCandidate,
			Target: protocol.TargetPub,
			Candidate: &protocol.Candidate{
				Candidate:     init.Candidate,
				SDPMid:        init.SDPMid,
				SDPMLineIndex: init.SDPMLineIndex,
			},
		})
	})

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("sfu: pub 连接状态 participant=%s state=%s", p.ID, state.String())
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed {
			p.Close()
		}
	})

	p.mu.Lock()
	p.pubPC = pc
	// 断线期间连接可能已被判失败并关闭；恢复后重新发布要让它重新变为"活着"，
	// 否则后续 Close 会因为 closed 提前返回，导致新连接泄漏。
	p.closed = false
	p.mu.Unlock()
	return pc, nil
}

func (p *Participant) addPublishedTrack(source string, remote *webrtc.TrackRemote, pc *webrtc.PeerConnection) (*PublishedTrack, error) {
	track, err := newPublishedTrack(p.ID, source, remote, pc)
	if err != nil {
		return nil, err
	}
	// 开启录制时，给视频轨挂一个落盘写入器（零转码：把已经在转发的 RTP 多写一份）。
	// 音频要等 Ogg/Opus 封装实现（见 mediarec 包说明），当前只录视频轨。
	// 只有**当前确实有一个录制会话在跑**才挂写入器：录制已经改成主持人运行时可启停，
	// 光有 recorder（功能可用）不等于"现在正在录"。
	if rec := p.room.manager.Recording(); rec != nil && rec.Recording() {
		codec := mediarec.Codec{
			Name:        "VP8",
			ClockRate:   90000,
			PayloadType: uint8(remote.PayloadType()),
			SSRC:        uint32(remote.SSRC()),
		}
		if track.info.Kind == protocol.KindAudio {
			codec = mediarec.Codec{
				Name:        "opus",
				ClockRate:   48000,
				PayloadType: uint8(remote.PayloadType()),
				SSRC:        uint32(remote.SSRC()),
			}
		}
		w, werr := rec.Open(p.room.ID, p.ID, source, codec)
		if werr != nil {
			log.Printf("sfu: 开启录制失败 track=%s: %v", track.info.ID, werr)
		} else {
			track.attachRecorder(w)
			// 录制器要等第一个关键帧才落盘，这里主动要一个，避免录制开头是空文件
			track.RequestKeyframe()
			log.Printf("sfu: 录制已开启 track=%s → %s", track.info.ID, w.Path())
		}
	}
	p.mu.Lock()
	p.published[track.info.ID] = track
	p.mu.Unlock()
	return track, nil
}

// ---------- 订阅：服务端发起 offer，客户端回 answer ----------

// Subscribe 把某条已发布轨道加到订阅者连接上，并返回需要发给客户端的 offer。
// 服务端始终是 sub 连接的 offerer，因此不会与客户端的 publish offer 冲突（无 glare）。
func (p *Participant) Subscribe(track *PublishedTrack) (string, error) {
	pc, err := p.ensureSubPC()
	if err != nil {
		return "", err
	}

	// 包一层，让"绑定失败"这类静默故障显式可见（见 tracklocal.go 的说明）
	st := &subSender{track: track}
	sender, err := pc.AddTrack(wrapTrackLocal(track.Local(), track.info.ID, func() {
		track.subscribers.Add(1)
		st.bound.Store(true)
	}))
	if err != nil {
		return "", fmt.Errorf("添加轨道到订阅连接失败: %w", err)
	}
	st.sender = sender
	p.mu.Lock()
	p.subSenders[track.info.ID] = st
	p.mu.Unlock()
	// 订阅端：必须持续读 RTCP（否则 Pion 内部管道阻塞）。
	// PLI/FIR 的转发由全局拦截器 keyframeForwarder 负责（按 SSRC 查表），这里只排空。
	safeGo("rtcp-sender/"+p.ID, func() { drainRTCP(sender) })

	// 新订阅者需要立刻拿到关键帧，否则会黑屏到发布者下一个自然关键帧；
	// 若轨道当前一个包都没有（屏幕画面静止等），NudgeKeyframe 会持续催几次。
	track.RequestKeyframe()
	track.NudgeKeyframe()

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return "", fmt.Errorf("创建订阅 offer 失败: %w", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		return "", fmt.Errorf("设置订阅本地描述失败: %w", err)
	}
	// 同样走 trickle（见 HandlePublishOffer 里的说明）：候选随后通过 candidate 消息补发
	return pc.LocalDescription().SDP, nil
}

// Unsubscribe 只停止向该参与者转发指定轨道：从 sub 连接上移除对应的
// RTPSender 并重协商，不断整条 sub 连接——其余订阅不受影响。
// 返回重协商的 offer SDP；空字符串表示无需重协商（调用方无需通知客户端）。
func (p *Participant) Unsubscribe(trackID string) (string, error) {
	p.mu.Lock()
	pc := p.subPC
	st := p.subSenders[trackID]
	p.mu.Unlock()

	if pc == nil || st == nil || st.sender == nil {
		return "", nil // 没订阅过（或连接已不存在）：无需重协商
	}
	if err := pc.RemoveTrack(st.sender); err != nil {
		return "", fmt.Errorf("从订阅连接移除轨道失败: %w", err)
	}
	p.mu.Lock()
	delete(p.subSenders, trackID)
	p.mu.Unlock()
	if st.bound.Load() {
		st.track.subscribers.Add(-1)
	}

	// 重协商：让客户端用新的 offer 更新远端描述（m-line 减少一条）
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return "", fmt.Errorf("创建取消订阅 offer 失败: %w", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		return "", fmt.Errorf("设置取消订阅本地描述失败: %w", err)
	}
	log.Printf("sfu: 已取消单条订阅（重协商，不断连接）participant=%s track=%s", p.ID, trackID)
	return pc.LocalDescription().SDP, nil
}

// HandleSubscribeAnswer 处理客户端对服务端订阅 offer 的应答（Offer/Answer 方向 2）。
func (p *Participant) HandleSubscribeAnswer(sdp string) error {
	p.mu.RLock()
	pc := p.subPC
	p.mu.RUnlock()
	if pc == nil {
		return fmt.Errorf("订阅者连接不存在")
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: sdp}); err != nil {
		return err
	}
	p.flushCandidates(protocol.TargetSub)
	return nil
}

func (p *Participant) ensureSubPC() (*webrtc.PeerConnection, error) {
	p.mu.RLock()
	pc := p.subPC
	p.mu.RUnlock()
	if pc != nil {
		return pc, nil
	}

	pc, err := p.engine.NewPeerConnection(p.ID)
	if err != nil {
		return nil, err
	}
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		init := c.ToJSON()
		p.NotifyTo(&protocol.Message{
			Type:   protocol.TypeCandidate,
			Target: protocol.TargetSub,
			Candidate: &protocol.Candidate{
				Candidate:     init.Candidate,
				SDPMid:        init.SDPMid,
				SDPMLineIndex: init.SDPMLineIndex,
			},
		})
	})
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("sfu: sub 连接状态 participant=%s state=%s", p.ID, state.String())
		if state == webrtc.PeerConnectionStateConnected && p.room != nil {
			// 传输建立完成：向已订阅的发布者再要一次关键帧，
			// 补上"订阅时发出的关键帧被 SRTP 未就绪窗口吞掉"的那个洞。
			p.room.requestKeyframesFor(p.ID)
		}
	})

	p.mu.Lock()
	p.subPC = pc
	p.closed = false
	p.mu.Unlock()
	return pc, nil
}

// AddICECandidate 处理客户端上报的 ICE 候选。
// 若目标连接或其远端描述尚未就绪，先缓存起来，等 SetRemoteDescription 之后由 flushCandidates 补入。
func (p *Participant) AddICECandidate(target string, c *protocol.Candidate) error {
	p.mu.Lock()
	var pc *webrtc.PeerConnection
	if target == protocol.TargetSub {
		pc = p.subPC
	} else {
		pc = p.pubPC
	}
	if pc == nil || pc.RemoteDescription() == nil {
		p.pendingCandidates[target] = append(p.pendingCandidates[target], *c)
		n := len(p.pendingCandidates[target])
		p.mu.Unlock()
		log.Printf("sfu: 缓存 ICE 候选（远端描述未就绪）participant=%s target=%s 已缓存 %d 个", p.ID, target, n)
		return nil
	}
	p.mu.Unlock()

	return pc.AddICECandidate(webrtc.ICECandidateInit{
		Candidate:     c.Candidate,
		SDPMid:        c.SDPMid,
		SDPMLineIndex: c.SDPMLineIndex,
	})
}

// flushCandidates 把缓存中的候选补交给目标连接（必须在 SetRemoteDescription 之后调用）。
func (p *Participant) flushCandidates(target string) {
	p.mu.Lock()
	list := p.pendingCandidates[target]
	p.pendingCandidates[target] = nil
	var pc *webrtc.PeerConnection
	if target == protocol.TargetSub {
		pc = p.subPC
	} else {
		pc = p.pubPC
	}
	p.mu.Unlock()

	if pc == nil || len(list) == 0 {
		return
	}
	added := 0
	for _, c := range list {
		err := pc.AddICECandidate(webrtc.ICECandidateInit{
			Candidate:     c.Candidate,
			SDPMid:        c.SDPMid,
			SDPMLineIndex: c.SDPMLineIndex,
		})
		if err == nil {
			added++
		}
	}
	log.Printf("sfu: 补入缓存候选 participant=%s target=%s 成功 %d/%d", p.ID, target, added, len(list))
}

// PublishedTracks 返回该参与者已发布的轨道（供离开时清理）。
func (p *Participant) PublishedTracks() []*PublishedTrack {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]*PublishedTrack, 0, len(p.published))
	for _, t := range p.published {
		out = append(out, t)
	}
	return out
}

// maxFlashTextRunes 是弹幕文字的字符上限（超出截断，不丢弃）。
const maxFlashTextRunes = 60

// handleDataMessage 处理数据通道上的消息。
//
// 分流原则：**瞬时事件走数据面中继，状态变更走信令广播**。
// 表情是瞬时的，直接中继给其他人即可，服务端不保存；
// 举手是状态，必须可靠、可恢复、且新入会者要能看到，所以落到房间状态并走信令广播。
func (p *Participant) handleDataMessage(data []byte) {
	var m protocol.DataMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return
	}
	if p.room == nil {
		return
	}
	// 数据面同样受权限约束：表情/举手属于"发布数据"，令牌没给就在这一层拦掉。
	// 注意这是服务端强制，不是靠前端隐藏按钮。
	if !p.CanPublishData() {
		return
	}
	switch m.Type {
	case protocol.DataReaction:
		if m.Emoji == "" || len([]rune(m.Emoji)) > 4 {
			return // 防御：不允许超长"表情"变成刷屏载荷
		}
		// 弹幕文字：**截断而不是丢弃**——用户打了一长句，不该整个消失。
		text := m.Text
		if r := []rune(text); len(r) > maxFlashTextRunes {
			text = string(r[:maxFlashTextRunes])
		}
		// 只认识两种 kind：空（表情浮层）与 action（互动动效）。未知值归零，
		// 免得客户端随便塞个字符串就造出前端不认识的分支。
		kind := m.Kind
		if kind != "" && kind != protocol.ReactionKindAction {
			kind = ""
		}
		p.room.OnReaction(p.ID, m.Emoji, text, kind)
	case protocol.DataRaiseHand:
		if m.Raised == nil {
			return
		}
		p.room.SetHand(p.ID, *m.Raised)
	default:
		// 未知类型忽略：数据面允许演进，不能因为新类型让旧服务端报错
	}
}

// SendData 通过数据通道向该参与者推送一条消息。
// 返回 false 表示通道未就绪——数据面语义是"尽力而为"，调用方不应因此报错。
func (p *Participant) SendData(data []byte) bool {
	p.mu.RLock()
	// **用服务端反向开的那条**（easylive-s2c）。
	//
	// 曾经改成"优先用客户端创建的那条"，理由是 SCTP 双向、而浏览器收不到反向通道。
	// 那是错的：signaling 的 e2e（TestE2EDataChannel）证明反向通道在标准客户端上完全可用，
	// 改成客户端通道反而把它弄坏了。浏览器收不到是**浏览器侧**的特例，
	// 不该为一个特例改动所有客户端的路径——弹幕/互动因此改走信令面，
	// 那条路（聊天/举手/白板）在任何客户端上都已验证可靠。
	dc := p.outDC
	p.mu.RUnlock()
	if dc == nil || dc.ReadyState() != webrtc.DataChannelStateOpen {
		return false
	}
	return dc.Send(data) == nil
}

// DataChannelReady 供 /debug/stats 观测（评测时可以确认数据面是否真的建立）。
func (p *Participant) DataChannelReady() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	// 与 SendData 保持一致：推送只走反向通道，就只看它
	return p.outDC != nil && p.outDC.ReadyState() == webrtc.DataChannelStateOpen
}

// AudioLevel 返回该参与者所有音频轨里最大的平滑电平（0—1）。
// 活跃说话人检测用它排序：一个人可能有麦克风与屏幕共享音频两条轨，取最大值即可。
func (p *Participant) AudioLevel() float64 {
	p.mu.RLock()
	tracks := make([]*PublishedTrack, 0, len(p.published))
	for _, t := range p.published {
		tracks = append(tracks, t)
	}
	p.mu.RUnlock()

	max := 0.0
	for _, t := range tracks {
		if l := t.AudioLevel(); l > max {
			max = l
		}
	}
	return max
}

func (p *Participant) removePublishedTrack(trackID string) {
	p.mu.Lock()
	delete(p.published, trackID)
	p.mu.Unlock()
}

// ResetMedia 关闭两条连接并清空已发布轨道。
// 用于"信令恢复了、但媒体面已经死了"的重连：身份保留，客户端随后用新连接重新发布。
func (p *Participant) ResetMedia() {
	p.mu.Lock()
	pub, sub := p.pubPC, p.subPC
	p.pubPC, p.subPC = nil, nil
	p.pubDC, p.outDC = nil, nil
	p.published = make(map[string]*PublishedTrack)
	p.subSenders = make(map[string]*subSender)
	p.pendingCandidates = make(map[string][]protocol.Candidate)
	p.closed = false
	p.mu.Unlock()

	if pub != nil {
		_ = pub.Close()
	}
	if sub != nil {
		_ = sub.Close()
	}
	log.Printf("sfu: 已清空媒体面（重连后需重新发布）participant=%s", p.ID)
}

func (p *Participant) IsClosed() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.closed
}

// Close 关闭两条连接并释放资源。
func (p *Participant) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	pub, sub := p.pubPC, p.subPC
	p.pubPC, p.subPC = nil, nil
	p.mu.Unlock()

	if pub != nil {
		_ = pub.Close()
	}
	if sub != nil {
		_ = sub.Close()
	}
	log.Printf("sfu: 参与者连接已关闭 participant=%s", p.ID)
}

// ---------- 可观测性（/debug/stats 用） ----------

// TrackDebug 描述一条已发布轨道的转发情况。
type TrackDebug struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Source  string `json:"source"`
	SSRC    uint32 `json:"ssrc"`
	Packets uint64 `json:"packets"`
	Bytes   uint64 `json:"bytes"`
}

// RTPStat 汇总一条 RTP 流的关键计数（来自 PeerConnection.GetStats）。
// 这是排查"订阅端黑屏"最有用的一组数字：
//   - receivedFromPublisher 全为 0 → 发布端根本没发媒体（编码器没出帧 / 协商问题）
//   - receivedFromPublisher 有值但 published.packets 为 0 → 服务端读到了包但没进入转发
//   - published.packets 有值而 sentToSubscriber 为 0 → 转发到订阅端的链路有问题
type RTPStat struct {
	SSRC             uint32 `json:"ssrc"`
	Kind             string `json:"kind"`
	Packets          uint32 `json:"packets"`
	Bytes            uint64 `json:"bytes"`
	PacketsLost      int32  `json:"packetsLost"`
	PLICount         uint32 `json:"pliCount"`
	NACKCount        uint32 `json:"nackCount"`
	FramesDecoded    uint32 `json:"framesDecoded"`
	KeyFramesDecoded uint32 `json:"keyFramesDecoded"`
	FramesEncoded    uint32 `json:"framesEncoded"`
	KeyFramesEncoded uint32 `json:"keyFramesEncoded"`
	FramesDropped    uint32 `json:"framesDropped"`
}

// TransceiverDebug 描述一条收发器：方向与发送端绑定情况。
// 排查"服务端没往订阅端发包"时看这里：如果 sender 没有 track 或译码器不匹配，绑定就不会建立。
type TransceiverDebug struct {
	PC            string `json:"pc"` // pub | sub
	Kind          string `json:"kind"`
	Mid           string `json:"mid"`
	Direction     string `json:"direction"`
	SenderTrackID string `json:"senderTrackId,omitempty"`
	SenderSSRC    uint32 `json:"senderSsrc,omitempty"`
}

type ParticipantDebug struct {
	ID                    string             `json:"id"`
	Name                  string             `json:"name"`
	PubState              string             `json:"pubState"`
	SubState              string             `json:"subState"`
	ReceivedFromPublisher []RTPStat          `json:"receivedFromPublisher"`
	SentToSubscriber      []RTPStat          `json:"sentToSubscriber"`
	Transceivers          []TransceiverDebug `json:"transceivers"`
	Published             []TrackDebug       `json:"published"`
	Subscriptions         []string           `json:"subscriptions"`
}

// DebugInfo 汇总该参与者的连接状态、媒体流计数与轨道转发计数。
func (p *Participant) DebugInfo(subscriptions []string) *ParticipantDebug {
	p.mu.RLock()
	pub, sub := p.pubPC, p.subPC
	closed := p.closed
	tracks := make([]TrackDebug, 0, len(p.published))
	for _, t := range p.published {
		tracks = append(tracks, TrackDebug{
			ID:      t.info.ID,
			Kind:    t.info.Kind,
			Source:  t.info.Source,
			SSRC:    trackSSRC(t),
			Packets: t.packets.Load(),
			Bytes:   t.bytes.Load(),
		})
	}
	p.mu.RUnlock()

	state := func(pc *webrtc.PeerConnection) string {
		if pc == nil {
			return "none"
		}
		return pc.ConnectionState().String()
	}

	info := &ParticipantDebug{
		ID:                    p.ID,
		Name:                  p.Name,
		PubState:              state(pub),
		SubState:              state(sub),
		ReceivedFromPublisher: inboundStats(pub),
		SentToSubscriber:      outboundStats(sub),
		Transceivers:          append(transceiverStats("pub", pub), transceiverStats("sub", sub)...),
		Published:             tracks,
		Subscriptions:         subscriptions,
	}
	if closed {
		info.PubState = "closed"
	}
	return info
}

func transceiverStats(label string, pc *webrtc.PeerConnection) []TransceiverDebug {
	out := []TransceiverDebug{}
	if pc == nil {
		return out
	}
	for _, tr := range pc.GetTransceivers() {
		item := TransceiverDebug{
			PC:        label,
			Kind:      tr.Kind().String(),
			Mid:       tr.Mid(),
			Direction: dirName(tr.Direction()),
		}
		if sender := tr.Sender(); sender != nil && sender.Track() != nil {
			item.SenderTrackID = sender.Track().ID()
			if params := sender.GetParameters(); len(params.Encodings) > 0 {
				item.SenderSSRC = uint32(params.Encodings[0].SSRC)
			}
		}
		out = append(out, item)
	}
	return out
}

func dirName(d webrtc.RTPTransceiverDirection) string {
	switch d {
	case webrtc.RTPTransceiverDirectionSendrecv:
		return "sendrecv"
	case webrtc.RTPTransceiverDirectionSendonly:
		return "sendonly"
	case webrtc.RTPTransceiverDirectionRecvonly:
		return "recvonly"
	case webrtc.RTPTransceiverDirectionInactive:
		return "inactive"
	default:
		return "unspecified"
	}
}

func trackSSRC(t *PublishedTrack) uint32 {
	if t.remote == nil {
		return 0
	}
	return uint32(t.remote.SSRC())
}

func inboundStats(pc *webrtc.PeerConnection) []RTPStat {
	out := []RTPStat{}
	if pc == nil {
		return out
	}
	for _, st := range pc.GetStats() {
		switch v := st.(type) {
		case webrtc.InboundRTPStreamStats:
			out = append(out, RTPStat{
				SSRC: uint32(v.SSRC), Kind: v.Kind, Packets: v.PacketsReceived, Bytes: v.BytesReceived,
				PacketsLost: v.PacketsLost, PLICount: v.PLICount, NACKCount: v.NACKCount,
				FramesDecoded: v.FramesDecoded, KeyFramesDecoded: v.KeyFramesDecoded, FramesDropped: v.FramesDropped,
			})
		case *webrtc.InboundRTPStreamStats:
			out = append(out, RTPStat{
				SSRC: uint32(v.SSRC), Kind: v.Kind, Packets: v.PacketsReceived, Bytes: v.BytesReceived,
				PacketsLost: v.PacketsLost, PLICount: v.PLICount, NACKCount: v.NACKCount,
				FramesDecoded: v.FramesDecoded, KeyFramesDecoded: v.KeyFramesDecoded, FramesDropped: v.FramesDropped,
			})
		}
	}
	return out
}

func outboundStats(pc *webrtc.PeerConnection) []RTPStat {
	out := []RTPStat{}
	if pc == nil {
		return out
	}
	for _, st := range pc.GetStats() {
		switch v := st.(type) {
		case webrtc.OutboundRTPStreamStats:
			out = append(out, RTPStat{
				SSRC: uint32(v.SSRC), Kind: v.Kind, Packets: v.PacketsSent, Bytes: v.BytesSent,
				PLICount: v.PLICount, NACKCount: v.NACKCount,
				FramesEncoded: v.FramesEncoded, KeyFramesEncoded: v.KeyFramesEncoded,
			})
		case *webrtc.OutboundRTPStreamStats:
			out = append(out, RTPStat{
				SSRC: uint32(v.SSRC), Kind: v.Kind, Packets: v.PacketsSent, Bytes: v.BytesSent,
				PLICount: v.PLICount, NACKCount: v.NACKCount,
				FramesEncoded: v.FramesEncoded, KeyFramesEncoded: v.KeyFramesEncoded,
			})
		}
	}
	return out
}
