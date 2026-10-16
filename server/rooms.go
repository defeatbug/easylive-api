package server

import (
	"crypto/rand"
	"errors"
	"log"
	"math/big"
	"strings"

	"github.com/gofiber/fiber/v2"

	"easylive-api/store"
)

// ---- 会议：谁创建，谁就是这个会议的主持人 ----
//
// 权限模型（与服务端其它地方一致）：角色只是**全站**层面的（唯一管理员 / 普通用户），
// 而"主持人"是**会议内**的身份 —— 由 rooms.owner_id 决定。
// 这样普通用户也能开会，且只会成为自己那个会议的主持人。

// roomCodeAlphabet 是会议编码的字符集：**只含大写字母与数字**。
//
// 刻意不排除 0/O、1/I 这类易混字符：需求就是"大写字母和数字"，
// 8 位 36 进制约有 2.8×10^12 种组合，碰撞概率极低，且撞了会自动换码重试。
const roomCodeAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

const roomCodeLength = 8

// newRoomCode 生成一个 8 位编码。
//
// 用 crypto/rand 而不是 math/rand：会议编码是**访问凭据**——
// 猜中编码就能进会议。可预测的随机数会让"暴力猜码"变得可行。
func newRoomCode() (string, error) {
	var sb strings.Builder
	sb.Grow(roomCodeLength)
	max := big.NewInt(int64(len(roomCodeAlphabet)))
	for i := 0; i < roomCodeLength; i++ {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		sb.WriteByte(roomCodeAlphabet[n.Int64()])
	}
	return sb.String(), nil
}

// isValidRoomCode 判断一个房间号是否是本系统生成的编码格式（8 位、大写字母或数字）。
func isValidRoomCode(code string) bool {
	if len(code) != roomCodeLength {
		return false
	}
	for i := 0; i < len(code); i++ {
		if !strings.ContainsRune(roomCodeAlphabet, rune(code[i])) {
			return false
		}
	}
	return true
}

type createRoomRequest struct {
	Title string `json:"title"`
}

// handleCreateRoom POST /rooms —— 创建一个会议，返回 8 位编码。
//
// 编码就是房间号：其它人用它加入（前端把编码填进"会议号"即可）。
func (s *Server) handleCreateRoom(c *fiber.Ctx) error {
	u, ok := s.requireUser(c)
	if !ok {
		return nil
	}
	if s.st == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "数据层不可用，暂时无法创建会议"})
	}
	var req createRoomRequest
	_ = c.BodyParser(&req) // 请求体可为空（只有标题可选）
	title := strings.TrimSpace(req.Title)
	if len([]rune(title)) > 60 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "会议标题最长 60 个字符"})
	}
	if title == "" {
		title = u.DisplayName + " 的会议"
	}

	// 撞码就换一个重试。理论上几乎不会发生，但"几乎"不是"不会"——
	// 一旦发生，用户看到的是"创建失败"，很难理解，所以这里自愈。
	var code string
	for attempt := 0; attempt < 5; attempt++ {
		candidate, err := newRoomCode()
		if err != nil {
			log.Printf("server: 生成会议编码失败 err=%v", err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "生成会议号失败，请重试"})
		}
		err = s.st.CreateRoom(c.UserContext(), candidate, title, u.Username, u.ID)
		if err == nil {
			code = candidate
			break
		}
		if !errors.Is(err, store.ErrRoomExists) {
			log.Printf("server: 创建会议失败 user=%s err=%v", u.Username, err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "创建会议失败，请重试"})
		}
	}
	if code == "" {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "会议号连续冲突，请重试"})
	}

	log.Printf("server: 会议已创建 room=%s owner=%s(%d) title=%q", code, u.Username, u.ID, title)
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{
		"roomId": code,
		"code":   code,
		"title":  title,
		// 创建者本人拿到的是"就是这个会议的主持人"，与 /token 的判定同一套逻辑
		"isHost": true,
	})
}

// handleMyRooms GET /rooms/mine —— 我创建的会议（用于列表页）。
func (s *Server) handleMyRooms(c *fiber.Ctx) error {
	u, ok := s.requireUser(c)
	if !ok {
		return nil
	}
	if s.st == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "数据层不可用"})
	}
	rooms, err := s.st.RoomsByOwner(c.UserContext(), u.ID, 50)
	if err != nil {
		log.Printf("server: 查询我的会议失败 user=%s err=%v", u.Username, err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "查询失败，请稍后重试"})
	}
	out := make([]fiber.Map, 0, len(rooms))
	for _, r := range rooms {
		out = append(out, fiber.Map{
			"roomId":    r.ID,
			"title":     r.Title,
			"createdAt": r.CreatedAt.UnixMilli(),
		})
	}
	return c.JSON(fiber.Map{"rooms": out})
}

// handleRoomInfo GET /rooms/:id —— 按编码查会议信息（加入前预览用）。
//
// 只暴露标题与是否存在，**不暴露创建者是谁**：知道编码就能加入，
// 但没必要让任何拿到编码的人顺带知道组织者的账号名。
func (s *Server) handleRoomInfo(c *fiber.Ctx) error {
	if _, ok := s.requireUser(c); !ok {
		return nil
	}
	if s.st == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "数据层不可用"})
	}
	id := strings.ToUpper(strings.TrimSpace(c.Params("id")))
	r, err := s.st.GetRoom(c.UserContext(), id)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "会议号不存在"})
	}
	return c.JSON(fiber.Map{"roomId": r.ID, "title": r.Title})
}
