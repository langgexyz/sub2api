//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"
)

// rateLimitedRepoStub 嵌入完整 mock 满足 AccountRepository，仅覆写 ListByGroup 返回指定账号。
type rateLimitedRepoStub struct {
	*mockAccountRepoForPlatform
	groupAccounts []Account
}

func (s *rateLimitedRepoStub) ListByGroup(ctx context.Context, groupID int64) ([]Account, error) {
	return s.groupAccounts, nil
}

func baseSchedulableAccount(id int64) Account {
	return Account{
		ID:          id,
		Status:      StatusActive,
		Schedulable: true,
	}
}

func TestAccountBlockedOnlyByRateLimit(t *testing.T) {
	now := time.Now()
	future := now.Add(2 * time.Hour)
	past := now.Add(-2 * time.Hour)

	tests := []struct {
		name string
		mut  func(a *Account)
		want bool
	}{
		{
			name: "rate limited only",
			mut:  func(a *Account) { a.RateLimitResetAt = ptrTime(future) },
			want: true,
		},
		{
			name: "rate limit reset in past",
			mut:  func(a *Account) { a.RateLimitResetAt = ptrTime(past) },
			want: false,
		},
		{
			name: "no rate limit",
			mut:  func(a *Account) {},
			want: false,
		},
		{
			name: "rate limited but also overloaded",
			mut: func(a *Account) {
				a.RateLimitResetAt = ptrTime(future)
				a.OverloadUntil = ptrTime(future)
			},
			want: false,
		},
		{
			name: "rate limited but also temp unschedulable",
			mut: func(a *Account) {
				a.RateLimitResetAt = ptrTime(future)
				a.TempUnschedulableUntil = ptrTime(future)
			},
			want: false,
		},
		{
			name: "rate limited but expired with auto pause",
			mut: func(a *Account) {
				a.RateLimitResetAt = ptrTime(future)
				a.AutoPauseOnExpired = true
				a.ExpiresAt = ptrTime(past)
			},
			want: false,
		},
		{
			name: "rate limited but not active",
			mut: func(a *Account) {
				a.RateLimitResetAt = ptrTime(future)
				a.Status = "disabled"
			},
			want: false,
		},
		{
			name: "rate limited but schedulable flag off",
			mut: func(a *Account) {
				a.RateLimitResetAt = ptrTime(future)
				a.Schedulable = false
			},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := baseSchedulableAccount(1)
			tc.mut(&a)
			if got := accountBlockedOnlyByRateLimit(&a, now); got != tc.want {
				t.Fatalf("accountBlockedOnlyByRateLimit = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestComputeAllAccountsRateLimited(t *testing.T) {
	now := time.Now()
	early := now.Add(30 * time.Minute)
	late := now.Add(3 * time.Hour)
	past := now.Add(-time.Hour)
	groupID := int64(7)

	newStub := func(accs []Account) *rateLimitedRepoStub {
		return &rateLimitedRepoStub{
			mockAccountRepoForPlatform: &mockAccountRepoForPlatform{},
			groupAccounts:              accs,
		}
	}

	t.Run("all rate limited returns earliest reset", func(t *testing.T) {
		a1 := baseSchedulableAccount(1)
		a1.RateLimitResetAt = ptrTime(late)
		a2 := baseSchedulableAccount(2)
		a2.RateLimitResetAt = ptrTime(early)
		repo := newStub([]Account{a1, a2})

		err := computeAllAccountsRateLimited(context.Background(), repo, &groupID)
		if err == nil {
			t.Fatal("expected AllAccountsRateLimitedError, got nil")
		}
		if err.Count != 2 {
			t.Fatalf("Count = %d, want 2", err.Count)
		}
		if !err.ResetAt.Equal(early) {
			t.Fatalf("ResetAt = %v, want earliest %v", err.ResetAt, early)
		}
		if !errors.Is(err, ErrNoAvailableAccounts) {
			t.Fatal("expected errors.Is(err, ErrNoAvailableAccounts) to hold via Unwrap")
		}
	})

	t.Run("empty group returns nil", func(t *testing.T) {
		if err := computeAllAccountsRateLimited(context.Background(), newStub(nil), &groupID); err != nil {
			t.Fatalf("want nil, got %v", err)
		}
	})

	t.Run("non rate-limit unschedulable returns nil", func(t *testing.T) {
		a := baseSchedulableAccount(1)
		a.OverloadUntil = ptrTime(late) // overloaded, not rate limited
		if err := computeAllAccountsRateLimited(context.Background(), newStub([]Account{a}), &groupID); err != nil {
			t.Fatalf("want nil (no rate-limited account), got %v", err)
		}
	})

	t.Run("expired reset returns nil", func(t *testing.T) {
		a := baseSchedulableAccount(1)
		a.RateLimitResetAt = ptrTime(past)
		if err := computeAllAccountsRateLimited(context.Background(), newStub([]Account{a}), &groupID); err != nil {
			t.Fatalf("want nil (reset already passed), got %v", err)
		}
	})

	t.Run("mixed counts only rate-limited-only accounts", func(t *testing.T) {
		rl := baseSchedulableAccount(1)
		rl.RateLimitResetAt = ptrTime(early)
		overloaded := baseSchedulableAccount(2)
		overloaded.OverloadUntil = ptrTime(late)
		err := computeAllAccountsRateLimited(context.Background(), newStub([]Account{rl, overloaded}), &groupID)
		if err == nil {
			t.Fatal("expected error when at least one account is rate-limited-only")
		}
		if err.Count != 1 {
			t.Fatalf("Count = %d, want 1", err.Count)
		}
		if !err.ResetAt.Equal(early) {
			t.Fatalf("ResetAt = %v, want %v", err.ResetAt, early)
		}
	})

	t.Run("nil groupID returns nil", func(t *testing.T) {
		a := baseSchedulableAccount(1)
		a.RateLimitResetAt = ptrTime(early)
		if err := computeAllAccountsRateLimited(context.Background(), newStub([]Account{a}), nil); err != nil {
			t.Fatalf("want nil for nil groupID, got %v", err)
		}
	})
}

func TestAllAccountsRateLimitedErrorMessage(t *testing.T) {
	err := &AllAccountsRateLimitedError{ResetAt: time.Now().Add(time.Hour), Count: 3}
	if !errors.Is(err, ErrNoAvailableAccounts) {
		t.Fatal("AllAccountsRateLimitedError must unwrap to ErrNoAvailableAccounts")
	}
	if msg := err.Error(); msg == "" {
		t.Fatal("Error() must be non-empty")
	}
}
