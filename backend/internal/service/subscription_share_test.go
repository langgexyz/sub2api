//go:build unit

package service

import "testing"

func TestShareOverLimit(t *testing.T) {
	// 安全系数 0.85
	cases := []struct {
		name     string
		used     float64
		capacity float64
		slots    int
		want     bool
	}{
		{"no_capacity_no_limit", 9999, 0, 1, false},      // 无反推容量 → 不限制
		{"under_share_n1", 100, 191, 1, false},           // share=162.35，100 未到
		{"over_share_n1", 170, 191, 1, true},             // 170 ≥ 162.35
		{"at_share_boundary", 162.35, 191, 1, true},      // 恰好达到即拒（硬顶）
		{"n2_smaller_share_triggers", 100, 191, 2, true}, // N=2 → share=81.18，100 超
		{"n2_under", 60, 191, 2, false},                  // 60 < 81.18
		{"slots_zero_treated_as_one", 170, 191, 0, true}, // slots<1 当 1
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := shareOverLimit(c.used, c.capacity, c.slots)
			if got != c.want {
				t.Fatalf("shareOverLimit(used=%.2f cap=%.2f slots=%d) = %v, want %v",
					c.used, c.capacity, c.slots, got, c.want)
			}
		})
	}
}
