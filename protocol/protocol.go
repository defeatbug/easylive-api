// Package protocol 定义 WebSocket 信令协议。本文件是协议的**唯一事实源**（Go 侧），
// 必须在 web/src/lib/protocol.ts 中保持 1:1 对齐。
//
// 之所以独立成一个包：sfu 包需要 TrackInfo / PeerInfo / RoomState 等类型，
// 而 signaling（传输层）需要依赖 sfu。协议类型放在中立包里可以避免包循环依赖。
//
// 设计要点：
//  1. 三类消息：请求（客户端发起，可能带 id 幂等键）、响应（服务端对某个请求的答复）、事件（服务端主动推送）。
//  2. 扁平结构：所有载荷放在同一个结构体里，按 type 取用，前端 TypeScript 用判别联合（discriminated union）镜像。
//  3. 会话与连接解耦：sessionId 由服务端下发，重连后仍可恢复（见 M4）。
package protocol

import "encoding/json"

type MsgType string

// ---------- 客户端 → 服务端 ----------
const (
	TypeJoin        MsgType = "join"        // {roomId, displayName, resumeToken?}  resumeToken 非空表示"断线重连恢复"
	TypeLeave       MsgType = "leave"       // {}
	TypePublish     MsgType = "publish"     // {sdp, source}        客户端 offer（发布者连接）；重复发送即为重协商（如开始/停止屏幕共享）
	TypeAnswer      MsgType = "answer"      // {target, sdp}        target: "pub" | "sub"
	TypeCandidate   MsgType = "candidate"   // {target, candidate}  双向使用
	TypeSubscribe   MsgType = "subscribe"   // {publisherId, trackId}
	TypeUnsubscribe MsgType = "unsubscribe" // {trackId}
	TypeUnpublish   MsgType = "unpublish"   // {trackId}            停止发布某条轨道（如结束屏幕共享）
	TypeMute        MsgType = "mute"        // {trackId, muted}
	TypeChat        MsgType = "chat"        // {text}
	TypePing        MsgType = "ping"        // {}
)

// ---------- 服务端 → 客户端 ----------
const (
	TypeJoined           MsgType = "joined"             // {sessionId, participantId, room, resumeToken, resumed}
	TypeParticipantState MsgType = "participant_state"  // {participantId, state}  参与者在"重连中/已恢复"之间切换
	TypePublishAnswer    MsgType = "publish_answer"     // {sdp}
	TypeSubscribeOffer   MsgType = "subscribe_offer"    // {sdp, track}       服务端发起（订阅者连接）
	TypeParticipantJoin  MsgType = "participant_joined" // {participant}
	TypeParticipantLeave MsgType = "participant_left"   // {participantId}
	TypeTrackPublished   MsgType = "track_published"    // {track}
	TypeTrackUnpublished MsgType = "track_unpublished"  // {trackId}
	TypeTrackMuted       MsgType = "track_muted"        // {trackId, muted}
	TypeActiveSpeakers   MsgType = "active_speakers"    // {speakers:[{participantId, level}]}  只在集合变化时推送
	TypeHandState        MsgType = "hand_state"         // {participantId, raised}  举手状态（进房间快照）
	TypeChatMessage      MsgType = "chat_message"       // {message}
	TypePong             MsgType = "pong"               // {}
	TypeError            MsgType = "error"              // {code, reason, id?}
)

// 媒体连接目标：发布者连接 或 订阅者连接（每个参与者各持一条，避免重复协商/glare）
const (
	TargetPub = "pub"
	TargetSub = "sub"
)

// 参与者连接状态（PeerInfo.State / participant_state 事件）
const (
	StateActive       = "active"       // 正常参会
	StateReconnecting = "reconnecting" // 连接断开，处于恢复宽限期内（轨道与订阅仍然保留）
)

// 轨道类型
const (
	KindAudio  = "audio"
	KindVideo  = "video"
	KindScreen = "screen"
)

// Message 是协议里唯一的消息结构。
type Message struct {
	Type MsgType `json:"type"`
	// ID 是请求的幂等键：服务端在处理结果中原样带回，客户端据此匹配响应。
	ID  string `json:"id,omitempty"`
	Seq uint64 `json:"seq,omitempty"` // 会话内递增序号（可选，用于排查乱序）
	TS  int64  `json:"ts,omitempty"`  // 服务端填充的毫秒时间戳

	// ---- 请求载荷 ----
	RoomID      string `json:"roomId,omitempty"`
	DisplayName string `json:"displayName,omitempty"`
	// ResumeToken 由服务端在 joined 中下发；断线后重连时原样带回，
	// 用于在宽限期内恢复**同一个参与者身份**（轨道与订阅不中断，对房间内其他人无感）。
	ResumeToken string `json:"resumeToken,omitempty"`
	// ResetMedia 与 ResumeToken 配合使用：客户端两条 PeerConnection 已经不可用、
	// 必须重新发布时置 true。服务端会清掉该参与者的旧轨道与旧连接（**保留身份**），
	// 否则新连接产生的新 SSRC 会让服务端为同一路媒体再建一条轨道（表现为多出一张卡片）。
	ResetMedia  bool       `json:"resetMedia,omitempty"`
	SDP         string     `json:"sdp,omitempty"`
	Target      string     `json:"target,omitempty"`
	Candidate   *Candidate `json:"candidate,omitempty"`
	PublisherID string     `json:"publisherId,omitempty"`
	TrackID     string     `json:"trackId,omitempty"`
	Muted       *bool      `json:"muted,omitempty"`
	Text        string     `json:"text,omitempty"`
	// Source 声明本次 publish 协商新增媒体的来源：camera / screen / microphone。
	// 说明：浏览器不允许自定义 MediaStream id（msid），因此无法只靠 SDP 区分摄像头与屏幕，
	// 这里用"本次协商声明的来源"来标注；更严谨的做法是自定义 RTP 头扩展（可作为后续工作）。
	Source string `json:"source,omitempty"`

	// ---- 服务端载荷 ----
	SessionID     string        `json:"sessionId,omitempty"`
	ParticipantID string        `json:"participantId,omitempty"`
	Participant   *PeerInfo     `json:"participant,omitempty"`
	Room          *RoomState    `json:"room,omitempty"`
	Track         *TrackInfo    `json:"track,omitempty"`
	ChatMessage   *ChatMessage  `json:"message,omitempty"`
	Speakers      []SpeakerInfo `json:"speakers,omitempty"`
	Raised        *bool         `json:"raised,omitempty"`
	// Resumed 为 true 表示本次 joined 复用了既有参与者身份（断线恢复而非重新入会）。
	Resumed bool   `json:"resumed,omitempty"`
	State   string `json:"state,omitempty"`
	Code    string `json:"code,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// Candidate 是 ICE 候选的传输格式（与 RTCIceCandidateInit 对齐）。
type Candidate struct {
	Candidate     string  `json:"candidate"`
	SDPMid        *string `json:"sdpMid,omitempty"`
	SDPMLineIndex *uint16 `json:"sdpMLineIndex,omitempty"`
}

type PeerInfo struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	JoinedAt int64  `json:"joinedAt"`
	// State 见 StateActive / StateReconnecting；空值等价于 active（兼容旧客户端）。
	State string `json:"state,omitempty"`
}

type TrackInfo struct {
	ID          string `json:"id"`
	PublisherID string `json:"publisherId"`
	Kind        string `json:"kind"`   // audio | video | screen
	Source      string `json:"source"` // microphone | camera | screen
	Muted       bool   `json:"muted"`
}

type RoomState struct {
	RoomID       string       `json:"roomId"`
	Participants []*PeerInfo  `json:"participants"`
	Tracks       []*TrackInfo `json:"tracks"`
	// Hands 是已举手参与者的 ID 列表。举手是**状态**而不是瞬时事件，
	// 因此进快照：新入会者据此也能看到谁举了手。
	Hands []string `json:"hands,omitempty"`
}

// ---------- 数据通道消息（SCTP 数据面，与 WebSocket 信令分开）----------
//
// 为什么要分两套：信令是**控制面**——可靠、有序、断线要能恢复，队列不能被高频小消息占满；
// 数据通道是**数据面**——低延迟、瞬时事件（表情、举手意图）丢了也无所谓。
// 二者混用会让重连快照、心跳这些关键消息被表情淹没。
const (
	DataReaction  = "reaction"   // {emoji}
	DataRaiseHand = "raise_hand" // {raised}
)

// DataMessage 是数据通道上的消息（与信令 Message 结构无关，单独一套）。
type DataMessage struct {
	Type   string `json:"type"`
	Emoji  string `json:"emoji,omitempty"`
	Raised *bool  `json:"raised,omitempty"`
	// FromID 由服务端在**中继时**填充：客户端不需要（也不应该）自己声明身份。
	FromID string `json:"fromId,omitempty"`
}

// SpeakerInfo 描述一位"正在说话"的参与者。
// Level 是归一化后的音频电平（0—1，1 为最大），由 RFC 6464 头扩展解析并做指数平滑。
type SpeakerInfo struct {
	ParticipantID string  `json:"participantId"`
	Level         float64 `json:"level"`
}

type ChatMessage struct {
	FromID   string `json:"fromId"`
	FromName string `json:"fromName"`
	Text     string `json:"text"`
	TS       int64  `json:"ts"`
}

// ---------- 错误码 ----------
const (
	ErrBadRequest = "bad_request"
	ErrNotJoined  = "not_joined"
	ErrRoomFull   = "room_full"
	ErrServerBusy = "server_at_capacity"
	ErrInternal   = "internal_error"
)

func New(t MsgType) *Message { return &Message{Type: t} }

// Encode 便于测试与日志使用。
func (m *Message) Encode() ([]byte, error) { return json.Marshal(m) }
