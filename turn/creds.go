// Package turn 生成 coturn 的临时凭证（REST API 方案）与客户端 ICE 服务器列表。
//
// 为什么需要它：STUN 只能做地址发现，双方都在对称 NAT 后（企业内网、部分校园网）
// 必须走中继。而把长期 TURN 密钥发给浏览器是不可接受的——任何人都能把你的中继
// 当免费代理用。coturn 的 use-auth-secret 方案让服务端用**共享密钥现签临时凭证**：
//
//	username   = <过期 Unix 时间戳>:<用户标识>
//	credential = base64(HMAC-SHA1(shared_secret, username))
//
// coturn 侧只需配置 --use-auth-secret --static-auth-secret=<同一个密钥>，
// 它自己会用同样的算法校验，过期即失效。
package turn

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"strconv"
	"time"
)

// ErrNoSecret 表示没有配置共享密钥（此时不应签发凭证）。
var ErrNoSecret = errors.New("未配置 TURN 共享密钥")

// Server 是下发给浏览器的 ICE 服务器描述（与 RTCIceServer 对齐）。
type Server struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

// Credential 生成一对临时凭证。
func Credential(secret, identity string, ttl time.Duration) (username, credential string, err error) {
	if secret == "" {
		return "", "", ErrNoSecret
	}
	if identity == "" {
		identity = "anonymous"
	}
	if ttl <= 0 {
		ttl = time.Hour
	}
	// 时间戳必须用 Unix 秒：coturn 按秒解析 username 的前缀
	username = strconv.FormatInt(time.Now().Add(ttl).Unix(), 10) + ":" + identity

	mac := hmac.New(sha1.New, []byte(secret))
	if _, werr := mac.Write([]byte(username)); werr != nil {
		return "", "", werr
	}
	credential = base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return username, credential, nil
}

// Servers 组装下发给客户端的 ICE 服务器列表：
// STUN（可选，用于直连探测）+ TURN（配了密钥才会带临时凭证）。
//
// 注意：没配密钥时**不能**下发不带凭证的 TURN 地址——那会让浏览器一直尝试一个
// 认证失败的中继，表现为"连接建立很慢甚至失败"，比直接不给更糟。
func Servers(stunURLs, turnURLs []string, secret, identity string, ttl time.Duration) []Server {
	out := make([]Server, 0, 2)
	if len(stunURLs) > 0 {
		out = append(out, Server{URLs: stunURLs})
	}
	if len(turnURLs) > 0 && secret != "" {
		user, cred, err := Credential(secret, identity, ttl)
		if err == nil {
			out = append(out, Server{URLs: turnURLs, Username: user, Credential: cred})
		}
	}
	return out
}

// TTLSeconds 把时长转成秒（HTTP 响应里给客户端看，便于它提前刷新凭证）。
func TTLSeconds(ttl time.Duration) int {
	if ttl <= 0 {
		return 0
	}
	return int(ttl / time.Second)
}
