package auth

import (
	"strings"
	"testing"
	"time"
)

// TestSessionRoundTrip 基本信息：签发的会话能被校验回同一个用户。
func TestSessionRoundTrip(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	exp := time.Now().Add(time.Hour)
	v := SignSession(secret, 42, exp)
	uid, err := VerifySession(secret, v, time.Now())
	if err != nil {
		t.Fatalf("校验失败: %v", err)
	}
	if uid != 42 {
		t.Fatalf("用户 ID 不符，实际 %d", uid)
	}
}

// TestSessionRejectsTamperAndExpiry 覆盖三类必须拒绝的情况。
//
// 会话是签名 Cookie：一旦校验被绕过，任何人都能伪造成任意用户（包括 host）。
func TestSessionRejectsTamperAndExpiry(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	other := []byte("ffffffffffffffffffffffffffffffff")
	v := SignSession(secret, 7, time.Now().Add(time.Hour))

	// 1) 换密钥（等价于伪造签名）
	if _, err := VerifySession(other, v, time.Now()); err == nil {
		t.Fatal("密钥不符应当拒绝")
	}
	// 2) 改载荷（把用户 ID 改大）
	parts := strings.SplitN(v, ".", 2)
	forged := SignSession(secret, 999, time.Now().Add(time.Hour))
	if _, err := VerifySession(secret, forged, time.Now()); err != nil {
		t.Fatalf("同样密钥签发的合法会话不应失败: %v", err)
	}
	tampered := strings.Replace(v, parts[0], parts[0][:len(parts[0])-2]+"AA", 1)
	if _, err := VerifySession(secret, tampered, time.Now()); err == nil {
		t.Fatal("篡改载荷应当拒绝")
	}
	// 3) 过期
	old := SignSession(secret, 7, time.Now().Add(-time.Minute))
	if _, err := VerifySession(secret, old, time.Now()); err == nil {
		t.Fatal("过期会话应当拒绝")
	}
	// 4) 垃圾输入不能 panic
	for _, bad := range []string{"", ".", "abc", "abc.", ".def", strings.Repeat("A", 500)} {
		if _, err := VerifySession(secret, bad, time.Now()); err == nil {
			t.Fatalf("垃圾输入 %q 应当拒绝", bad)
		}
	}
}

// TestPasswordHashAndVerify 口令哈希：正确口令通过、错误口令失败、且同一口令两次哈希不同。
func TestPasswordHashAndVerify(t *testing.T) {
	h1, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("哈希失败: %v", err)
	}
	h2, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("哈希失败: %v", err)
	}
	if h1 == h2 {
		t.Fatal("同一口令两次哈希应当不同（含随机盐）")
	}
	if !strings.HasPrefix(h1, "$argon2id$") {
		t.Fatalf("应当是 PHC 格式，实际 %s", h1[:min(20, len(h1))])
	}
	if ok, err := VerifyPassword("correct horse battery staple", h1); err != nil || !ok {
		t.Fatalf("正确口令应当通过: ok=%v err=%v", ok, err)
	}
	if ok, _ := VerifyPassword("wrong password", h1); ok {
		t.Fatal("错误口令不应通过")
	}
	// 过短口令拒绝
	if _, err := HashPassword("short"); err == nil {
		t.Fatal("过短口令应当拒绝")
	}
	// 畸形哈希返回错误而不是 panic
	if _, err := VerifyPassword("x", "not-a-hash"); err == nil {
		t.Fatal("畸形哈希应当报错")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
