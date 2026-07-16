//go:build integration

package repository

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"

	"github.com/Wei-Shaw/sub2api/internal/model"
)

type GroupModelRouteRepoSuite struct {
	suite.Suite
	ctx  context.Context
	repo *groupModelRouteRepository
}

func (s *GroupModelRouteRepoSuite) SetupTest() {
	s.ctx = context.Background()
	tx := testEntTx(s.T())
	s.repo = NewGroupModelRouteRepository(tx.Client()).(*groupModelRouteRepository)
}

func TestGroupModelRouteRepoSuite(t *testing.T) {
	suite.Run(t, new(GroupModelRouteRepoSuite))
}

func (s *GroupModelRouteRepoSuite) newRoute(groupID int64, pattern string, target int64) *model.GroupModelRoute {
	return &model.GroupModelRoute{
		GroupID:       groupID,
		ModelPattern:  pattern,
		TargetGroupID: target,
		Priority:      model.DefaultRoutePriority,
		Enabled:       true,
	}
}

func (s *GroupModelRouteRepoSuite) TestCreateAndGetByID() {
	created, err := s.repo.Create(s.ctx, s.newRoute(1, "grok-4.5", 5))
	require.NoError(s.T(), err)
	require.NotZero(s.T(), created.ID)
	require.Equal(s.T(), "grok-4.5", created.ModelPattern)
	require.Equal(s.T(), int64(5), created.TargetGroupID)
	require.Equal(s.T(), model.DefaultRoutePriority, created.Priority)
	require.True(s.T(), created.Enabled)
	require.False(s.T(), created.CreatedAt.IsZero(), "TimeMixin must populate created_at")

	got, err := s.repo.GetByID(s.ctx, created.ID)
	require.NoError(s.T(), err)
	require.NotNil(s.T(), got)
	require.Equal(s.T(), created.ID, got.ID)
}

// TestGetByIDNotFound 锁定本仓库约定：未找到返回 (nil, nil) 而不是 error。
func (s *GroupModelRouteRepoSuite) TestGetByIDNotFound() {
	got, err := s.repo.GetByID(s.ctx, 999999)
	require.NoError(s.T(), err)
	require.Nil(s.T(), got)
}

// TestUniquePatternPerGroup 同一分组内不得有两条同模式规则 —— 这是解析确定性的物理保证。
func (s *GroupModelRouteRepoSuite) TestUniquePatternPerGroup() {
	_, err := s.repo.Create(s.ctx, s.newRoute(1, "grok-4.5", 5))
	require.NoError(s.T(), err)

	_, err = s.repo.Create(s.ctx, s.newRoute(1, "grok-4.5", 6))
	require.Error(s.T(), err, "duplicate (group_id, model_pattern) must be rejected by the unique index")
}

// TestSamePatternDifferentGroups 不同源分组可以各自声明同名模式，互不干扰。
func (s *GroupModelRouteRepoSuite) TestSamePatternDifferentGroups() {
	_, err := s.repo.Create(s.ctx, s.newRoute(1, "grok-4.5", 5))
	require.NoError(s.T(), err)

	_, err = s.repo.Create(s.ctx, s.newRoute(2, "grok-4.5", 5))
	require.NoError(s.T(), err, "same pattern under a different source group must be allowed")
}

// TestMultipleGroupsTargetSameGroup 多个源分组指向同一目标分组（对齐 fallback_group_id 的 M2O 约定）。
func (s *GroupModelRouteRepoSuite) TestMultipleGroupsTargetSameGroup() {
	_, err := s.repo.Create(s.ctx, s.newRoute(1, "grok-*", 5))
	require.NoError(s.T(), err)

	_, err = s.repo.Create(s.ctx, s.newRoute(2, "grok-*", 5))
	require.NoError(s.T(), err, "multiple source groups may delegate to the same target group")
}

func (s *GroupModelRouteRepoSuite) TestListByGroupIDOrdersByPriority() {
	low := s.newRoute(1, "grok-a*", 5)
	low.Priority = 90
	_, err := s.repo.Create(s.ctx, low)
	require.NoError(s.T(), err)

	high := s.newRoute(1, "grok-b*", 5)
	high.Priority = 10
	_, err = s.repo.Create(s.ctx, high)
	require.NoError(s.T(), err)

	_, err = s.repo.Create(s.ctx, s.newRoute(2, "grok-c*", 5))
	require.NoError(s.T(), err)

	routes, err := s.repo.ListByGroupID(s.ctx, 1)
	require.NoError(s.T(), err)
	require.Len(s.T(), routes, 2, "must only return routes of the requested source group")
	require.Equal(s.T(), 10, routes[0].Priority, "lower priority number comes first")
	require.Equal(s.T(), 90, routes[1].Priority)
}

// TestListByGroupIDIncludesDisabled 列表要含 disabled 规则，admin 才看得见并能重新打开。
func (s *GroupModelRouteRepoSuite) TestListByGroupIDIncludesDisabled() {
	disabled := s.newRoute(1, "grok-4.5", 5)
	disabled.Enabled = false
	_, err := s.repo.Create(s.ctx, disabled)
	require.NoError(s.T(), err)

	routes, err := s.repo.ListByGroupID(s.ctx, 1)
	require.NoError(s.T(), err)
	require.Len(s.T(), routes, 1)
	require.False(s.T(), routes[0].Enabled)
}

func (s *GroupModelRouteRepoSuite) TestUpdate() {
	created, err := s.repo.Create(s.ctx, s.newRoute(1, "grok-4.5", 5))
	require.NoError(s.T(), err)

	created.TargetGroupID = 7
	created.Enabled = false
	created.Priority = 10

	updated, err := s.repo.Update(s.ctx, created)
	require.NoError(s.T(), err)
	require.Equal(s.T(), int64(7), updated.TargetGroupID)
	require.False(s.T(), updated.Enabled)
	require.Equal(s.T(), 10, updated.Priority)

	got, err := s.repo.GetByID(s.ctx, created.ID)
	require.NoError(s.T(), err)
	require.Equal(s.T(), int64(7), got.TargetGroupID, "update must be persisted, not just returned")
}

func (s *GroupModelRouteRepoSuite) TestDelete() {
	created, err := s.repo.Create(s.ctx, s.newRoute(1, "grok-4.5", 5))
	require.NoError(s.T(), err)

	require.NoError(s.T(), s.repo.Delete(s.ctx, created.ID))

	got, err := s.repo.GetByID(s.ctx, created.ID)
	require.NoError(s.T(), err)
	require.Nil(s.T(), got, "deleted route must be gone (table has no soft delete)")
}

func (s *GroupModelRouteRepoSuite) TestListAll() {
	_, err := s.repo.Create(s.ctx, s.newRoute(2, "grok-b*", 5))
	require.NoError(s.T(), err)
	_, err = s.repo.Create(s.ctx, s.newRoute(1, "grok-a*", 5))
	require.NoError(s.T(), err)

	routes, err := s.repo.ListAll(s.ctx)
	require.NoError(s.T(), err)
	require.Len(s.T(), routes, 2)
	require.Equal(s.T(), int64(1), routes[0].GroupID, "ListAll orders by group_id first")
	require.Equal(s.T(), int64(2), routes[1].GroupID)
}
