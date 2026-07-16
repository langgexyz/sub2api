package repository

import (
	"context"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/groupmodelroute"
	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

type groupModelRouteRepository struct {
	client *ent.Client
}

// NewGroupModelRouteRepository 创建跨组模型路由规则仓库
func NewGroupModelRouteRepository(client *ent.Client) service.GroupModelRouteRepository {
	return &groupModelRouteRepository{client: client}
}

// ListByGroupID 获取某个源分组的全部路由规则（含 disabled），按 model_pattern 升序
func (r *groupModelRouteRepository) ListByGroupID(ctx context.Context, groupID int64) ([]*model.GroupModelRoute, error) {
	routes, err := r.client.GroupModelRoute.Query().
		Where(groupmodelroute.GroupIDEQ(groupID)).
		Order(ent.Asc(groupmodelroute.FieldModelPattern), ent.Asc(groupmodelroute.FieldID)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	return r.toModels(routes), nil
}

// ListAll 获取全部路由规则，按 (group_id, model_pattern) 升序
func (r *groupModelRouteRepository) ListAll(ctx context.Context) ([]*model.GroupModelRoute, error) {
	routes, err := r.client.GroupModelRoute.Query().
		Order(ent.Asc(groupmodelroute.FieldGroupID), ent.Asc(groupmodelroute.FieldModelPattern), ent.Asc(groupmodelroute.FieldID)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	return r.toModels(routes), nil
}

// GetByID 根据 ID 获取规则，未找到返回 (nil, nil)
func (r *groupModelRouteRepository) GetByID(ctx context.Context, id int64) (*model.GroupModelRoute, error) {
	route, err := r.client.GroupModelRoute.Get(ctx, id)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return r.toModel(route), nil
}

// Create 创建规则
func (r *groupModelRouteRepository) Create(ctx context.Context, route *model.GroupModelRoute) (*model.GroupModelRoute, error) {
	created, err := r.client.GroupModelRoute.Create().
		SetGroupID(route.GroupID).
		SetModelPattern(route.ModelPattern).
		SetTargetGroupID(route.TargetGroupID).
		SetEnabled(route.Enabled).
		Save(ctx)
	if err != nil {
		return nil, err
	}
	return r.toModel(created), nil
}

// Update 更新规则
func (r *groupModelRouteRepository) Update(ctx context.Context, route *model.GroupModelRoute) (*model.GroupModelRoute, error) {
	updated, err := r.client.GroupModelRoute.UpdateOneID(route.ID).
		SetGroupID(route.GroupID).
		SetModelPattern(route.ModelPattern).
		SetTargetGroupID(route.TargetGroupID).
		SetEnabled(route.Enabled).
		Save(ctx)
	if err != nil {
		return nil, err
	}
	return r.toModel(updated), nil
}

// Delete 删除规则
func (r *groupModelRouteRepository) Delete(ctx context.Context, id int64) error {
	return r.client.GroupModelRoute.DeleteOneID(id).Exec(ctx)
}

func (r *groupModelRouteRepository) toModels(routes []*ent.GroupModelRoute) []*model.GroupModelRoute {
	out := make([]*model.GroupModelRoute, len(routes))
	for i, route := range routes {
		out[i] = r.toModel(route)
	}
	return out
}

func (r *groupModelRouteRepository) toModel(e *ent.GroupModelRoute) *model.GroupModelRoute {
	if e == nil {
		return nil
	}
	return &model.GroupModelRoute{
		ID:            e.ID,
		GroupID:       e.GroupID,
		ModelPattern:  e.ModelPattern,
		TargetGroupID: e.TargetGroupID,
		Enabled:       e.Enabled,
		CreatedAt:     e.CreatedAt,
		UpdatedAt:     e.UpdatedAt,
	}
}
