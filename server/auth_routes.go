package server

import (
	"errors"
	"log"
	"strconv"
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
//
// 为什么还要做容量兜底：键是"IP+用户名"，攻击者可以每换一个（伪造的）用户名
// 就往 map 里留一把时间戳，而旧实现只增不减 → 内存无界增长（慢速 DoS）。
// 现在做两件事：
//  1. 判定时顺手清掉**过期**条目、并把清空的键整个删掉（窗口外的条目本来就不参与判定）；
//  2. 键数到上限后先全表清一次过期条目，仍然满就淘汰"最久没出现过"的键。
//     淘汰会让那个键的计数从头开始（短暂放宽限速），换来内存硬上限——
//     真正的防线仍是"每个键每分钟 10 次"+ 全局 HTTP 限流，容量上限只防内存被灌爆。
var (
	loginMu       sync.Mutex
	loginAttempts = map[string][]time.Time{}
)

const (
	loginWindow       = time.Minute
	loginMaxPerWindow = 10
	// loginMaxKeys 是 map 的键数上限（不是尝试次数）：4096 个键最多约 4 万个时间戳，
	// 内存量级几百 KB，同时远大于真实部署的并发登录账号数。
	loginMaxKeys = 4096
)

func loginAllowed(key string) bool {
	loginMu.Lock()
	defer loginMu.Unlock()
	now := time.Now()

	// 只对"新键"做容量维护：已有键更新时不需要为它腾位置（否则满表时合法用户
	// 每次登录都会误伤一个无辜的键）。
	if _, exists := loginAttempts[key]; !exists && len(loginAttempts) >= loginMaxKeys {
		pruneLoginAttemptsLocked(now)
		if len(loginAttempts) >= loginMaxKeys {
			evictOldestLoginKeyLocked()
		}
	}

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
	if len(kept) == 0 {
		// 这个键在窗口内没有任何有效尝试：不保留空切片，避免"清空但不清键"
		delete(loginAttempts, key)
	}
	loginAttempts[key] = append(kept, now)
	return true
}

// pruneLoginAttemptsLocked 清掉所有窗口外的尝试记录，并删除清空了的键。
// 调用方必须持有 loginMu。
func pruneLoginAttemptsLocked(now time.Time) {
	for k, ts := range loginAttempts {
		kept := ts[:0]
		for _, t := range ts {
			if now.Sub(t) < loginWindow {
				kept = append(kept, t)
			}
		}
		if len(kept) == 0 {
			delete(loginAttempts, k)
			continue
		}
		loginAttempts[k] = kept
	}
}

// evictOldestLoginKeyLocked 淘汰最久没有新尝试的键，给新键腾出容量。
// 调用方必须持有 loginMu，且应保证 map 非空。
func evictOldestLoginKeyLocked() {
	var (
		oldestKey string
		oldest    time.Time
		found     bool
	)
	for k, ts := range loginAttempts {
		var last time.Time
		if n := len(ts); n > 0 {
			// 时间戳按追加顺序保存，最后一个就是最近一次尝试
			last = ts[n-1]
		}
		if !found || last.Before(oldest) {
			oldestKey, oldest, found = k, last, true
		}
	}
	if found {
		delete(loginAttempts, oldestKey)
	}
}

// 口令长度上下限。
//
// 上限为什么必须有：注册/改密/登录三个入口都在**验证之前**接收任意长度的口令；
// 没有上限时，一个几 MB 的"密码"能稳定消耗 CPU 与内存（登录入口还会拿它去跑 argon2），
// 是极廉价的 DoS 入口。128 字符远超正常人会用的长度，对真实用户没有可感知限制。
// 下限保持原有的"至少 8"语义（HashPassword 也有同样兜底），口径不变。
const (
	minPasswordLen = 8
	maxPasswordLen = 128
)

// passwordTooLong 按**字符数**判断（不是字节数）：中文等非 ASCII 口令不该被
// 按 3 倍字节数误伤（128 个汉字的口令没有理由拒绝）。
func passwordTooLong(p string) bool { return len([]rune(p)) > maxPasswordLen }

// userJSON 是返回给前端的用户视图：**永不包含口令哈希**，也不含头像字节。
//
// avatarUrl 带 ?v=<更新时间>：换头像后 URL 变化，浏览器不会用旧缓存。
func userJSON(u *store.User) fiber.Map {
	out := fiber.Map{
		"id":          u.ID,
		"username":    u.Username,
		"displayName": u.DisplayName,
		"role":        u.Role,
	}
	if url := avatarURLFor(u); url != "" {
		out["avatarUrl"] = url
	}
	return out
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
	uid, version, err := auth.VerifySession([]byte(s.cfg.SessionSecret), raw, time.Now())
	if err != nil {
		return nil
	}
	// 每次都从库里读：角色/停用状态的变更立刻生效，不需要等会话过期
	u, err := s.st.UserByID(c.UserContext(), uid)
	if err != nil || u.Disabled {
		return nil
	}
	// **会话版本必须一致**：登录/注册会把版本 +1，于是旧设备手上的 Cookie 当场失效。
	// 这是"同一账号只允许一处在线"的落点（签名 Cookie 本身没法吊销）。
	if version != u.SessionVersion {
		return nil
	}
	return u
}

func (s *Server) setSession(c *fiber.Ctx, userID, sessionVersion int64) {
	exp := time.Now().Add(s.cfg.SessionTTL)
	c.Cookie(&fiber.Cookie{
		Name:     auth.SessionCookieName,
		Value:    auth.SignSession([]byte(s.cfg.SessionSecret), userID, sessionVersion, exp),
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
	// 超长口令不可能是合法凭据（注册/改密端已限制 128 字符），直接拒绝：
	// 既不给 argon2 喂大输入，也不泄露任何账号存在性（判断只看请求形状）。
	if passwordTooLong(req.Password) {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "密码过长"})
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
		// 记下底层原因：数据库连不上时会走到这里，只回一句"查询用户失败"是查不出来的
		// （实测踩过：容器没了，前端只看到 500，服务端日志里什么都没有）。
		log.Printf("server: 查询用户失败 username=%s ip=%s err=%v", req.Username, c.IP(), err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "查询用户失败"})
	}
	// 先验口令、再判停用。顺序很重要：如果先看 disabled 就返回 403，
	// 攻击者只要试一个用户名，就能从 401/403 的差异判断"账号存在且已停用"
	// （未登录即可触发，是账号存在性泄露）。把口令校验放前面后，403 只会在
	// "口令确实正确、但账号被停用"时出现，拿到这个信息的前提是先知道正确口令。
	ok, verr := auth.VerifyPassword(req.Password, u.PasswordHash)
	if verr != nil || !ok {
		// 对外只给一句话：不区分"用户不存在"与"口令错误"，避免透露账号是否存在
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "用户名或密码错误"})
	}
	if u.Disabled {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "账号已停用"})
	}

	// 单点登录：版本 +1 让此前所有会话 Cookie 失效，并踢掉该账号已建立的连接
	version, err := s.st.BumpSessionVersion(c.UserContext(), u.ID)
	if err != nil {
		log.Printf("server: 更新会话版本失败 user=%s err=%v", u.Username, err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "登录失败，请稍后重试"})
	}
	kicked := s.hub.KickIdentity(u.Username, "")
	s.setSession(c, u.ID, version)
	u.SessionVersion = version
	log.Printf("server: 登录成功 user=%s role=%s ip=%s 顶掉旧连接=%d", u.Username, u.Role, c.IP(), kicked)
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

// handleCapabilities GET /auth/capabilities —— 让前端知道"这个部署能做什么"。
//
// 前端据此决定是否显示注册入口：未开放注册时不该摆一个点了必然失败的按钮。
// 它不要求登录，也不能泄露任何账号信息。
func (s *Server) handleCapabilities(c *fiber.Ctx) error {
	return c.JSON(fiber.Map{
		"authEnabled": s.hub.Authenticator().Enabled(),
		// 注册跟随鉴权：开着鉴权就能注册，没有第二个开关（前端据此显示注册入口）
		"signupEnabled": s.hub.Authenticator().Enabled(),
	})
}

// handleRegister POST /auth/register {username, password, displayName?}
//
// 注册即登录：成功后直接下发会话（与登录同一套 Cookie 与版本逻辑）。
//
// 两条刻意的限制：
//  1. 注册出来的账号**一律是 member**，绝不可能是 host —— 否则任何人都能注册成主持人；
//  2. 是否开放注册**跟随鉴权**（AUTH_MODE != off 即开放）：不额外加开关，
//     否则"要不要显示注册按钮"这种 UI 决定会被绑在一个安全开关上，很容易对不上。
func (s *Server) handleRegister(c *fiber.Ctx) error {
	if !s.hub.Authenticator().Enabled() {
		return c.Status(fiber.StatusNotImplemented).JSON(fiber.Map{
			"error": "鉴权未启用（AUTH_MODE=off），当前无需注册即可入会",
		})
	}
	if s.st == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "数据层不可用，暂时无法注册"})
	}
	var req struct {
		Username    string `json:"username"`
		Password    string `json:"password"`
		DisplayName string `json:"displayName"`
	}
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "请求体不是合法 JSON"})
	}
	req.Username = strings.TrimSpace(req.Username)
	req.DisplayName = strings.TrimSpace(req.DisplayName)
	switch {
	case len(req.Username) < minUsernameLen || len(req.Username) > maxUsernameLen:
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "用户名长度需在 3–32 个字符之间"})
	case !usernameRe.MatchString(req.Username):
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "用户名只能包含字母、数字与 . _ -"})
	case len(req.Password) < minPasswordLen:
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "密码至少 8 个字符"})
	case passwordTooLong(req.Password):
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "密码最长 128 个字符"})
	case len([]rune(req.DisplayName)) > maxDisplayNameLen:
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "昵称最长 32 个字符"})
	}
	if !loginAllowed(c.IP() + "|register") {
		return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{"error": "尝试过于频繁，请稍后再试"})
	}
	taken, err := s.st.UsernameTaken(c.UserContext(), req.Username, 0)
	if err != nil {
		log.Printf("server: 注册检查用户名失败 username=%s err=%v", req.Username, err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "注册失败，请稍后重试"})
	}
	if taken {
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "该用户名已被占用"})
	}
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	}
	if req.DisplayName == "" {
		req.DisplayName = req.Username
	}
	id, err := s.st.CreateUser(c.UserContext(), req.Username, req.DisplayName, hash, "member")
	if err != nil {
		log.Printf("server: 注册失败 username=%s err=%v", req.Username, err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "注册失败，请稍后重试"})
	}
	version, err := s.st.BumpSessionVersion(c.UserContext(), id)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "注册成功但登录失败，请手动登录"})
	}
	s.setSession(c, id, version)
	u, err := s.st.UserByID(c.UserContext(), id)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "注册成功但读取失败，请手动登录"})
	}
	log.Printf("server: 注册成功 user=%s role=member ip=%s（注册即登录）", u.Username, c.IP())
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"user": userJSON(u)})
}

// avatarURLFor 生成头像地址；没设置头像时返回空串。
//
// 抽出来是因为它有两处用法：HTTP 接口的响应，以及**会议内**参与者的头像
// （服务端按令牌身份查库后填进 PeerInfo）。两处必须完全一致，
// 否则会出现"个人资料里有头像、会议里却没有"这种不一致。
func avatarURLFor(u *store.User) string {
	if u == nil || u.AvatarUpdatedAt == nil {
		return ""
	}
	return "/auth/avatar/" + strconv.FormatInt(u.ID, 10) + "?v=" + strconv.FormatInt(u.AvatarUpdatedAt.Unix(), 10)
}
