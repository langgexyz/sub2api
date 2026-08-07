package service

// models.dev（opencode provider）模型目录数据源。
//
// 数据来源: https://models.dev/api.json 的 "opencode" provider
// （opencode zen 网关官方目录, api = https://opencode.ai/zen/v1）。
// 统一原则: 模型能力元数据以 models.dev 为准, 不再本地手维护 opencode 系
// 模型的能力名单; 同步命令见 scripts/sync-models-dev.sh。

import (
	"embed"
	"encoding/json"
	"sync"
)

//go:embed modelsdevdata/models-dev-opencode.json
var modelsDevOpenCodeData embed.FS

//go:embed modelsdevdata/models-dev-known-ids.json
var modelsDevKnownIDsData embed.FS

type modelsDevReasoningOption struct {
	Type   string   `json:"type"`
	Values []string `json:"values"`
}

type modelsDevModel struct {
	ID               string                     `json:"id"`
	Attachment       bool                       `json:"attachment"`
	Reasoning        bool                       `json:"reasoning"`
	ReasoningOptions []modelsDevReasoningOption `json:"reasoning_options"`
}

var (
	modelsDevOnce   sync.Once
	modelsDevByID   map[string]modelsDevModel
	modelsDevLoaded bool

	modelsDevKnownOnce sync.Once
	modelsDevKnownIDs  map[string]struct{}
)

// loadModelsDevKnownIDs 加载 models.dev 全量 provider 的已知模型 ID 集合
// （modelsdevdata/models-dev-known-ids.json，见 scripts/sync-models-dev.sh）。
func loadModelsDevKnownIDs() map[string]struct{} {
	modelsDevKnownOnce.Do(func() {
		modelsDevKnownIDs = map[string]struct{}{}
		raw, err := modelsDevKnownIDsData.ReadFile("modelsdevdata/models-dev-known-ids.json")
		if err != nil {
			return
		}
		var ids []string
		if err := json.Unmarshal(raw, &ids); err != nil {
			return
		}
		for _, id := range ids {
			modelsDevKnownIDs[id] = struct{}{}
		}
	})
	return modelsDevKnownIDs
}

// IsModelDevKnownID 判断模型 ID 是否存在于 models.dev 目录（任意 provider）。
// 用于 /v1/models 输出过滤：不在 models.dev 的自定义 ID（如 antigravity
// 档位变体 gemini-3-pro-high/low）不展示给客户端，客户端请求仍可路由。
func IsModelDevKnownID(modelID string) bool {
	_, ok := loadModelsDevKnownIDs()[modelID]
	return ok
}

func loadModelsDevOpenCode() (map[string]modelsDevModel, bool) {
	modelsDevOnce.Do(func() {
		raw, err := modelsDevOpenCodeData.ReadFile("modelsdevdata/models-dev-opencode.json")
		if err != nil {
			modelsDevByID = map[string]modelsDevModel{}
			return
		}
		var payload struct {
			Models map[string]modelsDevModel `json:"models"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil || payload.Models == nil {
			modelsDevByID = map[string]modelsDevModel{}
			return
		}
		modelsDevByID = payload.Models
		modelsDevLoaded = true
	})
	return modelsDevByID, modelsDevLoaded
}

// modelsDevEfforts 从 models.dev 目录取模型的 reasoning effort 档位。
// 档位来自 reasoning_options 里 type=effort 的 values，过滤 "none"（opencode
// 客户端专用关闭值，非 OpenAI 协议档位）；
// 收录但无 effort 档位（仅 toggle）返回 (nil, true)。
// 未收录返回 (nil, false)。
func modelsDevEfforts(modelID string) ([]string, bool) {
	byID, ok := loadModelsDevOpenCode()
	if !ok {
		return nil, false
	}
	m, ok := byID[modelID]
	if !ok || !m.Reasoning {
		return nil, false
	}
	for _, opt := range m.ReasoningOptions {
		if opt.Type != "effort" || len(opt.Values) == 0 {
			continue
		}
		out := make([]string, 0, len(opt.Values))
		for _, v := range opt.Values {
			if v != "none" {
				out = append(out, v)
			}
		}
		return out, true
	}
	return nil, true
}

// modelsDevAttachments 从 models.dev 目录取模型附件能力；未收录返回 (false, false)。
func modelsDevAttachments(modelID string) (bool, bool) {
	byID, ok := loadModelsDevOpenCode()
	if !ok {
		return false, false
	}
	m, ok := byID[modelID]
	if !ok {
		return false, false
	}
	return m.Attachment, true
}
