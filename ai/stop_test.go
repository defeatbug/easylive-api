package ai

// 回归测试：停止 AI 总结**不能阻塞调用方**。
//
// 场景：用户在会中点"停止 AI 总结"，信令读循环同步调 handleAISummary → Manager.Stop。
// 以前 Session.Stop 会在这里等每路转写器做完最后一次 Whisper、再等一次 DeepSeek 纪要
// （单次最长 150s），读循环被堵住后 60s 读超时会把**用户自己的连接**断开。

import (
	"testing"
	"time"

	"easylive-api/protocol"
	"easylive-api/sfu"
)

func TestSessionStopReturnsImmediately(t *testing.T) {
	mgr := New(Config{}) // 未配置也允许构造：Stop 的空会话不会走到 DeepSeek
	roomMgr := sfu.NewManager(nil, 8, 30*time.Second)
	room := roomMgr.GetOrCreate("room-ai-stop")

	// 先挂一个"看起来正在总结"的状态，验证 Stop 会把它清掉
	room.SetAISummaryProvider(func() *protocol.AISummaryState {
		return &protocol.AISummaryState{Active: true, Markdown: "# 旧纪要"}
	})

	s := &Session{
		roomID:       room.ID,
		room:         room,
		mgr:          mgr,
		transcribers: make(map[string]*Transcriber),
		startedAt:    time.Now(),
		stopCh:       make(chan struct{}),
	}

	start := time.Now()
	s.Stop()
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Stop 耗时 %v，必须快速返回（慢活在后台）", elapsed)
	}
	// 幂等：第二次直接返回，不 panic
	s.Stop()

	// 停止后房间快照不应再带 AI 总结（provider 已被清掉）
	if st := room.State(); st.AISummary != nil {
		t.Fatalf("停止后快照不应再带 AI 总结，实际 %+v", st.AISummary)
	}
}
