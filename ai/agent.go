package ai

import (
	"context"
	"fmt"
	"log"
	"math"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/pion/rtp"

	"easylive-api/protocol"
	"easylive-api/sfu"
)

// agentQueueSize 是待回答队列长度。问答是串行处理的（避免多人同时 @AI
// 导致语音重叠），队列满时直接告诉提问者稍等，而不是无限堆积。
const agentQueueSize = 8

// opusPayloadType 是服务端生成音频的 RTP payload type（Opus 动态类型的事实标准值）。
const opusPayloadType = 111

// askRequest 是一次 @AI 提问。
type askRequest struct {
	room     *sfu.Room
	fromName string
	question string
}

// rtpState 是虚拟参会者在单个房间的 RTP 序列状态。
// 只由 worker 协程读写，无需加锁。
type rtpState struct {
	ssrc uint32
	seq  uint16
	ts   uint32
}

// Agent 是 AI 问答助手：以虚拟参会者身份常驻，回答会议中的 @AI 提问。
//
// 链路：Ask 入队 → worker 串行处理 → DeepSeek（带纪要上下文）→
// 文字先落聊天框 → TTS 合成 → Opus 编码 → 虚拟音频轨 20ms 起搏写入 → 全员听到。
type Agent struct {
	cfg     Config
	summary func(roomID string) string // 纪要上下文（Hub 接 ai.Manager.SummaryMarkdown）

	ttsMu  sync.Mutex
	tts    TTSBackend
	ttsErr error // 构建失败时 Ask 直接返回它（fail fast，不重试）

	queue chan askRequest
	rtp   map[string]*rtpState // roomID → RTP 状态
}

// NewAgent 创建问答助手并启动后台 worker。cfg 未就绪也能创建——
// Ask 时会明确拒绝并在聊天框里说明原因，不影响正常信令流程。
func NewAgent(cfg Config, summary func(roomID string) string) *Agent {
	a := &Agent{
		cfg:     cfg,
		summary: summary,
		queue:   make(chan askRequest, agentQueueSize),
		rtp:     make(map[string]*rtpState),
	}
	go a.loop()
	return a
}

// Ready 报告问答助手是否可运行。
func (a *Agent) Ready() bool { return a.cfg.AgentReady() }

// UnreadyReason 返回不可用的具体原因。
func (a *Agent) UnreadyReason() string { return a.cfg.AgentUnreadyReason() }

// IsAIMention 判断聊天文本是否为 @AI 提问（大小写不敏感，@后必须有实质内容）。
func IsAIMention(text string) bool {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(strings.ToLower(t), "@ai") {
		return false
	}
	return trimQuestion(t) != ""
}

// Ask 提交一次 @AI 提问。非阻塞：未就绪/队列满时直接在聊天框说明原因并返回 nil，
// 调用方（信令读循环）无需处理错误。调用前请先用 IsAIMention 判断。
func (a *Agent) Ask(room *sfu.Room, fromName, question string) {
	question = trimQuestion(question)
	if question == "" {
		return
	}
	if !a.Ready() {
		a.chat(room, fmt.Sprintf("AI 助手未配置：%s", a.UnreadyReason()))
		return
	}
	select {
	case a.queue <- askRequest{room: room, fromName: fromName, question: question}:
	default:
		a.chat(room, "我正忙着回答上一位，稍等一下再 @我吧")
	}
}

// buildTTS 懒构建 TTS 后端（第一次真正被问到时）。失败则记录并持久返回。
func (a *Agent) buildTTS() error {
	a.ttsMu.Lock()
	defer a.ttsMu.Unlock()
	if a.tts != nil || a.ttsErr != nil {
		return a.ttsErr
	}
	tts, err := NewPiperLocal(a.cfg.PiperBin, a.cfg.PiperModelPath)
	if err != nil {
		a.ttsErr = err
		log.Printf("ai-agent: TTS 后端构建失败: %v", err)
		return err
	}
	a.tts = tts
	log.Printf("ai-agent: TTS 后端就绪（piper）")
	return nil
}

func (a *Agent) loop() {
	for req := range a.queue {
		a.handle(req)
	}
}

// handle 串行处理一次提问。
func (a *Agent) handle(req askRequest) {
	room := req.room
	if err := a.buildTTS(); err != nil {
		a.chat(room, fmt.Sprintf("AI 助手语音未就绪：%v", err))
		return
	}

	a.setState(room, protocol.AIAgentThinking)

	// 1. 想：DeepSeek，带上最近纪要做上下文
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	var contextText string
	if a.summary != nil {
		contextText = a.summary(room.ID)
	}
	answer, err := CompleteQA(ctx, a.cfg.DeepSeekAPIKey, a.cfg.DeepSeekModel, req.question, contextText)
	cancel()
	if err != nil {
		log.Printf("ai-agent: 问答失败 room=%s q=%q: %v", room.ID, truncate(req.question, 60), err)
		a.chat(room, "抱歉，我刚才走神了，没想出来（推理服务出错）")
		a.setState(room, protocol.AIAgentIdle)
		return
	}
	if answer == "" {
		answer = "这个问题我暂时答不上来，换个问法试试？"
	}

	// 2. 文字先行：聊天框立刻能看到答案，不用干等语音
	a.chat(room, answer)

	// 3. 说：TTS → 重采样 → Opus → 虚拟轨
	a.setState(room, protocol.AIAgentSpeaking)
	if err := a.speak(room, answer); err != nil {
		log.Printf("ai-agent: 语音合成/发送失败 room=%s: %v", room.ID, err)
		// 文字已经发出去了，语音失败不算全失败：降级为纯文字回答
	}
	a.setState(room, protocol.AIAgentIdle)
}

// speak 把回答合成为语音并以 20ms 起搏写入虚拟参会者轨道。
func (a *Agent) speak(room *sfu.Room, text string) error {
	a.ttsMu.Lock()
	tts := a.tts
	a.ttsMu.Unlock()
	if tts == nil {
		return fmt.Errorf("TTS 后端未初始化")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pcm, err := tts.Synthesize(ctx, text)
	if err != nil {
		return err
	}
	if len(pcm) == 0 {
		return nil
	}
	pcm48 := ResampleTo48000(pcm, tts.SampleRate())
	frames, err := EncodeOpusFrames(pcm48)
	if err != nil {
		return err
	}

	track, err := room.PublishAgentTrack()
	if err != nil {
		return fmt.Errorf("发布虚拟轨道失败: %w", err)
	}
	st := a.rtpStateFor(room.ID)

	// 20ms 起搏：实时写入，让订阅端按正常语音节奏播放。
	// 首帧立即发出（减少"提问→听到声音"的延迟），之后按 tick。
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for i, payload := range frames {
		if i > 0 {
			<-ticker.C
		}
		pkt := &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    opusPayloadType,
				SequenceNumber: st.seq,
				Timestamp:      st.ts,
				SSRC:           st.ssrc,
			},
			Payload: payload,
		}
		st.seq++
		st.ts += opusFrameSamples
		// 上报电平：让活跃说话人检测知道 AI 正在说话（虚拟轨没有 RTP 头扩展）
		start := i * opusFrameSamples
		end := start + opusFrameSamples
		if end > len(pcm48) {
			end = len(pcm48)
		}
		track.NoteAudioLevel(rmsLevel(pcm48[start:end]))
		if err := track.WriteAgentRTP(pkt); err != nil {
			return fmt.Errorf("写入虚拟轨道失败: %w", err)
		}
	}
	// 说完后电平归零：否则说话人高亮会多亮 600ms（AudioLevel 的 hold 窗口）
	track.NoteAudioLevel(0)
	log.Printf("ai-agent: 语音回答已发送 room=%s frames=%d", room.ID, len(frames))
	return nil
}

// rtpStateFor 取（或创建）房间的 RTP 状态。只由 worker 调用。
func (a *Agent) rtpStateFor(roomID string) *rtpState {
	st, ok := a.rtp[roomID]
	if !ok {
		st = &rtpState{ssrc: rand.Uint32(), seq: uint16(rand.Uint32()), ts: rand.Uint32()}
		a.rtp[roomID] = st
	}
	return st
}

// chat 以 AI 助手身份往房间发一条聊天消息。
func (a *Agent) chat(room *sfu.Room, text string) {
	room.Broadcast(&protocol.Message{
		Type: protocol.TypeChatMessage,
		ChatMessage: &protocol.ChatMessage{
			FromID:   protocol.AIAgentID,
			FromName: protocol.AIAgentName,
			Text:     text,
			TS:       time.Now().UnixMilli(),
		},
	}, "")
}

// setState 广播助手状态（thinking/speaking/idle）。
func (a *Agent) setState(room *sfu.Room, state string) {
	room.Broadcast(&protocol.Message{
		Type: protocol.TypeAIAgentState,
		AIAgent: &protocol.AIAgentState{
			Active: true,
			State:  state,
		},
	}, "")
	log.Printf("ai-agent: 状态变更 room=%s state=%s", room.ID, state)
}

// rmsLevel 计算一帧 PCM 的归一化电平（0-1），供说话人指示用。
func rmsLevel(pcm []int16) float64 {
	if len(pcm) == 0 {
		return 0
	}
	var sum float64
	for _, v := range pcm {
		f := float64(v) / 32768
		sum += f * f
	}
	rms := math.Sqrt(sum / float64(len(pcm)))
	// 语音 RMS 通常 0.05-0.3；除以 0.25 映射到 0-1，钳制
	if l := rms / 0.25; l < 1 {
		return l
	}
	return 1
}

// trimQuestion 清理 @AI 前缀（大小写不敏感，容忍前后空格），返回纯问题。
func trimQuestion(text string) string {
	t := strings.TrimSpace(text)
	lower := strings.ToLower(t)
	if strings.HasPrefix(lower, "@ai") {
		t = strings.TrimSpace(t[len("@ai"):])
	}
	return t
}
