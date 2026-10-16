package server

import (
	"log"
	"strconv"

	"github.com/gofiber/fiber/v2"
)

// ---- 管理员：查看所有用户 ----
//
// 全站只有一个管理员（由 users 的部分唯一索引兜底）。它的权限最高：
// 在任何会议里都是主持人，并且能分页查看全部用户信息。
//
// 边界说明：这里只做**查看**，不做"改别人的口令/删除账号"——
// 那属于账号生命周期的管理工作，需要单独设计（审计、二次确认、不可自锁等），
// 不该顺手加在这个接口上。

const (
	adminDefaultPageSize = 20
	adminMaxPageSize     = 100
)

// handleAdminUsers GET /admin/users?page=1&pageSize=20
func (s *Server) handleAdminUsers(c *fiber.Ctx) error {
	u, ok := s.requireUser(c)
	if !ok {
		return nil
	}
	if u.Role != "admin" {
		// 403 而不是 404：前端需要能区分"没权限"与"接口不存在"
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "需要管理员权限"})
	}
	if s.st == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "数据层不可用"})
	}

	page, _ := strconv.Atoi(c.Query("page", "1"))
	if page < 1 {
		page = 1
	}
	pageSize, _ := strconv.Atoi(c.Query("pageSize", strconv.Itoa(adminDefaultPageSize)))
	if pageSize < 1 {
		pageSize = adminDefaultPageSize
	}
	if pageSize > adminMaxPageSize {
		pageSize = adminMaxPageSize
	}

	total, err := s.st.CountUsers(c.UserContext())
	if err != nil {
		log.Printf("server: 统计用户失败 err=%v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "查询失败，请稍后重试"})
	}
	users, err := s.st.ListUsers(c.UserContext(), pageSize, (page-1)*pageSize)
	if err != nil {
		log.Printf("server: 列出用户失败 err=%v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "查询失败，请稍后重试"})
	}

	items := make([]fiber.Map, 0, len(users))
	for i := range users {
		item := userJSON(&users[i]) // 复用同一套序列化：绝不包含口令哈希
		if users[i].AvatarUpdatedAt != nil {
			item["avatarUrl"] = userJSON(&users[i])["avatarUrl"]
		}
		item["hasAvatar"] = users[i].AvatarUpdatedAt != nil
		items = append(items, item)
	}

	totalPages := (total + pageSize - 1) / pageSize
	return c.JSON(fiber.Map{
		"users":      items,
		"page":       page,
		"pageSize":   pageSize,
		"total":      total,
		"totalPages": totalPages,
	})
}
