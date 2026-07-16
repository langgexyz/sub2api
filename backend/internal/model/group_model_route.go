package model

import "time"

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
	Enabled       bool      `json:"enabled"`         // 是否启用
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}
