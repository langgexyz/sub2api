//go:build unit

package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/model"
)

func route(id int64, pattern string, target int64, priority int, enabled bool) *model.GroupModelRoute {
	return &model.GroupModelRoute{
		ID:            id,
		GroupID:       1,
		ModelPattern:  pattern,
		TargetGroupID: target,
		Priority:      priority,
		Enabled:       enabled,
	}
}

// TestResolveRouteLongestPatternWins 锁定 issue #82 拍板的匹配语义：模式最长优先。
func TestResolveRouteLongestPatternWins(t *testing.T) {
	routes := []*model.GroupModelRoute{
		route(1, "grok-*", 5, 50, true),
		route(2, "grok-4.5*", 6, 50, true),
	}

	got := ResolveRoute(routes, "grok-4.5-turbo")
	if got == nil {
		t.Fatal("expected a match, got nil")
	}
	if got.TargetGroupID != 6 {
		t.Fatalf("longest pattern should win: want target 6 (grok-4.5*), got %d", got.TargetGroupID)
	}
}

// TestResolveRouteExactBeatsWildcard 精确模式比同前缀长度的通配模式更具体。
func TestResolveRouteExactBeatsWildcard(t *testing.T) {
	routes := []*model.GroupModelRoute{
		route(1, "grok-4.5*", 5, 50, true),
		route(2, "grok-4.5", 6, 50, true),
	}

	got := ResolveRoute(routes, "grok-4.5")
	if got == nil {
		t.Fatal("expected a match, got nil")
	}
	if got.TargetGroupID != 6 {
		t.Fatalf("exact pattern should beat wildcard: want target 6, got %d", got.TargetGroupID)
	}
}

// TestResolveRouteIgnoresDisabled 关闭的规则立即回落，不参与匹配（负向用例 N8）。
func TestResolveRouteIgnoresDisabled(t *testing.T) {
	routes := []*model.GroupModelRoute{
		route(1, "grok-4.5", 5, 50, false),
	}

	if got := ResolveRoute(routes, "grok-4.5"); got != nil {
		t.Fatalf("disabled route must not match, got target %d", got.TargetGroupID)
	}
}

// TestResolveRouteNoMatch 未命中返回 nil，调用方据此留在源分组。
func TestResolveRouteNoMatch(t *testing.T) {
	routes := []*model.GroupModelRoute{
		route(1, "grok-*", 5, 50, true),
	}

	if got := ResolveRoute(routes, "claude-opus-4-5"); got != nil {
		t.Fatalf("expected no match for unrelated model, got target %d", got.TargetGroupID)
	}
}

// TestResolveRouteEmptyModel 空模型名不匹配任何规则（负向用例 N3：body 无 model 字段）。
func TestResolveRouteEmptyModel(t *testing.T) {
	routes := []*model.GroupModelRoute{
		route(1, "grok-*", 5, 50, true),
	}

	if got := ResolveRoute(routes, ""); got != nil {
		t.Fatalf("empty model must not match, got target %d", got.TargetGroupID)
	}
}

// TestResolveRouteDeterministicOnTie 同具体度时结果必须与遍历顺序无关。
// 这条守的是 GetRoutingAccountIDs 踩过的坑：那里直接 range map，多模式命中时结果随机。
func TestResolveRouteDeterministicOnTie(t *testing.T) {
	a := route(2, "grok-4.5", 6, 10, true)
	b := route(1, "grok-4.5", 7, 90, true)

	// 同一组内唯一索引保证不会有两条同模式规则，这里仅验决相函数本身确定。
	forward := ResolveRoute([]*model.GroupModelRoute{a, b}, "grok-4.5")
	reverse := ResolveRoute([]*model.GroupModelRoute{b, a}, "grok-4.5")

	if forward.TargetGroupID != reverse.TargetGroupID {
		t.Fatalf("resolution must not depend on slice order: forward=%d reverse=%d",
			forward.TargetGroupID, reverse.TargetGroupID)
	}
	if forward.TargetGroupID != 6 {
		t.Fatalf("lower priority number should win the tie: want 6, got %d", forward.TargetGroupID)
	}
}

// TestResolveRouteNilEntry 规则集里的 nil 条目不得 panic。
func TestResolveRouteNilEntry(t *testing.T) {
	routes := []*model.GroupModelRoute{nil, route(1, "grok-4.5", 5, 50, true)}

	got := ResolveRoute(routes, "grok-4.5")
	if got == nil || got.TargetGroupID != 5 {
		t.Fatal("nil entries must be skipped without panicking")
	}
}

func TestPatternSpecificity(t *testing.T) {
	cases := []struct {
		pattern string
		want    int
	}{
		{"grok-*", 5},    // 前缀 "grok-" 长度 5
		{"grok-4.5*", 8}, // 前缀 "grok-4.5" 长度 8
		{"grok-4.5", 9},  // 精确，len+1 使其胜过 "grok-4.5*"
		{"*", 0},         // 全匹配，最不具体（service 层校验已禁止入库）
	}

	for _, tc := range cases {
		if got := patternSpecificity(tc.pattern); got != tc.want {
			t.Errorf("patternSpecificity(%q) = %d, want %d", tc.pattern, got, tc.want)
		}
	}
}
