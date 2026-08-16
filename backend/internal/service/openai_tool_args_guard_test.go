package service

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 现场真实退化产物（IK8VCE，2026-08-14）。取自 OpenCode 会话记录
// part prt_fff10d05e001zHD62X4Z1dcBDq —— 模型把它填进了 bash 工具的 workdir 参数。
// 仅家目录前缀做了中性化（真实用户名不进开放仓），退化特征位于其后的协议残片部分，
// 不受影响。
// 注意它【不含控制字符】、长度未超 PATH_MAX、且因串里恰好有 `/`，切出的父目录真实存在
// —— 只靠「字符合法 + 路径存在」判不出来，必须靠协议残片这个信号。
const realDegeneratedWorkdir = `/srv/proj/Documents/Git...】【。】【”】【json error? tool call must proper. ` +
	`Need execute. +#+#+#+#+#+ assistant to=functions.bash av不卡免费播放  福利彩票天天彩 ` +
	`北京赛车微信? Let's do.numerusform to=functions.bash კომენტary _日本一级特黄大片{`

func TestToolArgsDegeneration_RealFieldSample(t *testing.T) {
	t.Parallel()

	args := `{"command":"sleep 600","workdir":` + jsonQuote(realDegeneratedWorkdir) + `}`
	require.NotEmpty(t, toolArgsDegenerationReason(args),
		"现场真实退化产物必须被判退化（回归锚点）")
}

func TestToolArgsDegeneration_Positive(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		args string
	}{
		{"协议残片 assistant to=functions",
			`{"workdir":"/srv/proj/x assistant to=functions.bash 天天彩票"}`},
		{"协议残片 json error?",
			`{"path":"/tmp/a json error? tool call must proper"}`},
		{"DSML 风格标签混入参数",
			`{"path":"/tmp/<|DSML|tool_calls>"}`},
		{"复读型 ./ 重复",
			`{"workdir":"/srv/proj/Documents/Git/` + strings.Repeat("./", 300) + `"}`},
		{"复读型 ../ 重复",
			`{"workdir":"/srv/proj/` + strings.Repeat("../", 40) + `"}`},
		{"单字段超长",
			`{"workdir":"/` + strings.Repeat("a", maxToolArgFieldLen+1) + `"}`},
		{"字段含 NUL",
			`{"workdir":"/tmp/a\u0000b"}`},
		{"字段含退格等 C0 控制字符",
			`{"workdir":"/tmp/a\u0008b"}`},
		{"字段含零宽格式控制符",
			`{"workdir":"/tmp/a\u200bb"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.NotEmpty(t, toolArgsDegenerationReason(tc.args), "应判退化")
		})
	}
}

// 负向用例是本守卫的核心约束：判据【绝不能】用词表或字符集，否则会误伤正常业务。
//
// 实测依据（同模型正常问答，2026-08-14）：
//   - 问「彩票销售系统的核心模块」-> 正常回答含「彩票」
//   - 问「Qt 的 numerusform 是什么」-> 正常回答含 numerusform
//   - 韩文提问 -> 正常回答就是韩文
//
// 这些内容同样可能合法地出现在工具参数里（用户就在做彩票系统 / 处理韩文目录）。
func TestToolArgsDegeneration_NegativeMustNotTrip(t *testing.T) {
	t.Parallel()

	// 真实的长文件内容：各行互不相同，用于验证长度不再充当主判据。
	longLines := make([]string, 0, 500)
	for i := 0; i < 500; i++ {
		longLines = append(longLines, "line "+strconv.Itoa(i)+": handle request and write response")
	}
	longContent := strings.Join(longLines, "\n") + "\n"

	cases := []struct {
		name string
		args string
	}{
		{"常规路径", `{"command":"ls -la","workdir":"/srv/proj/Documents/GitHub/sub2api"}`},
		{"相对路径", `{"command":"pwd","workdir":"."}`},
		{"中文目录名", `{"workdir":"/srv/proj/文档/项目目录"}`},
		{"含空格目录名", `{"workdir":"/srv/proj/My Documents/a b"}`},
		{"少量 .. 是合法相对路径", `{"workdir":"/srv/proj/a/../b/../c"}`},

		// --- 以下五条锁死「不得按词表/字符集判定」---
		{"业务词：用户在做彩票系统", `{"path":"/srv/lottery/彩票销售系统/config.yaml"}`},
		{"技术词：Qt 国际化文件", `{"path":"/srv/app/i18n/numerusform.ts"}`},
		{"韩文目录名", `{"workdir":"/srv/proj/문서/프로젝트"}`},
		{"日文目录名", `{"workdir":"/srv/proj/ドキュメント/プロジェクト"}`},
		{"俄文目录名", `{"workdir":"/srv/proj/Документы/проект"}`},

		// --- 以下锁死「编码类工具的多行参数不得判退化」（IK8Z7J 回归）---
		//
		// 这些是写码会话里最高频的工具调用形态。此前判据把 \n / \r / \t 当退化
		// 信号，导致它们在生产上被整条流终止，并错误呈现为 upstream_protocol_error。
		{"Write 写入多行文件内容",
			`{"filePath":"/srv/app/main.go","content":"package main\n\nfunc main() {\n\tprintln(1)\n}\n"}`},
		{"Edit 替换多行代码块",
			`{"filePath":"/srv/app/a.ts","oldString":"function a() {\n  return 1\n}","newString":"function a() {\n  return 2\n}"}`},
		{"Bash heredoc 多行脚本",
			`{"command":"cat <<'EOF' > /tmp/a.txt\nline1\nline2\nEOF"}`},
		{"提交信息含规范要求的多行正文",
			`{"command":"git commit -m \"fix: 修复解析\n\nWhy: 上游返回空字段\nTest: go test ./...\""}`},
		{"Makefile 制表符缩进",
			`{"filePath":"/srv/app/Makefile","content":"build:\n\tgo build ./...\n"}`},
		{"深缩进 YAML（空白重复不得判退化）",
			`{"filePath":"/srv/k8s/deploy.yaml","content":"spec:\n` + strings.Repeat(" ", 64) + `- name: api\n"}`},
		{"Markdown 分隔线（单字符重复不得判退化）",
			`{"filePath":"/srv/README.md","content":"# T\n\n` + strings.Repeat("-", 80) + `\n"}`},
		{"长文件内容（超过旧的 4096 单字段上限）",
			`{"filePath":"/srv/app/big.txt","content":` + jsonQuote(longContent) + `}`},

		// --- fail-open：不完整/非字符串不判退化 ---
		{"流式累积中的不完整 JSON", `{"command":"sleep 600","workdir":"/srv/proj/Doc`},
		{"空 arguments", ``},
		{"数字与布尔字段", `{"timeout":600,"force":true}`},
		{"嵌套对象", `{"opts":{"depth":3,"paths":["/a","/b"]}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Empty(t, toolArgsDegenerationReason(tc.args),
				"正常内容不得判退化（误伤即回归）")
		})
	}
}

// 守卫必须【累积】分片后再判：arguments 按 chunk 增量下发，单块看不出问题。
func TestToolArgsGuard_AccumulatesAcrossChunks(t *testing.T) {
	t.Parallel()

	g := newToolArgsDegenerationGuard(true)
	// 逐块喂入，任一单块都不足以判定，累积后才命中协议残片
	chunks := []string{`{"workdir":"/srv/proj/x `, `assistant `, `to=functions.`, `bash"}`}
	var tripped string
	for _, c := range chunks {
		if reason := g.Observe(0, c); reason != "" {
			tripped = reason
			break
		}
	}
	require.NotEmpty(t, tripped, "跨块的协议残片必须被累积后检出")
	require.True(t, g.Tripped())
}

func TestToolArgsGuard_DisabledIsNoop(t *testing.T) {
	t.Parallel()

	g := newToolArgsDegenerationGuard(false)
	require.Empty(t, g.Observe(0, `{"workdir":"assistant to=functions.bash"}`),
		"未启用时不得拦截（无 tools 的请求走这条路）")
	require.False(t, g.Tripped())
}

func TestToolArgsGuard_MultipleToolCallsTrackedSeparately(t *testing.T) {
	t.Parallel()

	g := newToolArgsDegenerationGuard(true)
	require.Empty(t, g.Observe(0, `{"path":"/a/b"}`))
	require.Empty(t, g.Observe(1, `{"path":"/c/d"}`))
	// 两路各自累积，互不串扰
	require.Empty(t, g.Observe(0, ``))
	require.False(t, g.Tripped())
}

func TestHasRunawayRepeat(t *testing.T) {
	t.Parallel()

	require.True(t, hasRunawayRepeat(strings.Repeat("./", 40)))
	require.True(t, hasRunawayRepeat(strings.Repeat("../", 30)))
	require.True(t, hasRunawayRepeat(strings.Repeat("ab", 50)))
	require.False(t, hasRunawayRepeat("/srv/proj/a/../b/../c"), "少量重复合法")
	require.False(t, hasRunawayRepeat("/srv/proj/Documents/GitHub/sub2api"))
	require.False(t, hasRunawayRepeat(""))
}

// --- 接线层：从 SSE payload 取分片 ---

func TestObserveToolArgsFrame_OnlyWatchesToolArgEvents(t *testing.T) {
	t.Parallel()

	// 核心约束：本守卫【绝不】碰普通文本事件。
	// 同样的字符串出现在 output_text.delta 里必须放行 —— 用户可能就在讨论这些内容
	// （实测：问「彩票系统」「Qt numerusform」时正常回答就含这些词）。
	textPayloads := []string{
		`{"type":"response.output_text.delta","delta":"assistant to=functions.bash 天天彩票"}`,
		`{"type":"response.output_text.delta","delta":"numerusform 用于 Qt 复数翻译"}`,
		`{"type":"response.output_text.delta","delta":"먼저 현재 컨텍스트를 확인하겠습니다"}`,
		`{"type":"response.completed","response":{"id":"resp_1"}}`,
		`not-json-at-all`,
	}
	for _, p := range textPayloads {
		g := newToolArgsDegenerationGuard(true)
		require.Empty(t, observeToolArgsFrame(g, p),
			"非工具参数事件不得触发守卫：%s", p)
		require.False(t, g.Tripped())
	}
}

func TestObserveToolArgsFrame_DetectsInArgumentsDelta(t *testing.T) {
	t.Parallel()

	g := newToolArgsDegenerationGuard(true)
	// 同样的内容出现在【工具参数】里就该拦 —— 语境不同：用户会谈论博彩，
	// 但不会把 assistant to=functions.bash 塞进一个路径参数。
	payload := `{"type":"response.function_call_arguments.delta","output_index":0,` +
		`"delta":"{\"workdir\":\"/x assistant to=functions.bash\"}"}`
	require.NotEmpty(t, observeToolArgsFrame(g, payload))
	require.True(t, g.Tripped())
}

func TestObserveToolArgsFrame_DoneUsesReplaceNotAppend(t *testing.T) {
	t.Parallel()

	// 回归：done 带全量 arguments，若与已累积的 delta 追加叠加，正常参数会被拼成
	// 超长而误判。这里用一个接近上限的合法值验证「替换而非追加」。
	//
	// 注意长串不能用 strings.Repeat 造 —— 那本身就命中「失控重复」判据，
	// 会把本用例变成验错东西（初版就踩了这个坑）。用递增路径段造非重复长串。
	var sb strings.Builder
	for i := 0; sb.Len() < maxToolArgFieldLen-200; i++ {
		sb.WriteString("/seg")
		sb.WriteString(strconv.Itoa(i))
	}
	args := `{"workdir":"` + sb.String() + `"}`
	require.False(t, hasRunawayRepeat(args), "测试数据自身不得命中重复判据")

	g := newToolArgsDegenerationGuard(true)
	deltaPayload := `{"type":"response.function_call_arguments.delta","output_index":0,"delta":` + jsonQuote(args) + `}`
	require.Empty(t, observeToolArgsFrame(g, deltaPayload), "单次合法长度不该拦")

	donePayload := `{"type":"response.function_call_arguments.done","output_index":0,"arguments":` + jsonQuote(args) + `}`
	require.Empty(t, observeToolArgsFrame(g, donePayload),
		"done 全量必须替换而非追加，否则会误判成超长")
	require.False(t, g.Tripped())
}

func TestObserveToolArgsFrame_DoneOnlyStillDetects(t *testing.T) {
	t.Parallel()

	// 兜住「上游省略增量事件、只发一次终态」的形态
	g := newToolArgsDegenerationGuard(true)
	payload := `{"type":"response.function_call_arguments.done","output_index":0,` +
		`"arguments":"{\"workdir\":\"/x json error? tool call must proper\"}"}`
	require.NotEmpty(t, observeToolArgsFrame(g, payload))
}

func TestObserveToolArgsFrame_NilAndDisabled(t *testing.T) {
	t.Parallel()

	payload := `{"type":"response.function_call_arguments.delta","output_index":0,` +
		`"delta":"{\"w\":\"assistant to=functions.bash\"}"}`
	require.Empty(t, observeToolArgsFrame(nil, payload), "nil 守卫不得 panic")
	require.Empty(t, observeToolArgsFrame(newToolArgsDegenerationGuard(false), payload),
		"未启用（请求无 tools）时不得拦截")
}

// --- raw 路径（CC 原生形状）---

func TestObserveRawChatToolArgs_DetectsDegeneration(t *testing.T) {
	t.Parallel()

	g := newToolArgsDegenerationGuard(true)
	// CC 原生 chunk：choices[].delta.tool_calls[].function.arguments
	chunk := `{"choices":[{"delta":{"tool_calls":[{"index":0,"function":` +
		`{"arguments":"{\"workdir\":\"/x assistant to=functions.bash\"}"}}]}}]}`
	require.NotEmpty(t, observeRawChatToolArgs(g, chunk))
	require.True(t, g.Tripped())
}

func TestObserveRawChatToolArgs_AccumulatesAcrossChunks(t *testing.T) {
	t.Parallel()

	g := newToolArgsDegenerationGuard(true)
	frag := func(s string) string {
		return `{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":` + jsonQuote(s) + `}}]}}]}`
	}
	var tripped string
	for _, part := range []string{`{"workdir":"/x `, `assistant `, `to=functions.bash"}`} {
		if reason := observeRawChatToolArgs(g, frag(part)); reason != "" {
			tripped = reason
			break
		}
	}
	require.NotEmpty(t, tripped, "raw 路径同样必须累积后再判")
}

func TestObserveRawChatToolArgs_NegativeMustNotTrip(t *testing.T) {
	t.Parallel()

	cases := []string{
		// 正常工具调用
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":\"/srv/app\"}"}}]}}]}`,
		// 业务词合法出现
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":\"/srv/彩票系统\"}"}}]}}]}`,
		// 韩文参数值
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"p\":\"/문서\"}"}}]}}]}`,
		// 纯文本 chunk（无 tool_calls）—— 本守卫不碰文本
		`{"choices":[{"delta":{"content":"assistant to=functions.bash 天天彩票"}}]}`,
		// 终止标记与非 JSON
		`[DONE]`,
		`{"choices":[]}`,
	}
	for _, c := range cases {
		g := newToolArgsDegenerationGuard(true)
		require.Empty(t, observeRawChatToolArgs(g, c), "不得误伤：%s", c)
		require.False(t, g.Tripped())
	}
}

func TestObserveRawChatToolArgs_ParallelToolCallsSeparate(t *testing.T) {
	t.Parallel()

	g := newToolArgsDegenerationGuard(true)
	// 两个并行 tool call 各自累积，不得互相拼接后误判
	chunk := `{"choices":[{"delta":{"tool_calls":[` +
		`{"index":0,"function":{"arguments":"{\"a\":\"/x\"}"}},` +
		`{"index":1,"function":{"arguments":"{\"b\":\"/y\"}"}}]}}]}`
	require.Empty(t, observeRawChatToolArgs(g, chunk))
	require.False(t, g.Tripped())
}

// jsonQuote 把任意字符串转成 JSON 字符串字面量（含转义），供构造测试输入用。
func jsonQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
