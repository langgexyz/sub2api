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

// AuthScheme tells the edge how to present the leased credential upstream.
// Zero value means "Authorization: Bearer <token>". Gemini-style key-in-query
// and Anthropic-style x-api-key + version headers are expressible here.
type AuthScheme struct {
	Header     string            `json:"header,omitempty"`      // e.g. "Authorization", "x-api-key"
	Prefix     string            `json:"prefix,omitempty"`      // e.g. "Bearer "
	QueryParam string            `json:"query_param,omitempty"` // e.g. "key" (Gemini)
	Extra      map[string]string `json:"extra,omitempty"`       // e.g. {"anthropic-version":"2023-06-01"}
}

func (a AuthScheme) apply(req *http.Request, token string) {
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

// UsageParser observes response bytes (teed by the edge) and reports usage.
type UsageParser interface {
	Write(p []byte) (int, error)
	Usage() (in, out int)
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

func (p *jsonUsageParser) Usage() (int, int) {
	var obj map[string]any
	if err := json.Unmarshal(p.buf.Bytes(), &obj); err != nil {
		return 0, 0
	}
	in, out, _ := extractUsageAny(obj)
	return in, out
}

// sseUsageParser scans `data:` lines as they stream by, parsing each JSON
// payload for usage and keeping the largest values seen (Anthropic emits input
// in message_start and cumulative output in message_delta; OpenAI/Gemini emit
// usage in the final chunk).
type sseUsageParser struct {
	pending bytes.Buffer
	maxIn   int
	maxOut  int
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
	if in, out, ok := extractUsageAny(obj); ok {
		if in > p.maxIn {
			p.maxIn = in
		}
		if out > p.maxOut {
			p.maxOut = out
		}
	}
}

func (p *sseUsageParser) Usage() (int, int) {
	// Flush any trailing line without a newline.
	if p.pending.Len() > 0 {
		p.scanLine(p.pending.String())
		p.pending.Reset()
	}
	return p.maxIn, p.maxOut
}

// extractUsageAny finds token usage across the shapes used by every supported
// provider: Anthropic (usage.input_tokens/output_tokens, nested under message
// or response), OpenAI chat (usage.prompt_tokens/completion_tokens), OpenAI
// responses (usage.input_tokens/output_tokens), Gemini (usageMetadata.
// promptTokenCount/candidatesTokenCount).
func extractUsageAny(obj map[string]any) (in, out int, found bool) {
	if obj == nil {
		return 0, 0, false
	}
	// Unwrap common envelopes.
	for _, key := range []string{"message", "response"} {
		if nested, ok := obj[key].(map[string]any); ok {
			if i, o, f := extractUsageAny(nested); f {
				in, out, found = max2(in, i), max2(out, o), true
			}
		}
	}
	if u, ok := obj["usage"].(map[string]any); ok {
		i := firstInt(u, "input_tokens", "prompt_tokens")
		o := firstInt(u, "output_tokens", "completion_tokens")
		if i > 0 || o > 0 {
			in, out, found = max2(in, i), max2(out, o), true
		}
	}
	if u, ok := obj["usageMetadata"].(map[string]any); ok {
		i := firstInt(u, "promptTokenCount")
		o := firstInt(u, "candidatesTokenCount")
		if i > 0 || o > 0 {
			in, out, found = max2(in, i), max2(out, o), true
		}
	}
	return in, out, found
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
