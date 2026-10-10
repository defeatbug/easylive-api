// Package auth 实现基于 JWT 的准入与权限模型。
//
// 设计参照 LiveKit 的 AccessToken / VideoGrant，两个核心点：
//  1. **身份由令牌决定**，客户端自报的昵称只在无鉴权模式下可信；
//  2. **权限是声明式的 grant**（能否进房间、能否发布/订阅/发数据、是否管理员），
//     由服务端在每条信令上校验——前端隐藏按钮只是体验，不是安全边界。
package auth

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Mode 是鉴权强度。
//
// 之所以设计成三态而不是简单的"开/关"：开题阶段的演示环境（局域网、无 HTTPS、随时要跑给别人看）
// 需要一键跑通；而部署到公网时必须强制校验。三态让同一份代码覆盖两种场景，
// 也让灰度上线（新老客户端共存）成为可能。
type Mode string

const (
	// ModeOff：不校验（默认）。本地演示与端到端测试用。
	ModeOff Mode = "off"
	// ModeOptional：带令牌就校验并使用令牌身份；不带则退化为"自报昵称"的老行为。
	ModeOptional Mode = "optional"
	// ModeRequired：必须带有效令牌，否则拒绝入会。
	ModeRequired Mode = "required"
)

func ParseMode(s string) Mode {
	switch Mode(strings.ToLower(strings.TrimSpace(s))) {
	case ModeOptional:
		return ModeOptional
	case ModeRequired:
		return ModeRequired
	default:
		return ModeOff
	}
}

// VideoGrant 描述一个令牌能做什么（字段名与 LiveKit 的 VideoGrant 对齐，便于对照阅读）。
type VideoGrant struct {
	RoomJoin       bool   `json:"roomJoin"`
	Room           string `json:"room,omitempty"` // 空表示可用于任意房间
	CanPublish     bool   `json:"canPublish"`
	CanSubscribe   bool   `json:"canSubscribe"`
	CanPublishData bool   `json:"canPublishData"`
	RoomAdmin      bool   `json:"roomAdmin"`
}

// Claims 是令牌载荷。Name 只是显示名，身份以 sub 为准。
type Claims struct {
	Name  string     `json:"name,omitempty"`
	Video VideoGrant `json:"video"`
	jwt.RegisteredClaims
}

// Identity 返回令牌声明的身份（客户端无法伪造）。
func (c *Claims) Identity() string {
	if c.Subject != "" {
		return c.Subject
	}
	return c.Name
}

// ErrNotConfigured 表示鉴权没启用（未配 JWT_SECRET 或 AUTH_MODE=off）。
var ErrNotConfigured = errors.New("鉴权未启用（未配置 JWT_SECRET 或 AUTH_MODE=off）")

// Authenticator 负责签发与校验令牌。
type Authenticator struct {
	secret []byte
	ttl    time.Duration
	mode   Mode
}

func New(secret string, ttl time.Duration, mode Mode) *Authenticator {
	if ttl <= 0 {
		ttl = 12 * time.Hour
	}
	return &Authenticator{secret: []byte(secret), ttl: ttl, mode: mode}
}

func (a *Authenticator) Mode() Mode { return a.mode }

// Enabled 表示是否真的启用校验。
//
// 注意：mode=required 但没配密钥时**视为未启用**并打日志降级，而不是"谁都进不来"——
// 漏配环境变量时前者能在日志里一眼看出来，后者会让人花半小时排查"为什么连不上"。
func (a *Authenticator) Enabled() bool { return a.mode != ModeOff && len(a.secret) > 0 }

func (a *Authenticator) TTL() time.Duration { return a.ttl }

// Issue 签发令牌。
func (a *Authenticator) Issue(identity, name string, g VideoGrant) (string, time.Time, error) {
	if !a.Enabled() {
		return "", time.Time{}, ErrNotConfigured
	}
	if strings.TrimSpace(identity) == "" {
		return "", time.Time{}, errors.New("identity 不能为空")
	}
	now := time.Now()
	exp := now.Add(a.ttl)
	claims := &Claims{
		Name:  name,
		Video: g,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    "easylive-api",
			Subject:   identity,
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now.Add(-time.Minute)), // 容忍轻微时钟偏差
			ExpiresAt: jwt.NewNumericDate(exp),
			ID:        newID(),
		},
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(a.secret)
	if err != nil {
		return "", time.Time{}, err
	}
	return signed, exp, nil
}

// Verify 校验令牌并返回载荷。
//
// 必须显式限定签名算法（WithValidMethods）：否则攻击者可以把 alg 改成 none
// 或换成非对称算法来做算法混淆攻击，这是 JWT 最经典的一类漏洞。
func (a *Authenticator) Verify(token string) (*Claims, error) {
	if !a.Enabled() {
		return nil, ErrNotConfigured
	}
	claims := &Claims{}
	_, err := jwt.ParseWithClaims(
		token,
		claims,
		func(*jwt.Token) (interface{}, error) { return a.secret, nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithLeeway(30*time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("令牌无效: %w", err)
	}
	return claims, nil
}

// newID 生成令牌的 jti（便于排查与将来做吊销名单）。
func newID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprint(time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
