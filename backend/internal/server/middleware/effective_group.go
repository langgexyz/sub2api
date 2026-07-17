package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// EffectiveGroupMiddleware 跨组模型路由中间件类型
type EffectiveGroupMiddleware gin.HandlerFunc

// ResolveEffectiveGroup 按请求模型把「有效分组」解析出来，写进 ctx。
//
// 为什么必须在协议路由之前跑：入口协议分支（routes/gateway.go 的 /v1/messages 与
// /v1/chat/completions）靠 getGroupPlatform 决定把请求交给哪个 gateway service，而
// 那个判断发生在 handler 之前；选号阶段的 resolveGatewayGroup 跳转则发生在 handler
// 内部——时机太晚，管不到协议分支。所以有效组必须在这里就定下来，否则会出现
// 「选号选了 Grok 账号，但请求已经按 anthropic 交给原生 Anthropic 栈」的错配。
//
// 影响范围严格限定在「协议选择 + 选号」：只写 ctx，不碰 apiKey.GroupID。计费/配额
// 继续走 apiKey.GroupID（源分组），这是 D1 的要求，也是这里不图省事直接改
// apiKey.Group 的原因——那样账单会跟着跳到目标分组去。
//
// 详见 docs/tech/cross-group-model-routing.md。
func ResolveEffectiveGroup(routeService *service.GroupModelRouteService, writeError GatewayErrorWriter) EffectiveGroupMiddleware {
	return func(c *gin.Context) {
		apiKey, ok := GetAPIKeyFromContext(c)
		if !ok || apiKey == nil || apiKey.GroupID == nil {
			c.Next()
			return
		}

		requestedModel := peekRequestedModel(c)
		if requestedModel == "" {
			// 无 model 字段 / body 非 JSON / 非 POST：没有路由键，留在源分组。
			// 这是 GET /v1beta/models 一类请求的正常路径，不是错误。
			c.Next()
			return
		}

		target, err := routeService.ResolveEffectiveGroup(c.Request.Context(), *apiKey.GroupID, requestedModel)
		if err != nil {
			// 路由声明了但目标坏了 / 成环 —— 显式报错，不静默回落源分组。
			// 静默回落会让 grok-4.5 的请求被源分组的 Claude 账号接走，排查时毫无线索。
			status := http.StatusServiceUnavailable
			if errors.Is(err, service.ErrRouteCycle) {
				status = http.StatusInternalServerError
			}
			service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonLocalFeatureGate)
			writeError(c, status, "Model routing is misconfigured for this group: "+err.Error())
			c.Abort()
			return
		}
		if target == nil {
			// 绝大多数请求走这里：没有路由命中，留在源分组。
			c.Next()
			return
		}

		ctx := context.WithValue(c.Request.Context(), ctxkey.EffectiveGroupID, target.ID)
		ctx = context.WithValue(ctx, ctxkey.EffectiveGroupPlatform, target.Platform)
		c.Request = c.Request.WithContext(ctx)
		c.Set(string(ContextKeyEffectiveGroup), target)

		c.Next()
	}
}

// peekRequestedModel 从请求体里偷看 model 字段，读完把 body 原样放回去。
//
// body 必须回填：下游 handler 还要完整解析它。任何 read 失败 / 非 JSON / 无 model
// 一律返回空串走原组，不在这里报错——这一层只负责路由，不负责校验请求合法性，
// 请求本身有问题该由对应 handler 用它自己的协议格式报错。
func peekRequestedModel(c *gin.Context) string {
	if c.Request == nil || c.Request.Body == nil || c.Request.Method != http.MethodPost {
		return ""
	}

	raw, err := io.ReadAll(c.Request.Body)
	// 无论读成功与否都必须把 body 放回去，否则下游拿到空 body。
	c.Request.Body = io.NopCloser(bytes.NewReader(raw))
	if err != nil || len(raw) == 0 {
		return ""
	}

	var probe struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return ""
	}
	return probe.Model
}

// GetEffectiveGroupFromContext 取出跨组路由命中的目标分组，未命中返回 (nil, false)。
func GetEffectiveGroupFromContext(c *gin.Context) (*service.Group, bool) {
	value, exists := c.Get(string(ContextKeyEffectiveGroup))
	if !exists {
		return nil, false
	}
	group, ok := value.(*service.Group)
	return group, ok
}
