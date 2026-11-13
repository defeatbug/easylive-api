package server

import (
	"crypto/subtle"

	"github.com/gofiber/fiber/v2"
)

// apiKeyMatches 以**常量时间**比较请求头里的运维密钥与配置值。
//
// 为什么不能用 ==：普通字符串比较在第一个不同字节处就返回，逐字节的耗时差
// 足以让攻击者按字节还原密钥（token/recordings/graphql 三处都用同一把密钥，
// 一旦猜出来就能签发令牌、下载全部录制、调用 GraphQL 变更）。
// crypto/subtle.ConstantTimeCompare 在长度不同时也会提前返回——长度本身不是秘密，
// 可以接受；这里额外把"未配置密钥"固定为不匹配，避免空密钥时 compare("","")==1。
func apiKeyMatches(got, want string) bool {
	if want == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// metricsAccessAllowed 判定 /metrics/overview 与 /metrics/tracks 的访问权限。
//
// 授权模型与 server/recordings.go 的 recordingAccessFor 保持一致（三层并列）：
//
//	① X-API-Key（脚本/运维/评测采集器）：放行；
//	② development 且鉴权关闭（本地演示、无登录的探针）：放行；
//	③ 已登录会话：放行——报表页是**同源 Cookie** 调用，这是浏览器侧唯一的正常路径；
//	    否则 401。
//
// 为什么不能继续裸奔：这两个端点会泄露房间/参会人规模、轨道编码与带宽等运行数据，
// 而且 /metrics/tracks?format=csv 会一次性吐出全部轨道统计，属于可被外部批量拉取的运维信息。
// 注意 ② 必须"环境 + 鉴权关闭"同时成立：只要开了鉴权，开发环境也不再豁免，
// 否则登录就白做了（与 recordings 的判断完全一致）。
func (s *Server) metricsAccessAllowed(c *fiber.Ctx) bool {
	if apiKeyMatches(c.Get("X-API-Key"), s.cfg.TokenAPIKey) {
		return true
	}
	if s.cfg.AppEnv == "development" && !s.hub.Authenticator().Enabled() {
		return true
	}
	return s.sessionUser(c) != nil
}
