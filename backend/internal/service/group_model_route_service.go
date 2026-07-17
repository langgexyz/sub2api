package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/model"
)

// GroupModelRouteRepository 定义跨组模型路由规则的数据访问接口
type GroupModelRouteRepository interface {
	// ListByGroupID 获取某个源分组的全部路由规则（含 disabled），按 model_pattern 升序
	ListByGroupID(ctx context.Context, groupID int64) ([]*model.GroupModelRoute, error)
	// ListAll 获取全部路由规则，按 (group_id, model_pattern) 升序
	ListAll(ctx context.Context) ([]*model.GroupModelRoute, error)
	// GetByID 根据 ID 获取规则，未找到返回 (nil, nil)
	GetByID(ctx context.Context, id int64) (*model.GroupModelRoute, error)
	// Create 创建规则
	Create(ctx context.Context, route *model.GroupModelRoute) (*model.GroupModelRoute, error)
	// Update 更新规则
	Update(ctx context.Context, route *model.GroupModelRoute) (*model.GroupModelRoute, error)
	// Delete 删除规则
	Delete(ctx context.Context, id int64) error
}

// routeCacheTTL 路由表本地缓存的存活时间。
//
// 取 30s 的依据：路由规则是「配好就不动」的配置，而 ResolveEffectiveGroup 挂在**每个**
// 网关请求上——不缓存就等于为了服务不到 1% 的请求给 100% 的流量加一次查库。
// 30s 是「admin 改完最迟多久生效」与「热路径查库频率」的折中：急停（enabled=false）
// 最迟 30s 生效，而稳态下每组每 30s 才查一次库。
// 注意：admin 经本 service 改动会立即失效本地缓存，30s 只是多实例部署时的兜底上限。
const routeCacheTTL = 30 * time.Second

type routeCacheEntry struct {
	routes    []*model.GroupModelRoute
	expiresAt time.Time
}

// routeGroupLookup 是本 service 对分组仓库的**最小**依赖：只需要按 ID 取分组。
//
// 刻意不直接吃 GroupRepository 大接口：一是本 service 确实只用这一个方法，二是收窄后
// 测试里造个假实现只要写一个方法，而不是为了测解析逻辑去实现二十个用不到的方法。
// GroupRepository 天然满足本接口，wire 注入不受影响。
type routeGroupLookup interface {
	GetByID(ctx context.Context, id int64) (*Group, error)
}

// GroupModelRouteService 跨组模型路由规则服务
//
// 职责边界：规则 CRUD + 解析语义（模型名 -> 目标分组）+ 热路径缓存。
type GroupModelRouteService struct {
	repo      GroupModelRouteRepository
	groupRepo routeGroupLookup

	// 按源分组缓存路由表，挡住热路径的重复查库。
	cache   map[int64]routeCacheEntry
	cacheMu sync.RWMutex
}

func NewGroupModelRouteService(repo GroupModelRouteRepository, groupRepo GroupRepository) *GroupModelRouteService {
	return &GroupModelRouteService{
		repo:      repo,
		groupRepo: groupRepo,
		cache:     make(map[int64]routeCacheEntry),
	}
}

// routesFor 取某个源分组的路由表，优先走本地缓存。
func (s *GroupModelRouteService) routesFor(ctx context.Context, groupID int64) ([]*model.GroupModelRoute, error) {
	s.cacheMu.RLock()
	entry, ok := s.cache[groupID]
	s.cacheMu.RUnlock()
	if ok && time.Now().Before(entry.expiresAt) {
		return entry.routes, nil
	}

	routes, err := s.repo.ListByGroupID(ctx, groupID)
	if err != nil {
		return nil, err
	}

	s.cacheMu.Lock()
	s.cache[groupID] = routeCacheEntry{routes: routes, expiresAt: time.Now().Add(routeCacheTTL)}
	s.cacheMu.Unlock()
	return routes, nil
}

// invalidateCache 清空本地路由缓存。
//
// 全清而非按 groupID 清：一次 admin 写可能同时影响源组与目标组（改 target_group_id
// 就跨了两个组），且路由表规模很小，全清的代价是下次请求多查一次库，比"精确清漏了
// 一个组导致规则不生效"划算得多。
func (s *GroupModelRouteService) invalidateCache() {
	s.cacheMu.Lock()
	s.cache = make(map[int64]routeCacheEntry)
	s.cacheMu.Unlock()
}

// ResolveRoute 在规则集中挑出请求模型命中的那条规则，无命中返回 nil。
//
// 匹配语义（issue #82 决策）：**模式最长优先**，与 account 层 ResolveMappedModel 的
// 语义保持一致。精确模式视为比同长度前缀模式更具体，故优先于通配模式。
//
// 最长优先已完全决定结果：受 (group_id, model_pattern) 唯一索引约束，同一分组内两条
// 不同模式不可能对同一模型产生相同具体度（两个不同的等长字符串不可能同时是同一个串的
// 前缀）。ID 决相只是兜底，保证即便唯一索引未来放宽，结果也不依赖 slice 顺序。
func ResolveRoute(routes []*model.GroupModelRoute, requestedModel string) *model.GroupModelRoute {
	if requestedModel == "" {
		return nil
	}

	var best *model.GroupModelRoute
	bestSpec := -1
	for _, r := range routes {
		if r == nil || !r.Enabled {
			continue
		}
		if !matchModelPattern(r.ModelPattern, requestedModel) {
			continue
		}
		spec := patternSpecificity(r.ModelPattern)
		if best == nil || spec > bestSpec || (spec == bestSpec && r.ID < best.ID) {
			best, bestSpec = r, spec
		}
	}
	return best
}

// patternSpecificity 度量模式的具体程度，数字越大越具体。
// 精确模式记为 len+1，使其胜过前缀长度相同的通配模式（如 "grok-4.5" 胜过 "grok-4.5*"）。
func patternSpecificity(pattern string) int {
	if strings.HasSuffix(pattern, "*") {
		return len(strings.TrimSuffix(pattern, "*"))
	}
	return len(pattern) + 1
}

// ErrRouteTargetUnavailable 跨组路由命中了，但目标分组不可用（不存在 / 已停用）。
//
// 刻意不静默回落源分组：路由是显式声明，声明了就该生效；目标坏了要让人看见，
// 而不是"悄悄用源组的号顶上"——那会让 grok-4.5 的请求静静地被 Claude 账号接走。
var ErrRouteTargetUnavailable = errors.New("cross-group route target is unavailable")

// ErrRouteCycle 跨组路由成环（A -> B -> A）。
var ErrRouteCycle = errors.New("cross-group route cycle detected")

// maxRouteHops 跨组路由的最大跳数。环检测已经挡住了成环，这个上限挡的是
// 「没成环但链过长」的病态配置（A->B->C->...），避免热路径为一次请求查十几次库。
const maxRouteHops = 4

// ResolveEffectiveGroup 按请求模型解析出该请求实际应该落到哪个分组。
//
// 返回 (nil, nil) 表示没有路由命中 —— 调用方应留在源分组，这是绝大多数请求的路径。
//
// 支持链式跳转（A 的 grok-4.5 -> B，B 的 grok-4.5 -> C），带 visited 环检测，
// 语义与 resolveGatewayGroup 的 fallback 链一致。
func (s *GroupModelRouteService) ResolveEffectiveGroup(ctx context.Context, sourceGroupID int64, requestedModel string) (*Group, error) {
	if requestedModel == "" {
		return nil, nil
	}

	visited := map[int64]struct{}{sourceGroupID: {}}
	currentID := sourceGroupID
	var resolved *Group

	for hop := 0; hop < maxRouteHops; hop++ {
		routes, err := s.routesFor(ctx, currentID)
		if err != nil {
			return nil, err
		}
		hit := ResolveRoute(routes, requestedModel)
		if hit == nil {
			return resolved, nil
		}

		if _, seen := visited[hit.TargetGroupID]; seen {
			return nil, fmt.Errorf("%w: group %d revisited via model %q", ErrRouteCycle, hit.TargetGroupID, requestedModel)
		}
		visited[hit.TargetGroupID] = struct{}{}

		target, err := s.groupRepo.GetByID(ctx, hit.TargetGroupID)
		if err != nil {
			return nil, err
		}
		if target == nil {
			return nil, fmt.Errorf("%w: route %d points at group %d which does not exist", ErrRouteTargetUnavailable, hit.ID, hit.TargetGroupID)
		}
		if target.Status != StatusActive {
			return nil, fmt.Errorf("%w: route %d points at group %d whose status is %q", ErrRouteTargetUnavailable, hit.ID, hit.TargetGroupID, target.Status)
		}

		resolved = target
		currentID = target.ID
	}

	return nil, fmt.Errorf("%w: exceeded %d hops from group %d via model %q", ErrRouteCycle, maxRouteHops, sourceGroupID, requestedModel)
}

// RoutedTarget 描述一条生效的跨组路由指向何处：目标分组 + 该路由的模型模式。
type RoutedTarget struct {
	Pattern string
	Group   *Group
}

// RoutedTargets 列出某个源分组所有生效路由的目标分组，供「聚合组的模型列表要包含哪些
// 目标组的模型」使用。
//
// 与 ResolveEffectiveGroup 的区别：那个是按**某一个**请求模型解析出唯一落点（热路径）；
// 这个是把**全部**路由目标摊开（模型列表这种冷路径）。
//
// 只看一跳：模型列表是给人/客户端看的，一跳已经覆盖「聚合组 -> 各平台组」这个实际形态；
// 多跳链的展开留到真有这种配置时再说，避免在冷路径上引入链式递归与环检测的复杂度。
// 目标分组不存在/已停用则跳过（列表不该因为一条坏路由整个报错）。
func (s *GroupModelRouteService) RoutedTargets(ctx context.Context, groupID int64) ([]RoutedTarget, error) {
	routes, err := s.routesFor(ctx, groupID)
	if err != nil {
		return nil, err
	}

	targets := make([]RoutedTarget, 0, len(routes))
	for _, r := range routes {
		if r == nil || !r.Enabled {
			continue
		}
		target, err := s.groupRepo.GetByID(ctx, r.TargetGroupID)
		if err != nil {
			return nil, err
		}
		if target == nil || target.Status != StatusActive {
			continue
		}
		targets = append(targets, RoutedTarget{Pattern: r.ModelPattern, Group: target})
	}
	return targets, nil
}

// MatchesPattern 报告模型名是否命中该路由的模式，供调用方筛目标分组的模型列表。
func (t RoutedTarget) MatchesPattern(model string) bool {
	return matchModelPattern(t.Pattern, model)
}

// ListByGroupID 获取某个源分组的全部路由规则
func (s *GroupModelRouteService) ListByGroupID(ctx context.Context, groupID int64) ([]*model.GroupModelRoute, error) {
	return s.repo.ListByGroupID(ctx, groupID)
}

// ListAll 获取全部路由规则
func (s *GroupModelRouteService) ListAll(ctx context.Context) ([]*model.GroupModelRoute, error) {
	return s.repo.ListAll(ctx)
}

// GetByID 根据 ID 获取规则，未找到返回 (nil, nil)
func (s *GroupModelRouteService) GetByID(ctx context.Context, id int64) (*model.GroupModelRoute, error) {
	return s.repo.GetByID(ctx, id)
}

// Create 创建规则，写入前做完整校验
func (s *GroupModelRouteService) Create(ctx context.Context, route *model.GroupModelRoute) (*model.GroupModelRoute, error) {
	if err := s.validate(ctx, route); err != nil {
		return nil, err
	}
	created, err := s.repo.Create(ctx, route)
	if err != nil {
		return nil, err
	}
	s.invalidateCache()
	return created, nil
}

// Update 更新规则，写入前做完整校验
func (s *GroupModelRouteService) Update(ctx context.Context, route *model.GroupModelRoute) (*model.GroupModelRoute, error) {
	if err := s.validate(ctx, route); err != nil {
		return nil, err
	}
	updated, err := s.repo.Update(ctx, route)
	if err != nil {
		return nil, err
	}
	s.invalidateCache()
	return updated, nil
}

// Delete 删除规则
func (s *GroupModelRouteService) Delete(ctx context.Context, id int64) error {
	if err := s.repo.Delete(ctx, id); err != nil {
		return err
	}
	s.invalidateCache()
	return nil
}

// validate 校验规则的自洽性与引用有效性。
//
// 这里挡住的都是「配置期能发现、运行期才爆就很难查」的错误：自指路由、指向不存在的
// 分组、空模式。跨组环（A->B->A）不在这里挡——环要在解析链路上看全局，P2 接入时复用
// resolveGatewayGroup 的 visited 检测。
func (s *GroupModelRouteService) validate(ctx context.Context, route *model.GroupModelRoute) error {
	if route.ModelPattern == "" {
		return fmt.Errorf("model_pattern is required")
	}
	if route.ModelPattern == "*" {
		return fmt.Errorf("model_pattern %q is too broad: it would delegate every model and make the source group's own accounts unreachable", route.ModelPattern)
	}
	if strings.Count(route.ModelPattern, "*") > 1 || (strings.Contains(route.ModelPattern, "*") && !strings.HasSuffix(route.ModelPattern, "*")) {
		return fmt.Errorf("model_pattern %q is invalid: only a single trailing * is supported", route.ModelPattern)
	}
	if route.GroupID == route.TargetGroupID {
		return fmt.Errorf("target_group_id must differ from group_id (self-routing is a no-op)")
	}

	src, err := s.groupRepo.GetByID(ctx, route.GroupID)
	if err != nil {
		return err
	}
	if src == nil {
		return fmt.Errorf("source group %d not found", route.GroupID)
	}

	target, err := s.groupRepo.GetByID(ctx, route.TargetGroupID)
	if err != nil {
		return err
	}
	if target == nil {
		return fmt.Errorf("target group %d not found", route.TargetGroupID)
	}

	return nil
}
