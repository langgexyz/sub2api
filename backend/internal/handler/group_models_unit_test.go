//go:build unit

package handler

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// TestDefaultModelIDsForPlatformNotEmpty 锁住聚合列表的兜底来源。
//
// 来由（线上实测）：聚合组的 /v1/models 一度只返回 21 个模型、全是 grok，claude 与
// gpt 一个不见。根因是 GetAvailableModels 在「没有任何账号配了 model_mapping」时返回
// nil（语义=用该平台的默认列表，由调用方补），而 anthropic/openai 组的账号恰恰都没配
// 显式 mapping；grok 唯独非空是因为 resolveModelMapping 会把 grok 的空 mapping 兜成
// xai.DefaultModelMapping。
//
// 所以聚合时必须能拿到各平台的默认列表 —— 这几个平台任一为空，聚合列表就会缺一块。
func TestDefaultModelIDsForPlatformNotEmpty(t *testing.T) {
	for _, platform := range []string{
		service.PlatformAnthropic,
		service.PlatformOpenAI,
		service.PlatformGemini,
	} {
		if got := defaultModelIDsForPlatform(platform); len(got) == 0 {
			t.Errorf("defaultModelIDsForPlatform(%q) is empty; the aggregation list falls back to it when a group's accounts carry no explicit model_mapping", platform)
		}
	}
}

// TestDefaultModelIDsForPlatformAnthropicHasClaude 兜底列表得真的含 claude 模型，
// 否则聚合组里 claude-* 那条路由等于白配。
func TestDefaultModelIDsForPlatformAnthropicHasClaude(t *testing.T) {
	ids := defaultModelIDsForPlatform(service.PlatformAnthropic)

	found := false
	for _, id := range ids {
		if len(id) >= 6 && id[:6] == "claude" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("anthropic defaults must contain claude models, got %v", ids)
	}
}

// TestDefaultModelIDsForPlatformOpenAIHasGPT 同理：gpt-* 那条路由靠它兜底。
func TestDefaultModelIDsForPlatformOpenAIHasGPT(t *testing.T) {
	ids := defaultModelIDsForPlatform(service.PlatformOpenAI)

	found := false
	for _, id := range ids {
		if len(id) >= 3 && id[:3] == "gpt" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("openai defaults must contain gpt models, got %v", ids)
	}
}

func TestDedupeStringsPreservingOrder(t *testing.T) {
	in := []string{"claude-opus", "gpt-5.5", "claude-opus", "grok-4.5", "gpt-5.5"}
	want := []string{"claude-opus", "gpt-5.5", "grok-4.5"}

	got := dedupeStringsPreservingOrder(in)
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order must be preserved (own models before routed ones): got %v, want %v", got, want)
		}
	}
}

func TestDedupeStringsPreservingOrderEmpty(t *testing.T) {
	if got := dedupeStringsPreservingOrder(nil); len(got) != 0 {
		t.Fatalf("nil in -> empty out, got %v", got)
	}
}
