package ai

import (
	"fmt"
	"log"
	"sync"

	"easylive-api/protocol"
	"easylive-api/sfu"
)

// Manager 管理各房间的 AI 总结会话（一个房间同时最多一个）。
type Manager struct {
	cfg Config

	mu       sync.Mutex
	sessions map[string]*Session // roomID → 会话
}

// New 创建 AI 管理器。cfg 不 Ready 也能创建——Start 时会明确报错。
func New(cfg Config) *Manager {
	return &Manager{cfg: cfg, sessions: make(map[string]*Session)}
}

// Config 返回当前配置（日志/排查用，不含密钥）。
func (m *Manager) Config() Config { return m.cfg }

// Start 启动房间的 AI 总结会话。已在运行则幂等返回 nil。
func (m *Manager) Start(room *sfu.Room, startedBy string) error {
	if !m.cfg.Ready() {
		return fmt.Errorf("AI 总结未配置：需要 OPENAI_API_KEY（Whisper 转写）与 DEEPSEEK_API_KEY（纪要生成）")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sessions[room.ID]; ok {
		return nil
	}
	s := newSession(m, room, startedBy)
	m.sessions[room.ID] = s
	// 让房间快照带上 AI 状态：中途入会的人能立刻看到纪要
	room.SetAISummaryProvider(s.state)
	room.Broadcast(&protocol.Message{
		Type:      protocol.TypeAISummaryState,
		AISummary: s.state(),
	}, "")
	log.Printf("ai: 会议总结已启动 room=%s by=%s", room.ID, startedBy)
	return nil
}

// Stop 停止房间的 AI 总结会话（幂等）。
func (m *Manager) Stop(roomID string) {
	m.mu.Lock()
	s, ok := m.sessions[roomID]
	if ok {
		delete(m.sessions, roomID)
	}
	m.mu.Unlock()
	if !ok {
		return
	 }
	s.Stop()
}

// State 返回房间的 AI 总结状态（未启用返回 Active=false）。
func (m *Manager) State(roomID string) *protocol.AISummaryState {
	m.mu.Lock()
	s, ok := m.sessions[roomID]
	m.mu.Unlock()
	if !ok {
		return &protocol.AISummaryState{Active: false}
	}
	return s.state()
}

// Active 返回房间的 AI 总结是否在运行。
func (m *Manager) Active(roomID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.sessions[roomID]
	return ok
}
