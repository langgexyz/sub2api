//go:build unit

package middleware

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func newPostCtx(t *testing.T, body string) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(body))
	c.Request = req
	return c
}

func TestPeekRequestedModel(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"正常取到 model", `{"model":"grok-4.5","messages":[]}`, "grok-4.5"},
		{"无 model 字段", `{"messages":[]}`, ""},
		{"model 为空串", `{"model":"","messages":[]}`, ""},
		{"非法 JSON", `{"model":`, ""},
		{"空 body", ``, ""},
		{"JSON 数组不是对象", `[1,2,3]`, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newPostCtx(t, tc.body)
			if got := peekRequestedModel(c); got != tc.want {
				t.Fatalf("peekRequestedModel() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPeekRequestedModelRestoresBody 守的是最容易炸的一处：偷看 model 之后 body
// 必须原样放回，否则下游 handler 拿到空 body，表现是所有请求莫名其妙参数错误。
func TestPeekRequestedModelRestoresBody(t *testing.T) {
	body := `{"model":"grok-4.5","messages":[{"role":"user","content":"hi"}]}`
	c := newPostCtx(t, body)

	_ = peekRequestedModel(c)

	got, err := io.ReadAll(c.Request.Body)
	if err != nil {
		t.Fatalf("read body back: %v", err)
	}
	if string(got) != body {
		t.Fatalf("body must be restored byte-for-byte:\n got: %s\nwant: %s", got, body)
	}
}

// TestPeekRequestedModelRestoresBodyOnInvalidJSON 非法 JSON 同样要回填 —— 让下游
// 用它自己的协议格式报错，而不是在这里因为 body 被吃掉而变成另一种错。
func TestPeekRequestedModelRestoresBodyOnInvalidJSON(t *testing.T) {
	body := `{"model":`
	c := newPostCtx(t, body)

	_ = peekRequestedModel(c)

	got, _ := io.ReadAll(c.Request.Body)
	if string(got) != body {
		t.Fatalf("body must be restored even when JSON is invalid: got %q want %q", got, body)
	}
}

// TestPeekRequestedModelIgnoresNonPost GET 请求没有路由键（如 /v1beta/models 列表），
// 直接返回空串走原组，不该去读 body。
func TestPeekRequestedModelIgnoresNonPost(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/v1beta/models", nil)

	if got := peekRequestedModel(c); got != "" {
		t.Fatalf("GET must not resolve a routing key, got %q", got)
	}
}

func TestPeekRequestedModelNilRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = nil

	if got := peekRequestedModel(c); got != "" {
		t.Fatalf("nil request must not panic and must return empty, got %q", got)
	}
}
