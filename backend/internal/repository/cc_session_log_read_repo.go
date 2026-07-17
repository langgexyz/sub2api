package repository

// request_response_logs 的只读仓储，供 CC 会话历史分析 service 使用。
// 与写侧（request_response_log_repo.go，异步 best-effort）分离：读侧是同步查询。

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

type ccSessionLogReadRepository struct {
	db *sql.DB
}

// NewCCSessionLogReadRepository 构造只读仓储。
func NewCCSessionLogReadRepository(db *sql.DB) service.CCSessionLogReadRepository {
	return &ccSessionLogReadRepository{db: db}
}

const ccReadQueryTimeout = 15 * time.Second

func (r *ccSessionLogReadRepository) ListSessions(ctx context.Context, q service.CCSessionListQuery) ([]service.CCSessionSummary, error) {
	ctx, cancel := context.WithTimeout(ctx, ccReadQueryTimeout)
	defer cancel()

	var where []string
	var args []any
	where = append(where, "r.session_hash IS NOT NULL")
	if q.UserID != nil {
		args = append(args, *q.UserID)
		where = append(where, "r.user_id = $"+strconv.Itoa(len(args)))
	}
	if q.Username != "" {
		args = append(args, q.Username)
		where = append(where, "u.username = $"+strconv.Itoa(len(args)))
	}
	if q.From != nil {
		args = append(args, *q.From)
		where = append(where, "r.created_at >= $"+strconv.Itoa(len(args)))
	}
	if q.To != nil {
		args = append(args, *q.To)
		where = append(where, "r.created_at <= $"+strconv.Itoa(len(args)))
	}
	args = append(args, q.Limit)
	limitPos := strconv.Itoa(len(args))

	query := `
		SELECT r.session_hash, r.user_id, u.username,
		       MIN(r.created_at), MAX(r.created_at), COUNT(*),
		       COALESCE(STRING_AGG(DISTINCT r.model, ','), ''),
		       BOOL_OR(r.request_truncated OR r.response_truncated),
		       (SELECT r2.request_body FROM request_response_logs r2
		          WHERE r2.session_hash = r.session_hash
		          ORDER BY r2.created_at ASC LIMIT 1)
		FROM request_response_logs r
		LEFT JOIN users u ON u.id = r.user_id
		WHERE ` + strings.Join(where, " AND ") + `
		GROUP BY r.session_hash, r.user_id, u.username
		ORDER BY MIN(r.created_at) DESC
		LIMIT $` + limitPos

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []service.CCSessionSummary
	for rows.Next() {
		var s service.CCSessionSummary
		var userID sql.NullInt64
		var username sql.NullString
		var models string
		var firstBody []byte
		if err := rows.Scan(&s.SessionHash, &userID, &username, &s.StartedAt, &s.EndedAt,
			&s.RequestCount, &models, &s.AnyTruncated, &firstBody); err != nil {
			return nil, err
		}
		if userID.Valid {
			s.UserID = &userID.Int64
		}
		s.Username = username.String
		s.Models = splitNonEmpty(models)
		s.FirstPromptExcerpt = firstPromptExcerpt(firstBody)
		out = append(out, s)
	}
	return out, rows.Err()
}

func (r *ccSessionLogReadRepository) GetSessionRows(ctx context.Context, sessionHash string) ([]service.CCSessionLogRow, error) {
	ctx, cancel := context.WithTimeout(ctx, ccReadQueryTimeout)
	defer cancel()

	const query = `
		SELECT model, request_body, response_body, request_truncated, response_truncated
		FROM request_response_logs
		WHERE session_hash = $1
		ORDER BY created_at ASC`

	rows, err := r.db.QueryContext(ctx, query, sessionHash)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []service.CCSessionLogRow
	for rows.Next() {
		var row service.CCSessionLogRow
		if err := rows.Scan(&row.Model, &row.RequestBody, &row.ResponseBody,
			&row.RequestTruncated, &row.ResponseTruncated); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func (r *ccSessionLogReadRepository) SearchPrompts(ctx context.Context, q service.CCPromptSearchQuery) ([]service.CCPromptHit, error) {
	ctx, cancel := context.WithTimeout(ctx, ccReadQueryTimeout)
	defer cancel()

	var where []string
	var args []any
	where = append(where, "session_hash IS NOT NULL")
	// 转义 LIKE 元字符，避免用户输入当通配。
	args = append(args, "%"+escapeLike(q.Query)+"%")
	where = append(where, "convert_from(request_body, 'UTF8') ILIKE $"+strconv.Itoa(len(args))+" ESCAPE '\\'")
	if q.UserID != nil {
		args = append(args, *q.UserID)
		where = append(where, "user_id = $"+strconv.Itoa(len(args)))
	}
	if q.From != nil {
		args = append(args, *q.From)
		where = append(where, "created_at >= $"+strconv.Itoa(len(args)))
	}
	if q.To != nil {
		args = append(args, *q.To)
		where = append(where, "created_at <= $"+strconv.Itoa(len(args)))
	}
	args = append(args, q.Limit)
	limitPos := strconv.Itoa(len(args))

	query := `
		SELECT session_hash, user_id, created_at, request_body
		FROM request_response_logs
		WHERE ` + strings.Join(where, " AND ") + `
		ORDER BY created_at ASC
		LIMIT $` + limitPos

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	seen := make(map[string]struct{})
	var out []service.CCPromptHit
	for rows.Next() {
		var hit service.CCPromptHit
		var userID sql.NullInt64
		var body []byte
		if err := rows.Scan(&hit.SessionHash, &userID, &hit.CreatedAt, &body); err != nil {
			return nil, err
		}
		if _, ok := seen[hit.SessionHash]; ok {
			continue // 每会话只留首个命中
		}
		seen[hit.SessionHash] = struct{}{}
		if userID.Valid {
			hit.UserID = &userID.Int64
		}
		hit.Excerpt = matchExcerpt(string(body), q.Query)
		out = append(out, hit)
	}
	return out, rows.Err()
}

// --- helpers ---

func splitNonEmpty(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// firstPromptExcerpt 用归一化函数从最早一条请求体里取首个客户提问的摘要。
func firstPromptExcerpt(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	replay := service.NormalizeSession("", []service.CCSessionLogRow{{RequestBody: body}})
	ps := service.ExtractPrompts(replay)
	if len(ps) == 0 {
		return ""
	}
	return truncateRunes(strings.ReplaceAll(ps[0].Text, "\n", " "), 120)
}

// matchExcerpt 返回命中关键词周围的片段窗口。
func matchExcerpt(text, query string) string {
	flat := strings.ReplaceAll(text, "\n", " ")
	idx := strings.Index(strings.ToLower(flat), strings.ToLower(query))
	if idx < 0 {
		return truncateRunes(flat, 120)
	}
	start := idx - 40
	if start < 0 {
		start = 0
	}
	seg := flat[start:]
	return truncateRunes(seg, 160)
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

// escapeLike 复用本包 channel_repo_pricing.go 中的实现（转义 \ % _）。
