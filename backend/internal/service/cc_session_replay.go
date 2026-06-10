package service

// CC 会话回放归一化：把一个 Claude Code 会话（session_hash）的多条
// request_response_logs 行，重建成逐轮 turns，供历史分析 MCP 消费。
//
// 设计见 sub2api docs/design：
//   - request_body 是 Anthropic Messages API 明文 JSON，每条请求已含到该轮为止的
//     完整 messages[]（user/assistant/tool_result 交错）。
//   - 主路径用「并集所有请求的 messages[]」重建（按出现顺序去重），可抵御 Claude Code
//     上下文压缩（早期请求保留压缩前原文）。
//   - 唯一不在任何 request_body 里的是最后一轮 assistant 回复，只在最后一条的
//     response_body(SSE) 里，需解码补上。
//
// 本文件是纯函数，不碰 DB，便于单测。

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
)

const (
	ccToolInputSummaryMax  = 200
	ccToolResultSummaryMax = 160
)

// CCSessionLogRow 是归一化的输入：一条 request_response_logs（已按 created_at 升序）。
type CCSessionLogRow struct {
	Model             string
	RequestBody       []byte
	ResponseBody      []byte
	RequestTruncated  bool
	ResponseTruncated bool
}

// CCReplayBlock 是一个 turn 内的内容块。
type CCReplayBlock struct {
	Type      string `json:"type"`                 // text | image | tool_use | tool_result
	Text      string `json:"text,omitempty"`       // type=text
	ToolName  string `json:"tool_name,omitempty"`  // type=tool_use
	ToolInput string `json:"tool_input,omitempty"` // type=tool_use，关键字段摘要
	Summary   string `json:"summary,omitempty"`    // type=tool_result，结果摘要
	Truncated bool   `json:"truncated,omitempty"`  // 摘要被截断
}

// CCReplayTurn 是一轮（一条 message）。
type CCReplayTurn struct {
	Seq    int             `json:"seq"`
	Role   string          `json:"role"` // user | assistant
	Blocks []CCReplayBlock `json:"blocks"`
}

// CCSessionReplay 是一个会话的完整归一化回放。
type CCSessionReplay struct {
	SessionHash  string         `json:"session_hash"`
	Model        string         `json:"model"`
	RequestCount int            `json:"request_count"`
	AnyTruncated bool           `json:"any_truncated"` // 任一请求/响应被截断，回放可能不全
	Compacted    bool           `json:"compacted"`     // 检测到上下文压缩摘要
	Turns        []CCReplayTurn `json:"turns"`
}

// --- Anthropic 请求体最小解析结构 ---

type ccAnthropicReq struct {
	Model    string             `json:"model"`
	Messages []ccAnthropicMsg   `json:"messages"`
}

type ccAnthropicMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string 或 []block
}

type ccAnthropicBlock struct {
	Type    string          `json:"type"`
	Text    string          `json:"text"`
	Name    string          `json:"name"`    // tool_use
	Input   json.RawMessage `json:"input"`   // tool_use
	Content json.RawMessage `json:"content"` // tool_result：string 或 []block
}

// NormalizeSession 把一个会话的多条记录重建成逐轮回放。rows 须按 created_at 升序。
func NormalizeSession(sessionHash string, rows []CCSessionLogRow) CCSessionReplay {
	out := CCSessionReplay{SessionHash: sessionHash, RequestCount: len(rows)}
	if len(rows) == 0 {
		return out
	}

	seen := make(map[string]struct{})
	var turns []CCReplayTurn

	for _, row := range rows {
		if row.RequestTruncated || row.ResponseTruncated {
			out.AnyTruncated = true
		}
		if out.Model == "" && row.Model != "" {
			out.Model = row.Model
		}
		var req ccAnthropicReq
		if err := json.Unmarshal(row.RequestBody, &req); err != nil {
			continue
		}
		for _, m := range req.Messages {
			blocks := decodeContent(m.Content)
			if isCompactionSummary(m.Role, blocks) {
				out.Compacted = true
			}
			fp := turnFingerprint(m.Role, blocks)
			if _, ok := seen[fp]; ok {
				continue
			}
			seen[fp] = struct{}{}
			turns = append(turns, CCReplayTurn{Role: m.Role, Blocks: blocks})
		}
	}

	// 最后一轮 assistant 回复只在最后一条 response_body(SSE) 里，补上。
	if last := rows[len(rows)-1]; len(last.ResponseBody) > 0 {
		if asst := decodeResponseSSE(last.ResponseBody); len(asst) > 0 {
			fp := turnFingerprint("assistant", asst)
			if _, ok := seen[fp]; !ok {
				turns = append(turns, CCReplayTurn{Role: "assistant", Blocks: asst})
			}
		}
	}

	for i := range turns {
		turns[i].Seq = i + 1
	}
	out.Turns = turns
	return out
}

// decodeContent 把一条 message 的 content（string 或 []block）转成归一化块。
func decodeContent(raw json.RawMessage) []CCReplayBlock {
	if len(raw) == 0 {
		return nil
	}
	// content 可能是纯字符串
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []CCReplayBlock{{Type: "text", Text: s}}
	}
	var blocks []ccAnthropicBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil
	}
	out := make([]CCReplayBlock, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case "text":
			out = append(out, CCReplayBlock{Type: "text", Text: b.Text})
		case "image":
			out = append(out, CCReplayBlock{Type: "image"})
		case "tool_use":
			out = append(out, CCReplayBlock{
				Type:      "tool_use",
				ToolName:  b.Name,
				ToolInput: summarizeToolInput(b.Input),
			})
		case "tool_result":
			sum, trunc := capStr(toolResultText(b.Content), ccToolResultSummaryMax)
			out = append(out, CCReplayBlock{Type: "tool_result", Summary: sum, Truncated: trunc})
		}
	}
	return out
}

// summarizeToolInput 从 tool_use 的 input 取关键字段（文件/命令/模式等）做摘要。
func summarizeToolInput(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		s, _ := capStr(string(raw), ccToolInputSummaryMax)
		return s
	}
	for _, k := range []string{"file_path", "command", "pattern", "path", "query", "prompt", "url"} {
		if v, ok := m[k]; ok {
			if vs, ok := v.(string); ok && vs != "" {
				s, _ := capStr(vs, ccToolInputSummaryMax)
				return s
			}
		}
	}
	b, _ := json.Marshal(m)
	s, _ := capStr(string(b), ccToolInputSummaryMax)
	return s
}

// toolResultText 把 tool_result 的 content（string 或 []block）拍平成文本。
func toolResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []ccAnthropicBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		} else if b.Type == "image" {
			parts = append(parts, "[image]")
		}
	}
	return strings.Join(parts, " ")
}

// isCompactionSummary 粗判一条 message 是否为 Claude Code 的上下文压缩摘要占位。
func isCompactionSummary(role string, blocks []CCReplayBlock) bool {
	if role != "user" || len(blocks) != 1 || blocks[0].Type != "text" {
		return false
	}
	t := blocks[0].Text
	return strings.Contains(t, "This session is being continued from a previous conversation") ||
		strings.Contains(t, "The conversation is summarized below") ||
		strings.Contains(t, "Analysis:") && strings.Contains(t, "Summary:")
}

// turnFingerprint 给一条 message 生成内容指纹，用于跨请求并集去重。
// 文本部分先合并再归一化（剥 <system-reminder> + 解 <session>），这样同一逻辑轮的
// 不同原始形态（一次 <session>P</session>、一次 reminder 注入后 + P）会去重为一条——
// CC 会把提醒重新注入历史，导致同一提问以多形态出现。
func turnFingerprint(role string, blocks []CCReplayBlock) string {
	var text strings.Builder
	var other strings.Builder
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if text.Len() > 0 {
				text.WriteByte('\n')
			}
			text.WriteString(b.Text)
		case "tool_use":
			other.WriteString("\x02tu")
			other.WriteString(b.ToolName)
			other.WriteString("\x03")
			other.WriteString(b.ToolInput)
		case "tool_result":
			other.WriteString("\x02tr")
			other.WriteString(b.Summary)
		case "image":
			other.WriteString("\x02img")
		}
	}
	return role + "\x00" + cleanUserText(text.String()) + "\x01" + other.String()
}

// cleanUserText 把 user 文本归一化为真实输入：剥 <system-reminder> 段 + 解 <session> 包裹。
func cleanUserText(s string) string {
	return unwrapSession(stripSystemReminders(s))
}

// --- SSE 响应解码（只为补最后一轮 assistant：text + tool_use 名 + 流式 input）---

func decodeResponseSSE(body []byte) []CCReplayBlock {
	type sseEvent struct {
		Type         string          `json:"type"`
		ContentBlock json.RawMessage `json:"content_block"`
		Delta        json.RawMessage `json:"delta"`
	}
	type cbStart struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	type cbDelta struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
	}

	var blocks []CCReplayBlock
	curIdx := -1 // 当前块在 blocks 里的下标
	var toolInputBuf bytes.Buffer

	flushToolInput := func() {
		if curIdx >= 0 && curIdx < len(blocks) && blocks[curIdx].Type == "tool_use" && toolInputBuf.Len() > 0 {
			blocks[curIdx].ToolInput = summarizeToolInput(json.RawMessage(toolInputBuf.Bytes()))
		}
		toolInputBuf.Reset()
	}

	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(line[len("data:"):])
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var ev sseEvent
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "content_block_start":
			flushToolInput()
			var cb cbStart
			_ = json.Unmarshal(ev.ContentBlock, &cb)
			switch cb.Type {
			case "text":
				blocks = append(blocks, CCReplayBlock{Type: "text"})
			case "tool_use":
				blocks = append(blocks, CCReplayBlock{Type: "tool_use", ToolName: cb.Name})
			default:
				blocks = append(blocks, CCReplayBlock{Type: cb.Type})
			}
			curIdx = len(blocks) - 1
		case "content_block_delta":
			var d cbDelta
			_ = json.Unmarshal(ev.Delta, &d)
			if curIdx < 0 || curIdx >= len(blocks) {
				continue
			}
			switch d.Type {
			case "text_delta":
				blocks[curIdx].Text += d.Text
			case "input_json_delta":
				toolInputBuf.WriteString(d.PartialJSON)
			}
		case "content_block_stop":
			flushToolInput()
		}
	}
	flushToolInput()

	// 去掉空文本块
	out := blocks[:0]
	for _, b := range blocks {
		if b.Type == "text" && strings.TrimSpace(b.Text) == "" {
			continue
		}
		out = append(out, b)
	}
	return out
}

// capStr 截断到 max 字符（按 rune），返回是否截断。
func capStr(s string, max int) (string, bool) {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= max {
		return s, false
	}
	return string(r[:max]), true
}

// --- prompts 视图：只取客户真实提问，剔除 system-reminder / 自动 recap / 纯 tool_result 轮 ---

// CCPrompt 是一条客户真实提问。
type CCPrompt struct {
	Seq  int    `json:"seq"`
	Text string `json:"text"`
}

// ExtractPrompts 从回放里抽出客户真实提问序列（mode=prompts）。
func ExtractPrompts(replay CCSessionReplay) []CCPrompt {
	var out []CCPrompt
	n := 0
	for _, t := range replay.Turns {
		if t.Role != "user" {
			continue
		}
		var sb strings.Builder
		hasImage := false
		for _, b := range t.Blocks {
			if b.Type == "text" {
				if sb.Len() > 0 {
					sb.WriteByte('\n')
				}
				sb.WriteString(b.Text)
			} else if b.Type == "image" {
				hasImage = true
			}
		}
		// 关键：Claude Code 常把用户真实输入接在 <system-reminder> 之后放进同一条 user
		// 消息（IDE 打开文件提醒 + 真提问同块）。必须剥离提醒段后保留剩余真实文本，
		// 而不是见到提醒就整条丢弃。再解开 <session> 包裹。
		text := cleanUserText(sb.String())
		if text == "" && hasImage {
			text = "[仅图]"
		}
		if text == "" {
			continue // 纯 tool_result / 纯提醒轮
		}
		if isAutoRecap(text) {
			continue
		}
		n++
		out = append(out, CCPrompt{Seq: n, Text: text})
	}
	return out
}

// stripSystemReminders 移除所有 <system-reminder>...</system-reminder> 段，返回剩余 trim 文本。
// 未闭合的提醒（无结束标签）视为延伸到结尾。
func stripSystemReminders(s string) string {
	const open, close = "<system-reminder", "</system-reminder>"
	for {
		i := strings.Index(s, open)
		if i < 0 {
			break
		}
		j := strings.Index(s[i:], close)
		if j < 0 {
			s = s[:i] // 未闭合：删到结尾
			break
		}
		s = s[:i] + s[i+j+len(close):]
	}
	return strings.TrimSpace(s)
}

// unwrapSession 解开 Claude Code 的 <session>...</session> 包裹，取内层真实输入。
func unwrapSession(s string) string {
	t := strings.TrimSpace(s)
	const open, close = "<session>", "</session>"
	if strings.HasPrefix(t, open) {
		if k := strings.LastIndex(t, close); k > len(open) {
			return strings.TrimSpace(t[len(open):k])
		}
	}
	return t
}

// isAutoRecap 判断是否为 Claude Code 自动生成的 "用户离开回来" recap，不是真实提问。
func isAutoRecap(text string) bool {
	return strings.HasPrefix(strings.TrimSpace(text), "The user stepped away")
}
