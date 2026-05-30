//go:build unit

package handler

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func TestEdgeAuthAndBase_APIKeyAnthropic(t *testing.T) {
	acc := &service.Account{
		Platform:    service.PlatformAnthropic,
		Type:        service.AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "tp-1", "base_url": "https://token-plan-cn.xiaomimimo.com/anthropic"},
	}
	scheme, base, err := edgeAuthAndBase(acc, "apikey")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if base != "https://token-plan-cn.xiaomimimo.com/anthropic" {
		t.Fatalf("base = %q", base)
	}
	if scheme.Header != "x-api-key" || scheme.Extra["anthropic-version"] == "" {
		t.Fatalf("anthropic apikey auth wrong: %+v", scheme)
	}
}

func TestEdgeAuthAndBase_APIKeyOpenAI(t *testing.T) {
	acc := &service.Account{
		Platform:    service.PlatformOpenAI,
		Type:        service.AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "sk-x", "base_url": "https://my-openai.example"},
	}
	scheme, base, err := edgeAuthAndBase(acc, "apikey")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if base != "https://my-openai.example" {
		t.Fatalf("base = %q", base)
	}
	// OpenAI => Authorization: Bearer (AuthScheme zero value: no Header).
	if scheme.Header != "" {
		t.Fatalf("openai apikey must use bearer (empty header), got %+v", scheme)
	}
}

func TestEdgeAuthAndBase_OAuthAnthropic(t *testing.T) {
	acc := &service.Account{Platform: service.PlatformAnthropic, Type: service.AccountTypeOAuth}
	scheme, base, err := edgeAuthAndBase(acc, "oauth")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if base != "https://api.anthropic.com" {
		t.Fatalf("oauth anthropic base = %q", base)
	}
	if scheme.Header != "" { // Authorization: Bearer <oauth token>
		t.Fatalf("oauth must use bearer, got %+v", scheme)
	}
}

func TestEdgeAuthAndBase_OAuthNonAnthropicUnsupported(t *testing.T) {
	acc := &service.Account{Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth}
	if _, _, err := edgeAuthAndBase(acc, "oauth"); err == nil {
		t.Fatalf("non-anthropic oauth must be unsupported")
	}
}

func TestEdgeAuthAndBase_UnknownTokenType(t *testing.T) {
	acc := &service.Account{Platform: service.PlatformAnthropic, Type: service.AccountTypeBedrock}
	if _, _, err := edgeAuthAndBase(acc, "bedrock"); err == nil {
		t.Fatalf("unknown token type must be unsupported")
	}
}
