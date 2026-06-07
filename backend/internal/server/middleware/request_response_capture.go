package middleware

import (
	"bytes"
	"encoding/json"
	"io"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// RequestResponseCapture 透明捕获网关请求体与响应体原文，异步落库供 Prompt 分析。
// 设计：对现有 handler/service 零侵入——request_id 复用 RequestLogger 注入的 ctx 值
// （与 usage_logs.request_id 同源可 JOIN），session_hash 自行从 metadata.user_id 解析。
// 必须注册在 RequestLogger 与鉴权中间件之后、网关 handler 之前。
//
// 全量留存：request/response 原文不截断（入站请求体已被网关 RequestBodyLimit 中间件
// 按 cfg.Gateway.MaxBodySize 限制）。request_truncated/response_truncated 列保留，恒为 false。
func RequestResponseCapture(repo service.RequestResponseLogRepository) gin.HandlerFunc {
	return func(c *gin.Context) {
		if repo == nil || c.Request == nil || c.Request.Body == nil {
			c.Next()
			return
		}

		reqBody, _ := io.ReadAll(c.Request.Body)
		c.Request.Body = io.NopCloser(bytes.NewReader(reqBody))

		tw := &teeResponseWriter{
			ResponseWriter: c.Writer,
			buf:            &bytes.Buffer{},
		}
		c.Writer = tw

		c.Next()

		ctx := c.Request.Context()
		log := &service.RequestResponseLog{
			RequestID:    service.ResolveUsageBillingRequestID(ctx, ""),
			SessionHash:  extractSessionHash(reqBody),
			Model:        extractModel(reqBody),
			Endpoint:     c.FullPath(),
			StatusCode:   c.Writer.Status(),
			Stream:       isStreamResponse(tw),
			RequestBody:  reqBody,
			ResponseBody: tw.buf.Bytes(),
		}
		if apiKey, ok := GetAPIKeyFromContext(c); ok && apiKey != nil {
			log.APIKeyID = &apiKey.ID
			if apiKey.User != nil {
				log.UserID = &apiKey.User.ID
			}
		}

		repo.Enqueue(log)
	}
}

// extractSessionHash 从请求体 metadata.user_id 解析出 Claude Code 会话 UUID。
// 非 Claude Code 客户端（无 metadata.user_id）返回空串。
func extractSessionHash(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var probe struct {
		Metadata struct {
			UserID string `json:"user_id"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return ""
	}
	if parsed := service.ParseMetadataUserID(probe.Metadata.UserID); parsed != nil {
		return parsed.SessionID
	}
	return ""
}

// extractModel 从请求体 model 字段解析模型名（Anthropic / OpenAI 通用）。
func extractModel(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var probe struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return ""
	}
	return probe.Model
}

// teeResponseWriter 在写客户端的同时把全部响应字节累积进 buf（不截断）。
type teeResponseWriter struct {
	gin.ResponseWriter
	buf *bytes.Buffer
}

func (w *teeResponseWriter) Write(b []byte) (int, error) {
	_, _ = w.buf.Write(b) // bytes.Buffer.Write 永不返回 error
	return w.ResponseWriter.Write(b)
}

func (w *teeResponseWriter) WriteString(s string) (int, error) {
	_, _ = w.buf.WriteString(s)
	return w.ResponseWriter.WriteString(s)
}

func isStreamResponse(w *teeResponseWriter) bool {
	ct := w.Header().Get("Content-Type")
	return bytes.Contains([]byte(ct), []byte("event-stream"))
}
