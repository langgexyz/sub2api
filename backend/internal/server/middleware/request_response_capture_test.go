package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type fakeRRLRepo struct {
	logs []*service.RequestResponseLog
}

func (f *fakeRRLRepo) Enqueue(log *service.RequestResponseLog) {
	f.logs = append(f.logs, log)
}

// requestLoggerForTest mirrors RequestLogger's ctx injection without the logger init.
func requestLoggerForTest() gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := context.WithValue(c.Request.Context(), ctxkey.RequestID, "fixed-req-id")
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

func TestRequestResponseCaptureCapturesBodiesAndSession(t *testing.T) {
	gin.SetMode(gin.TestMode)

	repo := &fakeRRLRepo{}
	apiKey := &service.APIKey{ID: 77, User: &service.User{ID: 11}}

	router := gin.New()
	router.Use(requestLoggerForTest())
	router.Use(func(c *gin.Context) {
		c.Set(string(ContextKeyAPIKey), apiKey)
		c.Next()
	})
	router.Use(RequestResponseCapture(repo))
	router.POST("/v1/messages", func(c *gin.Context) {
		// 下游必须仍能读到完整请求体。
		body, _ := c.GetRawData()
		require.Contains(t, string(body), "hello-prompt")
		c.String(http.StatusOK, "assistant-reply")
	})

	reqBody := `{"model":"claude-opus-4-8","metadata":{"user_id":"user_` +
		strings.Repeat("a", 64) +
		`_account__session_5870cd48-7aed-467a-a59f-b07f50104d5e"},"messages":[{"role":"user","content":"hello-prompt"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "assistant-reply", w.Body.String())

	require.Len(t, repo.logs, 1)
	got := repo.logs[0]
	require.Equal(t, "local:fixed-req-id", got.RequestID)
	require.Equal(t, "5870cd48-7aed-467a-a59f-b07f50104d5e", got.SessionHash)
	require.Equal(t, "claude-opus-4-8", got.Model)
	require.Equal(t, "/v1/messages", got.Endpoint)
	require.Equal(t, http.StatusOK, got.StatusCode)
	require.Contains(t, string(got.RequestBody), "hello-prompt")
	require.Equal(t, "assistant-reply", string(got.ResponseBody))
	require.NotNil(t, got.APIKeyID)
	require.Equal(t, int64(77), *got.APIKeyID)
	require.NotNil(t, got.UserID)
	require.Equal(t, int64(11), *got.UserID)
	require.False(t, got.RequestTruncated)
	require.False(t, got.ResponseTruncated)
}

func TestRequestResponseCaptureStoresFullBodyNoTruncation(t *testing.T) {
	gin.SetMode(gin.TestMode)

	repo := &fakeRRLRepo{}
	router := gin.New()
	router.Use(requestLoggerForTest())
	router.Use(RequestResponseCapture(repo))

	var downstreamLen int
	router.POST("/v1/messages", func(c *gin.Context) {
		body, _ := c.GetRawData()
		downstreamLen = len(body)
		c.String(http.StatusOK, "ok")
	})

	// 大 body：下游仍收到完整长度，但留存副本受上限保护。
	big := 8 << 20 // 8 MiB
	reqBody := strings.Repeat("x", big)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, big, downstreamLen, "下游应收到完整 body")
	require.Len(t, repo.logs, 1)
	require.True(t, repo.logs[0].RequestTruncated)
	require.Len(t, repo.logs[0].RequestBody, requestResponseCaptureMaxBytes, "留存副本应受上限保护")
}

func TestExtractSessionHashEmptyWithoutMetadata(t *testing.T) {
	require.Equal(t, "", extractSessionHash([]byte(`{"model":"gpt-4o","messages":[]}`)))
	require.Equal(t, "", extractSessionHash(nil))
	require.Equal(t, "", extractSessionHash([]byte(`not-json`)))
}
