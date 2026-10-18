package signaling

import (
	"log"
	"sync"
	"time"

	"easylive-api/ai"
	"easylive-api/auth"
	"easylive-api/config"
	"easylive-api/protocol"
	"easylive-api/sfu"
)

// resumeEntry 记录一个"可以恢复的参与者身份"。
// 只在连接断开时登记，恢复成功或宽限期结束时删除——所以令牌本身就是一次性的。
type resumeEntry struct {
	roomID        string
	participantID string
	expiresAt     time.Time
}

// Hub 管理所有信令会话（连接级），并把消息路由到 SFU 房间。
type Hub struct {
	manager *sfu.Manager
	cfg     *config.Config
	auth    *auth.Authenticator
	// ai 是 AI 会议总结管理器（房间级会话）。配置缺失时 Start 会明确报错，
	// 不影响正常信令流程。
	ai *ai.Manager

	mu      sync.RWMutex
	clients map[string]*Client // sessionID -> client

	resumeMu sync.Mutex
	resumes  map[string]resumeEntry // resumeToken -> 可恢复的参与者
}

func NewHub(manager *sfu.Manager, cfg *config.Config) *Hub {
	aiCfg := ai.ConfigFromEnv()
	if !aiCfg.Ready() {
		log.Printf("signaling: AI 总结未配置（缺 OPENAI_API_KEY 或 DEEPSEEK_API_KEY），ai_summary 请求会被拒绝")
	} else {
		log.Printf("signaling: AI 总结已就绪（Whisper=%s，DeepSeek=%s）", aiCfg.WhisperModel, aiCfg.DeepSeekModel)
	}
	return &Hub{
		manager: manager,
		cfg:     cfg,
		auth:    auth.New(cfg.JWTSecret, cfg.TokenTTL, auth.ParseMode(cfg.AuthMode)),
		ai:      ai.New(aiCfg),
		clients: make(map[string]*Client),
		resumes: make(map[string]resumeEntry),
	}
}

// Authenticator 暴露给 HTTP 层用于签发令牌（/token）。
func (h *Hub) Authenticator() *auth.Authenticator { return h.auth }

// authFailure 是鉴权失败的结构化结果（避免在多层里传 error + code 两个值）。
type authFailure struct {
	code   string
	reason string
}

// resolveClaims 解析并校验请求里的令牌。
//
// 三态语义：
//   - off：不校验，返回 nil（后续按"全权限"处理）
//   - optional：有令牌就校验并用它的身份；没有/无效则退化为自报昵称（便于灰度）
//   - required：必须带有效令牌，否则返回错误
func (h *Hub) resolveClaims(c *Client, msg *protocol.Message) (*auth.Claims, *authFailure) {
	a := h.auth
	if !a.Enabled() {
		// 配置说"要校验"但鉴权不可用（缺密钥等）：**绝不能**当成"不需要令牌"继续。
		// 因为"不需要令牌"在 grants() 里等价于 devGrants（全权限含 roomAdmin），
		// 那等于把主持人的钥匙发给所有人。启动时的 config.Validate 已经拦住这种配置，
		// 这里是纵深防御：万一有人绕过校验重建了 Hub，也要拒绝而不是放行。
		if a.Mode() != auth.ModeOff {
			log.Printf("signaling: 鉴权不可用但 AUTH_MODE=%s，拒绝连接 session=%s", a.Mode(), c.ID)
			return nil, &authFailure{protocol.ErrUnauthorized, "服务端鉴权不可用，拒绝入会"}
		}
		return nil, nil
	}
	if msg.Token == "" {
		if a.Mode() == auth.ModeRequired {
			return nil, &authFailure{protocol.ErrUnauthorized, "缺少访问令牌（AUTH_MODE=required）"}
		}
		return nil, nil
	}
	claims, err := a.Verify(msg.Token)
	if err != nil {
		if a.Mode() == auth.ModeRequired {
			return nil, &authFailure{protocol.ErrUnauthorized, err.Error()}
		}
		log.Printf("signaling: 令牌无效，但 AUTH_MODE=optional 按匿名处理 session=%s: %v", c.ID, err)
		return nil, nil
	}
	return claims, nil
}

// clientByParticipant 按参与者 ID 找到其信令连接（主持人踢人时要把连接也关掉）。
func (h *Hub) clientByParticipant(participantID string) *Client {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, cl := range h.clients {
		if _, pid, _ := cl.session(); pid == participantID {
			return cl
		}
	}
	return nil
}

// registerResume 登记一个可恢复的会话（连接断开时调用）。
func (h *Hub) registerResume(token, roomID, participantID string, ttl time.Duration) {
	if token == "" {
		return
	}
	h.resumeMu.Lock()
	defer h.resumeMu.Unlock()
	h.purgeExpiredLocked()
	h.resumes[token] = resumeEntry{roomID: roomID, participantID: participantID, expiresAt: time.Now().Add(ttl)}
}

// takeResume 取出并消费一个恢复令牌；令牌不可复用，避免旧连接凭旧令牌再次抢占身份。
func (h *Hub) takeResume(token string) (resumeEntry, bool) {
	if token == "" {
		return resumeEntry{}, false
	}
	h.resumeMu.Lock()
	defer h.resumeMu.Unlock()
	e, ok := h.resumes[token]
	if !ok {
		return resumeEntry{}, false
	}
	delete(h.resumes, token)
	if time.Now().After(e.expiresAt) {
		return resumeEntry{}, false
	}
	return e, true
}

// dropResume 主动离会时清理该参与者的恢复令牌（正常离开不应还能恢复）。
func (h *Hub) dropResume(participantID string) {
	h.resumeMu.Lock()
	defer h.resumeMu.Unlock()
	for k, v := range h.resumes {
		if v.participantID == participantID {
			delete(h.resumes, k)
		}
	}
}

// purgeExpiredLocked 惰性清理过期条目（无需额外 goroutine）。
func (h *Hub) purgeExpiredLocked() {
	now := time.Now()
	for k, v := range h.resumes {
		if now.After(v.expiresAt) {
			delete(h.resumes, k)
		}
	}
}

func (h *Hub) Register(c *Client) {
	h.mu.Lock()
	h.clients[c.ID] = c
	n := len(h.clients)
	h.mu.Unlock()

	if n >= h.cfg.WSMaxConnections {
		// 超出上限：拒绝新连接（保护服务器资源，与 egg-api 的做法一致）
		log.Printf("signaling: 连接数达到上限 %d，拒绝 session=%s", h.cfg.WSMaxConnections, c.ID)
		c.fail(nil, "server_at_capacity", "服务器连接数已达上限")
		c.Close()
		return
	}
	log.Printf("signaling: 连接建立 session=%s ip=%s（当前 %d）", c.ID, c.RemoteIP, n)
}

func (h *Hub) Unregister(c *Client) {
	h.mu.Lock()
	_, ok := h.clients[c.ID]
	delete(h.clients, c.ID)
	n := len(h.clients)
	h.mu.Unlock()

	if ok {
		log.Printf("signaling: 连接断开 session=%s（当前 %d）", c.ID, n)
	}
}

// KickIdentity 关闭某个身份的所有在线连接，返回被断开的数量。
//
// "同一账号只允许一处在线"需要两步：会话版本让旧 Cookie 失效（auth 层），
// 而**已经连上的 WebSocket 不会自己断开**——旧设备会继续开会、继续推流。
// 登录成功时调用它，才算真正把旧设备请下线。
func (h *Hub) KickIdentity(identity, exceptSessionID string) int {
	if identity == "" {
		return 0
	}
	h.mu.RLock()
	targets := make([]*Client, 0, 2)
	for id, c := range h.clients {
		if id == exceptSessionID {
			continue
		}
		if who := c.identity(); who != "" && who == identity {
			targets = append(targets, c)
		}
	}
	h.mu.RUnlock()
	for _, c := range targets {
		c.fail(nil, "session_replaced", "该账号已在其他地方登录，当前连接已断开")
		c.Close()
	}
	return len(targets)
}

func (h *Hub) Count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}
