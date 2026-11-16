package server

import (
	"bytes"
	"errors"
	"io"
	"log"
	"regexp"
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v2"

	"easylive-api/auth"
	"easylive-api/store"
)

// ---- 账号资料：昵称 / 用户名 / 密码 / 头像 ----
//
// 三条贯穿这里的规则：
//  1. **改任何登录身份都要求当前口令**：会话 Cookie 是签名而不是"持有即可为所欲为"，
//     被劫持的会话不该能把账号改名或改密带走；
//  2. **改完立刻生效**：角色与资料每次请求都从库里读，没有"Cookie 里还写着旧值"的问题；
//  3. 用户名与头像都经服务端校验（白名单/长度/大小），不信任客户端的说法。

const (
	maxDisplayNameLen = 32
	minUsernameLen    = 3
	maxUsernameLen    = 32
	maxAvatarBytes    = 512 * 1024
)

// 用户名只允许字母数字与 . _ -：它会出现在令牌身份、日志与参与者 ID 里，
// 放开成任意字符只会给日志注入和显示带来麻烦。
var usernameRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

var allowedAvatarMime = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/webp": true,
}

// 头像的真实格式只认文件头（魔数），不认 multipart 里声明的 Content-Type。
//
// 为什么必须这样：Content-Type 由客户端随便写（curl -F 'file=@x.html;type=image/png' 就能骗过
// 只查声明的实现），而 /auth/avatar/:id 会把存下来的 MIME 原样回给浏览器——
// 这正是"存储型 XSS"的经典路径（上传 HTML/JS，诱导别人打开头像 URL）。
// 真实字节只有三种可能：PNG / JPEG / WebP。
var (
	pngMagic  = []byte("\x89PNG\r\n\x1a\n")
	jpegMagic = []byte{0xFF, 0xD8, 0xFF}
)

// sniffAvatarMime 按文件头判定真实类型；不认识的格式返回 ok=false。
//
// WebP 的结构是 "RIFF" + 4 字节长度 + "WEBP" 容器标识，这里检查首尾两个魔数；
// 前 4 字节之后的大小字段不参与判断（它与文件是否完整无关，这里不做完整解码）。
func sniffAvatarMime(data []byte) (string, bool) {
	switch {
	case len(data) >= len(pngMagic) && bytes.Equal(data[:len(pngMagic)], pngMagic):
		return "image/png", true
	case len(data) >= len(jpegMagic) && bytes.Equal(data[:len(jpegMagic)], jpegMagic):
		return "image/jpeg", true
	case len(data) >= 12 && bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP")):
		return "image/webp", true
	}
	return "", false
}

// requireUser 取当前登录用户；未登录时已经回了 401，调用方直接 return。
func (s *Server) requireUser(c *fiber.Ctx) (*store.User, bool) {
	u := s.sessionUser(c)
	if u == nil {
		_ = c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "未登录"})
		return nil, false
	}
	return u, true
}

// verifyCurrentPassword 校验当前口令（改资料/改密码前必须通过）。
func (s *Server) verifyCurrentPassword(u *store.User, password string) bool {
	ok, err := auth.VerifyPassword(password, u.PasswordHash)
	return err == nil && ok
}

// handleUpdateProfile PATCH /auth/profile {displayName?, username?, currentPassword}
func (s *Server) handleUpdateProfile(c *fiber.Ctx) error {
	u, ok := s.requireUser(c)
	if !ok || s.st == nil {
		return nil
	}
	var req struct {
		DisplayName     *string `json:"displayName"`
		Username        *string `json:"username"`
		CurrentPassword string  `json:"currentPassword"`
	}
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "请求体不是合法 JSON"})
	}
	if req.CurrentPassword == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "修改资料需要提供当前密码"})
	}
	// 与登录同源的限速：这里是另一个可被用来在线猜口令的入口
	if !loginAllowed(c.IP() + "|profile|" + strings.ToLower(u.Username)) {
		return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{"error": "尝试过于频繁，请稍后再试"})
	}
	if !s.verifyCurrentPassword(u, req.CurrentPassword) {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "当前密码不正确"})
	}

	username := u.Username
	if req.Username != nil {
		username = strings.TrimSpace(*req.Username)
		switch {
		case len(username) < minUsernameLen || len(username) > maxUsernameLen:
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "用户名长度需在 3–32 个字符之间"})
		case !usernameRe.MatchString(username):
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "用户名只能包含字母、数字与 . _ -"})
		}
		if !strings.EqualFold(username, u.Username) {
			taken, err := s.st.UsernameTaken(c.UserContext(), username, u.ID)
			if err != nil {
				log.Printf("server: 检查用户名占用失败 user=%s err=%v", u.Username, err)
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "保存失败，请稍后重试"})
			}
			if taken {
				return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "该用户名已被占用"})
			}
		}
	}

	displayName := u.DisplayName
	if req.DisplayName != nil {
		displayName = strings.TrimSpace(*req.DisplayName)
		if len([]rune(displayName)) > maxDisplayNameLen {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "昵称最长 32 个字符"})
		}
	}

	if err := s.st.UpdateProfile(c.UserContext(), u.ID, username, displayName); err != nil {
		log.Printf("server: 更新资料失败 user=%s err=%v", u.Username, err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "保存失败，请稍后重试"})
	}
	updated, err := s.st.UserByID(c.UserContext(), u.ID)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "保存成功但读取失败，请刷新"})
	}
	log.Printf("server: 资料已更新 user=%s → %s", u.Username, updated.Username)
	return c.JSON(fiber.Map{"user": userJSON(updated)})
}

// handleChangePassword POST /auth/password {currentPassword, newPassword}
func (s *Server) handleChangePassword(c *fiber.Ctx) error {
	u, ok := s.requireUser(c)
	if !ok || s.st == nil {
		return nil
	}
	var req struct {
		CurrentPassword string `json:"currentPassword"`
		NewPassword     string `json:"newPassword"`
	}
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "请求体不是合法 JSON"})
	}
	if !loginAllowed(c.IP() + "|password|" + strings.ToLower(u.Username)) {
		return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{"error": "尝试过于频繁，请稍后再试"})
	}
	// 上限与注册/登录一致：不给 argon2 喂超大输入（登录入口已限制，这里补齐）。
	if passwordTooLong(req.NewPassword) {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "新密码最长 128 个字符"})
	}
	if !s.verifyCurrentPassword(u, req.CurrentPassword) {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "当前密码不正确"})
	}
	if req.NewPassword == req.CurrentPassword {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "新密码不能与当前密码相同"})
	}
	hash, err := auth.HashPassword(req.NewPassword)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	}
	if err := s.st.UpdatePassword(c.UserContext(), u.ID, hash); err != nil {
		log.Printf("server: 修改密码失败 user=%s err=%v", u.Username, err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "保存失败，请稍后重试"})
	}
	log.Printf("server: 密码已修改 user=%s（未记录任何口令内容）", u.Username)
	return c.SendStatus(fiber.StatusNoContent)
}

// handleUploadAvatar POST /auth/avatar（multipart 字段名 file）
func (s *Server) handleUploadAvatar(c *fiber.Ctx) error {
	u, ok := s.requireUser(c)
	if !ok || s.st == nil {
		return nil
	}
	file, err := c.FormFile("file")
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "缺少文件字段 file"})
	}
	// 先按声明的大小挡一道，真正读取时再用 LimitReader 兜底（声明的大小不可信）
	if file.Size > maxAvatarBytes {
		return c.Status(fiber.StatusRequestEntityTooLarge).JSON(fiber.Map{"error": "头像不能超过 512KB"})
	}
	mime := file.Header.Get("Content-Type")
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = strings.TrimSpace(mime[:i]) // 去掉 "; charset=..."
	}
	if !allowedAvatarMime[mime] {
		return c.Status(fiber.StatusUnsupportedMediaType).JSON(fiber.Map{"error": "只支持 PNG / JPEG / WebP"})
	}
	f, err := file.Open()
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "无法读取上传的文件"})
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxAvatarBytes+1))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "读取文件失败"})
	}
	if len(data) > maxAvatarBytes {
		return c.Status(fiber.StatusRequestEntityTooLarge).JSON(fiber.Map{"error": "头像不能超过 512KB"})
	}
	// 声明的 Content-Type 只用于上面的快速预筛；**真正落库的类型由文件头决定**。
	realMime, sniffed := sniffAvatarMime(data)
	if !sniffed {
		return c.Status(fiber.StatusUnsupportedMediaType).JSON(fiber.Map{"error": "只支持 PNG / JPEG / WebP（文件内容不是这三种格式）"})
	}
	if realMime != mime {
		// 声明与真实不一致本身不必拒绝（我们只存真实类型），但留痕便于排查客户端问题
		log.Printf("server: 头像声明类型 %s 与真实类型 %s 不一致 user=%s", mime, realMime, u.Username)
		mime = realMime
	}
	if err := s.st.SetAvatar(c.UserContext(), u.ID, mime, data); err != nil {
		log.Printf("server: 保存头像失败 user=%s err=%v", u.Username, err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "保存失败，请稍后重试"})
	}
	updated, err := s.st.UserByID(c.UserContext(), u.ID)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "保存成功但读取失败，请刷新"})
	}
	log.Printf("server: 头像已更新 user=%s mime=%s bytes=%d", u.Username, mime, len(data))
	// 上传响应是 JSON，但仍显式声明 nosniff：浏览器对同一路径的响应不应做类型猜测。
	c.Set(fiber.HeaderXContentTypeOptions, "nosniff")
	return c.JSON(fiber.Map{"user": userJSON(updated)})
}

// handleDeleteAvatar DELETE /auth/avatar —— 恢复默认头像
func (s *Server) handleDeleteAvatar(c *fiber.Ctx) error {
	u, ok := s.requireUser(c)
	if !ok || s.st == nil {
		return nil
	}
	if err := s.st.ClearAvatar(c.UserContext(), u.ID); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "操作失败，请稍后重试"})
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// handleGetAvatar GET /auth/avatar/:id —— 取头像字节。
//
// 权限：**任何已登录用户**都可以取（会议里要显示别人的头像）；未登录一律 401。
// 之所以不公开：头像是账号资料的一部分，公开后等于给了一个"按 ID 枚举用户"的接口。
func (s *Server) handleGetAvatar(c *fiber.Ctx) error {
	if _, ok := s.requireUser(c); !ok || s.st == nil {
		return nil
	}
	id, err := strconv.ParseInt(c.Params("id"), 10, 64)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "用户 ID 非法"})
	}
	mime, data, at, err := s.st.Avatar(c.UserContext(), id)
	if err != nil {
		if errors.Is(err, store.ErrUserNotFound) {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "该用户没有设置头像"})
		}
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "读取头像失败"})
	}
	// 按**真实字节**重新判定类型：库里老的 avatar_mime 是"客户端声明值"，不可信；
	// 即使判定不出来，也照样带 nosniff，浏览器不会按内容把响应猜成 HTML/JS。
	if realMime, ok := sniffAvatarMime(data); ok {
		mime = realMime
	}
	c.Set(fiber.HeaderXContentTypeOptions, "nosniff")
	c.Set("Content-Type", mime)
	// 内容不会变（换了头像 URL 也会变），可以放心长缓存
	c.Set("Cache-Control", "private, max-age=86400")
	c.Set("Last-Modified", at.UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT"))
	return c.Send(data)
}
