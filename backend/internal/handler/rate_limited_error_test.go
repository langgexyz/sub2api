//go:build unit

package handler

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func TestHumanizeDuration(t *testing.T) {
	cases := []struct {
		in   int
		want string
	}{
		{0, "1s"},       // clamp to >=1s
		{-5, "1s"},      // negative clamps too
		{8, "8s"},       // 单量级秒
		{59, "59s"},     // 边界
		{60, "1m"},      // 整分钟，秒为零不展示
		{150, "2m30s"},  // 两量级
		{3600, "1h"},    // 整小时
		{9339, "2h35m"}, // 截图实际值，跟用户期望「时分」对齐
		{86400, "1d"},   // 整天
		{90061, "1d1h"}, // 最多两量级，分秒被截断
	}
	for _, c := range cases {
		if got := humanizeDuration(c.in); got != c.want {
			t.Errorf("humanizeDuration(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

// rateLimitedFromNoAccounts 命中限流分支时：文案是 human-readable 时长（非裸秒数），
// 且 retryAfter 仍返回秒供 Retry-After header 使用。
func TestRateLimitedFromNoAccounts_HumanReadable(t *testing.T) {
	reset := time.Now().Add(2*time.Hour + 35*time.Minute)
	err := &service.AllAccountsRateLimitedError{ResetAt: reset, Count: 3}

	retryAfter, msg, ok := rateLimitedFromNoAccounts(err)
	if !ok {
		t.Fatalf("expected ok=true for AllAccountsRateLimitedError")
	}
	if retryAfter < 9000 || retryAfter > 9300 {
		t.Errorf("retryAfter seconds = %d, want ~9300 (for Retry-After header)", retryAfter)
	}
	if strings.Contains(msg, strconv.Itoa(retryAfter)+"s") {
		t.Errorf("msg should not expose raw seconds, got %q", msg)
	}
	if !strings.Contains(msg, "soonest") || !strings.Contains(msg, "h") {
		t.Errorf("msg should mention soonest account and an hour-scale duration, got %q", msg)
	}
}

// 非限流错误回退：不命中分支。
func TestRateLimitedFromNoAccounts_NonRateLimit(t *testing.T) {
	if _, _, ok := rateLimitedFromNoAccounts(errors.New("boom")); ok {
		t.Errorf("expected ok=false for non rate-limit error")
	}
}
