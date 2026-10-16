package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
)

// Room 是会议室（业务实体）。
//
// 为什么主键用房间号而不是自增 ID：房间号本身就是业务标识（用户输入/二维码里带的就是它），
// 用自增 ID 只会多一次"按号查 ID"的往返，且日志里全是数字不好读。
//
// 注意这里**只放元数据**：会话、轨道、订阅关系全在进程内存里（ADR D8），
// 数据库不参与媒体热路径。
type Room struct {
	ent.Schema
}

func (Room) Fields() []ent.Field {
	return []ent.Field{
		field.String("id").
			Comment("房间号（业务主键）").
			NotEmpty().
			Immutable(),
		field.String("title").
			Comment("房间标题，仅用于展示").
			Default(""),
		field.String("created_by").
			Comment("创建者身份（来自令牌 sub；无鉴权模式下是昵称）").
			Default(""),
		field.Int64("owner_id").
			Comment("创建者用户 ID；谁创建的会议，谁就是该会议的主持人。为空表示演示模式（无鉴权）下创建的").
			Optional().
			Nillable(),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
		field.Time("closed_at").
			Comment("关闭时间；为空表示仍在服务中").
			Optional().
			Nillable(),
	}
}

func (Room) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("meetings", Meeting.Type).
			// 删除房间时级联删除会议记录。注解必须挂在 edge.To 这一侧
			// （挂在反向的 edge.From 上不会生效，生成的描述里仍是 NoAction）。
			Annotations(entsql.OnDelete(entsql.Cascade)).
			Comment("该房间产生的会议记录"),
	}
}
