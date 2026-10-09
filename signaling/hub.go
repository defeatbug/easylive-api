package signaling

import (
	"log"
	"sync"
	"time"

	"easylive-api/config"
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

	mu      sync.RWMutex
	clients map[string]*Client // sessionID -> client

	resumeMu sync.Mutex
	resumes  map[string]resumeEntry // resumeToken -> 可恢复的参与者
}

func NewHub(manager *sfu.Manager, cfg *config.Config) *Hub {
	return &Hub{
		manager: manager,
		cfg:     cfg,
		clients: make(map[string]*Client),
		resumes: make(map[string]resumeEntry),
	}
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

func (h *Hub) Count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}
