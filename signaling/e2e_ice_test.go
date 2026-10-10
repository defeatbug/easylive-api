// 客户端 ICE 配置（/ice）的端到端测试。
//
// 为什么值得单独测：中继能不能用只取决于"下发的凭证是否被 coturn 认可"，
// 而这一步在本地无法真的连一次 coturn（需要公网服务器）。所以这里把**凭证算法**
// 用独立实现校验一遍：只要 HMAC-SHA1 + username 格式一致，coturn 就一定会接受。
package signaling_test

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

type iceServerJSON struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username"`
	Credential string   `json:"credential"`
}

type iceResponse struct {
	ICEServers       []iceServerJSON `json:"iceServers"`
	TTLSeconds       int             `json:"ttlSeconds"`
	TurnEnabled      bool            `json:"turnEnabled"`
	CredentialScheme string          `json:"credentialScheme"`
}

func fetchICE(t *testing.T, base, identity string) iceResponse {
	t.Helper()
	resp, err := http.Get(base + "/ice?identity=" + identity)
	if err != nil {
		t.Fatalf("请求 /ice 失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/ice 应返回 200，实际 %d", resp.StatusCode)
	}
	var out iceResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("解析 /ice 响应失败: %v", err)
	}
	return out
}

func TestE2EICEServers(t *testing.T) {
	ts := startServerFull(t, 0, "", "", "test")
	base := "http://" + strings.TrimSuffix(strings.TrimPrefix(ts.url, "ws://"), "/ws")

	// ---- 1) 什么都没配：返回空列表（前端据此回退到内置 STUN），且不谎称中继可用 ----
	got := fetchICE(t, base, "alice")
	if got.TurnEnabled {
		t.Fatalf("未配置 TURN_SECRET 时不应声称中继可用: %+v", got)
	}
	if len(got.ICEServers) != 0 {
		t.Fatalf("STUN/TURN 都没配时应返回空列表，实际 %+v", got.ICEServers)
	}

	// ---- 2) 只配 STUN：只下发 STUN（不带凭证）----
	ts.cfg.StunURLs = []string{"stun:stun.l.google.com:19302"}
	got = fetchICE(t, base, "alice")
	if len(got.ICEServers) != 1 || len(got.ICEServers[0].Username) != 0 {
		t.Fatalf("只配 STUN 时应只下发 STUN，实际 %+v", got.ICEServers)
	}

	// ---- 3) 再配 TURN：下发临时凭证，且凭证必须能被 coturn 校验通过 ----
	ts.cfg.TurnURLs = []string{"turn:turn.example.com:3478?transport=udp"}
	ts.cfg.TurnSecret = "shared-secret-for-test"
	ts.cfg.TurnCredTTL = 30 * time.Minute

	got = fetchICE(t, base, "alice")
	if !got.TurnEnabled {
		t.Fatalf("配置 TURN 后应标记 turnEnabled=true")
	}
	if got.CredentialScheme != "turn-rest-api" {
		t.Fatalf("凭证方案应为 coturn 的 use-auth-secret（turn-rest-api），实际 %q", got.CredentialScheme)
	}
	if got.TTLSeconds != int((30*time.Minute)/time.Second) {
		t.Fatalf("ttlSeconds 应为 1800，实际 %d", got.TTLSeconds)
	}
	if len(got.ICEServers) != 2 {
		t.Fatalf("应下发 STUN + TURN 两项，实际 %+v", got.ICEServers)
	}

	turnEntry := got.ICEServers[1]
	if turnEntry.Username == "" || turnEntry.Credential == "" {
		t.Fatalf("TURN 项必须带临时凭证，实际 %+v", turnEntry)
	}
	// 用户名必须是 <过期时间戳>:<identity>，且过期时间落在 TTL 窗口内
	parts := strings.SplitN(turnEntry.Username, ":", 2)
	if len(parts) != 2 || parts[1] != "alice" {
		t.Fatalf("TURN 用户名应为 <时间戳>:alice，实际 %q", turnEntry.Username)
	}
	// 过期时间必须落在 TTL 窗口内（coturn 按这个时间戳判定失效）
	expSec, perr := strconv.ParseInt(parts[0], 10, 64)
	if perr != nil {
		t.Fatalf("用户名前缀不是 Unix 秒时间戳: %q", turnEntry.Username)
	}
	if d := time.Until(time.Unix(expSec, 0)); d < 25*time.Minute || d > 31*time.Minute {
		t.Fatalf("凭证有效期应约等于 TURN_CRED_TTL(30m)，实际剩余 %s", d)
	}

	// 用独立实现重算一次凭证（coturn 侧就是这么校验的）
	mac := hmac.New(sha1.New, []byte(ts.cfg.TurnSecret))
	mac.Write([]byte(turnEntry.Username))
	want := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	if turnEntry.Credential != want {
		t.Fatalf("临时凭证算法不符：得到 %q，期望 %q（coturn 会拒绝）", turnEntry.Credential, want)
	}

	// ---- 4) 不同 identity 拿到不同用户名（便于在 coturn 日志里定位是谁在用中继）----
	other := fetchICE(t, base, "bob")
	if other.ICEServers[1].Username == turnEntry.Username {
		t.Fatalf("不同 identity 的凭证用户名不应相同")
	}
	if !strings.HasSuffix(other.ICEServers[1].Username, ":bob") {
		t.Fatalf("用户名应带上 identity，实际 %q", other.ICEServers[1].Username)
	}
}
