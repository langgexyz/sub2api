//go:build unit

package service

import (
	"context"
	"errors"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/model"
)

// fakeRouteRepo 只实现解析路径要用的 ListByGroupID，其余方法用不到即 panic —— 用不到
// 却被调用说明解析走了预期外的路，宁可炸也不要静默通过。
type fakeRouteRepo struct {
	byGroup map[int64][]*model.GroupModelRoute
	calls   int
}

func (f *fakeRouteRepo) ListByGroupID(_ context.Context, groupID int64) ([]*model.GroupModelRoute, error) {
	f.calls++
	return f.byGroup[groupID], nil
}
func (f *fakeRouteRepo) ListAll(context.Context) ([]*model.GroupModelRoute, error) {
	panic("ListAll must not be called on the resolve path")
}
func (f *fakeRouteRepo) GetByID(context.Context, int64) (*model.GroupModelRoute, error) {
	panic("GetByID must not be called on the resolve path")
}
func (f *fakeRouteRepo) Create(context.Context, *model.GroupModelRoute) (*model.GroupModelRoute, error) {
	panic("Create must not be called on the resolve path")
}
func (f *fakeRouteRepo) Update(context.Context, *model.GroupModelRoute) (*model.GroupModelRoute, error) {
	panic("Update must not be called on the resolve path")
}
func (f *fakeRouteRepo) Delete(context.Context, int64) error {
	panic("Delete must not be called on the resolve path")
}

type fakeGroupRepoForRoute struct {
	groups map[int64]*Group
}

func (f *fakeGroupRepoForRoute) GetByID(_ context.Context, id int64) (*Group, error) {
	return f.groups[id], nil
}

func newResolveSvc(routes map[int64][]*model.GroupModelRoute, groups map[int64]*Group) (*GroupModelRouteService, *fakeRouteRepo) {
	repo := &fakeRouteRepo{byGroup: routes}
	svc := &GroupModelRouteService{
		repo:      repo,
		groupRepo: &fakeGroupRepoForRoute{groups: groups},
		cache:     make(map[int64]routeCacheEntry),
	}
	return svc, repo
}

func rt(id, group int64, pattern string, target int64) *model.GroupModelRoute {
	return &model.GroupModelRoute{ID: id, GroupID: group, ModelPattern: pattern, TargetGroupID: target, Enabled: true}
}

func grp(id int64, platform, status string) *Group {
	return &Group{ID: id, Platform: platform, Status: status}
}

// TestResolveEffectiveGroupHit 主路径：anthropic 组发 grok-4.5 落到 grok 组。
func TestResolveEffectiveGroupHit(t *testing.T) {
	svc, _ := newResolveSvc(
		map[int64][]*model.GroupModelRoute{1: {rt(1, 1, "grok-4.5", 5)}},
		map[int64]*Group{5: grp(5, PlatformGrok, StatusActive)},
	)

	got, err := svc.ResolveEffectiveGroup(context.Background(), 1, "grok-4.5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil || got.ID != 5 || got.Platform != PlatformGrok {
		t.Fatalf("want group 5 (grok), got %+v", got)
	}
}

// TestResolveEffectiveGroupNoRoute 绝大多数请求的路径：没命中就留在源组。
func TestResolveEffectiveGroupNoRoute(t *testing.T) {
	svc, _ := newResolveSvc(
		map[int64][]*model.GroupModelRoute{1: {rt(1, 1, "grok-4.5", 5)}},
		map[int64]*Group{5: grp(5, PlatformGrok, StatusActive)},
	)

	got, err := svc.ResolveEffectiveGroup(context.Background(), 1, "claude-opus-4-5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("unrelated model must not route, got group %d", got.ID)
	}
}

// TestResolveEffectiveGroupTargetMissing 负向 N1：目标组不存在必须显式报错，
// 不能静默回落源组（否则 grok-4.5 会被源组的 Claude 账号接走）。
func TestResolveEffectiveGroupTargetMissing(t *testing.T) {
	svc, _ := newResolveSvc(
		map[int64][]*model.GroupModelRoute{1: {rt(1, 1, "grok-4.5", 99)}},
		map[int64]*Group{},
	)

	_, err := svc.ResolveEffectiveGroup(context.Background(), 1, "grok-4.5")
	if !errors.Is(err, ErrRouteTargetUnavailable) {
		t.Fatalf("want ErrRouteTargetUnavailable, got %v", err)
	}
}

// TestResolveEffectiveGroupTargetInactive 负向 N1：目标组停用同样显式报错。
func TestResolveEffectiveGroupTargetInactive(t *testing.T) {
	svc, _ := newResolveSvc(
		map[int64][]*model.GroupModelRoute{1: {rt(1, 1, "grok-4.5", 5)}},
		map[int64]*Group{5: grp(5, PlatformGrok, "inactive")},
	)

	_, err := svc.ResolveEffectiveGroup(context.Background(), 1, "grok-4.5")
	if !errors.Is(err, ErrRouteTargetUnavailable) {
		t.Fatalf("want ErrRouteTargetUnavailable for inactive target, got %v", err)
	}
}

// TestResolveEffectiveGroupCycle 负向 N2：A -> B -> A 必须被环检测挡住，不能死循环。
func TestResolveEffectiveGroupCycle(t *testing.T) {
	svc, _ := newResolveSvc(
		map[int64][]*model.GroupModelRoute{
			1: {rt(1, 1, "grok-4.5", 5)},
			5: {rt(2, 5, "grok-4.5", 1)},
		},
		map[int64]*Group{
			1: grp(1, PlatformAnthropic, StatusActive),
			5: grp(5, PlatformGrok, StatusActive),
		},
	)

	_, err := svc.ResolveEffectiveGroup(context.Background(), 1, "grok-4.5")
	if !errors.Is(err, ErrRouteCycle) {
		t.Fatalf("want ErrRouteCycle, got %v", err)
	}
}

// TestResolveEffectiveGroupChain 链式跳转 A -> B -> C，取最终落点。
func TestResolveEffectiveGroupChain(t *testing.T) {
	svc, _ := newResolveSvc(
		map[int64][]*model.GroupModelRoute{
			1: {rt(1, 1, "grok-4.5", 5)},
			5: {rt(2, 5, "grok-4.5", 6)},
		},
		map[int64]*Group{
			5: grp(5, PlatformGrok, StatusActive),
			6: grp(6, PlatformGrok, StatusActive),
		},
	)

	got, err := svc.ResolveEffectiveGroup(context.Background(), 1, "grok-4.5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil || got.ID != 6 {
		t.Fatalf("chain must resolve to the final group 6, got %+v", got)
	}
}

// TestResolveEffectiveGroupEmptyModel 空模型名无路由键，直接返回不命中。
func TestResolveEffectiveGroupEmptyModel(t *testing.T) {
	svc, repo := newResolveSvc(
		map[int64][]*model.GroupModelRoute{1: {rt(1, 1, "grok-4.5", 5)}},
		map[int64]*Group{5: grp(5, PlatformGrok, StatusActive)},
	)

	got, err := svc.ResolveEffectiveGroup(context.Background(), 1, "")
	if err != nil || got != nil {
		t.Fatalf("empty model must be a no-op, got (%v, %v)", got, err)
	}
	if repo.calls != 0 {
		t.Fatalf("empty model must not hit the repo at all, got %d calls", repo.calls)
	}
}

// TestResolveEffectiveGroupUsesCache 热路径缓存：反复解析同一条路由，查库次数不随
// 请求数增长。这条守的是「给 100% 流量加一次查库」那个性能回归。
//
// 稳态期望是 2 次而非 1 次：链式跳转要求命中后再查一次目标组有没有下一跳，
// 所以一条 1->5 的路由涉及两个组的路由表（group 1 与 group 5），各缓存一次。
func TestResolveEffectiveGroupUsesCache(t *testing.T) {
	svc, repo := newResolveSvc(
		map[int64][]*model.GroupModelRoute{1: {rt(1, 1, "grok-4.5", 5)}},
		map[int64]*Group{5: grp(5, PlatformGrok, StatusActive)},
	)

	for i := 0; i < 5; i++ {
		if _, err := svc.ResolveEffectiveGroup(context.Background(), 1, "grok-4.5"); err != nil {
			t.Fatalf("resolve %d: %v", i, err)
		}
	}
	if repo.calls != 2 {
		t.Fatalf("routes must be cached: want 2 repo calls (group 1 + group 5, each once) across 5 resolves, got %d", repo.calls)
	}
}

// TestInvalidateCacheForcesReload 写操作后必须重新读库，否则 admin 改完不生效。
func TestInvalidateCacheForcesReload(t *testing.T) {
	svc, repo := newResolveSvc(
		map[int64][]*model.GroupModelRoute{1: {rt(1, 1, "grok-4.5", 5)}},
		map[int64]*Group{5: grp(5, PlatformGrok, StatusActive)},
	)

	_, _ = svc.ResolveEffectiveGroup(context.Background(), 1, "grok-4.5")
	svc.invalidateCache()
	_, _ = svc.ResolveEffectiveGroup(context.Background(), 1, "grok-4.5")

	// 每轮解析查两个组（1 与 5）；失效后重来一轮，故 2 + 2。
	if repo.calls != 4 {
		t.Fatalf("invalidateCache must force a reload: want 4 repo calls (2 per resolve round), got %d", repo.calls)
	}
}

// TestResolveEffectiveGroupDisabledRoute 负向 N8：关掉的规则立即回落源组。
func TestResolveEffectiveGroupDisabledRoute(t *testing.T) {
	disabled := rt(1, 1, "grok-4.5", 5)
	disabled.Enabled = false
	svc, _ := newResolveSvc(
		map[int64][]*model.GroupModelRoute{1: {disabled}},
		map[int64]*Group{5: grp(5, PlatformGrok, StatusActive)},
	)

	got, err := svc.ResolveEffectiveGroup(context.Background(), 1, "grok-4.5")
	if err != nil || got != nil {
		t.Fatalf("disabled route must fall back to source group, got (%v, %v)", got, err)
	}
}
