package service

// RequestResponseLog 是一条请求/响应原文记录，用于 Prompt 质量分析。
// request_id 与 usage_logs.request_id 同源（ResolveUsageBillingRequestID），可 JOIN 出费用/token；
// session_hash 来自请求体 metadata.user_id 的 session_id（Claude Code 会话 UUID），用于按会话聚合。
type RequestResponseLog struct {
	RequestID         string
	SessionHash       string
	UserID            *int64
	APIKeyID          *int64
	Model             string
	Endpoint          string
	StatusCode        int
	Stream            bool
	RequestBody       []byte
	ResponseBody      []byte
	RequestTruncated  bool
	ResponseTruncated bool
}

// RequestResponseLogRepository 异步持久化请求/响应原文。
// 实现必须是非阻塞的 best-effort：队列满时丢弃并计数，绝不拖慢请求热路径。
type RequestResponseLogRepository interface {
	// Enqueue 提交一条记录异步落库；调用方不持有 log 的后续所有权。
	Enqueue(log *RequestResponseLog)
}
