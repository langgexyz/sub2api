package handler

import (
	"errors"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// rateLimitedFromNoAccounts 判断「无可用账号」的错误是否实为「分组内账号全部处于上游限流冷却中」。
// 是则返回 (retryAfterSeconds, 面向客户端的限流文案, true)，调用方据此返回 429 rate_limit_error + Retry-After，
// 而非误导性的 503 No available accounts。否则返回 (0, "", false)，调用方回退原 503 行为。
//
// 之所以放在 handler 层统一判定：service 层把账号全限流包装成 *service.AllAccountsRateLimitedError，
// 其 Unwrap 返回 service.ErrNoAvailableAccounts，所以现有 errors.Is 分支与 ops 容量分类完全不受影响。
func rateLimitedFromNoAccounts(err error) (int, string, bool) {
	var rlErr *service.AllAccountsRateLimitedError
	if !errors.As(err, &rlErr) {
		return 0, "", false
	}
	retryAfter := int(time.Until(rlErr.ResetAt).Seconds())
	if retryAfter < 1 {
		retryAfter = 1
	}
	msg := fmt.Sprintf("All upstream accounts are temporarily rate limited; please retry after %s (~%ds).",
		rlErr.ResetAt.Format("15:04:05 MST"), retryAfter)
	return retryAfter, msg, true
}
