package handler

import (
	"testing"
	"time"
)

// TestConcurrencyHelper_SlotWaitTimeout 验证并发槽等待上限的取值优先级：
// 配置了正值用配置值，未配置（<=0）回退到内置默认 maxConcurrencyWait。
func TestConcurrencyHelper_SlotWaitTimeout(t *testing.T) {
	h := NewConcurrencyHelper(nil, SSEPingFormatNone, time.Second)

	if got := h.slotWaitTimeout(); got != maxConcurrencyWait {
		t.Fatalf("default slotWaitTimeout = %v, want %v", got, maxConcurrencyWait)
	}

	h.SetWaitTimeout(30 * time.Minute)
	if got := h.slotWaitTimeout(); got != 30*time.Minute {
		t.Fatalf("configured slotWaitTimeout = %v, want %v", got, 30*time.Minute)
	}

	// 非正值显式回退到内置默认。
	h.SetWaitTimeout(0)
	if got := h.slotWaitTimeout(); got != maxConcurrencyWait {
		t.Fatalf("zero slotWaitTimeout = %v, want fallback %v", got, maxConcurrencyWait)
	}
	h.SetWaitTimeout(-1)
	if got := h.slotWaitTimeout(); got != maxConcurrencyWait {
		t.Fatalf("negative slotWaitTimeout = %v, want fallback %v", got, maxConcurrencyWait)
	}
}
