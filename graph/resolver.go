package graph

import (
	"errors"
	"time"

	"easylive-api/store"
)

// Resolver 是 gqlgen 的依赖注入点。
//
// 这里只注入一个 *store.Store：它内部同时持有 Ent（业务实体）与 sqlc（指标聚合），
// 因此 resolver 层完全不需要知道某条数据到底走的是哪一套——
// "两种访问层各归其位"这件事被封装在数据访问层内部。
type Resolver struct {
	Store *store.Store
}

func New(st *store.Store) *Resolver { return &Resolver{Store: st} }

// 说明：queryResolver / mutationResolver / roomResolver 这三个挂载点类型，
// 以及 Query()/Mutation()/Room() 访问器，都放在 schema.resolvers.go 里——
// 那是 gqlgen 自己管理的文件：重新生成时它会**保留已有函数的实现**、
// 只补新增字段的桩函数，因此把实现和挂载点放在一起才是它的预期用法。

// ---------- 辅助函数（放在本文件：gqlgen 只重写 schema.resolvers.go）----------
//
// 踩过的坑：`gqlgen generate` 会保留 resolver 函数的**函数体**，但会丢弃同文件里
// 非 resolver 的辅助函数。所以 storeOrErr / sinceFromMinutes 这类工具必须放在
// resolver.go —— 否则一次代码生成就让整个包编译不过。

// storeOrErr 在数据库不可用时给出可读的错误。
//
// 数据库在本项目里是可选依赖：连不上时媒体功能照常（会话与轨道都在内存里），
// 但业务接口（房间/会议/指标）确实不可用——把这件事说清楚，
// 比返回空列表让调用方猜要好。
func (r *Resolver) storeOrErr() (*store.Store, error) {
	if r.Store == nil {
		return nil, errors.New("数据库不可用：业务接口暂不可用（媒体功能不受影响）")
	}
	return r.Store, nil
}

// sinceFromMinutes 把"最近 N 分钟"换算成起始时间。
// 默认 60 分钟，并夹在 [1 分钟, 7 天] 之间：防止误传巨大值把一次查询变成全表扫描。
func sinceFromMinutes(m *int) time.Time {
	minutes := 60
	if m != nil {
		minutes = *m
	}
	if minutes < 1 {
		minutes = 60
	}
	if minutes > 7*24*60 {
		minutes = 7 * 24 * 60
	}
	return time.Now().Add(-time.Duration(minutes) * time.Minute)
}
