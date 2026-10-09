package signaling

import (
	"encoding/json"
	"log"
	"time"

	ws "github.com/fasthttp/websocket"
	fiberws "github.com/gofiber/contrib/websocket"
	"github.com/gofiber/fiber/v2"

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

	// 断线恢复：客户端带着上一次 joined 下发的 resumeToken 回来。
	// 命中则复用**同一个 participantId**，房间内其他人只会收到一次状态切换事件。
	if msg.ResumeToken != "" {
		if h.tryResume(c, msg) {
			return
		}
		log.Printf("signaling: resumeToken 无效或已过期，按全新入会处理 session=%s room=%s", c.ID, msg.RoomID)
	}

	room := h.manager.GetOrCreate(msg.RoomID)
	pid := newID("p")
	name := msg.DisplayName
	if name == "" {
		name = "参与者-" + pid[len(pid)-4:]
	}
	p := sfu.NewParticipant(pid, name, h.manager.Engine(), c)

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
func (h *Hub) tryResume(c *Client, msg *protocol.Message) bool {
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
	roomID, _, _ := c.session()
	if roomID == "" {
		c.fail(msg, protocol.ErrNotJoined, "尚未加入房间")
		return
	}
	if msg.Muted == nil {
		c.fail(msg, protocol.ErrBadRequest, "缺少 muted")
		return
	}
	room, ok := h.manager.Get(roomID)
	if !ok {
		c.fail(msg, protocol.ErrInternal, "房间不存在")
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
