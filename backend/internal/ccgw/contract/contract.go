package contract

import "errors"

// Wire types shared between ccdirect (data plane) and cchub (control plane).
//
// cchub owns account registry + credentials, admission control (concurrency /
// quota), scheduling and sticky sessions, and the usage ledger. It exposes two
// RPCs: Lease and Settle. ccdirect receives a client prompt, calls Lease to
// obtain an account + short-lived token + upstream endpoint, performs the
// upstream request itself from its own stable IP, streams the response back, and
// finally calls Settle to report usage so cchub can reconcile quota and release
// the slot.

// LeaseRequest is what ccdirect sends to cchub to obtain an account.
type LeaseRequest struct {
	APIKey      string `json:"api_key"`
	Model       string `json:"model"`
	SessionHash string `json:"session_hash"`
	RequestID   string `json:"request_id"`
	CCDirectID  string `json:"ccdirect_id"`
	Stream      bool   `json:"stream"`
}

// AuthScheme tells ccdirect how to present the leased credential upstream.
// Zero value means "Authorization: Bearer <token>". Gemini-style key-in-query
// and Anthropic-style x-api-key + version headers are expressible here.
//
// This is data only; the HTTP-applying behavior lives next to the edge relay
// (edgegw.applyAuthScheme) since contract must stay free of net/http.
type AuthScheme struct {
	Header     string            `json:"header,omitempty"`      // e.g. "Authorization", "x-api-key"
	Prefix     string            `json:"prefix,omitempty"`      // e.g. "Bearer "
	QueryParam string            `json:"query_param,omitempty"` // e.g. "key" (Gemini)
	Extra      map[string]string `json:"extra,omitempty"`       // e.g. {"anthropic-version":"2023-06-01"}
}

// Candidate is one schedulable upstream account, ranked. The primary is
// Candidates[0]; ccdirect fails over locally down the list without a second
// round-trip to cchub.
type Candidate struct {
	AccountID       string            `json:"account_id"`
	HomeCCDirectID  string            `json:"home_ccdirect_id"`
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

// LeaseResult is cchub's answer to a LeaseRequest.
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

// SettleRequest reports the outcome of a forwarded request back to cchub.
type SettleRequest struct {
	RequestID           string `json:"request_id"`
	APIKey              string `json:"api_key"`
	AccountID           string `json:"account_id"`
	SlotID              string `json:"slot_id"`
	SessionHash         string `json:"session_hash"`
	InputTokens         int    `json:"input_tokens"`
	OutputTokens        int    `json:"output_tokens"`
	CacheReadTokens     int    `json:"cache_read_tokens"`
	CacheCreationTokens int    `json:"cache_creation_tokens"`
	StatusCode          int    `json:"status_code"`
	LatencyMS           int64  `json:"latency_ms"`
	Partial             bool   `json:"partial"` // edge crashed / client disconnected mid-stream
}

// SettleResult acknowledges a Settle.
type SettleResult struct {
	RequestID    string  `json:"request_id"`
	Accepted     bool    `json:"accepted"`
	Duplicate    bool    `json:"duplicate"` // idempotency: requestID already settled
	QuotaCharged float64 `json:"quota_charged"`
}

// Sentinel errors returned by the coordinator; cchub maps these to HTTP status
// codes and protocol-specific error bodies.
var (
	ErrWaitQueueFull     = errors.New("contract: wait queue full")
	ErrConcurrencyFull   = errors.New("contract: concurrency limit reached")
	ErrBillingIneligible = errors.New("contract: billing ineligible (insufficient balance/quota)")
	ErrNoAccount         = errors.New("contract: no schedulable account")
	ErrInvalidRequest    = errors.New("contract: invalid lease request")
)
