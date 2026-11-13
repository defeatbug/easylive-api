package auth

import (
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// TestTokenIssuerVerified 校验端必须校验 iss。
//
// 只校验签名时，任何用同一密钥签出的 JWT（别的服务、或密钥复用场景下的其它用途令牌）
// 都会被当成合法入会凭据；把 iss 固定成 Issuer 之后，这类令牌会被拒绝。
func TestTokenIssuerVerified(t *testing.T) {
	secret := "0123456789abcdef0123456789abcdef"
	a := New(secret, time.Hour, ModeRequired)

	// 本服务签发的令牌：通过，且身份取自 sub
	tok, _, err := a.Issue("teacher", "Teacher", VideoGrant{RoomJoin: true})
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	claims, err := a.Verify(tok)
	if err != nil {
		t.Fatalf("本服务签发的令牌应当通过: %v", err)
	}
	if claims.Issuer != Issuer {
		t.Fatalf("issuer 应为 %q，实际 %q", Issuer, claims.Issuer)
	}
	if claims.Identity() != "teacher" {
		t.Fatalf("身份应为 teacher，实际 %q", claims.Identity())
	}

	// 同密钥、不同 issuer：必须拒绝
	forged := signWithIssuer(t, secret, "some-other-service")
	if _, err := a.Verify(forged); err == nil {
		t.Fatal("issuer 不匹配的令牌应当被拒绝")
	}

	// 完全没有 issuer：也必须拒绝
	noIssuer := signWithIssuer(t, secret, "")
	if _, err := a.Verify(noIssuer); err == nil {
		t.Fatal("缺少 issuer 的令牌应当被拒绝")
	}
}

// signWithIssuer 用与服务端相同的密钥但指定任意 issuer 签一个令牌（模拟伪造/串用场景）。
func signWithIssuer(t *testing.T, secret, issuer string) string {
	t.Helper()
	now := time.Now()
	claims := &Claims{
		Name: "attacker",
		Video: VideoGrant{
			RoomJoin: true,
		},
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "attacker",
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now.Add(-time.Minute)),
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
		},
	}
	if issuer != "" {
		claims.Issuer = issuer
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("构造测试令牌失败: %v", err)
	}
	return signed
}

// TestTokenRejectsWrongSecret 顺带确认签名密钥不符时仍然拒绝（回归保护）。
func TestTokenRejectsWrongSecret(t *testing.T) {
	a := New(strings.Repeat("a", 32), time.Hour, ModeRequired)
	tok := signWithIssuer(t, strings.Repeat("b", 32), Issuer)
	if _, err := a.Verify(tok); err == nil {
		t.Fatal("密钥不符的令牌应当被拒绝")
	}
}
