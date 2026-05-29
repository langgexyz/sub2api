//go:build unit

package quota

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

func TestReserveDebitsBalanceAndTracksReserved(t *testing.T) {
	l := New()
	l.SetBalance("k", 100)

	if err := l.Reserve("k", "r1", 30); err != nil {
		t.Fatalf("Reserve: unexpected error: %v", err)
	}

	if got := l.Balance("k"); got != 70 {
		t.Fatalf("Balance = %v, want 70", got)
	}
	if got := l.Reserved("k"); got != 30 {
		t.Fatalf("Reserved = %v, want 30", got)
	}
}

func TestReserveInsufficientLeavesBalanceUnchanged(t *testing.T) {
	l := New()
	l.SetBalance("k", 20)

	err := l.Reserve("k", "r1", 30)
	if !errors.Is(err, ErrInsufficient) {
		t.Fatalf("Reserve error = %v, want ErrInsufficient", err)
	}
	if got := l.Balance("k"); got != 20 {
		t.Fatalf("Balance = %v, want 20 (unchanged)", got)
	}
	if got := l.Reserved("k"); got != 0 {
		t.Fatalf("Reserved = %v, want 0", got)
	}
}

func TestReserveIdempotentDebitsOnce(t *testing.T) {
	l := New()
	l.SetBalance("k", 100)

	if err := l.Reserve("k", "r1", 30); err != nil {
		t.Fatalf("first Reserve: unexpected error: %v", err)
	}
	if err := l.Reserve("k", "r1", 30); err != nil {
		t.Fatalf("second Reserve: unexpected error: %v", err)
	}

	if got := l.Balance("k"); got != 70 {
		t.Fatalf("Balance = %v, want 70 (debited once)", got)
	}
	if got := l.Reserved("k"); got != 30 {
		t.Fatalf("Reserved = %v, want 30", got)
	}
}

func TestReconcileRefundsOverestimate(t *testing.T) {
	l := New()
	l.SetBalance("k", 100)

	if err := l.Reserve("k", "r1", 30); err != nil {
		t.Fatalf("Reserve: unexpected error: %v", err)
	}

	delta, err := l.Reconcile("k", "r1", 20)
	if err != nil {
		t.Fatalf("Reconcile: unexpected error: %v", err)
	}
	if delta != 10 {
		t.Fatalf("delta = %v, want 10 (refund)", delta)
	}
	// 100 - 30 (reserve) + 10 (refund) = 80
	if got := l.Balance("k"); got != 80 {
		t.Fatalf("Balance = %v, want 80", got)
	}
	if got := l.Reserved("k"); got != 0 {
		t.Fatalf("Reserved = %v, want 0", got)
	}
}

func TestReconcileChargesUnderestimate(t *testing.T) {
	l := New()
	l.SetBalance("k", 100)

	if err := l.Reserve("k", "r1", 30); err != nil {
		t.Fatalf("Reserve: unexpected error: %v", err)
	}

	delta, err := l.Reconcile("k", "r1", 50)
	if err != nil {
		t.Fatalf("Reconcile: unexpected error: %v", err)
	}
	if delta != -20 {
		t.Fatalf("delta = %v, want -20 (extra charge)", delta)
	}
	// 100 - 30 (reserve) + (-20) (extra charge) = 50
	if got := l.Balance("k"); got != 50 {
		t.Fatalf("Balance = %v, want 50", got)
	}
	if got := l.Reserved("k"); got != 0 {
		t.Fatalf("Reserved = %v, want 0", got)
	}
}

func TestReconcileIdempotentSecondCallNoOp(t *testing.T) {
	l := New()
	l.SetBalance("k", 100)

	if err := l.Reserve("k", "r1", 30); err != nil {
		t.Fatalf("Reserve: unexpected error: %v", err)
	}
	if _, err := l.Reconcile("k", "r1", 20); err != nil {
		t.Fatalf("first Reconcile: unexpected error: %v", err)
	}

	before := l.Balance("k")
	delta, err := l.Reconcile("k", "r1", 20)
	if err != nil {
		t.Fatalf("second Reconcile: unexpected error: %v", err)
	}
	if delta != 0 {
		t.Fatalf("delta = %v, want 0 (no-op)", delta)
	}
	if after := l.Balance("k"); after != before {
		t.Fatalf("Balance = %v, want %v (unchanged)", after, before)
	}
}

func TestReconcileUnknownRequestIsNoOp(t *testing.T) {
	l := New()
	l.SetBalance("k", 100)

	delta, err := l.Reconcile("k", "does-not-exist", 50)
	if err != nil {
		t.Fatalf("Reconcile: unexpected error: %v", err)
	}
	if delta != 0 {
		t.Fatalf("delta = %v, want 0", delta)
	}
	if got := l.Balance("k"); got != 100 {
		t.Fatalf("Balance = %v, want 100 (unchanged)", got)
	}
}

func TestConcurrentReserveDistinctRequestsNoRace(t *testing.T) {
	const (
		n        = 200
		estimate = 1.0
		initial  = float64(n) * estimate
	)
	l := New()
	l.SetBalance("k", initial)

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := l.Reserve("k", fmt.Sprintf("r-%d", i), estimate); err != nil {
				t.Errorf("Reserve r-%d: unexpected error: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	if got := l.Balance("k"); got != initial-float64(n)*estimate {
		t.Fatalf("Balance = %v, want %v", got, initial-float64(n)*estimate)
	}
	if got := l.Reserved("k"); got != float64(n)*estimate {
		t.Fatalf("Reserved = %v, want %v", got, float64(n)*estimate)
	}
}

func TestConcurrentReserveNoOversell(t *testing.T) {
	const (
		n        = 200
		estimate = 1.0
		// Balance is only enough for half of the goroutines.
		capacity = n / 2
		initial  = float64(capacity) * estimate
	)
	l := New()
	l.SetBalance("k", initial)

	var success int64
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := l.Reserve("k", fmt.Sprintf("r-%d", i), estimate)
			switch {
			case err == nil:
				atomic.AddInt64(&success, 1)
			case errors.Is(err, ErrInsufficient):
				// Expected once the balance is exhausted.
			default:
				t.Errorf("Reserve r-%d: unexpected error: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	// No oversell: successful reserves must not exceed available capacity.
	if got := atomic.LoadInt64(&success); float64(got)*estimate > initial {
		t.Fatalf("successful reserves * estimate = %v, exceeds initial balance %v", float64(got)*estimate, initial)
	}
	if got := l.Balance("k"); got < 0 {
		t.Fatalf("Balance = %v, must never go negative", got)
	}
}
