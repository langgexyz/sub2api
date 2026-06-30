package service

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// TestHandleNonStreamKeepalivePassthrough_AggregatesWithKeepalive 验证：
// 1) 等待上游 SSE 期间向客户端写入空白保活字节；
// 2) 读完后把 SSE 聚合回非流式最终 JSON；
// 3) 前导空白不破坏 JSON 可解析性。
func TestHandleNonStreamKeepalivePassthrough_AggregatesWithKeepalive(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// 缩短保活间隔以便单测覆盖保活时序（生产为 10s）。
	prev := openAINonStreamKeepaliveInterval
	openAINonStreamKeepaliveInterval = 30 * time.Millisecond
	defer func() { openAINonStreamKeepaliveInterval = prev }()

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	svc := &OpenAIGatewayService{cfg: &config.Config{}}

	pr, pw := io.Pipe()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       pr,
		Header:     http.Header{},
	}

	go func() {
		defer func() { _ = pw.Close() }()
		_, _ = pw.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n\n"))
		// 模拟慢上游：多个保活间隔内无最终事件。
		time.Sleep(150 * time.Millisecond)
		_, _ = pw.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"hi\"}]}],\"usage\":{\"input_tokens\":3,\"output_tokens\":5}}}\n\n"))
	}()

	result, err := svc.handleNonStreamKeepalivePassthrough(c.Request.Context(), resp, c, "model", "model")
	_ = pr.Close()
	require.NoError(t, err)
	require.NotNil(t, result)

	bodyStr := rec.Body.String()
	// 客户端收到的最终响应应包含聚合后的内容与用量。
	require.Contains(t, bodyStr, "output_text")
	require.Contains(t, bodyStr, "input_tokens")
	// 应当发出过保活空白字节（最终 JSON 前存在前导空白）。
	require.True(t, strings.HasPrefix(bodyStr, " "), "expected leading keepalive whitespace")
	// 前导空白不影响 JSON 解析。
	var parsed map[string]any
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(bodyStr)), &parsed), "trimmed body must be valid JSON")
}
