package admin

import (
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// GroupModelRouteHandler 处理跨组模型路由规则的 HTTP 请求
type GroupModelRouteHandler struct {
	service *service.GroupModelRouteService
}

// NewGroupModelRouteHandler 创建跨组模型路由规则处理器
func NewGroupModelRouteHandler(service *service.GroupModelRouteService) *GroupModelRouteHandler {
	return &GroupModelRouteHandler{service: service}
}

// CreateGroupModelRouteRequest 创建路由规则请求
type CreateGroupModelRouteRequest struct {
	GroupID       int64  `json:"group_id" binding:"required"`
	ModelPattern  string `json:"model_pattern" binding:"required"`
	TargetGroupID int64  `json:"target_group_id" binding:"required"`
	Enabled       *bool  `json:"enabled"`
}

// UpdateGroupModelRouteRequest 更新路由规则请求（部分更新，所有字段可选）
type UpdateGroupModelRouteRequest struct {
	GroupID       *int64  `json:"group_id"`
	ModelPattern  *string `json:"model_pattern"`
	TargetGroupID *int64  `json:"target_group_id"`
	Enabled       *bool   `json:"enabled"`
}

// List 获取路由规则
// GET /api/v1/admin/group-model-routes?group_id=1
func (h *GroupModelRouteHandler) List(c *gin.Context) {
	if raw := c.Query("group_id"); raw != "" {
		groupID, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			response.BadRequest(c, "Invalid group_id")
			return
		}
		routes, err := h.service.ListByGroupID(c.Request.Context(), groupID)
		if err != nil {
			response.ErrorFrom(c, err)
			return
		}
		response.Success(c, routes)
		return
	}

	routes, err := h.service.ListAll(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, routes)
}

// GetByID 根据 ID 获取路由规则
// GET /api/v1/admin/group-model-routes/:id
func (h *GroupModelRouteHandler) GetByID(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid route ID")
		return
	}

	route, err := h.service.GetByID(c.Request.Context(), id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if route == nil {
		response.NotFound(c, "Route not found")
		return
	}

	response.Success(c, route)
}

// Create 创建路由规则
// POST /api/v1/admin/group-model-routes
func (h *GroupModelRouteHandler) Create(c *gin.Context) {
	var req CreateGroupModelRouteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	route := &model.GroupModelRoute{
		GroupID:       req.GroupID,
		ModelPattern:  req.ModelPattern,
		TargetGroupID: req.TargetGroupID,
		Enabled:       true,
	}
	if req.Enabled != nil {
		route.Enabled = *req.Enabled
	}

	created, err := h.service.Create(c.Request.Context(), route)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	response.Success(c, created)
}

// Update 更新路由规则
// PUT /api/v1/admin/group-model-routes/:id
func (h *GroupModelRouteHandler) Update(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid route ID")
		return
	}

	var req UpdateGroupModelRouteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	// 先读现状再覆盖：部分 PUT 不得把未提交的字段写成零值。
	// 见 gotcha「admin settings 部分 PUT 清零非指针字段」——同类坑在本项目已致 prod 登录停摆。
	route, err := h.service.GetByID(c.Request.Context(), id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if route == nil {
		response.NotFound(c, "Route not found")
		return
	}

	if req.GroupID != nil {
		route.GroupID = *req.GroupID
	}
	if req.ModelPattern != nil {
		route.ModelPattern = *req.ModelPattern
	}
	if req.TargetGroupID != nil {
		route.TargetGroupID = *req.TargetGroupID
	}
	if req.Enabled != nil {
		route.Enabled = *req.Enabled
	}

	updated, err := h.service.Update(c.Request.Context(), route)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	response.Success(c, updated)
}

// Delete 删除路由规则
// DELETE /api/v1/admin/group-model-routes/:id
func (h *GroupModelRouteHandler) Delete(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid route ID")
		return
	}

	if err := h.service.Delete(c.Request.Context(), id); err != nil {
		response.ErrorFrom(c, err)
		return
	}

	response.Success(c, gin.H{"message": "Route deleted successfully"})
}
