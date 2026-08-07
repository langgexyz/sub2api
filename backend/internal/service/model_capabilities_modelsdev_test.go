//go:build unit

package service

import (
	"reflect"
	"testing"
)

// 与 models.dev（opencode provider）实际数据一致（2026-08-07 快照）。
func TestModelsDevCapabilities(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		modelID    string
		wantEff    []string
		wantAttach bool
	}{
		// 收录模型：档位与附件均以 models.dev 为准
		{"deepseek-v4-flash", "deepseek-v4-flash", []string{"low", "high", "max"}, false},
		{"longcat-2.0-free", "longcat-2.0-free", nil, false},
		{"deepseek-v4-flash-free", "deepseek-v4-flash-free", []string{"low", "high", "max"}, false},
		// "none"（opencode 关闭推理专用值）从协议档位过滤
		{"gpt-5.6-luna", "gpt-5.6-luna", []string{"low", "medium", "high", "xhigh", "max"}, true},
		// 收录但仅 toggle（无 effort 档位）→ 不支持可配置档位
		{"kimi-k2.5", "kimi-k2.5", nil, true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ModelReasoningEfforts(tc.modelID); !reflect.DeepEqual(got, tc.wantEff) {
				t.Fatalf("ModelReasoningEfforts(%q)=%v, want %v", tc.modelID, got, tc.wantEff)
			}
			if got := ModelSupportsAttachments(tc.modelID); got != tc.wantAttach {
				t.Fatalf("ModelSupportsAttachments(%q)=%v, want %v", tc.modelID, got, tc.wantAttach)
			}
		})
	}
}

// 未收录模型走本地保守名单（不受 models.dev 影响）。
func TestModelsDevFallback(t *testing.T) {
	t.Parallel()

	if got := ModelSupportsAttachments("gemini-2.5-pro"); !got {
		t.Fatalf("gemini-2.5-pro 应走本地名单 attachment=true, got %v", got)
	}
	if got := ModelSupportsAttachments("o3"); !got {
		t.Fatalf("o3 应走本地名单 attachment=true, got %v", got)
	}
	if got := ModelReasoningEfforts("glm-3"); !reflect.DeepEqual(got, []string{"high", "max"}) {
		t.Fatalf("glm-3 efforts=%v, want [high max]", got)
	}
}

// IsModelDevKnownID：models.dev 全量目录判定（/v1/models 输出过滤依据）。
func TestIsModelDevKnownID(t *testing.T) {
	t.Parallel()

	known := []string{"deepseek-v4-flash", "longcat-2.0-free", "gemini-2.5-pro", "grok-4.5", "gpt-5.6-luna"}
	for _, id := range known {
		if !IsModelDevKnownID(id) {
			t.Fatalf("IsModelDevKnownID(%q)=false, 应为 true（models.dev 收录）", id)
		}
	}

	custom := []string{"gemini-3-pro-high", "gemini-3-pro-low", "gemini-3.1-pro-high", "gemini-3.1-pro-low"}
	for _, id := range custom {
		if IsModelDevKnownID(id) {
			t.Fatalf("IsModelDevKnownID(%q)=true, 应为 false（自定义 ID 应被隐藏）", id)
		}
	}
}
