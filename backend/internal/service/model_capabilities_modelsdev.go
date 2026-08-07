package service

// models.dev 模型目录数据源（全量 provider，gzip 内嵌）。
//
// 数据来源: https://models.dev/api.json —— 所有 provider 的模型能力精简字段
// （attachment / reasoning / reasoning effort 档位），由
// scripts/sync-models-dev.sh 生成 models-dev-all.json.gz。
// 统一原则: 模型能力元数据一律以 models.dev 为准（任何 provider 收录的模型），
// 本地手维护名单仅作 models.dev 未收录模型（长尾/私有）的 fallback。

import (
	"bytes"
	"compress/gzip"
	"embed"
	"encoding/json"
	"io"
	"strings"
	"sync"
)

//go:embed modelsdevdata/models-dev-all.json.gz
var modelsDevAllData embed.FS

// modelsDevEntry 是 models.dev 单模型的精简能力字段。
type modelsDevEntry struct {
	Attachment bool     `json:"attachment"`
	Reasoning  bool     `json:"reasoning"`
	Efforts    []string `json:"efforts,omitempty"`
}

// modelsDevPayload 双层结构：opencode provider（zen 上游精确条目）优先，
// other（其余 provider 合并）作 fallback——同一模型 ID 在不同 provider 的
// 档位可能不同（如 deepseek-v4-flash: zen 三档 vs 其它四档），能力必须按
// 网关实际上游取。
type modelsDevPayload struct {
	OpenCode map[string]modelsDevEntry `json:"opencode"`
	Other    map[string]modelsDevEntry `json:"other"`
}

var (
	modelsDevOnce sync.Once
	modelsDevAll  modelsDevPayload
	// modelsDevLower 是大小写不敏感索引（models.dev 存在大小写变体 ID，
	// 如 DeepSeek-V4-Pro vs deepseek-v4-pro，客户端大小写不敏感）。
	modelsDevLower map[string]modelsDevEntry
)

// loadModelsDevAll 加载全量 models.dev 精简能力数据（gzip 解压）。
func loadModelsDevAll() modelsDevPayload {
	modelsDevOnce.Do(func() {
		raw, err := modelsDevAllData.ReadFile("modelsdevdata/models-dev-all.json.gz")
		if err != nil {
			return
		}
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return
		}
		decoded, err := io.ReadAll(zr)
		_ = zr.Close()
		if err != nil {
			return
		}
		_ = json.Unmarshal(decoded, &modelsDevAll)
		modelsDevLower = map[string]modelsDevEntry{}
		for mid, e := range modelsDevAll.OpenCode {
			modelsDevLower[strings.ToLower(mid)] = e
		}
		for mid, e := range modelsDevAll.Other {
			key := strings.ToLower(mid)
			if _, exists := modelsDevLower[key]; !exists {
				modelsDevLower[key] = e
			}
		}
	})
	return modelsDevAll
}

// modelsDevLookup 按小写归一索引取模型的 models.dev 条目。
// 索引构建时 opencode provider 优先、other 合并 fallback——模型 ID 大小写
// 变体（DeepSeek-V4-Pro vs deepseek-v4-pro）统一指向同一标准条目。
func modelsDevLookup(modelID string) (modelsDevEntry, bool) {
	loadModelsDevAll()
	e, ok := modelsDevLower[strings.ToLower(modelID)]
	return e, ok
}

// IsModelDevKnownID 判断模型 ID 是否存在于 models.dev 目录（任意 provider）。
// 用于 /v1/models 输出过滤：不在 models.dev 的自定义 ID（如 antigravity
// 档位变体 gemini-3-pro-high/low）不展示给客户端，客户端请求仍可路由。
func IsModelDevKnownID(modelID string) bool {
	_, ok := modelsDevLookup(modelID)
	return ok
}

// modelsDevEfforts 从 models.dev 取模型的 reasoning effort 档位
// （"none" 非协议档位已在数据生成时过滤）。
// 收录但无 effort 档位返回 (nil, true)；未收录返回 (nil, false)。
func modelsDevEfforts(modelID string) ([]string, bool) {
	entry, ok := modelsDevLookup(modelID)
	if !ok || !entry.Reasoning {
		return nil, false
	}
	return entry.Efforts, true
}

// modelsDevAttachments 从 models.dev 取模型附件能力；未收录返回 (false, false)。
func modelsDevAttachments(modelID string) (bool, bool) {
	entry, ok := modelsDevLookup(modelID)
	if !ok {
		return false, false
	}
	return entry.Attachment, true
}
