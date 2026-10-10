// 页面反复刷新场景的回归测试。
//
// 背景（用户实际报的问题）：一个访客连续点刷新，房间里就会堆出一屏"重连中"的瓦片，
// 在线人数被灌到房间上限。
//
// 根因链条：
//  1. 刷新时浏览器直接杀掉页面，WebSocket 断开前来不及发 leave，
//     服务端只能按 M4 的设计把它标记为"重连中"并保留一个宽限期；
//  2. 如果刷新后的新页面**不带 resumeToken** 重新入会，服务端就会把它当成新参与者，
//     于是每刷一次多一个身份，旧的还在宽限期里 → 堆积。
//
// 契约：带令牌刷新必须恢复**同一个 participantId**，房间人数不随刷新次数增长。
package signaling_test

import (
	"fmt"
	"testing"
	"time"

	"easylive-api/protocol"
)

func TestE2ERefreshLoopKeepsSingleParticipant(t *testing.T) {
	// 宽限期给足，确保"旧参与者"在整轮测试期间都不会自然过期——
	// 这样人数一旦增长就只能是"刷新被当成了新参与者"，而不是计时器碰巧清掉了它。
	const grace = 30 * time.Second
	ts := startServerFull(t, grace, "", "", "test")

	observer := dial(t, ts.url, "observer")
	defer observer.close()
	observer.join("room-refresh", "旁观者")

	var (
		token   string
		firstID string
	)

	const refreshes = 5
	for i := 0; i < refreshes; i++ {
		p := dial(t, ts.url, fmt.Sprintf("refresh-%d", i))
		msg := &protocol.Message{
			Type:        protocol.TypeJoin,
			RoomID:      "room-refresh",
			DisplayName: "访客492",
		}
		if token != "" {
			// 模拟刷新后的新页面：带上次的令牌，并且本端没有任何 PeerConnection
			// （所以必须让服务端清掉刷新前那套已死的轨道）
			msg.ResumeToken = token
			msg.ResetMedia = true
		}
		p.send(msg)
		joined := p.waitType(protocol.TypeJoined, 10*time.Second)

		if i > 0 {
			if !joined.Resumed {
				t.Fatalf("第 %d 次刷新应恢复同一身份（resumed=true），否则就是每次刷新都新建参与者", i+1)
			}
			if joined.ParticipantID != firstID {
				t.Fatalf("刷新后 participantId 应保持不变：%s → %s", firstID, joined.ParticipantID)
			}
		}
		firstID = joined.ParticipantID
		token = joined.ResumeToken
		if token == "" {
			t.Fatalf("joined 未下发 resumeToken，刷新后无法恢复身份")
		}

		// 房间始终只应有 2 人：旁观者 + 这个访客
		if got := len(joined.Room.Participants); got != 2 {
			t.Fatalf("第 %d 次刷新后房间应有 2 人，实际 %d 人：刷新被当成了新参与者（旧身份滞留在宽限期里）",
				i+1, got)
		}

		// 模拟页面被杀：直接断开，不发 leave
		_ = p.conn.Close()
	}

	// 收尾：旁观者应只看到"状态在重连与恢复之间切换"，而不是一串新参与者。
	// 这里不再断言事件序列（那属于 M4 的测试），人数不增长已经说明了问题。
}

// TestE2EFreshWithoutTokenCreatesGhost 记录"没有令牌时会发生什么"，
// 把当前行为（每次刷新 = 新参与者，直到宽限期结束）显式写下来。
//
// 这不是在鼓励这种用法，而是让"为什么必须持久化 resumeToken"有一个可执行的证据：
// 一旦将来有人把前端的令牌持久化改坏，这两个测试会同时告诉我们后果。
func TestE2EFreshWithoutTokenCreatesGhost(t *testing.T) {
	const grace = 30 * time.Second
	ts := startServerFull(t, grace, "", "", "test")

	observer := dial(t, ts.url, "observer2")
	defer observer.close()
	observer.join("room-ghost", "旁观者")

	for i := 0; i < 3; i++ {
		p := dial(t, ts.url, fmt.Sprintf("notoken-%d", i))
		p.send(&protocol.Message{Type: protocol.TypeJoin, RoomID: "room-ghost", DisplayName: "访客"})
		joined := p.waitType(protocol.TypeJoined, 10*time.Second)
		if joined.Resumed {
			t.Fatalf("不带令牌不应被识别为恢复")
		}
		// 旁观者 + i 个滞留在宽限期里的旧身份 = i + 2
		want := i + 2
		if got := len(joined.Room.Participants); got != want {
			t.Fatalf("无令牌刷新第 %d 次：预期 %d 人（旧身份滞留），实际 %d 人", i+1, want, got)
		}
		_ = p.conn.Close()
	}
}
