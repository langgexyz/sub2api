package service

// UpstreamRateLimitClientMessage 是网关在上游返回 429 时回给客户端的标准限流文案。
// 它跨协议统一（Anthropic/OpenAI/Gemini 的 429 映射、handler.mapUpstreamError 等都用这条）。
// 订阅份额用尽时也复用它——让"额度/份额用尽"对客户端呈现为与真实上游限流逐字一致的 rate_limit_error，
// 不泄露内部概念。改这条即同步所有"上游限流"出口（新代码引用此常量，勿再写字面量）。
const UpstreamRateLimitClientMessage = "Upstream rate limit exceeded, please retry later"
