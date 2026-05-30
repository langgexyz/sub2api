//go:build unit

package handler

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func TestEdgeUpstreamBaseURL_CustomBaseURL(t *testing.T) {
	// MiMo: anthropic-compatible fixed-key account with a custom base_url.
	acc := &service.Account{
		Platform:    service.PlatformAnthropic,
		Type:        service.AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "tp-1", "base_url": "https://token-plan-cn.xiaomimimo.com/anthropic"},
	}
	base, err := edgeUpstreamBaseURL(acc)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if base != "https://token-plan-cn.xiaomimimo.com/anthropic" {
		t.Fatalf("base = %q", base)
	}
}

func TestEdgeUpstreamBaseURL_AnthropicDefault(t *testing.T) {
	// OAuth account: no custom base_url -> platform default.
	acc := &service.Account{Platform: service.PlatformAnthropic, Type: service.AccountTypeOAuth}
	base, err := edgeUpstreamBaseURL(acc)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if base != "https://api.anthropic.com" {
		t.Fatalf("anthropic default = %q", base)
	}
}

func TestEdgeUpstreamBaseURL_OpenAIDefault(t *testing.T) {
	acc := &service.Account{Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth}
	base, err := edgeUpstreamBaseURL(acc)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if base != "https://api.openai.com" {
		t.Fatalf("openai default = %q", base)
	}
}

func TestEdgeUpstreamBaseURL_UnknownPlatformNoBase(t *testing.T) {
	// No custom base_url and a platform with no default => error.
	acc := &service.Account{Platform: "gemini", Type: service.AccountTypeOAuth}
	if _, err := edgeUpstreamBaseURL(acc); err == nil {
		t.Fatalf("missing base url must error")
	}
}
