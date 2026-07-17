//go:build unit

package service

import (
	"context"
	"testing"
	"time"
)

// 模拟真实 6e2b5cfe 会话的结构：第1条只有首个 user 提问；第2条带上完整历史
// （含第1条的 assistant 回复 + tool_result + system-reminder 噪音 + 第2个 user 提问）；
// 最后一轮 assistant 回复只在第2条的 response_body(SSE) 里。
func sampleRows() []CCSessionLogRow {
	row1Req := `{"model":"claude-sonnet-4-6","messages":[
		{"role":"user","content":"继续解决 v-show 的问题，我把依赖回退到旧版本，但线上还存在"}
	]}`

	row2Req := `{"model":"claude-sonnet-4-6","messages":[
		{"role":"user","content":"继续解决 v-show 的问题，我把依赖回退到旧版本，但线上还存在"},
		{"role":"assistant","content":[
			{"type":"text","text":"先读一下内存记录"},
			{"type":"tool_use","name":"Read","input":{"file_path":"/mem/project_vshow_bug.md"}}
		]},
		{"role":"user","content":[{"type":"tool_result","content":"根因：Fragment 多根节点"}]},
		{"role":"user","content":[{"type":"text","text":"<system-reminder>noise</system-reminder>"}]},
		{"role":"user","content":"当前已经是 Vue 3.3.4 旧版本了，还是有问题"}
	]}`

	row2Resp := "event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"我之前的分析有误"}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"t1","name":"Edit","input":{}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"file_path\":\"/comp/scheduleListByLine.vue\"}"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":1}` + "\n\n"

	return []CCSessionLogRow{
		{Model: "claude-sonnet-4-6", RequestBody: []byte(row1Req)},
		{Model: "claude-sonnet-4-6", RequestBody: []byte(row2Req), ResponseBody: []byte(row2Resp)},
	}
}

func TestNormalizeSession_UnionAndFinalAssistant(t *testing.T) {
	r := NormalizeSession("6e2b5cfe", sampleRows())

	if r.RequestCount != 2 {
		t.Fatalf("RequestCount=%d, want 2", r.RequestCount)
	}
	if r.Model != "claude-sonnet-4-6" {
		t.Errorf("Model=%q", r.Model)
	}
	// 并集去重后应是 6 轮：user / assistant / user(tool_result) / user(reminder) / user / assistant(末轮SSE)
	if len(r.Turns) != 6 {
		t.Fatalf("turns=%d, want 6; turns=%+v", len(r.Turns), r.Turns)
	}
	if r.Turns[0].Role != "user" || r.Turns[0].Blocks[0].Text == "" {
		t.Errorf("turn1 not first user prompt: %+v", r.Turns[0])
	}
	// turn2: assistant 含 text + tool_use Read
	a := r.Turns[1]
	if a.Role != "assistant" || len(a.Blocks) != 2 || a.Blocks[1].Type != "tool_use" || a.Blocks[1].ToolName != "Read" {
		t.Errorf("turn2 assistant tool_use wrong: %+v", a)
	}
	if a.Blocks[1].ToolInput != "/mem/project_vshow_bug.md" {
		t.Errorf("tool_use input summary=%q", a.Blocks[1].ToolInput)
	}
	// turn3: tool_result
	if r.Turns[2].Blocks[0].Type != "tool_result" || r.Turns[2].Blocks[0].Summary == "" {
		t.Errorf("turn3 tool_result wrong: %+v", r.Turns[2])
	}
	// 末轮 assistant 来自 SSE
	last := r.Turns[5]
	if last.Role != "assistant" || last.Blocks[0].Text != "我之前的分析有误" {
		t.Fatalf("last assistant text wrong: %+v", last)
	}
	if len(last.Blocks) != 2 || last.Blocks[1].ToolName != "Edit" || last.Blocks[1].ToolInput != "/comp/scheduleListByLine.vue" {
		t.Errorf("last assistant tool_use(SSE) wrong: %+v", last.Blocks)
	}
	// seq 连续
	for i, tn := range r.Turns {
		if tn.Seq != i+1 {
			t.Errorf("turn[%d].Seq=%d", i, tn.Seq)
		}
	}
}

func TestExtractPrompts_FiltersNoise(t *testing.T) {
	r := NormalizeSession("6e2b5cfe", sampleRows())
	ps := ExtractPrompts(r)
	if len(ps) != 2 {
		t.Fatalf("prompts=%d, want 2 (含两条真实提问，剔除 tool_result/system-reminder); got %+v", len(ps), ps)
	}
	if ps[0].Text != "继续解决 v-show 的问题，我把依赖回退到旧版本，但线上还存在" {
		t.Errorf("prompt1=%q", ps[0].Text)
	}
	if ps[1].Text != "当前已经是 Vue 3.3.4 旧版本了，还是有问题" {
		t.Errorf("prompt2=%q", ps[1].Text)
	}
	if ps[0].Seq != 1 || ps[1].Seq != 2 {
		t.Errorf("prompt seq wrong: %+v", ps)
	}
}

// 真数据 smoke 暴露的回归：真实输入接在 <system-reminder> 之后放进同一条 user 消息。
func TestExtractPrompts_RealPromptAfterReminder(t *testing.T) {
	req := `{"messages":[
		{"role":"user","content":"<system-reminder>The user opened the file schedule/index.vue in the IDE.</system-reminder>\n当前已经是旧版本了，还是有问题"}
	]}`
	r := NormalizeSession("x", []CCSessionLogRow{{RequestBody: []byte(req)}})
	ps := ExtractPrompts(r)
	if len(ps) != 1 {
		t.Fatalf("prompts=%d want 1; got %+v", len(ps), ps)
	}
	if ps[0].Text != "当前已经是旧版本了，还是有问题" {
		t.Errorf("剥离提醒后应保留真提问，得 %q", ps[0].Text)
	}
}

// <session> 包裹应解开。
func TestExtractPrompts_UnwrapSession(t *testing.T) {
	req := `{"messages":[{"role":"user","content":"<session>\n继续解决 v-show 的问题\n</session>"}]}`
	r := NormalizeSession("x", []CCSessionLogRow{{RequestBody: []byte(req)}})
	ps := ExtractPrompts(r)
	if len(ps) != 1 || ps[0].Text != "继续解决 v-show 的问题" {
		t.Errorf("session 解包失败: %+v", ps)
	}
}

// 纯提醒消息应被剔除（剥完为空）。
func TestExtractPrompts_ReminderOnlyDropped(t *testing.T) {
	req := `{"messages":[{"role":"user","content":"<system-reminder>just a reminder</system-reminder>"}]}`
	r := NormalizeSession("x", []CCSessionLogRow{{RequestBody: []byte(req)}})
	if ps := ExtractPrompts(r); len(ps) != 0 {
		t.Errorf("纯提醒应被剔除，得 %+v", ps)
	}
}

// 真数据 smoke 暴露的回归：同一逻辑提问以两种原始形态出现（一次 <session> 包裹，
// 一次 reminder 注入），应去重为一条、full replay 不出现重复 turn。
func TestNormalizeSession_DedupsReinjectedTurn(t *testing.T) {
	r1 := `{"messages":[{"role":"user","content":"<session>\n继续解决 v-show 的问题\n</session>"}]}`
	r2 := `{"messages":[
		{"role":"user","content":"<system-reminder>opened a file</system-reminder>\n继续解决 v-show 的问题"},
		{"role":"assistant","content":[{"type":"text","text":"好的"}]}
	]}`
	r := NormalizeSession("x", []CCSessionLogRow{
		{RequestBody: []byte(r1)},
		{RequestBody: []byte(r2)},
	})
	// 两种形态的同一 user 轮应去重 → user 轮只 1 条 + assistant 1 条
	userTurns := 0
	for _, tn := range r.Turns {
		if tn.Role == "user" {
			userTurns++
		}
	}
	if userTurns != 1 {
		t.Errorf("重复注入轮未去重，user 轮=%d want 1; turns=%+v", userTurns, r.Turns)
	}
	if ps := ExtractPrompts(r); len(ps) != 1 {
		t.Errorf("prompts=%d want 1（重复形态去重）", len(ps))
	}
}

func TestNormalizeSession_TruncationFlag(t *testing.T) {
	rows := sampleRows()
	rows[0].RequestTruncated = true
	r := NormalizeSession("x", rows)
	if !r.AnyTruncated {
		t.Error("AnyTruncated should be true when a row is truncated")
	}
}

func TestNormalizeSession_ImageOnlyPrompt(t *testing.T) {
	req := `{"messages":[{"role":"user","content":[{"type":"image"}]}]}`
	r := NormalizeSession("x", []CCSessionLogRow{{RequestBody: []byte(req)}})
	ps := ExtractPrompts(r)
	if len(ps) != 1 || ps[0].Text != "[仅图]" {
		t.Errorf("image-only prompt = %+v, want [仅图]", ps)
	}
}

func TestNormalizeSession_DetectsCompaction(t *testing.T) {
	req := `{"messages":[{"role":"user","content":"This session is being continued from a previous conversation that ran out of context"}]}`
	r := NormalizeSession("x", []CCSessionLogRow{{RequestBody: []byte(req)}})
	if !r.Compacted {
		t.Error("Compacted should be true on continuation summary")
	}
}

func TestNormalizeSession_StringAndBlockContent(t *testing.T) {
	req := `{"messages":[
		{"role":"user","content":"纯字符串内容"},
		{"role":"assistant","content":[{"type":"text","text":"块内容"}]}
	]}`
	r := NormalizeSession("x", []CCSessionLogRow{{RequestBody: []byte(req)}})
	if len(r.Turns) != 2 {
		t.Fatalf("turns=%d want 2", len(r.Turns))
	}
	if r.Turns[0].Blocks[0].Text != "纯字符串内容" || r.Turns[1].Blocks[0].Text != "块内容" {
		t.Errorf("content decode wrong: %+v", r.Turns)
	}
}

func TestNormalizeSession_Empty(t *testing.T) {
	r := NormalizeSession("x", nil)
	if len(r.Turns) != 0 || r.RequestCount != 0 {
		t.Errorf("empty session should yield no turns: %+v", r)
	}
}

// stubCCRepo 捕获 SearchPrompts 收到的 query，供断言 service 层默认值。
type stubCCRepo struct {
	gotSearch CCPromptSearchQuery
}

func (s *stubCCRepo) ListSessions(_ context.Context, _ CCSessionListQuery) ([]CCSessionSummary, error) {
	return nil, nil
}

func (s *stubCCRepo) GetSessionRows(_ context.Context, _ string) ([]CCSessionLogRow, error) {
	return nil, nil
}

func (s *stubCCRepo) SearchPrompts(_ context.Context, q CCPromptSearchQuery) ([]CCPromptHit, error) {
	s.gotSearch = q
	return nil, nil
}

func TestSearchPromptsDefaultsFromWindow(t *testing.T) {
	repo := &stubCCRepo{}
	svc := NewCCSessionReplayService(repo)

	// 不传 from：默认限定最近一窗，防 GB 级原文表无界扫描超时。
	if _, err := svc.SearchPrompts(context.Background(), CCPromptSearchQuery{Query: "x"}); err != nil {
		t.Fatalf("SearchPrompts: %v", err)
	}
	if repo.gotSearch.From == nil {
		t.Fatal("expected default From window, got nil")
	}
	wantFrom := time.Now().Add(-ccSearchDefaultWindow)
	if d := repo.gotSearch.From.Sub(wantFrom); d < -time.Minute || d > time.Minute {
		t.Fatalf("default From = %v, want ~%v", repo.gotSearch.From, wantFrom)
	}

	// 显式传 from：原样透传，允许查更早历史。
	explicit := time.Date(2026, 6, 16, 0, 0, 0, 0, time.UTC)
	if _, err := svc.SearchPrompts(context.Background(), CCPromptSearchQuery{Query: "x", From: &explicit}); err != nil {
		t.Fatalf("SearchPrompts explicit: %v", err)
	}
	if repo.gotSearch.From == nil || !repo.gotSearch.From.Equal(explicit) {
		t.Fatalf("explicit From = %v, want %v", repo.gotSearch.From, explicit)
	}
}
