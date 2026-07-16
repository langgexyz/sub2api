package service

import (
	"context"
	"fmt"
	"strings"

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

// GroupModelRouteService 跨组模型路由规则服务
//
// 职责边界：本服务只管规则的 CRUD 与解析语义（给定规则集 + 模型名 -> 目标分组）。
// 把解析结果接进请求链路是 P2 的事，P1 不改任何热路径行为。
type GroupModelRouteService struct {
	repo      GroupModelRouteRepository
	groupRepo GroupRepository
}

func NewGroupModelRouteService(repo GroupModelRouteRepository, groupRepo GroupRepository) *GroupModelRouteService {
	return &GroupModelRouteService{repo: repo, groupRepo: groupRepo}
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
	return s.repo.Create(ctx, route)
}

// Update 更新规则，写入前做完整校验
func (s *GroupModelRouteService) Update(ctx context.Context, route *model.GroupModelRoute) (*model.GroupModelRoute, error) {
	if err := s.validate(ctx, route); err != nil {
		return nil, err
	}
	return s.repo.Update(ctx, route)
}

// Delete 删除规则
func (s *GroupModelRouteService) Delete(ctx context.Context, id int64) error {
	return s.repo.Delete(ctx, id)
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
