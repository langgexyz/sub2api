package middleware

import (
	"bytes"
	"encoding/json"
	"io"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// requestResponseCaptureMaxBytes 单条 request/response 各自的最大留存字节数。
// 超出部分截断并打标，避免极端大请求（长上下文 + 缓存）撑爆内存与存储。
const requestResponseCaptureMaxBytes = 5 << 20 // 5 MiB

// RequestResponseCapture 透明捕获网关请求体与响应体原文，异步落库供 Prompt 分析。
// 设计：对现有 handler/service 零侵入——request_id 复用 RequestLogger 注入的 ctx 值
// （与 usage_logs.request_id 同源可 JOIN），session_hash 自行从 metadata.user_id 解析。
// 必须注册在 RequestLogger 与鉴权中间件之后、网关 handler 之前。
func RequestResponseCapture(repo service.RequestResponseLogRepository) gin.HandlerFunc {
	return func(c *gin.Context) {
		if repo == nil || c.Request == nil || c.Request.Body == nil {
			c.Next()
			return
		}

		fullBody, _ := io.ReadAll(c.Request.Body)
		// 还原完整 body 供下游 handler 转发——截断只作用于留存副本，绝不影响真实请求。
		c.Request.Body = io.NopCloser(bytes.NewReader(fullBody))
		reqBody, reqTruncated := capBytes(fullBody)

		tw := &teeResponseWriter{
			ResponseWriter: c.Writer,
			buf:            &bytes.Buffer{},
			limit:          requestResponseCaptureMaxBytes,
		}
		c.Writer = tw

		c.Next()

		ctx := c.Request.Context()
		log := &service.RequestResponseLog{
			RequestID:         service.ResolveUsageBillingRequestID(ctx, ""),
			SessionHash:       extractSessionHash(reqBody),
			Model:             extractModel(reqBody),
			Endpoint:          c.FullPath(),
			StatusCode:        c.Writer.Status(),
			Stream:            isStreamResponse(tw),
			RequestBody:       reqBody,
			ResponseBody:      tw.buf.Bytes(),
			RequestTruncated:  reqTruncated,
			ResponseTruncated: tw.truncated,
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

// capBytes 返回用于留存的字节副本（最多 limit 字节）并标记是否截断。
// 入参 data 为完整 body；调用方负责用完整 data 还原请求，本函数只决定存什么。
func capBytes(data []byte) ([]byte, bool) {
	if len(data) > requestResponseCaptureMaxBytes {
		return data[:requestResponseCaptureMaxBytes], true
	}
	return data, false
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

// teeResponseWriter 在写客户端的同时把响应字节累积进 buf（带上限）。
type teeResponseWriter struct {
	gin.ResponseWriter
	buf       *bytes.Buffer
	limit     int
	truncated bool
}

func (w *teeResponseWriter) Write(b []byte) (int, error) {
	w.capture(b)
	return w.ResponseWriter.Write(b)
}

func (w *teeResponseWriter) WriteString(s string) (int, error) {
	w.capture([]byte(s))
	return w.ResponseWriter.WriteString(s)
}

func (w *teeResponseWriter) capture(b []byte) {
	if w.buf.Len() >= w.limit {
		w.truncated = true
		return
	}
	remain := w.limit - w.buf.Len()
	if len(b) > remain {
		w.buf.Write(b[:remain])
		w.truncated = true
		return
	}
	w.buf.Write(b)
}

func isStreamResponse(w *teeResponseWriter) bool {
	ct := w.Header().Get("Content-Type")
	return bytes.Contains([]byte(ct), []byte("event-stream"))
}
