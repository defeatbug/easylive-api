package server

import (
	"fmt"
	"log"
	"os"

	"github.com/gofiber/fiber/v2"

	"easylive-api/store"
)

// ---- 录制列表与下载 ----
//
// 授权模型（评审 P0-2）：录制此前**只有运维密钥**一条路，结果是"会议录完了，
// 只有运维能下载"——主持人自己在界面上反而看不到。现在分三层：
//
//	① X-API-Key（脚本/运维）：看全部；
//	② 管理员：看全部；
//	③ 普通登录用户：只看**自己创建的会议**的录制（rooms.owner_id == 自己）。
//
// 为什么不发"下载令牌"：下载走的是同源 cookie（浏览器 <a>/fetch 都会带），
// 再加一层短期令牌只会多一套需要同步的密钥，而房间归属的判定本来就在服务端手里。
// 判定的粒度已经是**房间级**：拿到别人会议 id 也换不到下载权。

// recordingAccess 是本次请求的访问范围。
type recordingAccess struct {
	ok    bool  // 允许访问
	all   bool  // 可以看全部会话
	owner int64 // 非 all 时：只允许这个用户创建的会议
}

// recordingAccessFor 判定本次请求的访问范围。
func (s *Server) recordingAccessFor(c *fiber.Ctx) recordingAccess {
	// ① 运维密钥（与 /token、/graphql 同一套凭据）
	if s.cfg.TokenAPIKey != "" && c.Get("X-API-Key") == s.cfg.TokenAPIKey {
		return recordingAccess{ok: true, all: true}
	}
	// 开发环境 + 鉴权关闭（免登录演示 / 本地探针）：放行。
	// 条件必须**同时**成立：只要开了鉴权，演示豁免就不存在——否则登录就白做了。
	if s.cfg.AppEnv == "development" && !s.hub.Authenticator().Enabled() {
		return recordingAccess{ok: true, all: true}
	}
	// ②③ 登录会话
	u := s.sessionUser(c)
	if u == nil {
		return recordingAccess{}
	}
	if u.Role == "admin" {
		return recordingAccess{ok: true, all: true}
	}
	return recordingAccess{ok: true, owner: u.ID}
}

// allowedRooms 返回该访问范围能看到的房间集合；all=true 时返回 nil（表示不限）。
func (s *Server) allowedRooms(c *fiber.Ctx, acc recordingAccess) (map[string]bool, error) {
	if acc.all {
		return nil, nil
	}
	rooms, err := s.st.RoomsByOwner(c.UserContext(), acc.owner, 200)
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(rooms))
	for _, r := range rooms {
		set[r.ID] = true
	}
	return set, nil
}

// handleListRecordings GET /recordings —— 列出可访问的录制会话。
func (s *Server) handleListRecordings(c *fiber.Ctx) error {
	acc := s.recordingAccessFor(c)
	if !acc.ok {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "需要登录（或有效的 X-API-Key）"})
	}
	rec := s.manager.Recording()
	if rec == nil {
		// 没开启录制能力：明确返回 enabled=false，前端据此隐藏入口而不是报错
		return c.JSON(fiber.Map{
			"enabled":   false,
			"recording": fiber.Map{"active": false},
			"sessions":  []any{},
		})
	}
	sessions, err := rec.ListSessions()
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}

	visible := make([]any, 0, len(sessions))
	if acc.all {
		for _, s := range sessions {
			visible = append(visible, s)
		}
	} else {
		allowed, aerr := s.allowedRooms(c, acc)
		if aerr != nil {
			log.Printf("server: 查询可访问房间失败 user=%d err=%v", acc.owner, aerr)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "查询失败，请稍后重试"})
		}
		for _, sess := range sessions {
			// 只保留自己创建的会议的录制
			if allowed[sess.RoomID] {
				visible = append(visible, sess)
			}
		}
	}

	resp := fiber.Map{
		"enabled":   true,
		"recording": s.manager.RecordingState(),
		"sessions":  visible,
	}
	// 输出目录是服务端文件系统信息，只给运维看
	if acc.all {
		resp["dir"] = rec.Dir()
	}
	return c.JSON(resp)
}

// handleDownloadRecording GET /recordings/:session/:file —— 下载某条轨道文件。
func (s *Server) handleDownloadRecording(c *fiber.Ctx) error {
	acc := s.recordingAccessFor(c)
	if !acc.ok {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "需要登录（或有效的 X-API-Key）"})
	}
	if s.st == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "数据层不可用"})
	}
	rec := s.manager.Recording()
	if rec == nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "服务端未开启录制"})
	}

	// 先按会话 ID 找到它属于哪个房间，再做归属判定。
	// 这一步不能省：会话 ID 是可猜的（时间戳形式），而房间归属才是权限边界。
	sessions, err := rec.ListSessions()
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	sessionID := c.Params("session")
	found := false
	roomID := ""
	for _, sess := range sessions {
		if sess.ID == sessionID {
			found = true
			roomID = sess.RoomID
			break
		}
	}
	if !found {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "录制会话不存在"})
	}
	if !acc.all {
		allowed, aerr := s.allowedRooms(c, acc)
		if aerr != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "查询失败，请稍后重试"})
		}
		// 403 而不是 404：前端要能区分"没有权限"与"文件不存在"；
		// 这里已经确认会话存在，所以不泄露任何额外信息。
		if !allowed[roomID] {
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "无权下载该会议的录制"})
		}
	}

	// 路径穿越防护：只用白名单字符，并拒绝 ..
	path, ok := rec.SessionPath(sessionID, c.Params("file"))
	if !ok {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "非法文件名"})
	}
	if _, err := os.Stat(path); err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "文件不存在"})
	}
	_ = store.ErrUserNotFound // 保持与 store 包的同包依赖可读性（误删 import 会立刻编译失败）
	c.Set(fiber.HeaderContentDisposition, fmt.Sprintf("attachment; filename=%q", c.Params("file")))
	return c.SendFile(path)
}
