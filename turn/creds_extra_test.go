package turn

import "testing"

// TestStatic 覆盖"外部静态 TURN"的构造：这是 NAT 型部署（公网只放行 22/80/443）
// 唯一的出路——SFU 自己以 TURN 客户端身份申请中继候选，媒体全程走中继。
func TestStatic(t *testing.T) {
	if got := Static(nil, "u", "p"); got != nil {
		t.Fatalf("urls 为空应返回 nil，实际 %+v", got)
	}
	got := Static([]string{"turn:203.0.113.9:3478?transport=udp"}, "user1", "pass1")
	if len(got) != 1 || got[0].Username != "user1" || got[0].Credential != "pass1" {
		t.Fatalf("静态 TURN 构造不正确: %+v", got)
	}
}

// TestServersWithExtra 确认外部 TURN 会追加在自建 coturn（REST 临时凭证）之后。
func TestServersWithExtra(t *testing.T) {
	extra := Static([]string{"turn:203.0.113.9:443?transport=tcp"}, "u", "p")
	servers := Servers([]string{"stun:stun.example.com"}, []string{"turn:10.0.0.1:3478"}, "secret", "alice", 3600, extra...)
	if len(servers) != 3 {
		t.Fatalf("应有 STUN + 自建 TURN + 外部 TURN 三项，实际 %d 项: %+v", len(servers), servers)
	}
	last := servers[len(servers)-1]
	if last.Username != "u" || last.URLs[0] != "turn:203.0.113.9:443?transport=tcp" {
		t.Fatalf("最后一项应为外部 TURN，实际 %+v", last)
	}
	// 没配外部 TURN 时行为不变
	if got := Servers([]string{"stun:x"}, nil, "", "bob", 3600); len(got) != 1 {
		t.Fatalf("未配置 TURN 时应只有 STUN，实际 %+v", got)
	}
}
