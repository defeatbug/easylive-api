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
	stt STTBackend // 转写后端（Start 时按配置构建，失败则拒绝启动）

	mu       sync.Mutex
	sessions map[string]*Session // roomID → 会话
}

// New 创建 AI 管理器。cfg 不 Ready 也能创建——Start 时会明确报错。
func New(cfg Config) *Manager {
	return &Manager{cfg: cfg, sessions: make(map[string]*Session)}
}

// buildSTT 按配置构建转写后端。
func (m *Manager) buildSTT() (STTBackend, error) {
	if m.cfg.STTBackend == "api" {
		return NewWhisperAPI(m.cfg.OpenAIAPIKey, m.cfg.WhisperModel), nil
	}
	return NewWhisperLocal(m.cfg.WhisperBin, m.cfg.WhisperModelPath)
}

// Config 返回当前配置（日志/排查用，不含密钥）。
func (m *Manager) Config() Config { return m.cfg }

// Start 启动房间的 AI 总结会话。已在运行则幂等返回 nil。
func (m *Manager) Start(room *sfu.Room, startedBy string) error {
	if !m.cfg.Ready() {
		return fmt.Errorf("AI 总结未配置：%s", m.cfg.UnreadyReason())
	}
	// 先构建转写后端：本地模式缺二进制/模型时在这里就明确报错，
	// 而不是等到第一段音频来了才炸。
	stt, err := m.buildSTT()
	if err != nil {
		return fmt.Errorf("AI 总结转写后端初始化失败: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sessions[room.ID]; ok {
		return nil
	}
	m.stt = stt
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

// SummaryMarkdown 返回房间最近一次生成的纪要 markdown（没有则返回 ""）。
// 供 AI 问答助手做上下文：回答"刚才谁说了什么"这类问题时有依据。
func (m *Manager) SummaryMarkdown(roomID string) string {
	m.mu.Lock()
	s, ok := m.sessions[roomID]
	m.mu.Unlock()
	if !ok {
		return ""
	}
	return s.state().Markdown
}
