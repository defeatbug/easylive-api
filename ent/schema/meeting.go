package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
)

// Meeting 是一次会议记录：用于历史查询与评测章节的统计（峰值人数、峰值轨道数、时长）。
//
// 之所以把 peaks 也落库：评测里最常被问的两个数字就是"这个房间最多几个人"
// 和"最多同时多少路媒体"，而这两个峰值在会议结束后进程内存就没了。
type Meeting struct {
	ent.Schema
}

func (Meeting) Fields() []ent.Field {
	return []ent.Field{
		field.String("id").
			Comment("会议 ID（创建时生成的随机串）").
			NotEmpty().
			Immutable(),
		field.Time("started_at").
			Default(time.Now),
		field.Time("ended_at").
			Optional().
			Nillable(),
		field.Int("duration_sec").
			Comment("持续时长（秒）；结束前为 0").
			Default(0),
		field.Int("peak_participants").
			Comment("峰值在线人数").
			Default(0),
		field.Int("peak_tracks").
			Comment("峰值同时发布的轨道数（屏幕共享算第二路，与容量口径一致）").
			Default(0),
		// 显式声明外键列：不写的话 Ent 会按边名推导出 `room_meetings` 这种列名，
		// 读 SQL 的人（以及 sqlc 生成的字段名）都会一脸问号。
		field.String("room_id").
			Comment("所属房间号").
			NotEmpty(),
	}
}

func (Meeting) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("room", Room.Type).
			Ref("meetings").
			// 绑定到上面显式声明的字段，保证列名是 room_id
			Field("room_id").
			Unique().
			Required(),
	}
}
