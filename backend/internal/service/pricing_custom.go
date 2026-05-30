package service

// Custom model pricing overlay.
//
// Some upstreams sub2api serves are not in the LiteLLM price feed (e.g. Xiaomi
// MiMo). Rather than disable the remote feed, we MERGE these custom prices on
// top of whatever the feed (or fallback) provides — so the LiteLLM auto-update
// keeps working and custom models always remain priced. mergeCustomPricing runs
// at the end of parsePricingData, so it applies to every load path (remote /
// fallback / local file).
//
// Prices are per-token USD (LiteLLM convention). MiMo is published in CNY per 1M
// tokens; converted at the rate noted below. Update both the value and the rate
// comment when MiMo or the FX rate changes.
//
//	MiMo-V2.5-Pro: input CNY3/1M, output CNY6/1M, cache-read CNY0.025/1M
//	MiMo-V2.5:     input CNY1/1M, output CNY2/1M, cache-read CNY0.02/1M
//	FX: 1 USD = 6.766 CNY (2026-05-30)
var customModelPricing = map[string]*LiteLLMModelPricing{
	"mimo-v2.5-pro": {
		InputCostPerToken:           4.434e-07, // CNY3 / 6.766 / 1e6
		OutputCostPerToken:          8.868e-07, // CNY6 / 6.766 / 1e6
		CacheReadInputTokenCost:     3.695e-09, // CNY0.025 / 6.766 / 1e6
		CacheCreationInputTokenCost: 4.434e-07, // treat cache-write as input price
		LiteLLMProvider:             "mimo",
		Mode:                        "chat",
		SupportsPromptCaching:       true,
	},
	"mimo-v2.5": {
		InputCostPerToken:           1.478e-07, // CNY1 / 6.766 / 1e6
		OutputCostPerToken:          2.957e-07, // CNY2 / 6.766 / 1e6
		CacheReadInputTokenCost:     2.956e-09, // CNY0.02 / 6.766 / 1e6
		CacheCreationInputTokenCost: 1.478e-07,
		LiteLLMProvider:             "mimo",
		Mode:                        "chat",
		SupportsPromptCaching:       true,
	},
}

// mergeCustomPricing overlays customModelPricing onto a parsed price map. Custom
// entries take precedence (override the feed if it ever adds the same name).
func mergeCustomPricing(result map[string]*LiteLLMModelPricing) {
	for name, pricing := range customModelPricing {
		result[name] = pricing
	}
}
