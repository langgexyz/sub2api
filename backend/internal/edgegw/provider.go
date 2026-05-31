package edgegw

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
)

// Provider captures the per-platform protocol differences the edge must honor
// to support every upstream: where the model name lives (request body vs URL
// path) and how to read token usage out of the response. Auth is data-driven
// via AuthScheme (carried per account), so even within a platform an OAuth
// account and an API-key account can present credentials differently.
type Provider interface {
	Name() string
	// PrepareRequest rewrites the inbound path and body so the upstream sees the
	// mapped model. Returns the upstream path and (possibly rewritten) body.
	PrepareRequest(inboundPath string, body []byte, mappedModel string) (path string, newBody []byte)
	// NewUsageParser returns a sink the edge tees the response through to
	// extract input/output token usage (works for stream and non-stream).
	NewUsageParser(stream bool) UsageParser
}

// AuthScheme is defined in the shared contract package (data only). The
// HTTP-applying behavior stays here (edge side) as a free function since the
// contract package must not depend on net/http.

// applyAuthScheme presents token on req per the scheme. Zero value means
// "Authorization: Bearer <token>".
func applyAuthScheme(a AuthScheme, req *http.Request, token string) {
	if a.Header == "" && a.QueryParam == "" {
		req.Header.Set("Authorization", "Bearer "+token)
	} else {
		if a.QueryParam != "" {
			q := req.URL.Query()
			q.Set(a.QueryParam, token)
			req.URL.RawQuery = q.Encode()
		}
		if a.Header != "" {
			req.Header.Set(a.Header, a.Prefix+token)
		}
	}
	for k, v := range a.Extra {
		req.Header.Set(k, v)
	}
}

// providerRegistry maps a platform name to its Provider. Unknown platforms fall
// back to bodyModelProvider (top-level "model" rewrite), which covers any
// OpenAI-compatible upstream.
var providerRegistry = map[string]Provider{
	"anthropic":   bodyModelProvider{name: "anthropic"},
	"openai":      bodyModelProvider{name: "openai"},
	"gemini":      pathModelProvider{name: "gemini"},
	"antigravity": antigravityProvider{},
}

// ProviderFor resolves a Provider by platform name, never returning nil.
func ProviderFor(platform string) Provider {
	if p, ok := providerRegistry[strings.ToLower(platform)]; ok {
		return p
	}
	return bodyModelProvider{name: "default"}
}

// bodyModelProvider rewrites the top-level "model" field in the JSON body
// (Anthropic /v1/messages, OpenAI /v1/chat/completions and /v1/responses).
type bodyModelProvider struct{ name string }

func (p bodyModelProvider) Name() string { return p.name }

func (p bodyModelProvider) PrepareRequest(inboundPath string, body []byte, mappedModel string) (string, []byte) {
	return inboundPath, rewriteModel(body, mappedModel)
}

func (p bodyModelProvider) NewUsageParser(stream bool) UsageParser { return newUsageParser(stream) }

// pathModelProvider rewrites the model segment in the URL path
// (Gemini /v1beta/models/<model>:generateContent); the body has no model.
type pathModelProvider struct{ name string }

func (p pathModelProvider) Name() string { return p.name }

func (p pathModelProvider) PrepareRequest(inboundPath string, body []byte, mappedModel string) (string, []byte) {
	return rewriteGeminiPath(inboundPath, mappedModel), body
}

func (p pathModelProvider) NewUsageParser(stream bool) UsageParser { return newUsageParser(stream) }

// antigravityProvider proxies both the Anthropic-shaped (/messages) and
// Gemini-shaped (/v1beta) surfaces, so it picks the rewrite strategy by path.
type antigravityProvider struct{}

func (antigravityProvider) Name() string { return "antigravity" }

func (antigravityProvider) PrepareRequest(inboundPath string, body []byte, mappedModel string) (string, []byte) {
	if strings.Contains(inboundPath, "/v1beta/") || strings.Contains(inboundPath, "models/") {
		return rewriteGeminiPath(inboundPath, mappedModel), body
	}
	return inboundPath, rewriteModel(body, mappedModel)
}

func (antigravityProvider) NewUsageParser(stream bool) UsageParser { return newUsageParser(stream) }

// rewriteGeminiPath replaces the model in ".../models/<model>[:action]".
func rewriteGeminiPath(path, mappedModel string) string {
	if mappedModel == "" {
		return path
	}
	marker := "models/"
	i := strings.Index(path, marker)
	if i < 0 {
		return path
	}
	head := path[:i+len(marker)]
	rest := path[i+len(marker):]
	if colon := strings.IndexByte(rest, ':'); colon >= 0 {
		return head + mappedModel + rest[colon:]
	}
	if slash := strings.IndexByte(rest, '/'); slash >= 0 {
		return head + mappedModel + rest[slash:]
	}
	return head + mappedModel
}

// --- usage parsing ---

// Usage is the token usage the edge extracts from an upstream response and
// reports back to the center for billing (input/output + cache tokens).
type Usage struct {
	Input         int
	Output        int
	CacheRead     int
	CacheCreation int
}

func (u Usage) merge(o Usage) Usage {
	return Usage{
		Input:         max2(u.Input, o.Input),
		Output:        max2(u.Output, o.Output),
		CacheRead:     max2(u.CacheRead, o.CacheRead),
		CacheCreation: max2(u.CacheCreation, o.CacheCreation),
	}
}

func (u Usage) any() bool {
	return u.Input > 0 || u.Output > 0 || u.CacheRead > 0 || u.CacheCreation > 0
}

// UsageParser observes response bytes (teed by the edge) and reports usage.
type UsageParser interface {
	Write(p []byte) (int, error)
	Usage() Usage
}

// newUsageParser returns a streaming SSE scanner or a buffering JSON parser.
func newUsageParser(stream bool) UsageParser {
	if stream {
		return &sseUsageParser{}
	}
	return &jsonUsageParser{}
}

// jsonUsageParser buffers the (non-stream) body and extracts usage on demand.
type jsonUsageParser struct{ buf bytes.Buffer }

func (p *jsonUsageParser) Write(b []byte) (int, error) { return p.buf.Write(b) }

func (p *jsonUsageParser) Usage() Usage {
	var obj map[string]any
	if err := json.Unmarshal(p.buf.Bytes(), &obj); err != nil {
		return Usage{}
	}
	u, _ := extractUsageAny(obj)
	return u
}

// sseUsageParser scans `data:` lines as they stream by, parsing each JSON
// payload for usage and keeping the largest values seen (Anthropic emits input
// in message_start and cumulative output in message_delta; OpenAI/Gemini emit
// usage in the final chunk).
type sseUsageParser struct {
	pending bytes.Buffer
	max     Usage
}

func (p *sseUsageParser) Write(b []byte) (int, error) {
	n := len(b)
	_, _ = p.pending.Write(b)
	for {
		data := p.pending.Bytes()
		idx := bytes.IndexByte(data, '\n')
		if idx < 0 {
			break
		}
		line := string(data[:idx])
		p.pending.Next(idx + 1)
		p.scanLine(line)
	}
	return n, nil
}

func (p *sseUsageParser) scanLine(line string) {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "data:")
	line = strings.TrimSpace(line)
	if line == "" || line == "[DONE]" || !strings.HasPrefix(line, "{") {
		return
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(line), &obj); err != nil {
		return
	}
	if u, ok := extractUsageAny(obj); ok {
		p.max = p.max.merge(u)
	}
}

func (p *sseUsageParser) Usage() Usage {
	// Flush any trailing line without a newline.
	if p.pending.Len() > 0 {
		p.scanLine(p.pending.String())
		p.pending.Reset()
	}
	return p.max
}

// extractUsageAny finds token usage across the shapes used by every supported
// provider: Anthropic (usage.input_tokens/output_tokens, nested under message
// or response), OpenAI chat (usage.prompt_tokens/completion_tokens), OpenAI
// responses (usage.input_tokens/output_tokens), Gemini (usageMetadata.
// promptTokenCount/candidatesTokenCount).
// NOTE: This is a faithful, edge-local MIRROR of sub2api's per-protocol usage
// parsing (which lives case-by-case inside the gateway services, e.g.
// GatewayService.parseSSEUsagePassthrough and the openai/antigravity equivalents
// -- there is no standalone protocol usage-parser to import). The edge is a
// separate process and intentionally does not depend on the sub2api core, so it
// keeps its own parser. Keep these field conventions in sync with sub2api's:
// Anthropic input_tokens/output_tokens/cache_read_input_tokens/
// cache_creation_input_tokens; OpenAI prompt_tokens/completion_tokens +
// {prompt,input}_tokens_details.cached_tokens; Gemini usageMetadata.*.
func extractUsageAny(obj map[string]any) (Usage, bool) {
	if obj == nil {
		return Usage{}, false
	}
	var u Usage
	found := false
	// Unwrap common envelopes.
	for _, key := range []string{"message", "response"} {
		if nested, ok := obj[key].(map[string]any); ok {
			if nu, f := extractUsageAny(nested); f {
				u, found = u.merge(nu), true
			}
		}
	}
	if usage, ok := obj["usage"].(map[string]any); ok {
		got := Usage{
			Input:         firstInt(usage, "input_tokens", "prompt_tokens"),
			Output:        firstInt(usage, "output_tokens", "completion_tokens"),
			CacheRead:     firstInt(usage, "cache_read_input_tokens"),
			CacheCreation: firstInt(usage, "cache_creation_input_tokens"),
		}
		// OpenAI nests cached tokens under {prompt,input}_tokens_details.cached_tokens.
		if d, ok := usage["prompt_tokens_details"].(map[string]any); ok {
			got.CacheRead = max2(got.CacheRead, firstInt(d, "cached_tokens"))
		}
		if d, ok := usage["input_tokens_details"].(map[string]any); ok {
			got.CacheRead = max2(got.CacheRead, firstInt(d, "cached_tokens"))
		}
		if got.any() {
			u, found = u.merge(got), true
		}
	}
	if m, ok := obj["usageMetadata"].(map[string]any); ok {
		got := Usage{
			Input:     firstInt(m, "promptTokenCount"),
			Output:    firstInt(m, "candidatesTokenCount"),
			CacheRead: firstInt(m, "cachedContentTokenCount"),
		}
		if got.any() {
			u, found = u.merge(got), true
		}
	}
	return u, found
}

func firstInt(m map[string]any, keys ...string) int {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if f, ok := v.(float64); ok {
				return int(f)
			}
		}
	}
	return 0
}

func max2(a, b int) int {
	if a > b {
		return a
	}
	return b
}
