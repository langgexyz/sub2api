package cchub

import (
	"context"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/ccgw/contract"
)

// Admission is the central concurrency/quota gate. Reserve is non-blocking: it
// either reserves a slot and returns its id, or returns contract.ErrWaitQueueFull /
// contract.ErrConcurrencyFull. The blocking wait-with-keepalive that the monolith does
// today lives at the edge (which holds the client connection); the center only
// counts. Release frees the slot at Settle time.
type Admission interface {
	Reserve(ctx context.Context, apiKey, model string) (slotID string, err error)
	Release(ctx context.Context, slotID string)
}

// Billing checks whether the api key may spend right now (balance / quota /
// subscription). It is a pure precheck; actual deduction happens in UsageSink.
type Billing interface {
	CheckEligibility(ctx context.Context, apiKey, model string) error
}

// Scheduler returns ranked schedulable candidates for the request, excluding
// none initially. Candidates[0] is the best pick. Returns contract.ErrNoAccount when the
// group has no usable account.
type Scheduler interface {
	Select(ctx context.Context, req contract.LeaseRequest) ([]contract.Candidate, error)
}

// StickyStore maps a session hash to a previously bound account so a
// conversation keeps hitting the same upstream account (and thus the same edge
// / egress IP).
type StickyStore interface {
	Lookup(ctx context.Context, key string) (accountID string, ok bool)
	Bind(ctx context.Context, key, accountID string)
}

// UsageSink records settled usage and returns the quota charged. It must be
// idempotent on requestID at the ledger level; the coordinator also dedupes.
type UsageSink interface {
	Record(ctx context.Context, s contract.SettleRequest) (charged float64, err error)
}

// TokenMinter produces the short-lived credential the edge presents upstream.
// In production this wraps the account's real upstream access token with a
// per-request validity window; the edge uses it once and discards it.
type TokenMinter interface {
	Mint(accountID string, ttl time.Duration) (token string, expiresAt int64)
}

// Clock is injectable so tests are deterministic (Date.now is otherwise banned
// in this environment's workflow scripts; here it just keeps tests stable).
type Clock func() time.Time

// Coordinator is the control plane. It composes the small interfaces above into
// the two RPCs the edge consumes. It is safe for concurrent use.
type Coordinator struct {
	admission Admission
	billing   Billing
	scheduler Scheduler
	sticky    StickyStore
	usage     UsageSink
	minter    TokenMinter
	quota     QuotaReserver

	leaseTTL      time.Duration
	leaseEstimate float64
	costOf        func(contract.SettleRequest) float64
	now           Clock

	mu      sync.Mutex
	settled map[string]contract.SettleResult // requestID -> result, for idempotency
}

// QuotaReserver pre-debits an estimated cost at Lease and reconciles the actual
// cost at Settle, preventing double-spend across concurrent edges. Satisfied by
// ccgw/quota.Ledger; modeled as an interface so the coordinator
// stays decoupled and fake-testable.
type QuotaReserver interface {
	Reserve(apiKey, requestID string, estimate float64) error
	Reconcile(apiKey, requestID string, actual float64) (float64, error)
}

// Config configures a Coordinator. Billing, StickyStore, TokenMinter and Quota
// are optional; nil disables that step.
type Config struct {
	Admission Admission
	Billing   Billing
	Scheduler Scheduler
	Sticky    StickyStore
	Usage     UsageSink
	Minter    TokenMinter
	Quota     QuotaReserver
	// LeaseEstimate is the cost pre-debited at Lease when Quota is set.
	LeaseEstimate float64
	// CostFunc derives the actual cost reconciled at Settle. Defaults to
	// (input+output)/1000.
	CostFunc func(contract.SettleRequest) float64
	LeaseTTL time.Duration
	Now      Clock
}

// NewCoordinator builds a Coordinator. Admission, Scheduler and Usage are
// required.
func NewCoordinator(cfg Config) *Coordinator {
	ttl := cfg.LeaseTTL
	if ttl <= 0 {
		ttl = 2 * time.Minute
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	costOf := cfg.CostFunc
	if costOf == nil {
		costOf = func(s contract.SettleRequest) float64 {
			return float64(s.InputTokens+s.OutputTokens) / 1000.0
		}
	}
	return &Coordinator{
		admission:     cfg.Admission,
		billing:       cfg.Billing,
		scheduler:     cfg.Scheduler,
		sticky:        cfg.Sticky,
		usage:         cfg.Usage,
		minter:        cfg.Minter,
		quota:         cfg.Quota,
		leaseTTL:      ttl,
		leaseEstimate: cfg.LeaseEstimate,
		costOf:        costOf,
		now:           now,
		settled:       make(map[string]contract.SettleResult),
	}
}

// Lease runs the admission -> billing -> sticky -> schedule pipeline and mints
// short-lived tokens for the ranked candidates. On any error after a slot is
// reserved, the slot is released so a rejected lease never leaks admission.
func (co *Coordinator) Lease(ctx context.Context, req contract.LeaseRequest) (*contract.LeaseResult, error) {
	if req.APIKey == "" || req.Model == "" {
		return nil, contract.ErrInvalidRequest
	}

	// 1. Admission: reserve a concurrency slot (non-blocking limit check).
	slotID, err := co.admission.Reserve(ctx, req.APIKey, req.Model)
	if err != nil {
		return nil, err
	}
	// Release the slot (and refund any quota reservation) if we bail out before
	// returning a successful lease.
	ok := false
	reserved := false
	defer func() {
		if !ok {
			co.admission.Release(ctx, slotID)
			if reserved {
				// Refund the full pre-debit: no lease was granted.
				_, _ = co.quota.Reconcile(req.APIKey, req.RequestID, 0)
			}
		}
	}()

	// 2. Billing eligibility precheck.
	if co.billing != nil {
		if err := co.billing.CheckEligibility(ctx, req.APIKey, req.Model); err != nil {
			return nil, err
		}
	}

	// 2b. Quota pre-debit (prevents double-spend across concurrent edges).
	if co.quota != nil && req.RequestID != "" {
		if err := co.quota.Reserve(req.APIKey, req.RequestID, co.leaseEstimate); err != nil {
			return nil, contract.ErrBillingIneligible
		}
		reserved = true
	}

	// 3. Scheduling: ranked candidates.
	candidates, err := co.scheduler.Select(ctx, req)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, contract.ErrNoAccount
	}

	// 4. Sticky: if this session was bound to an account that is still in the
	//    candidate set, move it to the front so the conversation keeps the same
	//    upstream account (and egress IP).
	if co.sticky != nil && req.SessionHash != "" {
		if boundID, found := co.sticky.Lookup(ctx, req.SessionHash); found {
			candidates = promote(candidates, boundID)
		}
		// Bind the (possibly newly) chosen primary so concurrent requests stick.
		co.sticky.Bind(ctx, req.SessionHash, candidates[0].AccountID)
	}

	// 5. Mint a short-lived token per candidate.
	expiresAt := co.now().Add(co.leaseTTL).Unix()
	if co.minter != nil {
		for i := range candidates {
			tok, exp := co.minter.Mint(candidates[i].AccountID, co.leaseTTL)
			candidates[i].LeaseToken = tok
			expiresAt = exp
		}
	}

	ok = true
	return &contract.LeaseResult{
		RequestID:  req.RequestID,
		SlotID:     slotID,
		ExpiresAt:  expiresAt,
		Candidates: candidates,
	}, nil
}

// Settle records usage, reconciles quota, releases the admission slot, and
// rebinds the sticky session on success. It is idempotent on RequestID: a
// duplicate Settle returns the original result without double-charging or
// double-releasing.
func (co *Coordinator) Settle(ctx context.Context, req contract.SettleRequest) (*contract.SettleResult, error) {
	if req.RequestID != "" {
		co.mu.Lock()
		if prev, dup := co.settled[req.RequestID]; dup {
			co.mu.Unlock()
			prev.Duplicate = true
			return &prev, nil
		}
		// Reserve the requestID under the lock so a concurrent duplicate Settle
		// short-circuits BEFORE any side effects (usage/admission/quota), closing
		// the check-then-act race that would otherwise double-charge and
		// double-release.
		co.settled[req.RequestID] = contract.SettleResult{RequestID: req.RequestID, Accepted: true}
		co.mu.Unlock()
	}

	charged, err := co.usage.Record(ctx, req)
	if err != nil {
		return nil, err
	}

	// Release the admission slot exactly once.
	if req.SlotID != "" {
		co.admission.Release(ctx, req.SlotID)
	}

	// Reconcile the quota pre-debit against the actual cost (refund the
	// overestimate or charge the extra). Idempotent on requestID.
	if co.quota != nil && req.RequestID != "" {
		_, _ = co.quota.Reconcile(req.APIKey, req.RequestID, co.costOf(req))
	}

	// Keep the conversation pinned to the account that actually served it.
	if co.sticky != nil && req.SessionHash != "" && req.AccountID != "" && req.StatusCode < 400 {
		co.sticky.Bind(ctx, req.SessionHash, req.AccountID)
	}

	res := contract.SettleResult{
		RequestID:    req.RequestID,
		Accepted:     true,
		QuotaCharged: charged,
	}
	if req.RequestID != "" {
		co.mu.Lock()
		co.settled[req.RequestID] = res
		co.mu.Unlock()
	}
	return &res, nil
}

// promote moves the candidate with accountID to the front, preserving the
// relative order of the rest. If not present, the slice is unchanged.
func promote(candidates []contract.Candidate, accountID string) []contract.Candidate {
	idx := -1
	for i := range candidates {
		if candidates[i].AccountID == accountID {
			idx = i
			break
		}
	}
	if idx <= 0 {
		return candidates
	}
	chosen := candidates[idx]
	out := make([]contract.Candidate, 0, len(candidates))
	out = append(out, chosen)
	for i := range candidates {
		if i != idx {
			out = append(out, candidates[i])
		}
	}
	return out
}
