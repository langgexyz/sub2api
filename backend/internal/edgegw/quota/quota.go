// Package quota implements an in-memory, concurrency-safe quota ledger for the
// distributed edge gateway. The center uses it to prevent quota double-spend
// across many concurrent edges: at lease time it PRE-DEBITS an estimated cost
// (Reserve), and at settle time it RECONCILES against the actual cost
// (Reconcile), refunding the overestimate or charging the underestimate. Both
// operations are idempotent per requestID so a duplicate or retried Lease or
// Settle never double-charges.
package quota

import (
	"errors"
	"sync"
)

// ErrInsufficient is returned by Reserve when the available balance is smaller
// than the requested estimate. In that case nothing is debited.
var ErrInsufficient = errors.New("quota: insufficient balance")

// reservation records a single outstanding pre-debit so that Reconcile can
// release it and adjust the balance against the actual cost.
type reservation struct {
	apiKey   string
	estimate float64
}

// Ledger is a concurrency-safe quota ledger. The zero value is not usable;
// construct one with New.
type Ledger struct {
	mu       sync.Mutex
	balances map[string]float64
	// reservations is keyed by requestID; it holds only outstanding
	// (un-reconciled) pre-debits, which makes both Reserve and Reconcile
	// idempotent per requestID.
	reservations map[string]reservation
}

// New returns an empty Ledger ready for use.
func New() *Ledger {
	return &Ledger{
		balances:     make(map[string]float64),
		reservations: make(map[string]reservation),
	}
}

// SetBalance sets an api key's available balance to an absolute value.
func (l *Ledger) SetBalance(apiKey string, balance float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.balances[apiKey] = balance
}

// Balance returns the current available balance for an api key.
func (l *Ledger) Balance(apiKey string) float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.balances[apiKey]
}

// Reserve pre-debits estimate from apiKey's balance for requestID.
//
//   - If the available balance is smaller than estimate, it returns
//     ErrInsufficient and debits nothing.
//   - It is idempotent on requestID: a second Reserve with the same requestID
//     is a no-op returning nil and does NOT debit again.
func (l *Ledger) Reserve(apiKey, requestID string, estimate float64) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if _, ok := l.reservations[requestID]; ok {
		// Idempotent: this requestID is already reserved; do not debit again.
		return nil
	}

	if l.balances[apiKey] < estimate {
		return ErrInsufficient
	}

	l.balances[apiKey] -= estimate
	l.reservations[requestID] = reservation{apiKey: apiKey, estimate: estimate}
	return nil
}

// Reconcile settles requestID against the actual cost.
//
//   - It adjusts the balance by (estimate - actual): if actual < estimate it
//     refunds the difference; if actual > estimate it charges the extra.
//   - It releases the reservation and returns delta = estimate - actual (the
//     refund; a negative delta means an extra charge).
//   - It is idempotent on requestID: a second Reconcile, or one with no
//     matching outstanding reservation, is a no-op returning (0, nil).
//
// The apiKey argument is accepted for symmetry with Reserve; the reservation's
// own apiKey is authoritative for the balance adjustment.
func (l *Ledger) Reconcile(apiKey, requestID string, actual float64) (delta float64, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	res, ok := l.reservations[requestID]
	if !ok {
		// Idempotent: nothing outstanding for this requestID.
		return 0, nil
	}

	delta = res.estimate - actual
	l.balances[res.apiKey] += delta
	delete(l.reservations, requestID)
	return delta, nil
}

// Reserved returns the total currently-reserved (un-reconciled) amount for an
// api key. It is intended for observability and tests.
func (l *Ledger) Reserved(apiKey string) float64 {
	l.mu.Lock()
	defer l.mu.Unlock()

	var total float64
	for _, res := range l.reservations {
		if res.apiKey == apiKey {
			total += res.estimate
		}
	}
	return total
}
