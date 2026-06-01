package ccdirect

import (
	"net/url"
	"strings"
)

// JoinUpstreamURL joins an upstream base URL with the inbound request path,
// honoring the OpenAI-compatible convention where the base may already carry a
// version segment (e.g. ".../v1") that the path also has ("/v1/chat/...").
//
// Rules (mirrors sub2api's buildOpenAIEndpointURL so the edge produces the same
// URL the central gateway would):
//   - if the base already ends with the path (or the path minus a leading /v1),
//     use the base as-is;
//   - if the base ends with a version segment (/v1, /v2, /v1beta, ...), append
//     the path WITHOUT its leading /v1;
//   - otherwise append the full path.
//
// It is uniform across protocols: anthropic (".../anthropic" + "/v1/messages" ->
// ".../anthropic/v1/messages") and openai (".../v1" + "/v1/chat/completions" ->
// ".../v1/chat/completions") both come out right, with no per-protocol branch.
func JoinUpstreamURL(base, path string) string {
	b := strings.TrimRight(strings.TrimSpace(base), "/")
	p := "/" + strings.TrimLeft(strings.TrimSpace(path), "/")
	rel := strings.TrimPrefix(p, "/v1")
	if strings.HasSuffix(b, p) || (rel != p && strings.HasSuffix(b, rel)) {
		return b
	}
	if baseHasVersionSuffix(b) {
		return b + rel
	}
	return b + p
}

// baseHasVersionSuffix reports whether the base URL's last path segment looks
// like an API version (v + digit: v1, v2, v1beta, v1.5, ...).
func baseHasVersionSuffix(base string) bool {
	pathValue := ""
	if u, err := url.Parse(strings.TrimSpace(base)); err == nil && u.Host != "" {
		pathValue = u.Path
	} else if i := strings.Index(base, "/"); i >= 0 {
		pathValue = base[i:]
	}
	pathValue = strings.TrimRight(pathValue, "/")
	if pathValue == "" {
		return false
	}
	seg := pathValue[strings.LastIndex(pathValue, "/")+1:]
	return len(seg) >= 2 && (seg[0] == 'v' || seg[0] == 'V') && seg[1] >= '0' && seg[1] <= '9'
}
