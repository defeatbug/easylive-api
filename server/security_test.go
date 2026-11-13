package server

import (
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"

	"easylive-api/config"
	"easylive-api/signaling"
)

// ---- 这些测试只覆盖**纯 helper**（不启动 Fiber、不连数据库）----
//
// 做安全加固时最容易出的问题不是"逻辑写错"，而是"边界没测"：
// 限速的窗口/容量、头像魔数、密钥常量时间比较都是纯函数，正好可以无依赖地钉住行为。

// TestAPIKeyMatches 运维密钥比较：相等才通过，空配置/空请求头一律不通过。
//
// 空配置必须返回 false：crypto/subtle.ConstantTimeCompare(nil, nil) == 1，
// 如果 helper 少了 want=="" 这一道，未配置 TOKEN_API_KEY 时"不带 Key"会被当成匹配，
// recordings/metrics 的运维通道就变成对所有人开放。
func TestAPIKeyMatches(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
		ok   bool
	}{
		{"相等", "s3cret-key", "s3cret-key", true},
		{"不相等", "s3cret-kez", "s3cret-key", false},
		{"长短不同", "s3cret", "s3cret-key", false},
		{"请求头为空", "", "s3cret-key", false},
		{"未配置密钥", "", "", false},
		{"未配置但带了东西", "anything", "", false},
	}
	for _, tc := range cases {
		if got := apiKeyMatches(tc.got, tc.want); got != tc.ok {
			t.Errorf("%s: apiKeyMatches(%q,%q)=%v，期望 %v", tc.name, tc.got, tc.want, got, tc.ok)
		}
	}
}

// TestLoginAllowedWindow 保持"同一 IP+用户名 每分钟 10 次"的原有语义。
func TestLoginAllowedWindow(t *testing.T) {
	resetLoginAttempts(t)
	key := "127.0.0.1|teacher"
	for i := 0; i < loginMaxPerWindow; i++ {
		if !loginAllowed(key) {
			t.Fatalf("第 %d 次（窗口内）应当允许", i+1)
		}
	}
	if loginAllowed(key) {
		t.Fatalf("第 %d 次应当被限速", loginMaxPerWindow+1)
	}

	// 窗口滑过之后必须恢复：把已有时间戳整体挪到窗口外，下一次应当允许
	loginMu.Lock()
	old := time.Now().Add(-2 * loginWindow)
	for i := range loginAttempts[key] {
		loginAttempts[key][i] = old
	}
	loginMu.Unlock()
	if !loginAllowed(key) {
		t.Fatal("窗口外的尝试不应继续限速")
	}
}

// TestLoginAttemptsPruned 过期条目必须被清理、清空的键必须删除（内存不会只增不减）。
func TestLoginAttemptsPruned(t *testing.T) {
	resetLoginAttempts(t)
	now := time.Now()
	loginMu.Lock()
	loginAttempts["expired"] = []time.Time{now.Add(-2 * loginWindow), now.Add(-3 * loginWindow)}
	loginAttempts["fresh"] = []time.Time{now.Add(-loginWindow / 2)}
	pruneLoginAttemptsLocked(now)
	_, expiredExists := loginAttempts["expired"]
	fresh := loginAttempts["fresh"]
	loginMu.Unlock()

	if expiredExists {
		t.Fatal("全部过期的键应当被删除（否则 map 会一直变大）")
	}
	if len(fresh) != 1 {
		t.Fatalf("窗口内的记录不应被清掉，实际 %d 条", len(fresh))
	}
}

// TestEvictOldestLoginKey 容量上限兜底：满表时淘汰最久没有尝试的键。
//
// 语义取舍写在 loginAllowed 的注释里：淘汰会让该键计数归零（短暂放宽限速），
// 换来的是 map 有硬上限；这里钉住"淘汰的是最老的那个"。
func TestEvictOldestLoginKey(t *testing.T) {
	resetLoginAttempts(t)
	now := time.Now()
	loginMu.Lock()
	loginAttempts["oldest"] = []time.Time{now.Add(-30 * time.Second)}
	loginAttempts["middle"] = []time.Time{now.Add(-10 * time.Second)}
	loginAttempts["newest"] = []time.Time{now.Add(-time.Second)}
	evictOldestLoginKeyLocked()
	_, oldestExists := loginAttempts["oldest"]
	_, newestExists := loginAttempts["newest"]
	loginMu.Unlock()

	if oldestExists {
		t.Fatal("应当淘汰最久没有尝试的键")
	}
	if !newestExists {
		t.Fatal("最近有尝试的键不应被淘汰")
	}
}

// TestLoginAttemptsCapped 键数到上限后，新增键不能让 map 无限增长。
func TestLoginAttemptsCapped(t *testing.T) {
	resetLoginAttempts(t)
	now := time.Now()
	loginMu.Lock()
	for i := 0; i < loginMaxKeys; i++ {
		loginAttempts["key-"+strconv.Itoa(i)] = []time.Time{now}
	}
	full := len(loginAttempts)
	loginMu.Unlock()
	if full != loginMaxKeys {
		t.Fatalf("构造满表失败，实际 %d 个键", full)
	}

	if !loginAllowed("brand-new-key") {
		t.Fatal("满表时新键仍应允许（淘汰一个旧键），否则攻击者可以把所有人锁在门外")
	}
	loginMu.Lock()
	after := len(loginAttempts)
	_, newExists := loginAttempts["brand-new-key"]
	loginMu.Unlock()
	if after > loginMaxKeys {
		t.Fatalf("键数不应超过上限 %d，实际 %d", loginMaxKeys, after)
	}
	if !newExists {
		t.Fatal("新键应当已记录（本次登录已被计数）")
	}
}

// TestMetricsAccessAllowed /metrics/* 的三层授权模型。
//
// 这是本轮最容易被"看起来能用"掩盖的修复：报表页用同源 Cookie 调它，
// 所以既不能一刀切要求 X-API-Key（前端会被 401），也不能继续裸奔。
// 用 Fiber 的最小路由跑真实请求，覆盖四种组合。
func TestMetricsAccessAllowed(t *testing.T) {
	// newTestServer 只装配鉴权需要的字段；st=nil 时 sessionUser 恒为 nil，
	// 正好用来模拟"未登录"。hub 用 NewHub(nil, cfg)，只需它的 Authenticator。
	newTestServer := func(appEnv, authMode, jwtSecret, apiKey string) *Server {
		cfg := &config.Config{
			AppEnv:      appEnv,
			AuthMode:    authMode,
			JWTSecret:   jwtSecret,
			TokenAPIKey: apiKey,
		}
		return &Server{cfg: cfg, hub: signaling.NewHub(nil, cfg)}
	}
	// request 发一次 GET /metrics，返回是否放行（204）还是拒绝（401）。
	request := func(t *testing.T, s *Server, apiKey string) int {
		t.Helper()
		app := fiber.New()
		app.Get("/metrics", func(c *fiber.Ctx) error {
			if s.metricsAccessAllowed(c) {
				return c.SendStatus(fiber.StatusNoContent)
			}
			return c.Status(fiber.StatusUnauthorized).SendString("unauthorized")
		})
		req := httptest.NewRequest("GET", "/metrics", nil)
		if apiKey != "" {
			req.Header.Set("X-API-Key", apiKey)
		}
		resp, err := app.Test(req)
		if err != nil {
			t.Fatalf("请求失败: %v", err)
		}
		return resp.StatusCode
	}

	type testCase struct {
		name      string
		appEnv    string
		authMode  string
		jwtSecret string
		cfgAPIKey string
		headerKey string
		want      int
	}
	cases := []testCase{
		{
			name: "生产环境 + 正确 X-API-Key 放行", appEnv: "production", authMode: "required",
			jwtSecret: strings.Repeat("k", 32), cfgAPIKey: "ops-key", headerKey: "ops-key",
			want: fiber.StatusNoContent,
		},
		{
			name: "生产环境 + 错误 X-API-Key 拒绝", appEnv: "production", authMode: "required",
			jwtSecret: strings.Repeat("k", 32), cfgAPIKey: "ops-key", headerKey: "wrong",
			want: fiber.StatusUnauthorized,
		},
		{
			name: "生产环境 + 未登录无密钥拒绝", appEnv: "production", authMode: "required",
			jwtSecret: strings.Repeat("k", 32), cfgAPIKey: "ops-key",
			want: fiber.StatusUnauthorized,
		},
		{
			name: "开发环境 + 鉴权关闭免登录放行", appEnv: "development", authMode: "off",
			want: fiber.StatusNoContent,
		},
		{
			// 关键边界：只要开了鉴权，开发环境**不再**豁免（否则登录就白做了）
			name: "开发环境 + 鉴权开启 + 未登录拒绝", appEnv: "development", authMode: "required",
			jwtSecret: strings.Repeat("k", 32), want: fiber.StatusUnauthorized,
		},
	}
	for _, tc := range cases {
		s := newTestServer(tc.appEnv, tc.authMode, tc.jwtSecret, tc.cfgAPIKey)
		if got := request(t, s, tc.headerKey); got != tc.want {
			t.Errorf("%s: status=%d，期望 %d", tc.name, got, tc.want)
		}
	}
}

// resetLoginAttempts 在每个用例前清空全局限速状态，避免用例间互相污染。
func resetLoginAttempts(t *testing.T) {
	t.Helper()
	loginMu.Lock()
	loginAttempts = map[string][]time.Time{}
	loginMu.Unlock()
}

// TestPasswordTooLong 口令上限按字符数判断，且不改变"至少 8"的下限语义。
func TestPasswordTooLong(t *testing.T) {
	if passwordTooLong(strings.Repeat("a", maxPasswordLen)) {
		t.Fatal("恰好 128 字符不应判为过长")
	}
	if !passwordTooLong(strings.Repeat("a", maxPasswordLen+1)) {
		t.Fatal("129 字符应判为过长")
	}
	// 128 个汉字 = 384 字节，但只有 128 个字符：不该按字节数误伤
	if passwordTooLong(strings.Repeat("密", maxPasswordLen)) {
		t.Fatal("128 个汉字（384 字节）不应判为过长")
	}
	if passwordTooLong("") || passwordTooLong("12345678") {
		t.Fatal("空串与 8 字符口令不应判为过长")
	}
}

// TestSniffAvatarMime 头像类型只认文件头：声明类型不可信，非图片内容一律拒绝。
func TestSniffAvatarMime(t *testing.T) {
	png := append(append([]byte{}, pngMagic...), 0x00, 0x01, 0x02)
	jpeg := append(append([]byte{}, jpegMagic...), 0x00, 0x01)
	webp := []byte("RIFF\x24\x00\x00\x00WEBPVP8 ")
	gif := []byte("GIF89a\x01\x00\x01\x00")
	html := []byte("<html><script>alert(1)</script>")

	cases := []struct {
		name string
		data []byte
		want string
		ok   bool
	}{
		{"PNG", png, "image/png", true},
		{"JPEG", jpeg, "image/jpeg", true},
		{"WebP", webp, "image/webp", true},
		{"GIF 不在白名单", gif, "", false},
		{"HTML 伪装", html, "", false},
		{"空内容", nil, "", false},
		{"只有 RIFF 头不是 WebP", []byte("RIFF\x24\x00\x00\x00AVI "), "", false},
		{"截断的 PNG", pngMagic[:4], "", false},
	}
	for _, tc := range cases {
		got, ok := sniffAvatarMime(tc.data)
		if ok != tc.ok || got != tc.want {
			t.Errorf("%s: sniffAvatarMime()=(%q,%v)，期望 (%q,%v)", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}
