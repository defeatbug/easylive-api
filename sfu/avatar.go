package sfu

import (
	"log"
	"strings"
	"sync/atomic"
)

// 头像解析：把"令牌身份"映射成"头像地址"。
//
// 为什么用包级钩子而不是把 store 一路传下来：
//   - sfu 包只做媒体，不认识数据库；依赖方向应当是 server → sfu，而不是反过来；
//   - Participant 在 Room 里、Room 在 Manager 里，为了一条**展示用**的地址
//     把 store 透传三层，收益很小；
//   - 它的失败影响极小：没有头像就回落到昵称首字母（前端本来就有这个回落）。
//
// 代价是它是进程级状态（同一个进程只能有一个解析器），这在单节点部署下成立。
var avatarResolver atomic.Value // func(identity string) string

// SetAvatarResolver 由 server 包在启动时注入。传 nil 表示不解析（全部回落首字母）。
func SetAvatarResolver(fn func(identity string) string) {
	if fn == nil {
		avatarResolver = atomic.Value{}
		return
	}
	avatarResolver.Store(fn)
	// 打一行日志：否则"头像解析到底注入了没有"只能靠猜——
	// 会议室里头像不显示时，第一个要回答的就是这个问题。
	log.Printf("sfu: 已注入头像解析器（会议内按令牌身份查头像地址）")
}

// resolveAvatar 返回该身份的头像地址；未注入或查不到时返回空串。
func resolveAvatar(identity string) string {
	if strings.TrimSpace(identity) == "" {
		return ""
	}
	fn, _ := avatarResolver.Load().(func(string) string)
	if fn == nil {
		return ""
	}
	return fn(identity)
}
