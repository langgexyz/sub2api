package service

import (
	"encoding/json"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
)

const (
	dsmlToolCallOpenTag              = "<\uff5cDSML\uff5ctool_calls>"
	dsmlToolCallProtocolErrorMessage = "Upstream emitted a text-encoded DSML tool call instead of the required tool_calls protocol"
)

// responsesDSMLToolCallGuard 仅暂存可能的起始标签，普通文本仍即时流出，
// 被拆分到多个 chunk 的 DSML 标签也不会到达客户端。
type responsesDSMLToolCallGuard struct {
	enabled    bool
	classified bool
	candidate  string
	pending    []string
}

func newResponsesDSMLToolCallGuard(enabled bool) *responsesDSMLToolCallGuard {
	return &responsesDSMLToolCallGuard{enabled: enabled}
}

func (g *responsesDSMLToolCallGuard) Filter(payload string) ([]string, bool) {
	if !g.enabled || g.classified {
		return []string{payload}, false
	}

	var event struct {
		Type  string `json:"type"`
		Delta string `json:"delta"`
	}
	if err := json.Unmarshal([]byte(payload), &event); err != nil || event.Type != "response.output_text.delta" {
		if len(g.pending) == 0 {
			return []string{payload}, false
		}
		g.classified = true
		return g.flush(payload), false
	}

	if g.candidate == "" && !strings.HasPrefix(dsmlToolCallOpenTag, event.Delta) {
		g.classified = true
		return []string{payload}, false
	}

	g.pending = append(g.pending, payload)
	g.candidate += event.Delta
	if strings.HasPrefix(g.candidate, dsmlToolCallOpenTag) {
		return nil, true
	}
	if strings.HasPrefix(dsmlToolCallOpenTag, g.candidate) {
		return nil, false
	}

	g.classified = true
	return g.flush(), false
}

func (g *responsesDSMLToolCallGuard) flush(extra ...string) []string {
	payloads := append([]string(nil), g.pending...)
	payloads = append(payloads, extra...)
	g.pending = nil
	g.candidate = ""
	return payloads
}

func openAIChatRequestHasTools(body []byte) bool {
	var request struct {
		Tools []json.RawMessage `json:"tools"`
	}
	return json.Unmarshal(body, &request) == nil && len(request.Tools) > 0
}

func responsesContainsDSMLToolCall(response *apicompat.ResponsesResponse) bool {
	if response == nil {
		return false
	}
	for _, output := range response.Output {
		if output.Type != "message" {
			continue
		}
		for _, part := range output.Content {
			if part.Type == "output_text" && strings.Contains(part.Text, dsmlToolCallOpenTag) {
				return true
			}
		}
	}
	return false
}
