package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"

	"github.com/Wei-Shaw/sub2api/ent/schema/mixins"
)

// GroupModelRoute 跨组模型路由规则：源分组的某个模型模式委派给目标分组处理。
//
// 这是「选组」层，与 Group.model_routing 的「组内选账号」层正交：跨组路由先跑决定
// 落到哪个分组，目标分组自己的 model_routing 再决定组内账号优先级。
//
// 详见 docs/tech/cross-group-model-routing.md (issue #82)。
type GroupModelRoute struct {
	ent.Schema
}

func (GroupModelRoute) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "group_model_routes"},
	}
}

func (GroupModelRoute) Mixin() []ent.Mixin {
	return []ent.Mixin{
		mixins.TimeMixin{},
	}
}

func (GroupModelRoute) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("group_id").
			Comment("源分组 ID（api_key 绑定的分组）"),
		field.String("model_pattern").
			MaxLen(200).
			NotEmpty().
			Comment("模型模式，支持末尾 * 通配，如 grok-4.5 或 grok-*"),
		field.Int64("target_group_id").
			Comment("目标分组 ID，命中后请求改由该分组调度"),
		field.Bool("enabled").
			Default(true).
			Comment("是否启用，关闭后立即回落源分组行为"),
	}
}

// Edges 故意为空：group_id / target_group_id 直接作为字段使用，不定义 edge。
// 与 Group.fallback_group_id 的既有约定一致（见 group.go），允许多个分组指向同一个目标分组。
func (GroupModelRoute) Edges() []ent.Edge {
	return nil
}

func (GroupModelRoute) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("group_id", "enabled"),
		index.Fields("group_id", "model_pattern").Unique(),
	}
}
