//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/model"
)

// TestRoutedTargetsListsEnabledRoutes 聚合组的模型列表要靠它把路由目标摊开。
func TestRoutedTargetsListsEnabledRoutes(t *testing.T) {
	svc, _ := newResolveSvc(
		map[int64][]*model.GroupModelRoute{
			6: {rt(1, 6, "gpt-*", 4), rt(2, 6, "grok-*", 5)},
		},
		map[int64]*Group{
			4: grp(4, PlatformOpenAI, StatusActive),
			5: grp(5, PlatformGrok, StatusActive),
		},
	)

	targets, err := svc.RoutedTargets(context.Background(), 6)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(targets) != 2 {
		t.Fatalf("want 2 targets, got %d", len(targets))
	}

	byPattern := map[string]int64{}
	for _, tg := range targets {
		byPattern[tg.Pattern] = tg.Group.ID
	}
	if byPattern["gpt-*"] != 4 || byPattern["grok-*"] != 5 {
		t.Fatalf("targets wired wrong: %+v", byPattern)
	}
}

// TestRoutedTargetsSkipsDisabled 关掉的路由不该出现在模型列表里。
func TestRoutedTargetsSkipsDisabled(t *testing.T) {
	disabled := rt(1, 6, "gpt-*", 4)
	disabled.Enabled = false
	svc, _ := newResolveSvc(
		map[int64][]*model.GroupModelRoute{6: {disabled}},
		map[int64]*Group{4: grp(4, PlatformOpenAI, StatusActive)},
	)

	targets, err := svc.RoutedTargets(context.Background(), 6)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(targets) != 0 {
		t.Fatalf("disabled route must not surface in the models list, got %+v", targets)
	}
}

// TestRoutedTargetsSkipsDeadTarget 目标组不存在/停用时跳过而不是报错 —— 一条坏路由
// 不该让整个模型列表挂掉（与热路径的 ResolveEffectiveGroup 刻意不同：那里必须显式报错，
// 因为静默回落会让用户拿到别的模型的回答；这里只是列表少一项）。
func TestRoutedTargetsSkipsDeadTarget(t *testing.T) {
	svc, _ := newResolveSvc(
		map[int64][]*model.GroupModelRoute{
			6: {rt(1, 6, "gpt-*", 99), rt(2, 6, "grok-*", 5), rt(3, 6, "x-*", 7)},
		},
		map[int64]*Group{
			5: grp(5, PlatformGrok, StatusActive),
			7: grp(7, PlatformOpenAI, "inactive"),
		},
	)

	targets, err := svc.RoutedTargets(context.Background(), 6)
	if err != nil {
		t.Fatalf("a dead target must not fail the whole list: %v", err)
	}
	if len(targets) != 1 || targets[0].Group.ID != 5 {
		t.Fatalf("want only the live target (group 5), got %+v", targets)
	}
}

func TestRoutedTargetsEmptyForPlainGroup(t *testing.T) {
	svc, _ := newResolveSvc(map[int64][]*model.GroupModelRoute{}, map[int64]*Group{})

	targets, err := svc.RoutedTargets(context.Background(), 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(targets) != 0 {
		t.Fatalf("a group with no routes must yield no targets, got %+v", targets)
	}
}

// TestRoutedTargetMatchesPattern 列表要用「目标组真实可用的模型 ∩ 路由模式」来筛，
// 这条锁住筛子本身：gpt-* 收 gpt-5.6，但不收 codex-auto-review（后者要单独一条精确规则）。
func TestRoutedTargetMatchesPattern(t *testing.T) {
	target := RoutedTarget{Pattern: "gpt-*", Group: grp(4, PlatformOpenAI, StatusActive)}

	cases := map[string]bool{
		"gpt-5.6":           true,
		"gpt-image-1":       true,
		"codex-auto-review": false,
		"grok-4.5":          false,
		"claude-opus-4-5":   false,
	}
	for m, want := range cases {
		if got := target.MatchesPattern(m); got != want {
			t.Errorf("MatchesPattern(%q) = %v, want %v", m, got, want)
		}
	}
}
