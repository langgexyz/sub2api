package service

import (
	"context"
	"fmt"
	"time"
)

// AllAccountsRateLimitedError 表示分组内本可调度的账号全部处于上游限流冷却中
// （RateLimitResetAt 在未来），候选为空的主因是限流而非缺账号。
// Unwrap 返回 ErrNoAvailableAccounts，保持所有现有 errors.Is(ErrNoAvailableAccounts) 判断兼容；
// handler 层用 errors.As 取出 ResetAt 映射为 429 + Retry-After，而非误导性的 503 No available accounts。
type AllAccountsRateLimitedError struct {
	ResetAt time.Time // 最早恢复时间（取候选账号中最小的 RateLimitResetAt）
	Count   int       // 处于限流冷却中的账号数
}

func (e *AllAccountsRateLimitedError) Error() string {
	return fmt.Sprintf("all %d upstream account(s) rate limited until %s", e.Count, e.ResetAt.Format(time.RFC3339))
}

// Unwrap 让 errors.Is(err, ErrNoAvailableAccounts) 仍成立，向后兼容所有现有分支与 ops 日志分类。
func (e *AllAccountsRateLimitedError) Unwrap() error { return ErrNoAvailableAccounts }

// accountBlockedOnlyByRateLimit 判断账号是否「除限流冷却外其余条件都满足调度」，
// 即 IsSchedulable() 仅因 RateLimitResetAt 在未来而返回 false。与 Account.IsSchedulable 逐条对齐。
func accountBlockedOnlyByRateLimit(a *Account, now time.Time) bool {
	if a == nil || !a.IsActive() || !a.Schedulable {
		return false
	}
	if a.AutoPauseOnExpired && a.ExpiresAt != nil && !now.Before(*a.ExpiresAt) {
		return false
	}
	if a.OverloadUntil != nil && now.Before(*a.OverloadUntil) {
		return false
	}
	if a.TempUnschedulableUntil != nil && now.Before(*a.TempUnschedulableUntil) {
		return false
	}
	if a.IsAPIKeyOrBedrock() && a.IsQuotaExceeded() {
		return false
	}
	return a.RateLimitResetAt != nil && now.Before(*a.RateLimitResetAt)
}

// computeAllAccountsRateLimited 在选号失败（候选为空）时判定：分组内是否存在「本可调度、仅因上游限流被挡」
// 的账号。存在则返回携带最早恢复时间的 *AllAccountsRateLimitedError，否则返回 nil（调用方回退 ErrNoAvailableAccounts）。
// 抽成包级函数供 GatewayService / OpenAIGatewayService 共用（两者各持独立 accountRepo）。
func computeAllAccountsRateLimited(ctx context.Context, repo AccountRepository, groupID *int64) *AllAccountsRateLimitedError {
	if groupID == nil || repo == nil {
		return nil
	}
	accounts, err := repo.ListByGroup(ctx, *groupID)
	if err != nil || len(accounts) == 0 {
		return nil
	}
	now := time.Now()
	var earliest *time.Time
	count := 0
	for i := range accounts {
		a := &accounts[i]
		if !accountBlockedOnlyByRateLimit(a, now) {
			continue
		}
		count++
		if earliest == nil || a.RateLimitResetAt.Before(*earliest) {
			earliest = a.RateLimitResetAt
		}
	}
	if count == 0 || earliest == nil {
		return nil
	}
	return &AllAccountsRateLimitedError{ResetAt: *earliest, Count: count}
}

// allAccountsRateLimitedErr 见 computeAllAccountsRateLimited。
func (s *GatewayService) allAccountsRateLimitedErr(ctx context.Context, groupID *int64) *AllAccountsRateLimitedError {
	return computeAllAccountsRateLimited(ctx, s.accountRepo, groupID)
}
