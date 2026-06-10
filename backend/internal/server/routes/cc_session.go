package routes

import (
	"github.com/Wei-Shaw/sub2api/internal/handler"

	"github.com/gin-gonic/gin"
)

// registerCCSessionRoutes 注册 Claude Code 会话历史分析的只读端点（admin）。
func registerCCSessionRoutes(admin *gin.RouterGroup, h *handler.Handlers) {
	g := admin.Group("/cc-sessions")
	{
		g.GET("", h.CCSession.ListSessions)        // 列某用户会话
		g.GET("/search", h.CCSession.SearchPrompts) // 跨会话检索提问
		g.GET("/:hash/replay", h.CCSession.GetReplay) // 回放会话（mode=full|prompts）
	}
}
