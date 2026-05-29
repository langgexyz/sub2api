// Package edgegw implements the control-plane / data-plane split for the
// distributed edge gateway (see docs/tech/distributed-edge.md).
//
// The center (control plane) owns account registry + credentials, admission
// control (concurrency / quota), scheduling and sticky sessions, and the usage
// ledger. It exposes two RPCs: Lease and Settle. The edge (data plane) receives
// a client prompt, calls Lease to obtain an account + short-lived token +
// upstream endpoint, performs the upstream request itself from its own stable
// IP, streams the response back to the client, and finally calls Settle to
// report usage so the center can reconcile quota and release the slot.
//
// This package is intentionally decoupled from the heavy service/Ent layer: it
// depends only on the small interfaces declared here. The in-memory
// implementations (memimpl.go) make the whole system runnable and testable
// without Postgres/Redis; production wiring backs the same interfaces with the
// real GatewayService / BillingCacheService / ConcurrencyService.
package edgegw

import "errors"

// LeaseRequest is what an edge sends to the center to obtain an account.
type LeaseRequest struct {
	APIKey      string `json:"api_key"`
	Model       string `json:"model"`
	SessionHash string `json:"session_hash"`
	RequestID   string `json:"request_id"`
	EdgeID      string `json:"edge_id"`
	Stream      bool   `json:"stream"`
}

// Candidate is one schedulable upstream account, ranked. The primary is
// Candidates[0]; the edge fails over locally down the list without a second
// round-trip to the center.
type Candidate struct {
	AccountID       string            `json:"account_id"`
	HomeEdgeID      string            `json:"home_edge_id"`
	Platform        string            `json:"platform"` // selects the edge-side Provider (anthropic/openai/gemini/antigravity)
	UpstreamBaseURL string            `json:"upstream_base_url"`
	LeaseToken      string            `json:"lease_token"` // short-lived; edge uses then discards
	AuthScheme      AuthScheme        `json:"auth_scheme"` // how the edge presents the token upstream
	ModelMapping    map[string]string `json:"model_mapping"`
}

// MappedModel applies the candidate's model mapping, falling back to the
// requested model when no mapping entry exists.
func (c Candidate) MappedModel(requested string) string {
	if c.ModelMapping != nil {
		if mapped, ok := c.ModelMapping[requested]; ok && mapped != "" {
			return mapped
		}
	}
	return requested
}

// LeaseResult is the center's answer to a LeaseRequest.
type LeaseResult struct {
	RequestID  string      `json:"request_id"`
	SlotID     string      `json:"slot_id"`    // opaque handle, returned in Settle to release admission
	ExpiresAt  int64       `json:"expires_at"` // unix seconds; lease-token validity
	Candidates []Candidate `json:"candidates"` // ranked; [0] is primary, sticky-bound first
}

// Primary returns the first (preferred) candidate.
func (r *LeaseResult) Primary() (Candidate, bool) {
	if r == nil || len(r.Candidates) == 0 {
		return Candidate{}, false
	}
	return r.Candidates[0], true
}

// SettleRequest reports the outcome of a forwarded request back to the center.
type SettleRequest struct {
	RequestID    string `json:"request_id"`
	APIKey       string `json:"api_key"`
	AccountID    string `json:"account_id"`
	SlotID       string `json:"slot_id"`
	SessionHash  string `json:"session_hash"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
	StatusCode   int    `json:"status_code"`
	LatencyMS    int64  `json:"latency_ms"`
	Partial      bool   `json:"partial"` // edge crashed / client disconnected mid-stream
}

// SettleResult acknowledges a Settle.
type SettleResult struct {
	RequestID    string  `json:"request_id"`
	Accepted     bool    `json:"accepted"`
	Duplicate    bool    `json:"duplicate"` // idempotency: requestID already settled
	QuotaCharged float64 `json:"quota_charged"`
}

// Sentinel errors returned by the coordinator; the center server maps these to
// HTTP status codes and protocol-specific error bodies.
var (
	ErrWaitQueueFull     = errors.New("edgegw: wait queue full")
	ErrConcurrencyFull   = errors.New("edgegw: concurrency limit reached")
	ErrBillingIneligible = errors.New("edgegw: billing ineligible (insufficient balance/quota)")
	ErrNoAccount         = errors.New("edgegw: no schedulable account")
	ErrInvalidRequest    = errors.New("edgegw: invalid lease request")
)
