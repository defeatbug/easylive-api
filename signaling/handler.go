package signaling

import (
	"encoding/json"
	"log"
	"time"

	ws "github.com/fasthttp/websocket"
	fiberws "github.com/gofiber/contrib/websocket"
	"github.com/gofiber/fiber/v2"

	"easylive-api/auth"
	"easylive-api/protocol"
	"easylive-api/sfu"
)

// Mount 把信令入口挂到 Fiber 路由（约定：GET <basePath>/ws）。
func Mount(router fiber.Router, hub *Hub) {
	router.Use(func(c *fiber.Ctx) error {
		if fiberws.IsWebSocketUpgrade(c) {
			c.Locals("clientIP", c.IP())
			return c.Next()
		}
		// 非 WebSocket 请求直接拒绝，避免误用
		return fiber.ErrUpgradeRequired
	})
	router.Get("/", fiberws.New(hub.handleConnection))
}

// handleConnection 是每条连接的主循环：读消息 → 分发 → 回写由 writePump 负责。
func (h *Hub) handleConnection(conn *fiberws.Conn) {
	ip := "unknown"
	if v := conn.Locals("clientIP"); v != nil {
		if s, ok := v.(string); ok && s != "" {
			ip = s
		}
	}

	client := newClient(conn, h, h.cfg, ip)
	go client.writePump()
	h.Register(client)
	defer client.Close()

	conn.SetReadLimit(maxMessageLen)
	_ = conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	})

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			if ws.IsUnexpectedCloseError(err, ws.CloseGoingAway, ws.CloseNormalClosure) {
				log.Printf("signaling: 读异常 session=%s: %v", client.ID, err)
			}
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(60 * time.Second))

		var msg protocol.Message
		if err := json.Unmarshal(data, &msg); err != nil {
			client.fail(nil, protocol.ErrBadRequest, "消息不是合法 JSON")
			continue
		}
		h.dispatch(client, &msg)
	}
}

// dispatch 是协议的唯一分发点：请求 → 响应，状态变更 → 事件。
func (h *Hub) dispatch(c *Client, msg *protocol.Message) {
	switch msg.Type {
	case protocol.TypePing:
		c.reply(msg, &protocol.Message{Type: protocol.TypePong})

	case protocol.TypeJoin:
		h.handleJoin(c, msg)

	case protocol.TypeLeave:
		h.handleLeave(c, msg)

	case protocol.TypePublish:
		h.handlePublish(c, msg)

	case protocol.TypeAnswer:
		h.handleAnswer(c, msg)

	case protocol.TypeCandidate:
		h.handleCandidate(c, msg)

	case protocol.TypeSubscribe:
		h.handleSubscribe(c, msg)

	case protocol.TypeUnsubscribe:
		h.handleUnsubscribe(c, msg)

	case protocol.TypeUnpublish:
		h.handleUnpublish(c, msg)

	case protocol.TypeMute:
		h.handleMute(c, msg)

	case protocol.TypeChat:
		h.handleChat(c, msg)

	case protocol.TypeModerate:
		h.handleModerate(c, msg)

	default:
		c.fail(msg, protocol.ErrBadRequest, "未知消息类型: "+string(msg.Type))
	}
}

func (h *Hub) handleJoin(c *Client, msg *protocol.Message) {
	if msg.RoomID == "" {
		c.fail(msg, protocol.ErrBadRequest, "缺少 roomId")
		return
	}
	if _, pid, _ := c.session(); pid != "" {
		c.fail(msg, protocol.ErrBadRequest, "该连接已加入房间")
		return
	}

	// ---- 鉴权：身份与权限以令牌为准，客户端自报字段只在无鉴权模式下可信 ----
	claims, aerr := h.resolveClaims(c, msg)
	if aerr != nil {
		c.fail(msg, aerr.code, aerr.reason)
		return
	}
	if claims != nil {
		if !claims.Video.RoomJoin {
			c.fail(msg, protocol.ErrForbidden, "令牌未授予入会权限")
			return
		}
		if claims.Video.Room != "" && claims.Video.Room != msg.RoomID {
			c.fail(msg, protocol.ErrForbidden, "令牌不适用于该房间")
			return
		}
	}

	// 断线恢复：客户端带着上一次 joined 下发的 resumeToken 回来。
	// 命中则复用**同一个 participantId**，房间内其他人只会收到一次状态切换事件。
	if msg.ResumeToken != "" {
		if h.tryResume(c, msg, claims) {
			return
		}
		log.Printf("signaling: resumeToken 无效或已过期，按全新入会处理 session=%s room=%s", c.ID, msg.RoomID)
	}

	room := h.manager.GetOrCreate(msg.RoomID)
	pid := newID("p")

	// 身份与显示名：令牌存在时以令牌为准（昵称不可伪造）
	identity := ""
	name := msg.DisplayName
	if claims != nil {
		identity = claims.Identity()
		if claims.Name != "" {
			name = claims.Name
		} else if name == "" {
			name = identity
		}
	}
	if name == "" {
		name = "参与者-" + pid[len(pid)-4:]
	}

	p := sfu.NewParticipant(pid, name, h.manager.Engine(), c)
	c.setClaims(claims)
	p.SetIdentity(identity)
	p.SetCanPublishData(c.grants().CanPublishData)

	if err := room.Join(p); err != nil {
		c.fail(msg, protocol.ErrRoomFull, err.Error())
		return
	}
	c.setSession(msg.RoomID, pid, p)

	// 恢复令牌：断线后凭它回到同一个参与者身份（有效期 = ResumeGrace）。
	token := newID("r")
	p.SetResumeToken(token)
	c.setResumeToken(token)

	// joined 里带上房间快照：客户端据此直接建立订阅，无需额外的拉取请求。
	c.reply(msg, &protocol.Message{
		Type:          protocol.TypeJoined,
		SessionID:     c.ID,
		ParticipantID: pid,
		ResumeToken:   token,
		Room:          room.State(),
	})
}

// tryResume 处理断线恢复请求，成功时直接下发 joined 并返回 true。
func (h *Hub) tryResume(c *Client, msg *protocol.Message, claims *auth.Claims) bool {
	entry, ok := h.takeResume(msg.ResumeToken)
	if !ok || entry.roomID != msg.RoomID {
		return false
	}
	room, ok := h.manager.Get(entry.roomID)
	if !ok {
		return false
	}
	p, ok := room.Resume(entry.participantID, c, msg.ResetMedia)
	if !ok {
		return false
	}
	// 鉴权开启时，恢复请求的令牌身份必须与原参与者一致：
	// 否则拿到别人 resumeToken 的人就能顶替其身份（令牌泄漏的影响面会扩大）。
	if claims != nil && p.Identity() != claims.Identity() {
		log.Printf("signaling: 恢复请求的令牌身份与原参与者不一致，拒绝 session=%s", c.ID)
		return false
	}
	c.setClaims(claims)
	p.SetCanPublishData(c.grants().CanPublishData)
	c.setSession(entry.roomID, entry.participantID, p)
	c.setResumeToken(msg.ResumeToken) // 令牌延续使用，支持反复断线
	c.reply(msg, &protocol.Message{
		Type:          protocol.TypeJoined,
		SessionID:     c.ID,
		ParticipantID: entry.participantID,
		Resumed:       true,
		ResumeToken:   msg.ResumeToken,
		Room:          room.State(),
	})
	return true
}

func (h *Hub) handleLeave(c *Client, msg *protocol.Message) {
	roomID, pid, _ := c.session()
	if roomID == "" {
		c.fail(msg, protocol.ErrNotJoined, "尚未加入房间")
		return
	}
	c.markLeft() // 主动离会：不要进入恢复宽限期
	if room, ok := h.manager.Get(roomID); ok {
		room.Leave(pid)
	}
	h.dropResume(pid)
	c.setSession("", "", nil)
}

func (h *Hub) handlePublish(c *Client, msg *protocol.Message) {
	_, _, p := c.session()
	if p == nil {
		c.fail(msg, protocol.ErrNotJoined, "尚未加入房间")
		return
	}
	if !c.requireGrant(msg, c.grants().CanPublish, "发布媒体") {
		return
	}
	if msg.SDP == "" {
		c.fail(msg, protocol.ErrBadRequest, "缺少 sdp")
		return
	}
	answer, err := p.HandlePublishOffer(msg.SDP, msg.Source)
	if err != nil {
		log.Printf("signaling: 处理 publish 失败 session=%s: %v", c.ID, err)
		c.fail(msg, protocol.ErrInternal, err.Error())
		return
	}
	c.reply(msg, &protocol.Message{Type: protocol.TypePublishAnswer, SDP: answer})
}

func (h *Hub) handleAnswer(c *Client, msg *protocol.Message) {
	_, _, p := c.session()
	if p == nil {
		c.fail(msg, protocol.ErrNotJoined, "尚未加入房间")
		return
	}
	if err := p.HandleSubscribeAnswer(msg.SDP); err != nil {
		c.fail(msg, protocol.ErrInternal, err.Error())
	}
}

func (h *Hub) handleCandidate(c *Client, msg *protocol.Message) {
	_, _, p := c.session()
	if p == nil {
		c.fail(msg, protocol.ErrNotJoined, "尚未加入房间")
		return
	}
	if msg.Candidate == nil {
		c.fail(msg, protocol.ErrBadRequest, "缺少 candidate")
		return
	}
	if msg.Target == "" {
		msg.Target = protocol.TargetPub
	}
	if err := p.AddICECandidate(msg.Target, msg.Candidate); err != nil {
		// 候选到达早于连接建立是正常现象，降级为日志
		log.Printf("signaling: 添加 ICE 候选失败 session=%s target=%s: %v", c.ID, msg.Target, err)
	}
}

func (h *Hub) handleSubscribe(c *Client, msg *protocol.Message) {
	roomID, pid, _ := c.session()
	if roomID == "" {
		c.fail(msg, protocol.ErrNotJoined, "尚未加入房间")
		return
	}
	room, ok := h.manager.Get(roomID)
	if !ok {
		c.fail(msg, protocol.ErrInternal, "房间不存在")
		return
	}
	if !c.requireGrant(msg, c.grants().CanSubscribe, "订阅媒体") {
		return
	}
	track, ok := room.Track(msg.TrackID)
	if !ok {
		c.fail(msg, protocol.ErrBadRequest, "轨道不存在: "+msg.TrackID)
		return
	}
	offer, err := room.Subscribe(pid, msg.TrackID)
	if err != nil {
		c.fail(msg, protocol.ErrInternal, err.Error())
		return
	}
	// 订阅由服务端发起 offer（sub 连接），客户端回 answer
	c.reply(msg, &protocol.Message{Type: protocol.TypeSubscribeOffer, SDP: offer, Track: track.Info()})
}

func (h *Hub) handleUnsubscribe(c *Client, msg *protocol.Message) {
	roomID, pid, _ := c.session()
	if roomID == "" {
		c.fail(msg, protocol.ErrNotJoined, "尚未加入房间")
		return
	}
	room, ok := h.manager.Get(roomID)
	if !ok {
		c.fail(msg, protocol.ErrInternal, "房间不存在")
		return
	}
	if !c.requireGrant(msg, c.grants().CanSubscribe, "订阅媒体") {
		return
	}
	if err := room.Unsubscribe(pid, msg.TrackID); err != nil {
		c.fail(msg, protocol.ErrInternal, err.Error())
	}
}

// handleUnpublish 处理"停止发布某条轨道"（例如用户结束屏幕共享）。
func (h *Hub) handleUnpublish(c *Client, msg *protocol.Message) {
	roomID, pid, _ := c.session()
	if roomID == "" {
		c.fail(msg, protocol.ErrNotJoined, "尚未加入房间")
		return
	}
	if !c.requireGrant(msg, c.grants().CanPublish, "取消发布") {
		return
	}
	if msg.TrackID == "" {
		c.fail(msg, protocol.ErrBadRequest, "缺少 trackId")
		return
	}
	room, ok := h.manager.Get(roomID)
	if !ok {
		return // 房间已回收，无需处理
	}
	if err := room.Unpublish(pid, msg.TrackID); err != nil {
		c.fail(msg, protocol.ErrBadRequest, err.Error())
	}
}

func (h *Hub) handleMute(c *Client, msg *protocol.Message) {
	roomID, pid, _ := c.session()
	if roomID == "" {
		c.fail(msg, protocol.ErrNotJoined, "尚未加入房间")
		return
	}
	if msg.Muted == nil {
		c.fail(msg, protocol.ErrBadRequest, "缺少 muted")
		return
	}
	if !c.requireGrant(msg, c.grants().CanPublish, "修改静音状态") {
		return
	}
	room, ok := h.manager.Get(roomID)
	if !ok {
		c.fail(msg, protocol.ErrInternal, "房间不存在")
		return
	}
	// 归属校验：只能改自己的轨道。改别人的要走 moderate（需要 roomAdmin）——
	// 否则任何人只要猜到 trackId 就能把别人静音，这是典型的越权漏洞（IDOR）。
	if track, ok := room.Track(msg.TrackID); !ok {
		c.fail(msg, protocol.ErrBadRequest, "轨道不存在: "+msg.TrackID)
		return
	} else if info := track.Info(); info.PublisherID != pid && !c.grants().RoomAdmin {
		c.fail(msg, protocol.ErrForbidden, "无权修改他人的轨道")
		return
	}
	if err := room.SetTrackMuted(msg.TrackID, *msg.Muted); err != nil {
		c.fail(msg, protocol.ErrBadRequest, err.Error())
	}
}

func (h *Hub) handleChat(c *Client, msg *protocol.Message) {
	roomID, pid, p := c.session()
	if roomID == "" || p == nil {
		c.fail(msg, protocol.ErrNotJoined, "尚未加入房间")
		return
	}
	if !c.requireGrant(msg, c.grants().CanPublishData, "发送消息") {
		return
	}
	if msg.Text == "" || len(msg.Text) > 2000 {
		c.fail(msg, protocol.ErrBadRequest, "聊天内容为空或过长")
		return
	}
	room, ok := h.manager.Get(roomID)
	if !ok {
		c.fail(msg, protocol.ErrInternal, "房间不存在")
		return
	}
	room.Broadcast(&protocol.Message{
		Type: protocol.TypeChatMessage,
		ChatMessage: &protocol.ChatMessage{
			FromID:   pid,
			FromName: p.Name,
			Text:     msg.Text,
			TS:       time.Now().UnixMilli(),
		},
	}, "")
}

// handleModerate 是主持人管理入口（强制静音 / 移出房间）。
//
// 两点设计考虑：
//  1. 权限在服务端强制校验（roomAdmin），前端隐藏按钮只是体验；
//  2. 踢人必须先把连接标记为"主动离开"再关闭——否则 Close 会走断线恢复分支，
//     被踢的人反而能在宽限期内回到房间，那就成了笑话。
func (h *Hub) handleModerate(c *Client, msg *protocol.Message) {
	roomID, pid, _ := c.session()
	if roomID == "" {
		c.fail(msg, protocol.ErrNotJoined, "尚未加入房间")
		return
	}
	if !c.requireGrant(msg, c.grants().RoomAdmin, "主持人操作") {
		return
	}
	if msg.TargetID == "" {
		c.fail(msg, protocol.ErrBadRequest, "缺少 targetId")
		return
	}
	if msg.TargetID == pid {
		c.fail(msg, protocol.ErrBadRequest, "不能对自己执行该操作")
		return
	}
	room, ok := h.manager.Get(roomID)
	if !ok {
		c.fail(msg, protocol.ErrInternal, "房间不存在")
		return
	}
	if _, ok := room.ParticipantByID(msg.TargetID); !ok {
		c.fail(msg, protocol.ErrBadRequest, "目标参与者不在房间内")
		return
	}

	switch msg.Action {
	case protocol.ActionMute:
		room.ForceMute(msg.TargetID, true)
	case protocol.ActionKick:
		room.Leave(msg.TargetID) // 广播 participant_left 与轨道下线
		if target := h.clientByParticipant(msg.TargetID); target != nil {
			target.markLeft() // 关键：不要让它进入恢复宽限期
			target.Close()
		} else {
			log.Printf("signaling: 未找到被踢者的信令连接 target=%s（房间内已移除，但其连接仍存活）", msg.TargetID)
		}
		log.Printf("signaling: 主持人移出参与者 room=%s by=%s target=%s", roomID, pid, msg.TargetID)
	default:
		c.fail(msg, protocol.ErrBadRequest, "未知的主持人操作: "+msg.Action)
		return
	}

	// 回执：前端据此提示"操作成功"（幂等键 id 原样带回）
	c.reply(msg, &protocol.Message{Type: protocol.TypeModerate, TargetID: msg.TargetID, Action: msg.Action})
}
