//go:build unit

package ccdirect

import "testing"

func TestJoinUpstreamURL(t *testing.T) {
	cases := []struct {
		name string
		base string
		path string
		want string
	}{
		{"anthropic mimo", "https://token-plan-cn.xiaomimimo.com/anthropic", "/v1/messages", "https://token-plan-cn.xiaomimimo.com/anthropic/v1/messages"},
		{"openai mimo (no double v1)", "https://token-plan-cn.xiaomimimo.com/v1", "/v1/chat/completions", "https://token-plan-cn.xiaomimimo.com/v1/chat/completions"},
		{"openai default", "https://api.openai.com", "/v1/chat/completions", "https://api.openai.com/v1/chat/completions"},
		{"anthropic default", "https://api.anthropic.com", "/v1/messages", "https://api.anthropic.com/v1/messages"},
		{"base trailing slash", "https://x.example/v1/", "/v1/chat/completions", "https://x.example/v1/chat/completions"},
		{"host only (httptest style)", "http://127.0.0.1:8080", "/v1/messages", "http://127.0.0.1:8080/v1/messages"},
		{"already complete", "https://x.example/v1/chat/completions", "/v1/chat/completions", "https://x.example/v1/chat/completions"},
		{"v2 base", "https://x.example/v2", "/v1/chat/completions", "https://x.example/v2/chat/completions"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := JoinUpstreamURL(c.base, c.path); got != c.want {
				t.Fatalf("JoinUpstreamURL(%q,%q) = %q, want %q", c.base, c.path, got, c.want)
			}
		})
	}
}
