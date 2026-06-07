package repository

import (
	"context"
	"database/sql"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

const (
	requestResponseLogQueueSize  = 1024
	requestResponseLogWorkers    = 2
	requestResponseLogInsertTime = 5 * time.Second
)

type requestResponseLogRepository struct {
	db      *sql.DB
	queue   chan *service.RequestResponseLog
	dropped atomic.Int64
	once    sync.Once
}

// NewRequestResponseLogRepository 构造异步请求/响应原文写入仓储。
// db 为 nil（如部分测试 / 退化模式）时 Enqueue 变为无操作。
func NewRequestResponseLogRepository(db *sql.DB) service.RequestResponseLogRepository {
	r := &requestResponseLogRepository{
		db:    db,
		queue: make(chan *service.RequestResponseLog, requestResponseLogQueueSize),
	}
	if db != nil {
		r.start()
	}
	return r
}

func (r *requestResponseLogRepository) start() {
	r.once.Do(func() {
		for i := 0; i < requestResponseLogWorkers; i++ {
			go r.worker()
		}
	})
}

func (r *requestResponseLogRepository) Enqueue(log *service.RequestResponseLog) {
	if r == nil || r.db == nil || log == nil {
		return
	}
	select {
	case r.queue <- log:
	default:
		// 队列满：丢弃并计数，绝不阻塞请求热路径。
		if n := r.dropped.Add(1); n%100 == 1 {
			logger.LegacyPrintf("repository.request_response_log", "queue full, dropped %d records so far", n)
		}
	}
}

func (r *requestResponseLogRepository) worker() {
	for log := range r.queue {
		r.insert(log)
	}
}

func (r *requestResponseLogRepository) insert(log *service.RequestResponseLog) {
	ctx, cancel := context.WithTimeout(context.Background(), requestResponseLogInsertTime)
	defer cancel()

	const query = `
		INSERT INTO request_response_logs (
			request_id, session_hash, user_id, api_key_id, model, endpoint,
			status_code, stream, request_body, response_body,
			request_truncated, response_truncated
		) VALUES (
			$1, $2, $3, $4, $5, $6,
			$7, $8, $9, $10,
			$11, $12
		)`

	if _, err := r.db.ExecContext(ctx, query,
		log.RequestID,
		nullableString(log.SessionHash),
		log.UserID,
		log.APIKeyID,
		log.Model,
		log.Endpoint,
		log.StatusCode,
		log.Stream,
		log.RequestBody,
		log.ResponseBody,
		log.RequestTruncated,
		log.ResponseTruncated,
	); err != nil {
		logger.LegacyPrintf("repository.request_response_log", "insert failed (request_id=%s): %v", log.RequestID, err)
	}
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
