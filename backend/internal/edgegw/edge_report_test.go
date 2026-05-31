//go:build unit

package edgegw

import (
	"testing"
	"time"
)

func reportClock(t *time.Time) Clock { return func() time.Time { return *t } }

func TestAnomalyReporter_AggregatesByKind(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	r := newAnomalyReporter(reportClock(&now))

	r.record("lease_failed", "no account")
	now = now.Add(time.Second)
	r.record("lease_failed", "rate limited") // same kind -> count 2, latest message
	r.record("upstream_failed", "502 bad gateway")

	items := r.drain()
	if len(items) != 2 {
		t.Fatalf("want 2 kinds, got %d", len(items))
	}
	byKind := map[string]ReportItemView{}
	for _, it := range items {
		byKind[it.Kind] = ReportItemView{Count: it.Count, Message: it.Message, First: it.FirstAt, Last: it.LastAt}
	}
	lf, ok := byKind["lease_failed"]
	if !ok || lf.Count != 2 {
		t.Fatalf("lease_failed: want count 2, got %+v", lf)
	}
	if lf.Message != "rate limited" {
		t.Fatalf("lease_failed: want latest message 'rate limited', got %q", lf.Message)
	}
	if lf.Last <= lf.First {
		t.Fatalf("lease_failed: want last (%d) > first (%d)", lf.Last, lf.First)
	}
	if byKind["upstream_failed"].Count != 1 {
		t.Fatalf("upstream_failed: want count 1, got %d", byKind["upstream_failed"].Count)
	}
}

func TestAnomalyReporter_DrainResets(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	r := newAnomalyReporter(reportClock(&now))
	r.record("x", "1")
	if got := r.drain(); len(got) != 1 {
		t.Fatalf("first drain: want 1, got %d", len(got))
	}
	if got := r.drain(); got != nil {
		t.Fatalf("second drain after reset: want nil, got %v", got)
	}
}

func TestAnomalyReporter_NilSafe(t *testing.T) {
	var r *anomalyReporter
	r.record("x", "y") // must not panic
}

// ReportItemView is a local test view to assert without depending on the
// contract package's exact field ordering.
type ReportItemView struct {
	Count       int
	Message     string
	First, Last int64
}
