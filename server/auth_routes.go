package server

import (
	"errors"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"

	"easylive-api/auth"
	"easylive-api/store"
)

// ---- 登录：用户名 + 密码 ----
//
// 设计取舍（写给论文与后来的人）：
//  1. 凭据只有用户名/口令两条，口令用 argon2id 存哈希（auth/password.go），
//     明文只在这一个函数的入参里存在，不落日志、不进响应。
//  2. 会话用**签名 Cookie**（auth/session.go）而不是服务端会话表：无状态、多实例天然共享。
//     代价是登出只清 Cookie、服务端无法单独吊销某个会话——要支持"强制下线"就得加会话表。
//  3. **权限不进 Cookie**：角色的唯一来源是数据库，每次请求重新读（sessionUser），
//     所以改一次角色立刻生效，也不会出现"Cookie 里写着 host 就永远是 host"。
//  4. 前端**只拿面向当前用户与房间的令牌**（/token 用会话签发），
//     令牌签名密钥永远不出现在浏览器侧。

// loginAttempts 是登录尝试的固定窗口限速（按 IP+用户名）。
//
// 为什么必须限速：登录是最容易被离线/在线爆破的入口，
// 而 argon2id 单次几十毫秒的设计目标是"让爆破变贵"，不是"让爆破不可能"。
var (
	loginMu       sync.Mutex
	loginAttempts = map[string][]time.Time{}
)

const loginWindow = time.Minute
const loginMaxPerWindow = 10

func loginAllowed(key string) bool {
	loginMu.Lock()
	defer loginMu.Unlock()
	now := time.Now()
	kept := loginAttempts[key][:0:0]
	for _, t := range loginAttempts[key] {
		if now.Sub(t) < loginWindow {
			kept = append(kept, t)
		}
	}
	if len(kept) >= loginMaxPerWindow {
		loginAttempts[key] = kept
		return false
	}
	loginAttempts[key] = append(kept, now)
	return true
}

// userJSON 是返回给前端的用户视图：**永不包含口令哈希**。
func userJSON(u *store.User) fiber.Map {
	return fiber.Map{
		"id":          u.ID,
		"username":    u.Username,
		"displayName": u.DisplayName,
		"role":        u.Role,
	}
}

// sessionUser 解析当前会话对应的用户。
//
// 未登录、Cookie 被篡改、会话过期、用户被停用 → 一律返回 nil（调用方按"未登录"处理），
// 而不是报错：调用方只需要知道"这次请求没有有效的登录身份"。
func (s *Server) sessionUser(c *fiber.Ctx) *store.User {
	if s.st == nil || len(s.cfg.SessionSecret) == 0 {
		return nil
	}
	raw := c.Cookies(auth.SessionCookieName)
	if raw == "" {
		return nil
	}
	uid, err := auth.VerifySession([]byte(s.cfg.SessionSecret), raw, time.Now())
	if err != nil {
		return nil
	}
	// 每次都从库里读：角色/停用状态的变更立刻生效，不需要等会话过期
	u, err := s.st.UserByID(c.UserContext(), uid)
	if err != nil || u.Disabled {
		return nil
	}
	return u
}

func (s *Server) setSession(c *fiber.Ctx, u *store.User) {
	exp := time.Now().Add(s.cfg.SessionTTL)
	c.Cookie(&fiber.Cookie{
		Name:     auth.SessionCookieName,
		Value:    auth.SignSession([]byte(s.cfg.SessionSecret), u.ID, exp),
		Path:     "/",
		HTTPOnly: true, // 前端脚本读不到：会话不该暴露给 JS
		Secure:   s.cfg.AppEnv != "development",
		SameSite: "Lax",
		Expires:  exp,
	})
}

func (s *Server) clearSession(c *fiber.Ctx) {
	c.Cookie(&fiber.Cookie{
		Name:     auth.SessionCookieName,
		Value:    "",
		Path:     "/",
		HTTPOnly: true,
		Secure:   s.cfg.AppEnv != "development",
		SameSite: "Lax",
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
	})
}

// handleLogin POST /auth/login {username, password}
func (s *Server) handleLogin(c *fiber.Ctx) error {
	if !s.hub.Authenticator().Enabled() {
		return c.Status(fiber.StatusNotImplemented).JSON(fiber.Map{
			"error": "鉴权未启用（AUTH_MODE=off 或未配置 JWT_SECRET），当前无需登录即可入会",
		})
	}
	if s.st == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "数据层不可用，无法登录"})
	}
	if len(s.cfg.SessionSecret) == 0 {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "服务端未配置会话密钥"})
	}

	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "请求体不是合法 JSON"})
	}
	req.Username = strings.TrimSpace(req.Username)
	if req.Username == "" || req.Password == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "username 与 password 必填"})
	}
	if !loginAllowed(c.IP() + "|" + strings.ToLower(req.Username)) {
		return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{"error": "尝试过于频繁，请稍后再试"})
	}

	u, err := s.st.UserByUsername(c.UserContext(), req.Username)
	if err != nil {
		if errors.Is(err, store.ErrUserNotFound) {
			// 仍然走一遍哈希校验：否则"用户不存在"会明显更快，可被用来枚举用户名
			auth.DummyVerify(req.Password)
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "用户名或密码错误"})
		}
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "查询用户失败"})
	}
	if u.Disabled {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "账号已停用"})
	}
	ok, verr := auth.VerifyPassword(req.Password, u.PasswordHash)
	if verr != nil || !ok {
		// 对外只给一句话：不区分"用户不存在"与"口令错误"，避免透露账号是否存在
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "用户名或密码错误"})
	}

	s.setSession(c, u)
	log.Printf("server: 登录成功 user=%s role=%s ip=%s", u.Username, u.Role, c.IP())
	return c.JSON(fiber.Map{"user": userJSON(u), "expiresAt": time.Now().Add(s.cfg.SessionTTL).UnixMilli()})
}

// handleLogout POST /auth/logout —— 清 Cookie（注意不是服务端吊销，见 auth/session.go 说明）
func (s *Server) handleLogout(c *fiber.Ctx) error {
	s.clearSession(c)
	return c.Status(fiber.StatusNoContent).Send(nil)
}

// handleMe GET /auth/me —— 前端用它判断"是否已登录、我是谁、能不能当主持人"
func (s *Server) handleMe(c *fiber.Ctx) error {
	u := s.sessionUser(c)
	if u == nil {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "未登录"})
	}
	return c.JSON(fiber.Map{"user": userJSON(u)})
}
