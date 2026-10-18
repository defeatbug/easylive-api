package ai

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"easylive-api/protocol"
	"easylive-api/sfu"
)

// Session 是一次房间的 AI 总结会话：管一批轨道转写器、攒转写片段、定时生成纪要。
type Session struct {
	roomID string
	room   *sfu.Room
	mgr    *Manager

	mu           sync.Mutex
	transcribers map[string]*Transcriber // trackID → 转写器
	segments     []Segment
	markdown     string
	updatedAt    time.Time
	segmentCount int // 已广播的纪要对应的片段数（用于判断是否有新内容）

	startedAt     time.Time
	startedBy     string
	startedByName string

	unregisterHook func()
	stopCh         chan struct{}
	stopOnce       sync.Once
	wg             sync.WaitGroup
}

func newSession(mgr *Manager, room *sfu.Room, startedBy string) *Session {
	s := &Session{
		roomID:        room.ID,
		room:          room,
		mgr:           mgr,
		transcribers:  make(map[string]*Transcriber),
		startedAt:     time.Now(),
		startedBy:     startedBy,
		startedByName: room.ParticipantName(startedBy),
		stopCh:        make(chan struct{}),
	}
	// 挂载已在发布的音频轨道
	for _, tr := range room.AudioTracks() {
		s.attachTrack(tr)
	}
	// 订阅后续的轨道发布/下线
	s.unregisterHook = room.OnTrackEvent(func(tr *sfu.PublishedTrack, added bool) {
		select {
		case <-s.stopCh:
			return // 会话已结束，忽略
		default:
		}
		if added {
			s.attachTrack(tr)
		} else {
			s.detachTrack(tr.Info().ID)
		}
	})
	s.wg.Add(1)
	go s.summarizeLoop()
	return s
}

// attachTrack 给一条音频轨道挂上转写器（幂等）。
func (s *Session) attachTrack(tr *sfu.PublishedTrack) {
	info := tr.Info()
	if info.Kind != protocol.KindAudio {
		return
	}
	// 先取 speaker 名：此时不持有 s.mu，保证锁顺序恒为 room → session，
	// 与 Room.State()（持 room.mu 读锁调 provider → 取 s.mu）一致，避免死锁。
	speaker := s.room.ParticipantName(info.PublisherID)
	s.mu.Lock()
	if _, ok := s.transcribers[info.ID]; ok {
		s.mu.Unlock()
		return
	}
	tb := NewTranscriber(speaker, s.mgr.cfg.ChunkSeconds,
		s.mgr.cfg.OpenAIAPIKey, s.mgr.cfg.WhisperModel, s.mgr.cfg.WhisperLang,
		func(seg Segment) { s.onSegment(seg) })
	s.transcribers[info.ID] = tb
	s.mu.Unlock()
	tr.AttachAISink(tb)
	log.Printf("ai: 转写已挂载 room=%s track=%s speaker=%s", s.roomID, info.ID, speaker)
}

// detachTrack 摘掉一条轨道的转写器（转写器会把缓冲音频做最后一次转写）。
func (s *Session) detachTrack(trackID string) {
	s.mu.Lock()
	tb, ok := s.transcribers[trackID]
	if ok {
		delete(s.transcribers, trackID)
	}
	s.mu.Unlock()
	if !ok {
		return
	}
	// 注意：不在锁里 Stop（Stop 会等转写协程做完最后一次 Whisper 调用，可能几十秒）
	tb.Stop()
	// 轨道对象可能已经不在房间里了；能拿到就摘掉 tap，拿不到也无妨（读循环已结束）
	for _, tr := range s.room.AudioTracks() {
		if tr.Info().ID == trackID {
			tr.DetachAISink()
			break
		}
	}
	log.Printf("ai: 转写已摘除 room=%s track=%s", s.roomID, trackID)
}

// onSegment 收到一段转写文本，攒起来等下一次纪要生成。
func (s *Session) onSegment(seg Segment) {
	s.mu.Lock()
	s.segments = append(s.segments, seg)
	s.mu.Unlock()
	log.Printf("ai: 转写片段 room=%s [%s] %s：%s", s.roomID, seg.StartedAt.Format("15:04:05"), seg.Speaker, truncate(seg.Text, 60))
}

// summarizeLoop 定时检查新片段，有新内容就重新生成纪要并广播。
func (s *Session) summarizeLoop() {
	defer s.wg.Done()
	ticker := time.NewTicker(s.mgr.cfg.SummaryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			s.mu.Lock()
			n := len(s.segments)
			dirty := n > 0 && n != s.segmentCount
			s.mu.Unlock()
			if !dirty {
				continue
			}
			if err := s.regenerate(); err != nil {
				log.Printf("ai: 生成纪要失败 room=%s: %v", s.roomID, err)
				continue
			}
			s.mu.Lock()
			s.segmentCount = len(s.segments)
			s.mu.Unlock()
			s.broadcastUpdate()
		}
	}
}

// regenerate 用当前全部转写片段调 DeepSeek 生成 markdown 纪要。
func (s *Session) regenerate() error {
	s.mu.Lock()
	segs := make([]Segment, len(s.segments))
	copy(segs, s.segments)
	s.mu.Unlock()

	var b strings.Builder
	for _, sg := range segs {
		fmt.Fprintf(&b, "[%s] %s：%s\n", sg.StartedAt.Format("15:04:05"), sg.Speaker, sg.Text)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	md, err := Complete(ctx, s.mgr.cfg.DeepSeekAPIKey, s.mgr.cfg.DeepSeekModel, b.String())
	if err != nil {
		return err
	}
	if md == "" {
		return fmt.Errorf("deepseek 返回空纪要")
	}
	s.mu.Lock()
	s.markdown = md
	s.updatedAt = time.Now()
	s.mu.Unlock()
	log.Printf("ai: 纪要已更新 room=%s 片段=%d 长度=%d", s.roomID, len(segs), len(md))
	return nil
}

// state 返回当前状态快照（只读 s.mu 与预存字段，不碰 room 锁——
// Room.State() 持 r.mu 读锁时调 provider，里面再取 room 锁有死锁风险）。
func (s *Session) state() *protocol.AISummaryState {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := &protocol.AISummaryState{
		Active:        true,
		StartedAt:     s.startedAt.UnixMilli(),
		StartedBy:     s.startedBy,
		StartedByName: s.startedByName,
		SegmentCount:  len(s.segments),
	}
	if s.markdown != "" {
		st.Markdown = s.markdown
		st.UpdatedAt = s.updatedAt.UnixMilli()
	}
	return st
}

func (s *Session) broadcastUpdate() {
	s.room.Broadcast(&protocol.Message{
		Type:      protocol.TypeAISummaryUpdate,
		AISummary: s.state(),
	}, "")
}

// Stop 结束会话：停定时器、摘掉所有 tap、做最后一次纪要（有新片段才做）、广播关闭状态。
func (s *Session) Stop() {
	s.stopOnce.Do(func() {
		close(s.stopCh)
		s.wg.Wait()
		if s.unregisterHook != nil {
			s.unregisterHook()
		}
		// 摘掉所有轨道的 tap
		s.mu.Lock()
		tbs := make([]*Transcriber, 0, len(s.transcribers))
		for _, tb := range s.transcribers {
			tbs = append(tbs, tb)
		}
		s.transcribers = make(map[string]*Transcriber)
		s.mu.Unlock()
		for _, tr := range s.room.AudioTracks() {
			tr.DetachAISink()
		}
		for _, tb := range tbs {
			tb.Stop() // 最后一次转写（缓冲音频）
		}
		// 有新片段没进纪要？收尾生成一次
		s.mu.Lock()
		dirty := len(s.segments) > 0 && len(s.segments) != s.segmentCount
		s.mu.Unlock()
		if dirty {
			if err := s.regenerate(); err != nil {
				log.Printf("ai: 收尾纪要失败 room=%s: %v", s.roomID, err)
			} else {
				s.mu.Lock()
				s.segmentCount = len(s.segments)
				s.mu.Unlock()
			}
		}
		s.room.Broadcast(&protocol.Message{
			Type:      protocol.TypeAISummaryState,
			AISummary: &protocol.AISummaryState{Active: false},
		}, "")
		s.room.SetAISummaryProvider(nil)
		log.Printf("ai: 会议总结已停止 room=%s", s.roomID)
	})
}
