//go:build unit

package service

import (
	"math"
	"testing"
	"time"
)

func TestBuildWindowProgress(t *testing.T) {
	now := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)

	t.Run("normal", func(t *testing.T) {
		reset := now.Add(4 * 24 * time.Hour)
		start := reset.Add(-7 * 24 * time.Hour)
		w := buildWindowProgress(166, 21, start, reset, now)
		if math.Abs(w.Percentage-12.65) > 0.1 {
			t.Fatalf("percentage = %.2f, want ~12.65", w.Percentage)
		}
		if math.Abs(w.RemainingUSD-145) > 0.01 {
			t.Fatalf("remaining = %.2f, want 145", w.RemainingUSD)
		}
		if w.ResetsInSeconds <= 0 {
			t.Fatalf("resets_in_seconds should be positive, got %d", w.ResetsInSeconds)
		}
	})

	t.Run("over_limit_clamps", func(t *testing.T) {
		w := buildWindowProgress(6.65, 8, now.Add(-5*time.Hour), now.Add(1*time.Hour), now)
		if w.Percentage != 100 {
			t.Fatalf("over-limit percentage must clamp to 100, got %.2f", w.Percentage)
		}
		if w.RemainingUSD != 0 {
			t.Fatalf("over-limit remaining must clamp to 0, got %.2f", w.RemainingUSD)
		}
	})

	t.Run("expired_reset_clamps_to_zero", func(t *testing.T) {
		w := buildWindowProgress(100, 10, now.Add(-10*time.Hour), now.Add(-1*time.Hour), now)
		if w.ResetsInSeconds != 0 {
			t.Fatalf("past reset must clamp resets_in_seconds to 0, got %d", w.ResetsInSeconds)
		}
	})

	t.Run("zero_limit_no_div", func(t *testing.T) {
		w := buildWindowProgress(0, 5, now, now.Add(time.Hour), now)
		if w.Percentage != 0 {
			t.Fatalf("zero limit must give 0 percentage (no div by zero), got %.2f", w.Percentage)
		}
	})
}
