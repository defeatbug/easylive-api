package signaling

import (
	"encoding/json"
	"log"
	"sync/atomic"
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

// messageFloods 统计因超速被断开的连接数（观测用）。
var messageFloods atomic.Uint64

// MessageFloodCount 返回被断开的超速连接数。
func MessageFloodCount() uint64 { return messageFloods.Load() }

// handleConnection 是每条连接的主循环：读消息 → 分发 → 回写由 writePump 负责。
func (h *Hub) handleConnection(conn *fiberws.Conn) {
	ip := "unknown"
	if v := conn.Locals("clientIP"); v != nil {
		if s, ok := v.(string); ok && s != "" {
			ip = s
		}
	}

	client := newClient(conn, h, h.cfg, ip)
	// writePump 也要隔离：它 panic 会带走整个进程（所有人断开）
	sfu.SafeGo("signaling-write-pump/"+client.ID, client.writePump)
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

		// 单连接消息总预算：防止一个客户端刷消息、每条都引发全员广播。
		// 超限直接断开——正常客户端碰不到这个上限。
		if !client.allowMessage() {
			messageFloods.Add(1)
			log.Printf("signaling: 消息速率超限，断开 session=%s", client.ID)
			client.fail(nil, protocol.ErrBadRequest, "消息发送过快，连接已断开")
			return
		}

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

	case protocol.TypeFlash:
		h.handleFlash(c, msg)
	case protocol.TypeChat:
		h.handleChat(c, msg)

	case protocol.TypeModerate:
		h.handleModerate(c, msg)

	case protocol.TypeRecord:
		h.handleRecord(c, msg)

	case protocol.TypeAISummary:
		h.handleAISummary(c, msg)

	case protocol.TypeWhiteboard:
		h.handleWhiteboard(c, msg)

	case protocol.TypeHandState:
		h.handleHandState(c, msg)

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
		// 把**服务端生效的权限**随 joined 下发（无鉴权模式是 devGrants 全权限）。
		// 前端据此显示/隐藏主持人功能；只从令牌推断会与无鉴权模式不一致。
		Grants: c.grants(),
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
	// 鉴权开启时，恢复请求的令牌身份必须与原参与者一致：
	// 否则拿到别人 resumeToken 的人就能顶替其身份（令牌泄漏的影响面会扩大）。
	// 注意：校验必须在 room.Resume() 之前——Resume 会切换 notifier、停掉宽限期
	// 计时器并广播状态变更，校验失败时不应产生这些副作用（否则旧参与者已被
	// 激活，外层又会创建一个新参与者，同一身份出现两个人）。
	if claims != nil {
		p0, ok := room.ParticipantByID(entry.participantID)
		if !ok || p0.Identity() != claims.Identity() {
			log.Printf("signaling: 恢复请求的令牌身份与原参与者不一致，拒绝 session=%s", c.ID)
			// 令牌在 takeResume 时已被消费；身份校验失败不算一次真正的恢复，
			// 把令牌放回去，真正的持有者仍可在宽限期内重试。
			if ttl := time.Until(entry.expiresAt); ttl > 0 {
				h.registerResume(msg.ResumeToken, entry.roomID, entry.participantID, ttl)
			}
			return false
		}
	}
	p, ok := room.Resume(entry.participantID, c, msg.ResetMedia)
	if !ok {
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
		Grants:        c.grants(),
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
	offer, err := room.Unsubscribe(pid, msg.TrackID)
	if err != nil {
		c.fail(msg, protocol.ErrInternal, err.Error())
		return
	}
	// 订阅连接保留：把重协商的 offer 回给客户端，走已有的 subscribe 应答流程；
	// offer 为空表示之前就没订阅，无需通知
	if offer != "" {
		c.reply(msg, &protocol.Message{Type: protocol.TypeSubscribeOffer, SDP: offer})
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

// maxFlashTextRunes 是弹幕文字上限（超出截断，不丢弃）。
// 与 sfu 包里的同名常量保持一致；两个包各留一份是刻意的——
// signaling 不该为了一个数字去 import sfu（那会把媒体依赖带进信令层）。
const maxFlashTextRunes = 60

// handleFlash 广播一条弹幕 / 互动动效（**信令面**）。
//
// 与 handleChat 同一形状：服务端填 FromID/FromName（客户端无法伪造发送者），
// 然后广播给房间里**所有人**（含发送者自己）——发送者不需要本地回显，
// 这样"我看到的"和"别人看到的"永远是同一条消息，不会出现两边不一致。
func (h *Hub) handleFlash(c *Client, msg *protocol.Message) {
	roomID, pid, p := c.session()
	if roomID == "" || p == nil {
		c.fail(msg, protocol.ErrNotJoined, "尚未加入房间")
		return
	}
	if !c.requireGrant(msg, c.grants().CanPublishData, "发送弹幕") {
		return
	}
	emoji := msg.Emoji
	if emoji == "" || len([]rune(emoji)) > 8 {
		c.fail(msg, protocol.ErrBadRequest, "表情为空或过长")
		return
	}
	// 文字**截断而不是拒绝**：用户打了一长句，不该整条发不出去
	text := msg.Text
	if r := []rune(text); len(r) > maxFlashTextRunes {
		text = string(r[:maxFlashTextRunes])
	}
	// 只认识两种 kind：空与 action。未知值归零，免得客户端随便塞字符串。
	kind := msg.Kind
	if kind != "" && kind != protocol.ReactionKindAction {
		kind = ""
	}
	room, ok := h.manager.Get(roomID)
	if !ok {
		c.fail(msg, protocol.ErrInternal, "房间不存在")
		return
	}
	room.Broadcast(&protocol.Message{
		Type: protocol.TypeFlashMessage,
		Flash: &protocol.FlashMessage{
			FromID:   pid,
			FromName: p.Name,
			Emoji:    emoji,
			Text:     text,
			Kind:     kind,
			TS:       time.Now().UnixMilli(),
		},
	}, "") // 空 = 不排除任何人，发送者也会收到自己的那条

	// 带文字的弹幕**同时**记一条聊天消息：
	//   · 弹幕是瞬时的（飘几秒就没了），聊天是**历史**——发过的话应该能在聊天里找到；
	//   · 只有带文字时才记：纯表情发一条聊天记录会变成刷屏；
	//   · 由服务端广播而不是客户端本地塞：这样房间里**所有人**的聊天记录都一致。
	if text != "" {
		room.Broadcast(&protocol.Message{
			Type: protocol.TypeChatMessage,
			ChatMessage: &protocol.ChatMessage{
				FromID:   pid,
				FromName: p.Name,
				Text:     text,
				TS:       time.Now().UnixMilli(),
			},
		}, "")
	}
}

// handleHandState 处理客户端经**信令面**上报的举手。
//
// 为什么举手有两条路：数据面（SCTP）延迟更低，是首选；但数据面依赖发布连接完成协商，
// 而"没有摄像头 / 权限被拒"的用户可能从未协商过——那两个功能就静默失效了。
// 举手本质是**状态**（要可靠、要进房间快照、新人要能看到），所以信令面才是它天然的家，
// 数据面只是更快的同一条语义。两条路最终都调用同一个 Room.SetHand，重复送达幂等。
func (h *Hub) handleHandState(c *Client, msg *protocol.Message) {
	roomID, pid, p := c.session()
	if roomID == "" || p == nil {
		c.fail(msg, protocol.ErrNotJoined, "尚未加入房间")
		return
	}
	if msg.Raised == nil {
		c.fail(msg, protocol.ErrBadRequest, "缺少 raised")
		return
	}
	if !c.requireGrant(msg, c.grants().CanPublishData, "举手") {
		return
	}
	room, ok := h.manager.Get(roomID)
	if !ok {
		return
	}
	room.SetHand(pid, *msg.Raised)
}

// handleModerate 是主持人管理入口（强制静音 / 移出房间）。
//
// 两点设计考虑：
//  1. 权限在服务端强制校验（roomAdmin），前端隐藏按钮只是体验；
//  2. 踢人必须先把连接标记为"主动离开"再关闭——否则 Close 会走断线恢复分支，
//     被踢的人反而能在宽限期内回到房间，那就成了笑话。
//
// handleWhiteboard 处理一条白板操作（**所有参会者**都可以画）。
//
// 三件服务端必须做的事：
//  1. **校验与裁剪**：客户端不可信——越界坐标/超大点数组/非法颜色都在 Sanitize 里纠正或拒绝，
//     By 与 Seq 由服务端填写（不能自报身份，也不能自定顺序）；
//  2. **限速**：白板是高频小消息，挡住"按住不放刷满房间状态"；
//  3. **记入房间状态**：这样中途进来的人能从 joined 快照完整还原画面。
func (h *Hub) handleWhiteboard(c *Client, msg *protocol.Message) {
	roomID, pid, _ := c.session()
	if roomID == "" {
		c.fail(msg, protocol.ErrNotJoined, "尚未加入房间")
		return
	}
	if !c.grants().CanPublishData {
		c.fail(msg, protocol.ErrForbidden, "当前令牌无发布数据权限")
		return
	}
	if msg.Op == nil {
		c.fail(msg, protocol.ErrBadRequest, "缺少白板操作载荷")
		return
	}
	if !c.allowWhiteboardOp() {
		c.fail(msg, protocol.ErrBadRequest, "白板操作过于频繁")
		return
	}
	op := *msg.Op
	if err := op.Sanitize(); err != nil {
		c.fail(msg, protocol.ErrBadRequest, "白板操作非法: "+err.Error())
		return
	}
	room, ok := h.manager.Get(roomID)
	if !ok {
		c.fail(msg, protocol.ErrInternal, "房间不存在")
		return
	}
	accepted, ok := room.AddWhiteboardOp(op, pid)
	if !ok {
		// 白板已满 / 无可撤销内容（撤销只撤自己的笔画）：明确告知，而不是静默丢弃
		c.fail(msg, protocol.ErrBadRequest, "白板内容已达上限，或当前没有你可以撤销/清空的内容")
		return
	}
	// 广播给所有人（含提交者）：白板是共享状态，不做"仅他人"的差异化
	room.Broadcast(&protocol.Message{Type: protocol.TypeWhiteboardOp, Op: &accepted}, "")
}

// handleRecord 是录制开关（**任何已入会的成员**都可以用）。
//
// 设计上与 moderate 完全一致：**权限在服务端强制校验**，前端隐藏按钮只是体验。
// 录制是"转发即录制"（零转码），所以启停都是即时的：
// 开始 = 给房间里**已经在发布**的轨道挂上写入器；停止 = 摘掉并收尾清单。
func (h *Hub) handleRecord(c *Client, msg *protocol.Message) {
	roomID, pid, _ := c.session()
	if roomID == "" {
		c.fail(msg, protocol.ErrNotJoined, "尚未加入房间")
		return
	}
	// **任何已入会的成员都可以录制**（不再要求主持人）：
	// 录制是"转发即录制"，不额外消耗转码资源；会议内容对参与者本来就是可见的，
	// 而且主持人不一定在线——把录制权限只给主持人，反而会让"想留个记录"变成做不到的事。
	// 仍然要求已入会（上面已经校验），所以房间外的人无法操作。

	switch msg.Action {
	case protocol.ActionRecordStart:
		if _, err := h.manager.StartRecording(roomID, pid); err != nil {
			c.fail(msg, protocol.ErrInternal, "开始录制失败: "+err.Error())
			return
		}
		log.Printf("signaling: 录制开始 room=%s by=%s", roomID, pid)
	case protocol.ActionRecordStop:
		if sess := h.manager.StopRecording(); sess != nil {
			log.Printf("signaling: 录制结束 room=%s by=%s session=%s 文件=%d", roomID, pid, sess.ID, len(sess.Files))
		}
	default:
		c.fail(msg, protocol.ErrBadRequest, "未知的录制动作: "+msg.Action)
		return
	}

	// 广播给**所有人**（含操作者自己）：红点是所有人都该看到的房间状态，
	// 不是操作者的私人反馈。
	st := h.manager.RecordingState()
	if room, ok := h.manager.Get(roomID); ok {
		room.Broadcast(&protocol.Message{Type: protocol.TypeRecordingState, Recording: st, TargetID: msg.TargetID}, "")
	}
	c.reply(msg, &protocol.Message{Type: protocol.TypeRecord, Action: msg.Action, Recording: st})
}

// handleAISummary 是 AI 会议总结的开关（**任何已入会的成员**都可以用）。
//
// 与录制同一权限哲学：转写与纪要只处理参会者本来就能听到的音频，不引入新的
// 信息暴露面；是否开启由房间内的人决定。API Key 缺失时 Start 会明确报错，
// 前端据此提示用户联系管理员配置。
func (h *Hub) handleAISummary(c *Client, msg *protocol.Message) {
	roomID, pid, _ := c.session()
	if roomID == "" {
		c.fail(msg, protocol.ErrNotJoined, "尚未加入房间")
		return
	}
	room, ok := h.manager.Get(roomID)
	if !ok {
		c.fail(msg, protocol.ErrNotJoined, "房间不存在")
		return
	}

	switch msg.Action {
	case protocol.ActionAISummaryStart:
		if err := h.ai.Start(room, pid); err != nil {
			c.fail(msg, protocol.ErrInternal, "启动 AI 总结失败: "+err.Error())
			return
		}
		log.Printf("signaling: AI 总结启动 room=%s by=%s", roomID, pid)
	case protocol.ActionAISummaryStop:
		h.ai.Stop(roomID)
		log.Printf("signaling: AI 总结停止 room=%s by=%s", roomID, pid)
	default:
		c.fail(msg, protocol.ErrBadRequest, "未知的 AI 总结动作: "+msg.Action)
		return
	}

	// 广播给**所有人**（含操作者）：总结状态是房间共享状态；
	// 内容更新走 ai_summary_update，由 ai 会话定时推送。
	st := h.ai.State(roomID)
	room.Broadcast(&protocol.Message{Type: protocol.TypeAISummaryState, AISummary: st}, "")
	c.reply(msg, &protocol.Message{Type: protocol.TypeAISummary, Action: msg.Action, AISummary: st})
}

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
