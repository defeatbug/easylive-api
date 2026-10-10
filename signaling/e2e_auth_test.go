// 准入与权限模型的端到端测试。
//
// 覆盖三件事：
//  1. 鉴权 required 模式下：无令牌 / 房间不匹配 / 权限不足都会被服务端拒绝——**服务端强制**，
//     不是靠前端隐藏按钮；
//  2. 令牌身份优先于客户端自报昵称（防止冒充）；
//  3. 主持人管理（强制静音、移出房间）：非管理员被拒；踢人必须**立刻**离会而不是进入恢复宽限期。
package signaling_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"easylive-api/auth"
	"easylive-api/config"
	"easylive-api/protocol"
)

const testSecret = "test-secret-please-change"

// issueToken 直接用服务端同款配置签一个令牌（等价于线上登录后签发）。
func issueToken(t *testing.T, cfg *config.Config, identity, name string, g auth.VideoGrant) string {
	t.Helper()
	a := auth.New(cfg.JWTSecret, cfg.TokenTTL, auth.ParseMode(cfg.AuthMode))
	token, _, err := a.Issue(identity, name, g)
	if err != nil {
		t.Fatalf("签发令牌失败: %v", err)
	}
	return token
}

// joinWithToken 带令牌入会
func joinWithToken(p *peer, roomID, displayName, token string) *protocol.Message {
	p.send(&protocol.Message{Type: protocol.TypeJoin, RoomID: roomID, DisplayName: displayName, Token: token})
	return p.waitType(protocol.TypeJoined, 10*time.Second)
}

// waitError 等待一条指定错误码的响应（注意：不能用 waitType，它遇到 error 会直接失败）
func waitError(t *testing.T, p *peer, code string, timeout time.Duration) *protocol.Message {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case m := <-p.events:
			if m.Type == protocol.TypeError && m.Code == code {
				return m
			}
			if m.Type == protocol.TypeJoined {
				t.Fatalf("[%s] 期望被拒绝（%s），却入会成功了", p.name, code)
			}
		case <-deadline:
			t.Fatalf("[%s] 等待错误 %s 超时", p.name, code)
			return nil
		}
	}
}

func TestE2EAuthRequired(t *testing.T) {
	ts := startServerFull(t, 0, "required", testSecret, "development")

	// ---- 1) 无令牌：直接拒绝 ----
	anon := dial(t, ts.url, "anon")
	defer anon.close()
	anon.send(&protocol.Message{Type: protocol.TypeJoin, RoomID: "room-auth", DisplayName: "匿名"})
	if ev := waitError(t, anon, protocol.ErrUnauthorized, 5*time.Second); ev.Reason == "" {
		t.Fatalf("拒绝时应给出原因: %+v", ev)
	}

	// ---- 2) 令牌有效但不属于该房间：拒绝 ----
	otherRoom := issueToken(t, ts.cfg, "u-other", "别人", auth.VideoGrant{
		RoomJoin: true, Room: "other-room", CanPublish: true, CanSubscribe: true, CanPublishData: true,
	})
	wrong := dial(t, ts.url, "wrong-room")
	defer wrong.close()
	wrong.send(&protocol.Message{Type: protocol.TypeJoin, RoomID: "room-auth", DisplayName: "x", Token: otherRoom})
	waitError(t, wrong, protocol.ErrForbidden, 5*time.Second)

	// ---- 3) 正常入会：身份与昵称都以令牌为准 ----
	limited := issueToken(t, ts.cfg, "u-1", "张三", auth.VideoGrant{
		RoomJoin: true, Room: "room-auth",
		CanPublish: false, CanSubscribe: true, CanPublishData: true,
	})
	u := dial(t, ts.url, "user")
	defer u.close()
	joined := joinWithToken(u, "room-auth", "我要冒充管理员", limited)

	var self *protocol.PeerInfo
	for _, p := range joined.Room.Participants {
		if p.ID == joined.ParticipantID {
			self = p
		}
	}
	if self == nil {
		t.Fatalf("房间快照里找不到自己: %+v", joined.Room)
	}
	if self.Name != "张三" {
		t.Fatalf("显示名应取自令牌（张三），而不是客户端自报，实际 %q", self.Name)
	}
	if self.Identity != "u-1" {
		t.Fatalf("identity 应为令牌的 sub=u-1，实际 %q", self.Identity)
	}

	// ---- 4) 无 canPublish：发布被拒 ----
	u.send(&protocol.Message{Type: protocol.TypePublish, SDP: "v=0"})
	waitError(t, u, protocol.ErrForbidden, 5*time.Second)

	// ---- 5) 有 canSubscribe 但轨道不存在：是 bad_request 而不是 forbidden ----
	// （这个区分很重要：权限不足与参数错误必须能分辨，否则前端没法给出正确的提示）
	u.send(&protocol.Message{Type: protocol.TypeSubscribe, TrackID: "no-such-track"})
	waitError(t, u, protocol.ErrBadRequest, 5*time.Second)

	// ---- 6) 有 canPublishData：聊天放行 ----
	u.send(&protocol.Message{Type: protocol.TypeChat, Text: "hello"})
	msg := u.waitType(protocol.TypeChatMessage, 5*time.Second)
	if msg.ChatMessage == nil || msg.ChatMessage.Text != "hello" {
		t.Fatalf("聊天应放行: %+v", msg.ChatMessage)
	}

	// ---- 7) 只读令牌：发布 / 订阅 / 发数据全部被拒 ----
	readonly := issueToken(t, ts.cfg, "u-2", "只读观察者", auth.VideoGrant{
		RoomJoin: true, Room: "room-auth", CanPublish: false, CanSubscribe: false, CanPublishData: false,
	})
	ro := dial(t, ts.url, "readonly")
	defer ro.close()
	joinWithToken(ro, "room-auth", "只读观察者", readonly)

	ro.send(&protocol.Message{Type: protocol.TypePublish, SDP: "v=0"})
	waitError(t, ro, protocol.ErrForbidden, 5*time.Second)

	ro.send(&protocol.Message{Type: protocol.TypeSubscribe, TrackID: "no-such-track"})
	waitError(t, ro, protocol.ErrForbidden, 5*time.Second)

	ro.send(&protocol.Message{Type: protocol.TypeChat, Text: "hi"})
	waitError(t, ro, protocol.ErrForbidden, 5*time.Second)
}

func TestE2EModerate(t *testing.T) {
	// 宽限期故意设长：用来证明"踢人不会进入恢复宽限期，而是立刻离会"
	ts := startServerFull(t, 5*time.Second, "required", testSecret, "development")

	adminTok := issueToken(t, ts.cfg, "admin-1", "主持人", auth.VideoGrant{
		RoomJoin: true, Room: "room-mod", CanPublish: true, CanSubscribe: true, CanPublishData: true, RoomAdmin: true,
	})
	userTok := issueToken(t, ts.cfg, "user-1", "小白", auth.VideoGrant{
		RoomJoin: true, Room: "room-mod", CanPublish: true, CanSubscribe: true, CanPublishData: true,
	})

	admin := dial(t, ts.url, "admin")
	defer admin.close()
	adminJoined := joinWithToken(admin, "room-mod", "主持人", adminTok)

	user := dial(t, ts.url, "user")
	defer user.close()
	userJoined := joinWithToken(user, "room-mod", "小白", userTok)
	_ = admin.waitType(protocol.TypeParticipantJoin, 5*time.Second)

	// ---- 1) 非管理员执行主持人操作：拒绝 ----
	user.send(&protocol.Message{
		Type: protocol.TypeModerate, Action: protocol.ActionMute, TargetID: adminJoined.ParticipantID,
	})
	waitError(t, user, protocol.ErrForbidden, 5*time.Second)

	// ---- 2) 管理员强制静音：目标收到 force_mute，管理员收到回执 ----
	admin.send(&protocol.Message{
		Type: protocol.TypeModerate, Action: protocol.ActionMute, TargetID: userJoined.ParticipantID, ID: "mod-1",
	})
	forced := user.waitType(protocol.TypeForceMute, 5*time.Second)
	if forced.Muted == nil || !*forced.Muted {
		t.Fatalf("目标应收到 force_mute(muted=true): %+v", forced)
	}
	ack := admin.waitType(protocol.TypeModerate, 5*time.Second)
	if ack.ID != "mod-1" || ack.TargetID != userJoined.ParticipantID {
		t.Fatalf("回执不正确: %+v", ack)
	}

	// ---- 3) 管理员踢人：立刻离会（不是 5s 宽限期），且目标连接被关闭 ----
	start := time.Now()
	admin.send(&protocol.Message{
		Type: protocol.TypeModerate, Action: protocol.ActionKick, TargetID: userJoined.ParticipantID, ID: "mod-2",
	})
	left := admin.waitType(protocol.TypeParticipantLeave, 3*time.Second)
	if left.ParticipantID != userJoined.ParticipantID {
		t.Fatalf("应收到被踢者的 participant_left: %+v", left)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("踢人应立即生效，实际耗时 %s（宽限期 5s 说明走进了恢复分支）", elapsed)
	}
	select {
	case <-user.done:
	case <-time.After(3 * time.Second):
		t.Fatalf("被踢者的连接应被服务端关闭")
	}
}

func TestE2ETokenEndpoint(t *testing.T) {
	ts := startServerFull(t, 0, "required", testSecret, "development")
	base := "http://" + ts.url[len("ws://"):]
	base = base[:len(base)-len("/ws")]

	post := func(url string, body map[string]any, apiKey string) (int, map[string]any) {
		raw, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		if apiKey != "" {
			req.Header.Set("X-API-Key", apiKey)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("请求 /token 失败: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}

	// ---- dev 环境：无需 API Key，签发后可直接入会 ----
	code, out := post(base+"/token", map[string]any{"roomId": "room-tok", "identity": "u-9", "name": "小明"}, "")
	if code != http.StatusOK {
		t.Fatalf("dev 环境签发应成功，实际 %d: %v", code, out)
	}
	token, _ := out["token"].(string)
	if token == "" {
		t.Fatalf("/token 未返回 token: %v", out)
	}
	p := dial(t, ts.url, "tok")
	defer p.close()
	joined := joinWithToken(p, "room-tok", "小明", token)
	if joined.Resumed {
		t.Fatalf("首次入会不应是 resumed")
	}

	// ---- 缺字段：400 ----
	if code, _ := post(base+"/token", map[string]any{"identity": "u-1"}, ""); code != http.StatusBadRequest {
		t.Fatalf("缺少 roomId 应返回 400，实际 %d", code)
	}

	// ---- 非 development 环境：必须带 X-API-Key ----
	prod := startServerFull(t, 0, "required", testSecret, "production")
	prodBase := "http://" + prod.url[len("ws://"):]
	prodBase = prodBase[:len(prodBase)-len("/ws")]

	// 未配置 TOKEN_API_KEY：出于安全直接拒绝签发（否则等于人人可自签管理员令牌）
	if code, _ := post(prodBase+"/token", map[string]any{"roomId": "r", "identity": "u"}, ""); code != http.StatusForbidden {
		t.Fatalf("production 且未配 TOKEN_API_KEY 应 403，实际 %d", code)
	}

	// 配置了 Key 之后：缺 Key → 401；Key 正确 → 200 且能拿到令牌
	prod.cfg.TokenAPIKey = "s3cret-key"
	if code, _ := post(prodBase+"/token", map[string]any{"roomId": "r", "identity": "u"}, ""); code != http.StatusUnauthorized {
		t.Fatalf("带 Key 配置时缺 X-API-Key 应 401，实际 %d", code)
	}
	code, out = post(prodBase+"/token", map[string]any{"roomId": "r", "identity": "u"}, "s3cret-key")
	if code != http.StatusOK || out["token"] == "" {
		t.Fatalf("X-API-Key 正确时应签发成功，实际 %d: %v", code, out)
	}
}
