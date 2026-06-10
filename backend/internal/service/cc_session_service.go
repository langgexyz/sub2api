package service

// CC 会话历史分析 service：在归一化纯函数（cc_session_replay.go）之上，
// 提供「列会话 / 回放会话 / 检索提问」三类只读能力，供 admin handler + MCP 消费。
// 数据来自 request_response_logs（网关已全量捕获）。

import (
	"context"
	"time"
)

// CCSessionSummary 是会话列表项。
type CCSessionSummary struct {
	SessionHash        string    `json:"session_hash"`
	UserID             *int64    `json:"user_id,omitempty"`
	Username           string    `json:"username,omitempty"`
	StartedAt          time.Time `json:"started_at"`
	EndedAt            time.Time `json:"ended_at"`
	RequestCount       int       `json:"request_count"`
	Models             []string  `json:"models"`
	FirstPromptExcerpt string    `json:"first_prompt_excerpt"`
	AnyTruncated       bool      `json:"any_truncated"`
}

// CCSessionListQuery 是列会话的查询条件（user_id 或 username 二选一必填）。
type CCSessionListQuery struct {
	UserID   *int64
	Username string
	From     *time.Time
	To       *time.Time
	Limit    int
}

// CCPromptSearchQuery 是跨会话检索提问的条件。
type CCPromptSearchQuery struct {
	Query    string
	UserID   *int64
	From     *time.Time
	To       *time.Time
	Limit    int
}

// CCPromptHit 是检索命中项。
type CCPromptHit struct {
	SessionHash string    `json:"session_hash"`
	UserID      *int64    `json:"user_id,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	Excerpt     string    `json:"excerpt"`
}

// CCSessionLogReadRepository 是 request_response_logs 的只读仓储。
type CCSessionLogReadRepository interface {
	// ListSessions 按用户列会话摘要（按 started_at 倒序）。
	ListSessions(ctx context.Context, q CCSessionListQuery) ([]CCSessionSummary, error)
	// GetSessionRows 取某会话的全部记录，按 created_at 升序（供归一化）。
	GetSessionRows(ctx context.Context, sessionHash string) ([]CCSessionLogRow, error)
	// SearchPrompts 跨会话按关键词检索请求正文（命中即返回会话与片段）。
	SearchPrompts(ctx context.Context, q CCPromptSearchQuery) ([]CCPromptHit, error)
}

// CCSessionReplayService 组合只读仓储 + 归一化纯函数。
type CCSessionReplayService struct {
	repo CCSessionLogReadRepository
}

// NewCCSessionReplayService 构造历史分析 service。
func NewCCSessionReplayService(repo CCSessionLogReadRepository) *CCSessionReplayService {
	return &CCSessionReplayService{repo: repo}
}

const ccDefaultListLimit = 50

// ListSessions 列某用户全部会话。
func (s *CCSessionReplayService) ListSessions(ctx context.Context, q CCSessionListQuery) ([]CCSessionSummary, error) {
	if q.Limit <= 0 || q.Limit > 500 {
		q.Limit = ccDefaultListLimit
	}
	return s.repo.ListSessions(ctx, q)
}

// GetReplay 回放单会话（full：逐轮含动作）。
func (s *CCSessionReplayService) GetReplay(ctx context.Context, sessionHash string) (CCSessionReplay, error) {
	rows, err := s.repo.GetSessionRows(ctx, sessionHash)
	if err != nil {
		return CCSessionReplay{}, err
	}
	return NormalizeSession(sessionHash, rows), nil
}

// GetPrompts 回放后只取客户真实提问（prompts 视图）。
func (s *CCSessionReplayService) GetPrompts(ctx context.Context, sessionHash string) ([]CCPrompt, error) {
	replay, err := s.GetReplay(ctx, sessionHash)
	if err != nil {
		return nil, err
	}
	return ExtractPrompts(replay), nil
}

// SearchPrompts 跨会话检索。
func (s *CCSessionReplayService) SearchPrompts(ctx context.Context, q CCPromptSearchQuery) ([]CCPromptHit, error) {
	if q.Limit <= 0 || q.Limit > 200 {
		q.Limit = ccDefaultListLimit
	}
	return s.repo.SearchPrompts(ctx, q)
}
