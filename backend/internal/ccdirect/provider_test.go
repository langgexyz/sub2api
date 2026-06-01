//go:build unit

package ccdirect

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/ccgw/contract"
)

func TestProviderFor_Resolution(t *testing.T) {
	cases := map[string]string{
		"anthropic": "anthropic", "openai": "openai", "gemini": "gemini",
		"antigravity": "antigravity", "ANTHROPIC": "anthropic", "unknown": "default",
	}
	for in, want := range cases {
		if got := ProviderFor(in).Name(); got != want {
			t.Errorf("ProviderFor(%q).Name() = %q, want %q", in, got, want)
		}
	}
}

func TestBodyModelProvider_RewritesBodyKeepsPath(t *testing.T) {
	p := ProviderFor("anthropic")
	body := []byte(`{"model":"claude-x","messages":[]}`)
	path, out := p.PrepareRequest("/v1/messages", body, "upstream-y")
	if path != "/v1/messages" {
		t.Fatalf("path should be unchanged, got %q", path)
	}
	var m struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(out, &m)
	if m.Model != "upstream-y" {
		t.Fatalf("body model not rewritten: %q", m.Model)
	}
}

func TestGeminiProvider_RewritesPathKeepsBody(t *testing.T) {
	p := ProviderFor("gemini")
	body := []byte(`{"contents":[]}`)
	path, out := p.PrepareRequest("/v1beta/models/gemini-pro:generateContent", body, "gemini-1.5-flash")
	if path != "/v1beta/models/gemini-1.5-flash:generateContent" {
		t.Fatalf("gemini path model not rewritten: %q", path)
	}
	if string(out) != string(body) {
		t.Fatalf("gemini body should be unchanged")
	}
}

func TestAntigravity_PicksStrategyByPath(t *testing.T) {
	p := ProviderFor("antigravity")
	// gemini-shaped path -> path rewrite
	path, _ := p.PrepareRequest("/antigravity/v1beta/models/m:streamGenerateContent", []byte(`{}`), "mapped")
	if !strings.Contains(path, "models/mapped:streamGenerateContent") {
		t.Fatalf("antigravity gemini path not rewritten: %q", path)
	}
	// anthropic-shaped path -> body rewrite
	_, out := p.PrepareRequest("/antigravity/v1/messages", []byte(`{"model":"a"}`), "mapped")
	if !strings.Contains(string(out), `"mapped"`) {
		t.Fatalf("antigravity anthropic body not rewritten: %s", out)
	}
}

func TestAuthScheme_Apply(t *testing.T) {
	mkReq := func() *http.Request {
		r, _ := http.NewRequest(http.MethodPost, "http://up/v1/x", nil)
		return r
	}

	// default -> Authorization: Bearer
	r := mkReq()
	applyAuthScheme(contract.AuthScheme{}, r, "tok")
	if r.Header.Get("Authorization") != "Bearer tok" {
		t.Fatalf("default auth: %q", r.Header.Get("Authorization"))
	}

	// anthropic-style x-api-key + version
	r = mkReq()
	applyAuthScheme(contract.AuthScheme{Header: "x-api-key", Extra: map[string]string{"anthropic-version": "2023-06-01"}}, r, "sk-1")
	if r.Header.Get("x-api-key") != "sk-1" || r.Header.Get("anthropic-version") != "2023-06-01" {
		t.Fatalf("x-api-key scheme wrong: %v", r.Header)
	}
	if r.Header.Get("Authorization") != "" {
		t.Fatalf("x-api-key scheme must not set Authorization")
	}

	// gemini-style key query param
	r = mkReq()
	applyAuthScheme(contract.AuthScheme{QueryParam: "key"}, r, "g-1")
	if r.URL.Query().Get("key") != "g-1" {
		t.Fatalf("gemini query auth: %q", r.URL.RawQuery)
	}
}

func TestUsageParser_AnthropicNonStream(t *testing.T) {
	p := newUsageParser(false)
	_, _ = p.Write([]byte(`{"type":"message","usage":{"input_tokens":50,"output_tokens":120,"cache_read_input_tokens":192,"cache_creation_input_tokens":7}}`))
	u := p.Usage()
	if u.Input != 50 || u.Output != 120 || u.CacheRead != 192 || u.CacheCreation != 7 {
		t.Fatalf("anthropic json usage: %+v", u)
	}
}

func TestUsageParser_AnthropicSSE(t *testing.T) {
	p := newUsageParser(true)
	stream := "event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":42,"output_tokens":1,"cache_read_input_tokens":192}}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","usage":{"output_tokens":99}}` + "\n\n" +
		"data: [DONE]\n\n"
	_, _ = p.Write([]byte(stream))
	u := p.Usage()
	if u.Input != 42 || u.Output != 99 || u.CacheRead != 192 {
		t.Fatalf("anthropic sse usage: %+v", u)
	}
}

func TestUsageParser_OpenAIChatNonStream(t *testing.T) {
	p := newUsageParser(false)
	_, _ = p.Write([]byte(`{"usage":{"prompt_tokens":33,"completion_tokens":77,"prompt_tokens_details":{"cached_tokens":16}}}`))
	u := p.Usage()
	if u.Input != 33 || u.Output != 77 || u.CacheRead != 16 {
		t.Fatalf("openai chat usage: %+v", u)
	}
}

func TestUsageParser_OpenAIResponsesSSE(t *testing.T) {
	p := newUsageParser(true)
	stream := `data: {"type":"response.completed","response":{"usage":{"input_tokens":10,"output_tokens":20,"input_tokens_details":{"cached_tokens":4}}}}` + "\n\n"
	_, _ = p.Write([]byte(stream))
	u := p.Usage()
	if u.Input != 10 || u.Output != 20 || u.CacheRead != 4 {
		t.Fatalf("openai responses usage: %+v", u)
	}
}

func TestUsageParser_GeminiNonStreamAndSSE(t *testing.T) {
	p := newUsageParser(false)
	_, _ = p.Write([]byte(`{"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":15,"cachedContentTokenCount":3}}`))
	if u := p.Usage(); u.Input != 5 || u.Output != 15 || u.CacheRead != 3 {
		t.Fatalf("gemini json usage: %+v", u)
	}

	ps := newUsageParser(true)
	_, _ = ps.Write([]byte(`data: {"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":3}}` + "\n\n"))
	if u := ps.Usage(); u.Input != 7 || u.Output != 3 {
		t.Fatalf("gemini sse usage: %+v", u)
	}
}

func TestUsageParser_SSEChunkedWrites(t *testing.T) {
	// Usage must be found even when bytes arrive split across Write calls.
	p := newUsageParser(true)
	full := `data: {"usage":{"input_tokens":11,"output_tokens":22}}` + "\n\n"
	for i := 0; i < len(full); i += 7 {
		end := i + 7
		if end > len(full) {
			end = len(full)
		}
		_, _ = p.Write([]byte(full[i:end]))
	}
	if u := p.Usage(); u.Input != 11 || u.Output != 22 {
		t.Fatalf("chunked sse usage: %+v", u)
	}
}
