package service

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 端到端：拿【真实模型退化产物】喂守卫，而不是人工构造串自证。
//
// 样本由客户端仓的 model-degeneration-probe skill 采集（三条件构造，命中率 30-42%）。
// 该 skill 不在本仓，故本用例在采集不到时 skip —— 但只要能采到就必须全部拦下。
//
// 跑法：go test ./internal/service -run TestToolArgsGuard_RealModelSamples -count=1
func TestToolArgsGuard_RealModelSamples(t *testing.T) {
	probe := strings.TrimSpace(mustHome() + "/.claude/skills/model-degeneration-probe/bin/degeneration-probe")
	if !fileExists(probe) {
		t.Skip("model-degeneration-probe 不可用（该 skill 在客户端仓），跳过端到端采样")
	}

	cmd := exec.Command(probe, "--model", "gpt-5.6-terra", "--rounds", "8", "--dump-samples")
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("probe 执行失败（网络/配额）：%v", err)
	}
	// --dump-samples：每行一个命中样本的 arguments 原文（JSON 字符串字面量）
	var samples []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var s string
		if json.Unmarshal([]byte(line), &s) == nil && s != "" {
			samples = append(samples, s)
		}
	}
	if len(samples) == 0 {
		t.Skip("本轮未命中退化（概率性，30-42%），无样本可验")
	}

	// 真正的断言：每个真实退化样本都必须被守卫拦下。
	for i, sample := range samples {
		g := newToolArgsDegenerationGuard(true)
		payload := `{"type":"response.function_call_arguments.delta","output_index":0,"delta":` +
			jsonQuote(sample) + `}`
		reason := observeToolArgsFrame(g, payload)
		require.NotEmpty(t, reason, "真实退化样本 #%d 漏网（长度 %d）", i, len(sample))
		require.NotContains(t, reason, "彩票", "拦截理由不得回显原文")
	}
	t.Logf("端到端：%d 个真实退化样本全部拦下", len(samples))
}

func mustHome() string {
	out, err := exec.Command("sh", "-c", "echo $HOME").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func fileExists(p string) bool {
	if p == "" {
		return false
	}
	return exec.Command("test", "-x", p).Run() == nil
}

// 真实样本的【离线】回归：把现场那条逐字钉进用例，不依赖网络。
// 这是端到端验证的可重复替代 —— probe 采样是概率性的，CI 里不能依赖它。
func TestToolArgsGuard_RealSampleOfflineRegression(t *testing.T) {
	t.Parallel()

	// 走完整守卫链路（Responses 事件形状），而非直接调判定函数
	g := newToolArgsDegenerationGuard(true)
	args := `{"command":"sleep 600","workdir":` + jsonQuote(realDegeneratedWorkdir) + `}`
	payload := `{"type":"response.function_call_arguments.delta","output_index":0,"delta":` + jsonQuote(args) + `}`

	reason := observeToolArgsFrame(g, payload)
	require.NotEmpty(t, reason, "现场真实退化产物必须被完整链路拦下")
	require.True(t, g.Tripped())
	require.NotContains(t, reason, "彩票", "拦截理由不得回显原文（垃圾内容不该二次出现在日志里）")
	require.NotContains(t, reason, "assistant to=", "同上")
}
