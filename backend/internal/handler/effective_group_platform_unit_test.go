//go:build unit

package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

func ctxWithEffectivePlatform(t *testing.T, platform string) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	if platform != "" {
		req = req.WithContext(context.WithValue(req.Context(), ctxkey.EffectiveGroupPlatform, platform))
	}
	c.Request = req
	return c
}

func anthropicKey() *service.APIKey {
	return &service.APIKey{Group: &service.Group{Platform: service.PlatformAnthropic, AllowMessagesDispatch: false}}
}

// TestAllowMessagesDispatchAcrossGroupRoute 锁定线上实测炸出来的 bug（issue #82 P2）：
// 跨组路由把 grok-4.5 导到 grok 组后，这道闸若按**源组**判，会把已经路由成功的请求
// 挡在门外，报 "This group does not allow /v1/messages dispatch"。
func TestAllowMessagesDispatchAcrossGroupRoute(t *testing.T) {
	c := ctxWithEffectivePlatform(t, service.PlatformGrok)

	if !allowOpenAICompatibleMessagesDispatch(c, anthropicKey()) {
		t.Fatal("cross-group route to a grok group must be allowed even though the source group has AllowMessagesDispatch=false")
	}
}

// TestAllowMessagesDispatchWithoutRouteKeepsSourceGate 未命中路由时必须保持原行为：
// 按源组的 AllowMessagesDispatch 判，不能因为这次改动放开了原本禁止的调度。
func TestAllowMessagesDispatchWithoutRouteKeepsSourceGate(t *testing.T) {
	c := ctxWithEffectivePlatform(t, "")

	if allowOpenAICompatibleMessagesDispatch(c, anthropicKey()) {
		t.Fatal("without a cross-group route the source group's AllowMessagesDispatch=false must still block")
	}
}

// TestRequestPlatformFollowsEffectiveGroup 跨组后必须按 grok 做协议转换，
// 否则请求会被按 OpenAI 协议转换后发给 xAI 上游。
func TestRequestPlatformFollowsEffectiveGroup(t *testing.T) {
	c := ctxWithEffectivePlatform(t, service.PlatformGrok)

	if got := openAICompatibleRequestPlatform(c, anthropicKey()); got != service.PlatformGrok {
		t.Fatalf("want %q for a request routed to a grok group, got %q", service.PlatformGrok, got)
	}
}

// TestRequestPlatformWithoutRoute 未命中路由时回落源组平台的原行为。
func TestRequestPlatformWithoutRoute(t *testing.T) {
	c := ctxWithEffectivePlatform(t, "")
	openAIKey := &service.APIKey{Group: &service.Group{Platform: service.PlatformOpenAI}}

	if got := openAICompatibleRequestPlatform(c, openAIKey); got != service.PlatformOpenAI {
		t.Fatalf("want %q without a route, got %q", service.PlatformOpenAI, got)
	}
}

func TestEffectiveGroupPlatformFallback(t *testing.T) {
	cases := []struct {
		name      string
		ctxValue  string
		apiKey    *service.APIKey
		wantValue string
	}{
		{"ctx 有值优先", service.PlatformGrok, anthropicKey(), service.PlatformGrok},
		{"ctx 无值回落 apiKey", "", anthropicKey(), service.PlatformAnthropic},
		{"apiKey 为 nil 不 panic", "", nil, ""},
		{"apiKey.Group 为 nil 不 panic", "", &service.APIKey{}, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := ctxWithEffectivePlatform(t, tc.ctxValue)
			if got := effectiveGroupPlatform(c, tc.apiKey); got != tc.wantValue {
				t.Fatalf("effectiveGroupPlatform() = %q, want %q", got, tc.wantValue)
			}
		})
	}
}
