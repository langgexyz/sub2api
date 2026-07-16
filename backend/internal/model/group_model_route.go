package model

import "time"

// DefaultRoutePriority 是新建路由规则的默认 priority。
// 取 50 与 account_groups.priority 的既有约定对齐：留出 0-49 给「要压过默认的」、
// 51-99 给「要垫在默认之后的」，两侧都有空间，不必为插入一条规则重排全表。
// 必须与 ent/schema/group_model_route.go 里 priority 字段的 Default 保持一致。
const DefaultRoutePriority = 50

// GroupModelRoute 跨组模型路由规则：源分组的某个模型模式委派给目标分组处理。
//
// 这是「选组」层，与 Group.ModelRouting 的「组内选账号」层正交：跨组路由先跑决定落到
// 哪个分组，目标分组自己的 ModelRouting 再决定组内账号优先级。
//
// 详见 docs/tech/cross-group-model-routing.md (issue #82)。
type GroupModelRoute struct {
	ID            int64     `json:"id"`
	GroupID       int64     `json:"group_id"`        // 源分组 ID（api_key 绑定的分组）
	ModelPattern  string    `json:"model_pattern"`   // 模型模式，支持末尾 * 通配
	TargetGroupID int64     `json:"target_group_id"` // 目标分组 ID
	Priority      int       `json:"priority"`        // 同长度模式命中时的二级决相，数字越小越优先
	Enabled       bool      `json:"enabled"`         // 是否启用
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}
