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

import (
	"encoding/json"
	"fmt"
	"math"

	// 引用 auth 是为了让 joined 能下发**服务端实际生效**的权限。
	// 这不破坏本包"中立"的初衷：中立是为了避免循环依赖，而 auth 不引用任何内部包，
	// 依赖方向是单向的（protocol ← signaling/auth）。权限形状本身就是线上协议的一部分。
	"easylive-api/auth"
)

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
	// 弹幕 / 互动：**走信令面**而不是数据面。
	//
	// 为什么改：数据面（SCTP）虽然延迟更低，但它依赖发布连接协商完成，实测在同一套
	// SDP 时序下出现了"服务端日志显示送达 1/1、对端浏览器却收不到"的情况，
	// 通道双方都报 open 也照样丢。举手当初就是因为同类原因保留了信令面这条路
	// （见 signaling/handler.go 里 handleHandState 的说明），弹幕与互动是**低频**消息，
	// 走信令完全够用，而且这条路上的聊天/举手/白板都已验证可靠。
	TypeFlash MsgType = "flash" // {emoji, text?, kind?}
	// TypeHandState 是**双向**消息：
	//   客户端 → 服务端：{raised} 上报举手（数据面不可用时的信令回退路径）
	//   服务端 → 客户端：{participantId, raised} 广播状态（并进房间快照，新人可见）
	TypeHandState MsgType = "hand_state" // 见上方双向说明
	TypePing      MsgType = "ping"       // {}
	TypeModerate  MsgType = "moderate"   // {targetId, action}  action: mute | kick（需要 roomAdmin 权限）
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
	TypeChatMessage      MsgType = "chat_message"       // {message}
	TypeFlashMessage     MsgType = "flash_message"      // {flash}
	TypePong             MsgType = "pong"               // {}
	TypeForceMute        MsgType = "force_mute"         // {muted}  服务端下发给被强制静音的参与者
	TypeError            MsgType = "error"              // {code, reason, id?}
	// TypeRecord 由**主持人**发起：{action: start|stop}，服务端校验 roomAdmin 后
	// 开始/结束一次录制会话，并向全房间广播 TypeRecordingState。
	TypeRecord MsgType = "record"
	// TypeWhiteboard 由**任何参会者**发起：{op:{kind, stroke}}。
	// 服务端做校验/裁剪/编号后广播为 TypeWhiteboardOp，并把它记进房间状态。
	TypeWhiteboard MsgType = "whiteboard"
	// TypeWhiteboardOp 服务端广播一条被接受的白板操作：{op:{kind, stroke, by, seq}}
	TypeWhiteboardOp MsgType = "whiteboard_op"
	// TypeRecordingState 服务端广播：{recording:{active, sessionId, startedAt, startedBy, files}}
	// 参会者据此显示"录制中"红点；入会快照 room.recording 也会带上它，
	// 保证中途进来的人同样看得到。
	TypeRecordingState MsgType = "recording_state"
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
	// Token 是鉴权令牌（JWT）。AUTH_MODE=required 时 join 必须携带；
	// 携带时其声明的身份与权限**优先于**客户端自报字段（昵称无法伪造）。
	Token string `json:"token,omitempty"`
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
	// Emoji / Kind 是弹幕与互动的入参（客户端 → 服务端）。
	// Emoji 复用数据面那个名字，Kind 为空表示普通表情浮层、"action" 表示互动动效。
	Emoji string `json:"emoji,omitempty"`
	Kind  string `json:"kind,omitempty"`
	// TargetID / Action 用于主持人管理（moderate）
	TargetID string `json:"targetId,omitempty"`
	Action   string `json:"action,omitempty"`
	// Recording 是录制状态载荷（record 的广播 / recording_state 消息）
	Recording *RecordingState `json:"recording,omitempty"`

	// Flash 是服务端广播出去的弹幕/互动（fromId/fromName 由服务端填，客户端无法伪造）
	Flash *FlashMessage `json:"flash,omitempty"`
	// Op 是白板操作载荷（whiteboard / whiteboard_op 用）
	Op *WhiteboardOp `json:"op,omitempty"`
	// Grants 是**服务端实际生效**的权限，随 joined 下发。
	//
	// 为什么必须由服务端下发：无鉴权模式（AUTH_MODE=off）下服务端给的是全权限 devGrants，
	// 而前端无法从"没有令牌"推断出这一点——于是服务端允许的操作（例如主持人录制）
	// 在界面上被藏掉。让服务端把生效权限说清楚，界面就不必猜。
	Grants *auth.VideoGrant `json:"grants,omitempty"`
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
	ID   string `json:"id"`
	Name string `json:"name"`
	// Identity 是**令牌身份**（跨断线重连保持不变），与 ID（服务端会话内标识）区分开，
	// 与 LiveKit 的 identity / sid 对应。无鉴权模式下退化为昵称。
	Identity string `json:"identity,omitempty"`
	JoinedAt int64  `json:"joinedAt"`
	// State 见 StateActive / StateReconnecting；空值等价于 active（兼容旧客户端）。
	State string `json:"state,omitempty"`
	// AvatarURL 是该参与者的头像地址；为空表示没设置头像，前端回落成首字母。
	// 由**服务端**按令牌身份查库填充，客户端无法自报——否则任何人都能挂别人的头像。
	AvatarURL string `json:"avatarUrl,omitempty"`
}

type TrackInfo struct {
	ID          string `json:"id"`
	PublisherID string `json:"publisherId"`
	Kind        string `json:"kind"`   // audio | video | screen
	Source      string `json:"source"` // microphone | camera | screen
	Muted       bool   `json:"muted"`
}

// 白板操作类型。
const (
	WhiteboardStroke = "stroke" // 一笔（含橡皮，用 tool 区分）
	WhiteboardClear  = "clear"  // 清空
	WhiteboardUndo   = "undo"   // 撤销上一笔
)

// 白板工具（服务端只做白名单校验，不解释绘制语义）。
var whiteboardTools = map[string]bool{
	"pen": true, "eraser": true, "line": true, "rect": true, "ellipse": true,
}

// WhiteboardOp 是一条白板操作。
type WhiteboardOp struct {
	Kind   string  `json:"kind"`             // stroke | clear | undo
	Stroke *Stroke `json:"stroke,omitempty"` // kind=stroke 时存在
	// By/Seq 由**服务端**填写：参与者 ID 与房间内递增序号。
	// 客户端提交时带的值一律被覆盖（不能自报身份，也不能自定顺序）。
	By  string `json:"by,omitempty"`
	Seq uint64 `json:"seq,omitempty"`
	// UndoSeq 仅 kind=undo 时由服务端填写：被撤销的那一笔的 Seq。
	// 客户端据此精确删除对应笔画，而不是盲目弹掉最后一笔。
	UndoSeq uint64 `json:"undoSeq,omitempty"`
}

// Stroke 是一笔画。
//
// 坐标**归一化到 0..1**：参会者窗口大小各不相同，用设备像素会导致"我画的圆在别人那里变形"。
// 归一化后前端按自己的画布尺寸缩放，任何人看到的图形比例都一致。
type Stroke struct {
	Tool   string       `json:"tool"`   // pen | eraser | line | rect | ellipse
	Color  string       `json:"color"`  // #RRGGBB
	Width  float64      `json:"width"`  // 1..64（归一化坐标下的千分之几，前端按宽度换算）
	Points [][2]float64 `json:"points"` // [[x,y],...]，x/y ∈ [0,1]
}

// 白板上限：房间内存上限，也是给客户端的明确边界。
const (
	MaxWhiteboardOps       = 600 // 一个房间最多保留多少条操作
	MaxWhiteboardPoints    = 400 // 单笔最多多少个点
	MaxWhiteboardOpsPerSec = 30  // 单个连接每秒最多提交多少条（防刷屏）
)

// Sanitize 校验并**就地纠正**一条客户端提交的白板操作。
//
// 为什么在服务端做：客户端不可信——越界坐标会让别人的画布出现画面外的笔迹，
// 超大 points 数组会拖垮所有人的渲染，而 By/Seq 更是必须由服务端填写。
func (o *WhiteboardOp) Sanitize() error {
	switch o.Kind {
	case WhiteboardClear, WhiteboardUndo:
		o.Stroke = nil
		return nil
	case WhiteboardStroke:
	default:
		return fmt.Errorf("未知的白板操作: %q", o.Kind)
	}
	s := o.Stroke
	if s == nil {
		return fmt.Errorf("stroke 操作缺少 stroke 载荷")
	}
	if !whiteboardTools[s.Tool] {
		return fmt.Errorf("未知的画笔工具: %q", s.Tool)
	}
	if !validHexColor(s.Color) {
		s.Color = "#ff4d4f" // 颜色非法就用默认色，不因此丢用户的一笔
	}
	if s.Width <= 0 {
		s.Width = 3
	}
	if s.Width > 64 {
		s.Width = 64
	}
	if len(s.Points) < 1 {
		return fmt.Errorf("笔画没有点")
	}
	if len(s.Points) > MaxWhiteboardPoints {
		return fmt.Errorf("笔画点数超限（%d > %d）", len(s.Points), MaxWhiteboardPoints)
	}
	// 坐标夹到 [0,1]；NaN/Inf 直接判非法（否则会污染整块画布的状态）
	for i := range s.Points {
		for j := 0; j < 2; j++ {
			v := s.Points[i][j]
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return fmt.Errorf("笔画坐标非法")
			}
			if v < 0 {
				v = 0
			} else if v > 1 {
				v = 1
			}
			s.Points[i][j] = v
		}
	}
	return nil
}

func validHexColor(s string) bool {
	if len(s) != 7 || s[0] != '#' {
		return false
	}
	for _, c := range s[1:] {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// RecordingState 是录制状态（广播 + 入会快照都用它）。
type RecordingState struct {
	Active    bool   `json:"active"`
	SessionID string `json:"sessionId,omitempty"`
	StartedAt int64  `json:"startedAt,omitempty"`
	StartedBy string `json:"startedBy,omitempty"`
	// Files 是当前会话已登记的文件数（"录了几路"），便于前端显示
	Files int `json:"files,omitempty"`
}

type RoomState struct {
	// Recording 指示当前房间是否正在录制（入会快照携带，中途进来的也能看到红点）
	Recording    *RecordingState `json:"recording,omitempty"`
	RoomID       string          `json:"roomId"`
	Participants []*PeerInfo     `json:"participants"`
	Tracks       []*TrackInfo    `json:"tracks"`
	// Hands 是已举手参与者的 ID 列表。举手是**状态**而不是瞬时事件，
	// 因此进快照：新入会者据此也能看到谁举了手。
	Hands []string `json:"hands,omitempty"`
	// Whiteboard 是房间当前的白板内容。同样进快照：中途进来的人不该看到一块空白板
	// （白板存的是操作序列而不是位图，所以这里能完整还原）。
	Whiteboard []WhiteboardOp `json:"whiteboard,omitempty"`
}

// ---------- 数据通道消息（SCTP 数据面，与 WebSocket 信令分开）----------
//
// 为什么要分两套：信令是**控制面**——可靠、有序、断线要能恢复，队列不能被高频小消息占满；
// 数据通道是**数据面**——低延迟、瞬时事件（表情、举手意图）丢了也无所谓。
// 二者混用会让重连快照、心跳这些关键消息被表情淹没。
const (
	DataReaction = "reaction" // {emoji, text?, kind?}
	// DataReaction 的两种用法：
	//   · 只带 emoji            → 原来的表情浮层（有人在角落里冒个表情）
	//   · emoji + text          → **弹幕**：底栏上方滚一条"谁：说了什么"
	//   · kind="action"         → **互动**：小红花之类从底栏往上弹一下
	// 都走 DataReaction 而不是各开一种类型：它们的载荷形状一样、频率特征一样
	// （瞬时、可丢），多开类型只会让中继与限流各写一遍。
	DataRaiseHand = "raise_hand" // {raised}

	// ReactionKindAction 表示这条 reaction 是**互动动效**（小红花之类的底部弹起），
	// 而不是角落里的表情浮层。空串等价于普通表情。
	ReactionKindAction = "action"
)

// DataMessage 是数据通道上的消息（与信令 Message 结构无关，单独一套）。
type DataMessage struct {
	Type   string `json:"type"`
	Emoji  string `json:"emoji,omitempty"`
	Raised *bool  `json:"raised,omitempty"`
	// Text 是表情附带的一句话（弹幕用）。服务端会截断到 60 字**而不是丢弃**：
	// 用户的输入不该因为超长就整个消失，截断是更友好的降级。
	Text string `json:"text,omitempty"`
	// Kind 区分"表情浮层"与"互动动效"：空 = 表情，action = 互动（小红花等）。
	Kind string `json:"kind,omitempty"`
	// FromName 由服务端在中继时填（和 FromID 一样，客户端无法伪造）。
	// 弹幕要显示"谁发的"，而名字必须是**服务端认定的那个**，否则任何人都能冒充别人发言。
	FromName string `json:"fromName,omitempty"`
	// FromID 由服务端在**中继时**填充：客户端不需要（也不应该）自己声明身份。
	FromID string `json:"fromId,omitempty"`
}

// SpeakerInfo 描述一位"正在说话"的参与者。
// Level 是归一化后的音频电平（0—1，1 为最大），由 RFC 6464 头扩展解析并做指数平滑。
type SpeakerInfo struct {
	ParticipantID string  `json:"participantId"`
	Level         float64 `json:"level"`
}

// FlashMessage 是一条弹幕或互动动效（走信令面广播）。
//
// 与聊天分开而不是复用 ChatMessage：聊天是**内容**（可能要进历史、有面板），
// 弹幕是**瞬时事件**（飘一下就走、不落库），生命周期不同，混在一起以后会互相牵制。
type FlashMessage struct {
	FromID   string `json:"fromId"`
	FromName string `json:"fromName"`
	Emoji    string `json:"emoji"`
	Text     string `json:"text,omitempty"`
	// Kind 为空是普通表情浮层；"action" 是互动动效（小红花等）
	Kind string `json:"kind,omitempty"`
	TS   int64  `json:"ts"`
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
	// 鉴权与权限相关
	ErrUnauthorized = "unauthorized" // 未带令牌 / 令牌无效或过期
	ErrForbidden    = "forbidden"    // 令牌有效但权限不足（房间不匹配、不能发布等）
)

// 主持人管理动作
const (
	// 强制静音某人（服务端停止转发其音频）
	ActionMute = "mute"
	// 录制动作（与 moderate 复用同一个 action 字段，避免再加一层结构）
	ActionRecordStart = "start"
	ActionRecordStop  = "stop"
	ActionKick        = "kick" // 移出房间
)

func New(t MsgType) *Message { return &Message{Type: t} }

// Encode 便于测试与日志使用。
func (m *Message) Encode() ([]byte, error) { return json.Marshal(m) }
