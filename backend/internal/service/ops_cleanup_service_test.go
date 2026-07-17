package service

import (
	"testing"
	"time"
)

func TestOpsCleanupPlan(t *testing.T) {
	now := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name         string
		days         int
		wantOK       bool
		wantTruncate bool
		wantCutoff   time.Time
	}{
		{name: "negative skips", days: -1, wantOK: false},
		{name: "zero truncates", days: 0, wantOK: true, wantTruncate: true},
		{name: "positive yields past cutoff", days: 7, wantOK: true, wantCutoff: now.AddDate(0, 0, -7)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cutoff, truncate, ok := opsCleanupPlan(now, tc.days)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if truncate != tc.wantTruncate {
				t.Fatalf("truncate = %v, want %v", truncate, tc.wantTruncate)
			}
			if !tc.wantTruncate && !cutoff.Equal(tc.wantCutoff) {
				t.Fatalf("cutoff = %v, want %v", cutoff, tc.wantCutoff)
			}
		})
	}
}

func TestRequestResponseLogPlanDays(t *testing.T) {
	now := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name   string
		days   int
		wantOK bool
	}{
		// 0 = 永不清理：经映射后 plan 必须跳过，绝不允许落到 TRUNCATE 分支。
		{name: "zero means never clean", days: 0, wantOK: false},
		{name: "positive keeps ttl delete", days: 30, wantOK: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cutoff, truncate, ok := opsCleanupPlan(now, requestResponseLogPlanDays(tc.days))
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if truncate {
				t.Fatalf("request_response_logs must never be truncated, got truncate=true")
			}
			if ok && !cutoff.Equal(now.AddDate(0, 0, -tc.days)) {
				t.Fatalf("cutoff = %v, want %v", cutoff, now.AddDate(0, 0, -tc.days))
			}
		})
	}
}

func TestIsMissingRelationError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil is not missing", err: nil, want: false},
		{name: "match relation does not exist", err: fakeErr(`pq: relation "ops_error_logs" does not exist`), want: true},
		{name: "match case-insensitive", err: fakeErr(`ERROR: Relation "x" Does Not Exist`), want: true},
		{name: "non-matching error", err: fakeErr("connection refused"), want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isMissingRelationError(tc.err); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

type fakeErr string

func (e fakeErr) Error() string { return string(e) }
