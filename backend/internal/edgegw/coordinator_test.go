//go:build unit

package edgegw

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// --- fakes ---

type fakeAdmission struct {
	mu         sync.Mutex
	reserveErr error
	reserved   int
	released   int
	lastSlot   string
}

func (f *fakeAdmission) Reserve(_ context.Context, _, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reserveErr != nil {
		return "", f.reserveErr
	}
	f.reserved++
	f.lastSlot = "slot-1"
	return f.lastSlot, nil
}

func (f *fakeAdmission) Release(_ context.Context, _ string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released++
}

type fakeBilling struct{ err error }

func (f *fakeBilling) CheckEligibility(_ context.Context, _, _ string) error { return f.err }

type fakeScheduler struct {
	candidates []Candidate
	err        error
	calls      int
}

func (f *fakeScheduler) Select(_ context.Context, _ LeaseRequest) ([]Candidate, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return append([]Candidate(nil), f.candidates...), nil
}

type fakeSticky struct {
	boundID string
	found   bool
	binds   []string
}

func (f *fakeSticky) Lookup(_ context.Context, _ string) (string, bool) {
	return f.boundID, f.found
}
func (f *fakeSticky) Bind(_ context.Context, _, accountID string) {
	f.binds = append(f.binds, accountID)
}

type fakeUsage struct {
	mu      sync.Mutex
	records []SettleRequest
}

func (f *fakeUsage) Record(_ context.Context, s SettleRequest) (float64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records = append(f.records, s)
	return 1.5, nil
}

type fakeMinter struct{}

func (fakeMinter) Mint(accountID string, _ time.Duration) (string, int64) {
	return "tok-" + accountID, 9999
}

func threeCandidates() []Candidate {
	return []Candidate{
		{AccountID: "a", UpstreamBaseURL: "http://a"},
		{AccountID: "b", UpstreamBaseURL: "http://b"},
		{AccountID: "c", UpstreamBaseURL: "http://c"},
	}
}

func fixedClock() Clock {
	t := time.Unix(1_700_000_000, 0)
	return func() time.Time { return t }
}

// --- tests ---

func TestLease_HappyPath(t *testing.T) {
	adm := &fakeAdmission{}
	sch := &fakeScheduler{candidates: threeCandidates()}
	usage := &fakeUsage{}
	co := NewCoordinator(Config{
		Admission: adm, Scheduler: sch, Usage: usage,
		Minter: fakeMinter{}, Now: fixedClock(),
	})

	res, err := co.Lease(context.Background(), LeaseRequest{APIKey: "k", Model: "m", RequestID: "r1"})
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	if len(res.Candidates) != 3 {
		t.Fatalf("want 3 candidates, got %d", len(res.Candidates))
	}
	primary, ok := res.Primary()
	if !ok || primary.AccountID != "a" {
		t.Fatalf("primary = %+v ok=%v", primary, ok)
	}
	if primary.LeaseToken != "tok-a" {
		t.Fatalf("token not minted: %q", primary.LeaseToken)
	}
	if adm.reserved != 1 || adm.released != 0 {
		t.Fatalf("admission balance wrong: reserved=%d released=%d", adm.reserved, adm.released)
	}
}

func TestLease_InvalidRequest_NoSlotReserved(t *testing.T) {
	adm := &fakeAdmission{}
	co := NewCoordinator(Config{Admission: adm, Scheduler: &fakeScheduler{}, Usage: &fakeUsage{}})
	if _, err := co.Lease(context.Background(), LeaseRequest{APIKey: "", Model: "m"}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("want ErrInvalidRequest, got %v", err)
	}
	if adm.reserved != 0 {
		t.Fatalf("slot should not be reserved on invalid request")
	}
}

func TestLease_ConcurrencyFull_ShortCircuits(t *testing.T) {
	adm := &fakeAdmission{reserveErr: ErrConcurrencyFull}
	sch := &fakeScheduler{candidates: threeCandidates()}
	co := NewCoordinator(Config{Admission: adm, Scheduler: sch, Usage: &fakeUsage{}})
	if _, err := co.Lease(context.Background(), LeaseRequest{APIKey: "k", Model: "m"}); !errors.Is(err, ErrConcurrencyFull) {
		t.Fatalf("want ErrConcurrencyFull, got %v", err)
	}
	if sch.calls != 0 {
		t.Fatalf("scheduler must not be called when admission rejects")
	}
}

func TestLease_BillingIneligible_ReleasesSlot(t *testing.T) {
	adm := &fakeAdmission{}
	co := NewCoordinator(Config{
		Admission: adm, Billing: &fakeBilling{err: ErrBillingIneligible},
		Scheduler: &fakeScheduler{candidates: threeCandidates()}, Usage: &fakeUsage{},
	})
	if _, err := co.Lease(context.Background(), LeaseRequest{APIKey: "k", Model: "m"}); !errors.Is(err, ErrBillingIneligible) {
		t.Fatalf("want ErrBillingIneligible, got %v", err)
	}
	if adm.reserved != 1 || adm.released != 1 {
		t.Fatalf("slot must be released on billing rejection: reserved=%d released=%d", adm.reserved, adm.released)
	}
}

func TestLease_NoAccount_ReleasesSlot(t *testing.T) {
	adm := &fakeAdmission{}
	co := NewCoordinator(Config{Admission: adm, Scheduler: &fakeScheduler{candidates: nil}, Usage: &fakeUsage{}})
	if _, err := co.Lease(context.Background(), LeaseRequest{APIKey: "k", Model: "m"}); !errors.Is(err, ErrNoAccount) {
		t.Fatalf("want ErrNoAccount, got %v", err)
	}
	if adm.released != 1 {
		t.Fatalf("slot must be released when no account: released=%d", adm.released)
	}
}

func TestLease_StickyPromotesBoundAccount(t *testing.T) {
	adm := &fakeAdmission{}
	sticky := &fakeSticky{boundID: "c", found: true}
	co := NewCoordinator(Config{
		Admission: adm, Scheduler: &fakeScheduler{candidates: threeCandidates()},
		Sticky: sticky, Usage: &fakeUsage{}, Minter: fakeMinter{},
	})
	res, err := co.Lease(context.Background(), LeaseRequest{APIKey: "k", Model: "m", SessionHash: "s"})
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	primary, _ := res.Primary()
	if primary.AccountID != "c" {
		t.Fatalf("sticky-bound account c must be primary, got %s", primary.AccountID)
	}
	if len(res.Candidates) != 3 {
		t.Fatalf("promotion must preserve all candidates, got %d", len(res.Candidates))
	}
	if len(sticky.binds) == 0 || sticky.binds[len(sticky.binds)-1] != "c" {
		t.Fatalf("sticky must rebind chosen primary, binds=%v", sticky.binds)
	}
}

func TestSettle_RecordsReleasesAndBinds(t *testing.T) {
	adm := &fakeAdmission{}
	usage := &fakeUsage{}
	sticky := &fakeSticky{}
	co := NewCoordinator(Config{Admission: adm, Scheduler: &fakeScheduler{}, Usage: usage, Sticky: sticky})
	res, err := co.Settle(context.Background(), SettleRequest{
		RequestID: "r1", AccountID: "a", SlotID: "slot-1", SessionHash: "s",
		InputTokens: 100, OutputTokens: 200, StatusCode: 200,
	})
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if !res.Accepted || res.QuotaCharged != 1.5 {
		t.Fatalf("unexpected settle result: %+v", res)
	}
	if len(usage.records) != 1 {
		t.Fatalf("usage must be recorded once, got %d", len(usage.records))
	}
	if adm.released != 1 {
		t.Fatalf("slot must be released, released=%d", adm.released)
	}
	if len(sticky.binds) != 1 || sticky.binds[0] != "a" {
		t.Fatalf("settle must rebind sticky to served account, binds=%v", sticky.binds)
	}
}

func TestSettle_IdempotentOnRequestID(t *testing.T) {
	adm := &fakeAdmission{}
	usage := &fakeUsage{}
	co := NewCoordinator(Config{Admission: adm, Scheduler: &fakeScheduler{}, Usage: usage})
	req := SettleRequest{RequestID: "dup", AccountID: "a", SlotID: "slot-1", StatusCode: 200}

	first, err := co.Settle(context.Background(), req)
	if err != nil {
		t.Fatalf("first settle: %v", err)
	}
	if first.Duplicate {
		t.Fatalf("first settle must not be a duplicate")
	}
	second, err := co.Settle(context.Background(), req)
	if err != nil {
		t.Fatalf("second settle: %v", err)
	}
	if !second.Duplicate {
		t.Fatalf("second settle must be flagged duplicate")
	}
	if len(usage.records) != 1 {
		t.Fatalf("duplicate settle must not double-charge: records=%d", len(usage.records))
	}
	if adm.released != 1 {
		t.Fatalf("duplicate settle must not double-release: released=%d", adm.released)
	}
}

func TestSettle_NoBindOnUpstreamError(t *testing.T) {
	sticky := &fakeSticky{}
	co := NewCoordinator(Config{Admission: &fakeAdmission{}, Scheduler: &fakeScheduler{}, Usage: &fakeUsage{}, Sticky: sticky})
	_, err := co.Settle(context.Background(), SettleRequest{
		RequestID: "r", AccountID: "a", SlotID: "slot-1", SessionHash: "s", StatusCode: 503,
	})
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if len(sticky.binds) != 0 {
		t.Fatalf("must not bind sticky on upstream error status, binds=%v", sticky.binds)
	}
}
