package handler

import (
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// CCSessionHandler 暴露 Claude Code 会话历史分析的只读 admin 端点。
// 数据源 request_response_logs；用于内部改进研发流程（问法复盘/教练）。
type CCSessionHandler struct {
	svc *service.CCSessionReplayService
}

// NewCCSessionHandler 构造 handler。
func NewCCSessionHandler(svc *service.CCSessionReplayService) *CCSessionHandler {
	return &CCSessionHandler{svc: svc}
}

// ListSessions GET /admin/cc-sessions?user_id=&username=&from=&to=&limit=
func (h *CCSessionHandler) ListSessions(c *gin.Context) {
	var q service.CCSessionListQuery
	if v := c.Query("user_id"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			response.BadRequest(c, "invalid user_id")
			return
		}
		q.UserID = &id
	}
	q.Username = c.Query("username")
	if q.UserID == nil && q.Username == "" {
		response.BadRequest(c, "user_id or username is required")
		return
	}
	from, to, ok := parseTimeRange(c)
	if !ok {
		return
	}
	q.From, q.To = from, to
	q.Limit = atoiDefault(c.Query("limit"), 0)

	res, err := h.svc.ListSessions(c.Request.Context(), q)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, res)
}

// GetReplay GET /admin/cc-sessions/:hash/replay?mode=full|prompts
func (h *CCSessionHandler) GetReplay(c *gin.Context) {
	hash := c.Param("hash")
	if hash == "" {
		response.BadRequest(c, "session_hash is required")
		return
	}
	if c.Query("mode") == "prompts" {
		ps, err := h.svc.GetPrompts(c.Request.Context(), hash)
		if err != nil {
			response.ErrorFrom(c, err)
			return
		}
		response.Success(c, gin.H{"session_hash": hash, "prompts": ps})
		return
	}
	replay, err := h.svc.GetReplay(c.Request.Context(), hash)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, replay)
}

// SearchPrompts GET /admin/cc-sessions/search?q=&user_id=&from=&to=&limit=
func (h *CCSessionHandler) SearchPrompts(c *gin.Context) {
	q := c.Query("q")
	if q == "" {
		response.BadRequest(c, "q is required")
		return
	}
	sq := service.CCPromptSearchQuery{Query: q}
	if v := c.Query("user_id"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			response.BadRequest(c, "invalid user_id")
			return
		}
		sq.UserID = &id
	}
	from, to, ok := parseTimeRange(c)
	if !ok {
		return
	}
	sq.From, sq.To = from, to
	sq.Limit = atoiDefault(c.Query("limit"), 0)

	res, err := h.svc.SearchPrompts(c.Request.Context(), sq)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, res)
}

// parseTimeRange 解析 from/to（RFC3339）。解析失败时已回写 400 并返回 ok=false。
func parseTimeRange(c *gin.Context) (from, to *time.Time, ok bool) {
	if v := c.Query("from"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			response.BadRequest(c, "invalid from (need RFC3339)")
			return nil, nil, false
		}
		from = &t
	}
	if v := c.Query("to"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			response.BadRequest(c, "invalid to (need RFC3339)")
			return nil, nil, false
		}
		to = &t
	}
	return from, to, true
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}
