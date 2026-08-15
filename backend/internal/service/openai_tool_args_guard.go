package service

import (
	"encoding/json"
	"regexp"
	"strings"
	"unicode"
)

// 工具参数退化守卫（IK8VPN）。
//
// 背景：模型被逼在字符串内部持续产出又无合法内容可写时会发生生成退化 —— 先复读，
// 复读耗尽后跳进预训练语料的低概率尾部（那片区域里中文博彩、成人广告、Qt 本地化标签、
// 多语言碎片本就混杂在一起）。现场样本（IK8VCE）：模型把 210 字符垃圾填进 bash 工具的
// workdir 参数，客户端未校验就当路径访问文件系统，失败后把原文回显给终端用户。
//
// 与 responsesDSMLToolCallGuard 的分工（刻意不合并）：
//   - DSML 守卫：认单一固定标签 <｜DSML｜tool_calls>，判「文本里混进了工具调用」
//   - 本守卫：判 tool_call.arguments 的【结构】是否已退化，与具体词汇无关
// 两者时机相同（首 token 前切账号 / 已输出则协议错误终止），处置复用同一套。
//
// --- 为什么只拦工具参数、绝不拦普通文本 -------------------------------------
//
// 实测反例（同模型、正常业务问答，2026-08-14）：
//
//	提问「彩票销售系统的核心模块」   -> 正常回答含「彩票」
//	提问「Qt 的 numerusform 是什么」 -> 正常回答含 numerusform
//	韩文提问                        -> 正常回答就是韩文
//
// 所以【词表与字符集判据在文本层必然误伤】。放到工具参数上才安全：用户会谈论博彩，
// 但不会把 `assistant to=functions.bash` 塞进一个文件路径参数 —— 语境完全不同。
//
// 同理，本守卫的判据里【没有】博彩/成人词表，也【没有】Unicode 区段判断：
// 那会退化成按语言歧视（误伤日/韩/俄文参数值），而纯 ASCII 的复读串反而漏网。

const toolArgsDegenerationErrorMessage = "Upstream emitted a degenerated tool call payload (protocol fragment / runaway repetition / oversized field)"

const (
	// 单个字符串字段的长度上限。真实工具参数（路径、命令、查询串）远低于此；
	// 现场退化样本 210 起步，实测最大 5896。
	maxToolArgFieldLen = 4096

	// 整个 arguments 的长度上限。留足余量给合法的大 payload（如长文本编辑），
	// 只拦明显失控的。
	maxToolArgsTotalLen = 65536

	// 同一子串连续重复的次数上限。`./././.`、`../../..` 是复读退化的典型形态；
	// 少量重复是合法相对路径，所以按【连续次数】判而不是禁用。
	maxToolArgRepeatRun = 16
)

// 协议残片：训练语料里的工具调用协议标记被当作普通文本吐进了参数值。
// 这是最强信号 —— 正常的工具参数绝不会包含这些。
var toolArgsProtocolFragment = regexp.MustCompile(
	`assistant\s+to=functions\.|(?i)json\s+error\?|(?i)tool\s+call\s+must\s+proper|<\|?[A-Za-z]+\|?tool_calls>`,
)

// hasRunawayRepeat 判断是否存在「同一短子串连续重复」的复读退化。
//
// 为什么不用正则：Go 的 regexp 是 RE2，【不支持反向引用】，`(.{1,8})\1{16,}` 直接
// 编译失败。改为显式扫描，顺带避免了正则在超长输入上的回溯风险。
//
// 判据：对 1..8 字符的窗口，检查是否有连续重复超过 maxToolArgRepeatRun 次。
// 少量重复是合法的（`../..`），只拦失控的（现场样本 `./` 重复上千次）。
func hasRunawayRepeat(s string) bool {
	n := len(s)
	if n < 2*maxToolArgRepeatRun {
		return false
	}
	for unit := 1; unit <= 8; unit++ {
		if n < unit*(maxToolArgRepeatRun+1) {
			break
		}
		run := 1
		for i := unit; i+unit <= n; i += unit {
			if s[i:i+unit] == s[i-unit:i] {
				run++
				if run > maxToolArgRepeatRun {
					return true
				}
			} else {
				run = 1
			}
		}
	}
	return false
}

// toolArgsDegenerationReason 判断一段 tool_call.arguments 是否已退化。
// 返回空串表示正常；非空为拦截理由（【不含参数原文】—— 原文可能是垃圾内容，
// 不该二次出现在日志与错误响应里）。
func toolArgsDegenerationReason(arguments string) string {
	if arguments == "" {
		return ""
	}
	if len(arguments) > maxToolArgsTotalLen {
		return "arguments 总长度超限"
	}
	if toolArgsProtocolFragment.MatchString(arguments) {
		return "arguments 含工具调用协议残片"
	}
	if hasRunawayRepeat(arguments) {
		return "arguments 含失控重复片段"
	}

	// 逐字段检查。arguments 是 JSON 字符串；解析失败不判退化 ——
	// 流式累积过程中它本来就可能是不完整 JSON（fail-open，见下方 Observe 的注释）。
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(arguments), &fields) != nil {
		return ""
	}
	for _, raw := range fields {
		var value string
		if json.Unmarshal(raw, &value) != nil {
			continue // 非字符串字段（数字/布尔/嵌套）不在本守卫范围
		}
		if len(value) > maxToolArgFieldLen {
			return "单个参数字段长度超限"
		}
		if hasControlChars(value) {
			return "参数字段含控制字符"
		}
	}
	return ""
}

// hasControlChars 判断字符串是否含 C0 控制字符（含 NUL / 换行 / 制表符）。
// 真实的工具参数值不含这些；换行还兼具命令注入风险。
func hasControlChars(s string) bool {
	for _, r := range s {
		if r == '\n' || r == '\r' || r == '\t' || r == 0 {
			return true
		}
		if r < 0x20 || r == 0x7F {
			return true
		}
		if unicode.Is(unicode.Cf, r) && r != '\uFEFF' {
			return true // 零宽/格式控制符，正常参数不该有
		}
	}
	return false
}

// toolArgsDegenerationGuard 累积流式 tool_call.arguments 分片并判定退化。
//
// 为什么必须累积：arguments 按 chunk 增量下发（实测形如 `{"inp` + `ut": "dir"}`），
// 单块看不出问题 —— 逐块判定会漏掉所有跨块的退化形态。
type toolArgsDegenerationGuard struct {
	enabled   bool
	tripped   bool
	perCall   map[int]*strings.Builder // tool_call index -> 累积的 arguments
	lastIndex int
}

func newToolArgsDegenerationGuard(enabled bool) *toolArgsDegenerationGuard {
	return &toolArgsDegenerationGuard{enabled: enabled, perCall: map[int]*strings.Builder{}}
}

// Observe 累积一个分片并返回拦截理由（空串 = 放行）。
//
// fail-open：本守卫只在【确定】退化时拦截。累积中途的不完整 JSON、非字符串字段、
// 解析失败一律放行 —— 守卫的 bug 绝不能卡死正常工具调用（同 guardrails 的不变量 #1）。
func (g *toolArgsDegenerationGuard) Observe(index int, fragment string) string {
	if !g.enabled || g.tripped || fragment == "" {
		return ""
	}
	buf, ok := g.perCall[index]
	if !ok {
		buf = &strings.Builder{}
		g.perCall[index] = buf
	}
	buf.WriteString(fragment)
	g.lastIndex = index

	if reason := toolArgsDegenerationReason(buf.String()); reason != "" {
		g.tripped = true
		return reason
	}
	return ""
}

// Replace 用全量 arguments 覆盖该路的累积值再判定（用于 *.done 事件）。
// 与 Observe 的区别是替换而非追加 —— done 带的是全量，追加会与已累积的 delta
// 重复叠加，把正常参数拼成「超长」而误判。
func (g *toolArgsDegenerationGuard) Replace(index int, arguments string) string {
	if !g.enabled || g.tripped || arguments == "" {
		return ""
	}
	buf := &strings.Builder{}
	buf.WriteString(arguments)
	g.perCall[index] = buf
	g.lastIndex = index

	if reason := toolArgsDegenerationReason(arguments); reason != "" {
		g.tripped = true
		return reason
	}
	return ""
}

// Tripped 报告本次响应是否已判定退化。
func (g *toolArgsDegenerationGuard) Tripped() bool { return g.tripped }

// observeToolArgsFrame 从一条 Responses SSE payload 里取出工具参数分片喂给守卫。
// 返回非空即为拦截理由。
//
// 只认 response.function_call_arguments.delta —— 工具参数是逐块增量下发的，
// 按 output_index 分路累积（一次响应可能有多个并行 tool call）。
// 其它事件类型一律放行：本守卫不碰普通文本（在文本层用这类判据必然误伤，见文件头注释）。
func observeToolArgsFrame(guard *toolArgsDegenerationGuard, payload string) string {
	if guard == nil || !guard.enabled || guard.tripped {
		return ""
	}
	var event struct {
		Type        string `json:"type"`
		OutputIndex int    `json:"output_index"`
		Delta       string `json:"delta"`
		Arguments   string `json:"arguments"`
	}
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return "" // 非 JSON / 解析失败 -> fail-open
	}
	switch event.Type {
	case "response.function_call_arguments.delta":
		return guard.Observe(event.OutputIndex, event.Delta)
	case "response.function_call_arguments.done":
		// done 带【全量】arguments。若 delta 已累积过，这里再追加会重复叠加，
		// 可能把正常参数拼成「超长」而误判 —— 所以用替换语义而非追加。
		// 保留本分支是为了兜住「上游省略增量事件、只发一次终态」的形态。
		return guard.Replace(event.OutputIndex, event.Arguments)
	default:
		return ""
	}
}

// observeRawChatToolArgs 从一条【Chat Completions 原生格式】的 SSE payload 里取出
// 工具参数分片喂给守卫。返回非空即为拦截理由。
//
// 为什么需要单独一个：raw 路径（forwardAsRawChatCompletions，DeepSeek/Kimi/GLM 等
// 第三方 APIKey 上游走这条）不做协议转换，chunk 是 CC 原生形状
// `choices[].delta.tool_calls[].function.arguments`，与 Responses 事件结构完全不同。
// 两条路径都要防护，否则第三方上游的退化产物照样直达客户端。
func observeRawChatToolArgs(guard *toolArgsDegenerationGuard, payload string) string {
	if guard == nil || !guard.enabled || guard.tripped {
		return ""
	}
	var chunk struct {
		Choices []struct {
			Delta struct {
				ToolCalls []struct {
					Index    *int `json:"index"`
					Function struct {
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
		return "" // 非 JSON（含 [DONE]）-> fail-open
	}
	for _, choice := range chunk.Choices {
		for _, call := range choice.Delta.ToolCalls {
			if call.Function.Arguments == "" {
				continue
			}
			idx := 0
			if call.Index != nil {
				idx = *call.Index
			}
			if reason := guard.Observe(idx, call.Function.Arguments); reason != "" {
				return reason
			}
		}
	}
	return ""
}
