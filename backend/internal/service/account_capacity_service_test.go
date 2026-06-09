//go:build unit

package service

import (
	"math"
	"testing"
)

func almostEqual(a, b, eps float64) bool {
	return math.Abs(a-b) <= eps
}

func TestInferCapacity(t *testing.T) {
	cases := []struct {
		name    string
		cost    float64
		util    float64
		minUtil float64
		wantCap float64
		wantOK  bool
	}{
		// prod 实测：账号 009，7d $21.03 / util 0.11 → $191
		{"prod_7d", 21.0313, 0.11, 0.05, 191.19, true},
		// prod 实测：5h $4.51 / util 0.18 → $25
		{"prod_5h", 4.5128, 0.18, 0.05, 25.07, true},
		{"below_threshold", 10, 0.03, 0.05, 0, false},
		{"zero_util", 10, 0, 0.05, 0, false},
		// 无信号：窗口内零成本(如凌晨低峰 5h 无流量)即便 util 快照非零也跳过，不写假的 0
		{"zero_cost", 0, 0.18, 0.05, 0, false},
		{"negative_cost", -1, 0.2, 0.05, 0, false},
		{"exactly_threshold", 10, 0.05, 0.05, 200, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cap, ok := inferCapacity(c.cost, c.util, c.minUtil)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if ok && !almostEqual(cap, c.wantCap, 0.5) {
				t.Fatalf("cap = %.4f, want ~%.4f", cap, c.wantCap)
			}
		})
	}
}

func TestEWMACapacity(t *testing.T) {
	cases := []struct {
		name             string
		prev, raw, alpha float64
		want             float64
	}{
		{"no_prev", 0, 191, 0.3, 191},
		{"smooth", 200, 191, 0.3, 197.3}, // 0.3*191 + 0.7*200
		{"alpha_out_of_range_defaults", 200, 191, 0, 197.3},
		{"alpha_one_takes_raw", 200, 191, 1, 191},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ewmaCapacity(c.prev, c.raw, c.alpha)
			if !almostEqual(got, c.want, 0.01) {
				t.Fatalf("ewma = %.4f, want %.4f", got, c.want)
			}
		})
	}
}

func TestSnapTier(t *testing.T) {
	const baseline = 191.0
	cases := []struct {
		name     string
		capacity float64
		want     string
	}{
		{"exact_pro", 191, TierPro},
		{"exact_5x", 955, TierMax5x},
		{"exact_10x", 1910, TierMax10x},
		{"exact_20x", 3820, TierMax20x},
		{"near_5x", 900, TierMax5x},
		{"near_10x", 2000, TierMax10x},
		{"low_to_pro", 150, TierPro},
		{"zero_unknown", 0, TierUnknown},
		{"negative_unknown", -5, TierUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := snapTier(c.capacity, baseline)
			if got != c.want {
				t.Fatalf("snapTier(%.0f) = %s, want %s", c.capacity, got, c.want)
			}
		})
	}
	if snapTier(191, 0) != TierUnknown {
		t.Fatalf("zero baseline must be unknown")
	}
}

func TestCapacityConfidence(t *testing.T) {
	cases := []struct {
		name      string
		util, ref float64
		want      float64
	}{
		{"full_at_ref", 0.5, 0.5, 1.0},
		{"half", 0.25, 0.5, 0.5},
		{"low", 0.11, 0.5, 0.22},
		{"clamp_high", 0.8, 0.5, 1.0},
		{"zero_ref_defaults", 0.25, 0, 0.5},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := capacityConfidence(c.util, c.ref)
			if !almostEqual(got, c.want, 0.001) {
				t.Fatalf("confidence = %.4f, want %.4f", got, c.want)
			}
		})
	}
}
