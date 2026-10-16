package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// SessionCookieName 是会话 Cookie 名。
const SessionCookieName = "easylive_session"

// ErrBadSession 表示会话值缺失、被篡改或已过期。
var ErrBadSession = errors.New("会话无效或已过期")

// 会话用**签名 Cookie**而不是服务端会话表：
//   - 好处：无状态、不查库、多实例天然共享；
//   - 代价：登出只是让浏览器丢掉 Cookie，服务端无法单方面吊销某个会话。
//     对毕设的演示规模够用；要支持"强制下线"就得加会话表并存储随机 ID。
//     这一取舍写在这里，避免以后有人误以为登出等于吊销。
//
// 结构：base64url(userID|expUnix|HMAC-SHA256(userID|expUnix))。
// 权限**不放进 Cookie**：角色每次请求都从数据库读，改一下用户角色立刻生效，
// 也避免"Cookie 里写着 host 就永远是 host"这种越权。
func SignSession(secret []byte, userID int64, exp time.Time) string {
	payload := fmt.Sprintf("%d|%d", userID, exp.Unix())
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(payload))
	sig := mac.Sum(nil)
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." +
		base64.RawURLEncoding.EncodeToString(sig)
}

// VerifySession 校验签名与有效期，返回用户 ID。
func VerifySession(secret []byte, value string, now time.Time) (int64, error) {
	dot := strings.LastIndex(value, ".")
	if dot <= 0 {
		return 0, ErrBadSession
	}
	payloadRaw, err := base64.RawURLEncoding.DecodeString(value[:dot])
	if err != nil {
		return 0, ErrBadSession
	}
	sig, err := base64.RawURLEncoding.DecodeString(value[dot+1:])
	if err != nil {
		return 0, ErrBadSession
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(payloadRaw)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return 0, ErrBadSession
	}
	parts := strings.Split(string(payloadRaw), "|")
	if len(parts) != 2 {
		return 0, ErrBadSession
	}
	userID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, ErrBadSession
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, ErrBadSession
	}
	if now.After(time.Unix(exp, 0)) {
		return 0, ErrBadSession
	}
	return userID, nil
}
