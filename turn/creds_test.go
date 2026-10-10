package turn

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestCredentialFormat 校验用户名格式（coturn 依赖"过期时间戳:标识"这个前缀解析）。
func TestCredentialFormat(t *testing.T) {
	before := time.Now().Add(time.Hour).Unix()
	user, cred, err := Credential("s3cret", "alice", time.Hour)
	if err != nil {
		t.Fatalf("生成凭证失败: %v", err)
	}
	after := time.Now().Add(time.Hour).Unix()

	parts := strings.SplitN(user, ":", 2)
	if len(parts) != 2 || parts[1] != "alice" {
		t.Fatalf("用户名格式应为 <过期时间戳>:<标识>，实际 %q", user)
	}
	exp, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		t.Fatalf("用户名前缀不是 Unix 秒时间戳: %q", user)
	}
	if exp < before || exp > after {
		t.Fatalf("过期时间不在预期窗口内: %d（期望 %d—%d）", exp, before, after)
	}
	if raw, derr := base64.StdEncoding.DecodeString(cred); derr != nil || len(raw) != sha1.Size {
		t.Fatalf("凭证应是 base64(HMAC-SHA1)（20 字节），实际 %q 长度 %d", cred, len(raw))
	}
}

// TestCredentialAlgorithm 用独立实现锁住算法。
//
// 这不是同义反复：它把"username 用原始字节参与 HMAC、结果标准 base64、SHA-1 而非 SHA-256"
// 这几件事钉死。一旦有人改动实现（例如换成 SHA-256），coturn 会静默拒绝所有中继，
// 而这个测试会先失败，省掉一次"公网偶发连不上"的排查。
func TestCredentialAlgorithm(t *testing.T) {
	const secret = "test-shared-secret"
	user, cred, err := Credential(secret, "bob", 2*time.Hour)
	if err != nil {
		t.Fatalf("生成凭证失败: %v", err)
	}
	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write([]byte(user))
	want := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	if cred != want {
		t.Fatalf("凭证算法不符：得到 %q，期望 %q", cred, want)
	}
}

func TestCredentialErrors(t *testing.T) {
	if _, _, err := Credential("", "alice", time.Hour); err == nil {
		t.Fatalf("没有共享密钥时应当报错，而不是签出无效凭证")
	}
	// 空标识回退为 anonymous，避免生成 ":<空>" 这种 coturn 无法归属的用户名
	user, _, err := Credential("s", "", time.Hour)
	if err != nil || !strings.HasSuffix(user, ":anonymous") {
		t.Fatalf("空标识应回退为 anonymous，实际 %q err=%v", user, err)
	}
	// 非正数 TTL 回退为 1 小时
	user2, _, _ := Credential("s", "a", 0)
	exp, _ := strconv.ParseInt(strings.SplitN(user2, ":", 2)[0], 10, 64)
	if d := time.Until(time.Unix(exp, 0)); d < 55*time.Minute || d > 65*time.Minute {
		t.Fatalf("TTL=0 应回退为约 1 小时，实际剩余 %s", d)
	}
}

func TestServers(t *testing.T) {
	stun := []string{"stun:stun.l.google.com:19302"}
	turnURLs := []string{"turn:turn.example.com:3478?transport=udp"}

	// 没配密钥：只给 STUN（绝不下发无凭证的 TURN）
	got := Servers(stun, turnURLs, "", "u", time.Hour)
	if len(got) != 1 || got[0].Username != "" {
		t.Fatalf("无密钥时只应下发 STUN，实际 %+v", got)
	}

	// 配了密钥：STUN + TURN（带临时凭证）
	got = Servers(stun, turnURLs, "s3cret", "u", time.Hour)
	if len(got) != 2 {
		t.Fatalf("应下发 STUN + TURN 两项，实际 %+v", got)
	}
	if got[1].Username == "" || got[1].Credential == "" || len(got[1].URLs) != 1 {
		t.Fatalf("TURN 项应带临时凭证，实际 %+v", got[1])
	}

	// 只有 TURN、没有 STUN 也要能工作
	got = Servers(nil, turnURLs, "s3cret", "u", time.Hour)
	if len(got) != 1 || got[0].Username == "" {
		t.Fatalf("无 STUN 时应只下发 TURN，实际 %+v", got)
	}
}
