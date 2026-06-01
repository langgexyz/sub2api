package cchub

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/edgegw/contract"
)

// This file holds in-memory, dependency-free implementations of the control
// plane interfaces. They make the distributed system runnable and testable end
// to end without Postgres/Redis. Production swaps these for Redis-backed
// admission and the real GatewayService / BillingCacheService scheduler.

// AccountConfig describes one upstream account the center can schedule.
type AccountConfig struct {
	ID              string              `json:"id"`
	Platform        string              `json:"platform,omitempty"`
	HomeEdgeID      string              `json:"home_edge_id,omitempty"`
	UpstreamBaseURL string              `json:"upstream_base_url"`
	UpstreamToken   string              `json:"upstream_token"`            // the real upstream credential (never leaves the center as-is)
	ModelMapping    map[string]string   `json:"model_mapping,omitempty"`   // requested -> upstream model
	Models          []string            `json:"models,omitempty"`          // supported requested models; empty = all
	MaxConcurrency  int                 `json:"max_concurrency,omitempty"` // per-account concurrency cap; 0 = unlimited
	GroupAPIKeys    []string            `json:"group_api_keys,omitempty"`  // api keys allowed to use this account; empty = all
	AuthScheme      contract.AuthScheme `json:"auth_scheme,omitempty"`     // how the edge presents the token upstream
}

func (a AccountConfig) supportsModel(model string) bool {
	if len(a.Models) == 0 {
		return true
	}
	for _, m := range a.Models {
		if m == model {
			return true
		}
	}
	return false
}

func (a AccountConfig) allowsAPIKey(apiKey string) bool {
	if len(a.GroupAPIKeys) == 0 {
		return true
	}
	for _, k := range a.GroupAPIKeys {
		if k == apiKey {
			return true
		}
	}
	return false
}

// MemRegistry is an in-memory account registry that doubles as a Scheduler. It
// ranks candidates by current in-flight load (least-loaded first) so it spreads
// requests across accounts the way the real load-aware scheduler does.
type MemRegistry struct {
	mu       sync.RWMutex
	accounts []AccountConfig
	inflight map[string]*int64 // accountID -> in-flight counter
}

// NewMemRegistry builds a registry from a static account list.
func NewMemRegistry(accounts []AccountConfig) *MemRegistry {
	r := &MemRegistry{
		accounts: append([]AccountConfig(nil), accounts...),
		inflight: make(map[string]*int64, len(accounts)),
	}
	for _, a := range accounts {
		var n int64
		r.inflight[a.ID] = &n
	}
	return r
}

// Account returns the raw account config by id (used by the token minter).
func (r *MemRegistry) Account(id string) (AccountConfig, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, a := range r.accounts {
		if a.ID == id {
			return a, true
		}
	}
	return AccountConfig{}, false
}

// acquireLoad increments an account's in-flight counter (called by the center
// when a lease is granted) so concurrent leases see updated load.
func (r *MemRegistry) acquireLoad(accountID string) {
	r.mu.RLock()
	c := r.inflight[accountID]
	r.mu.RUnlock()
	if c != nil {
		atomic.AddInt64(c, 1)
	}
}

// releaseLoad decrements an account's in-flight counter at settle time.
func (r *MemRegistry) releaseLoad(accountID string) {
	r.mu.RLock()
	c := r.inflight[accountID]
	r.mu.RUnlock()
	if c != nil && atomic.AddInt64(c, -1) < 0 {
		atomic.StoreInt64(c, 0)
	}
}

// Select implements Scheduler: filter by api key + model, then rank by
// edge-affinity first (accounts homed on the requesting edge come first, so an
// edge serves its own accounts and the upstream call egresses from that edge's
// stable IP) and least-load second. Cross-edge candidates remain as failover.
func (r *MemRegistry) Select(_ context.Context, req contract.LeaseRequest) ([]contract.Candidate, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	type scored struct {
		acc      AccountConfig
		load     int64
		affinity int // 0 = homed on the requesting edge, 1 = elsewhere
	}
	var pool []scored
	for _, a := range r.accounts {
		if !a.allowsAPIKey(req.APIKey) || !a.supportsModel(req.Model) {
			continue
		}
		load := atomic.LoadInt64(r.inflight[a.ID])
		affinity := 1
		if req.EdgeID != "" && a.HomeEdgeID == req.EdgeID {
			affinity = 0
		}
		pool = append(pool, scored{acc: a, load: load, affinity: affinity})
	}
	if len(pool) == 0 {
		return nil, contract.ErrNoAccount
	}
	// Stable sort by (affinity asc, load asc) via insertion sort (pools are tiny).
	less := func(a, b scored) bool {
		if a.affinity != b.affinity {
			return a.affinity < b.affinity
		}
		return a.load < b.load
	}
	for i := 1; i < len(pool); i++ {
		for j := i; j > 0 && less(pool[j], pool[j-1]); j-- {
			pool[j], pool[j-1] = pool[j-1], pool[j]
		}
	}

	candidates := make([]contract.Candidate, 0, len(pool))
	for _, s := range pool {
		candidates = append(candidates, contract.Candidate{
			AccountID:       s.acc.ID,
			HomeEdgeID:      s.acc.HomeEdgeID,
			Platform:        s.acc.Platform,
			UpstreamBaseURL: s.acc.UpstreamBaseURL,
			AuthScheme:      s.acc.AuthScheme,
			ModelMapping:    s.acc.ModelMapping,
		})
	}
	return candidates, nil
}

// MemAdmission is an in-memory concurrency gate keyed by api key, plus a global
// per-account cap consulted via the registry. It models the central admission
// counters that are Redis-backed in production.
type MemAdmission struct {
	maxPerKey int

	mu     sync.Mutex
	perKey map[string]int    // apiKey -> in-flight
	slots  map[string]string // slotID -> apiKey
	seq    int64
}

// NewMemAdmission caps concurrent in-flight leases per api key. maxPerKey <= 0
// means unlimited.
func NewMemAdmission(maxPerKey int) *MemAdmission {
	return &MemAdmission{
		maxPerKey: maxPerKey,
		perKey:    make(map[string]int),
		slots:     make(map[string]string),
	}
}

// Reserve implements Admission.
func (a *MemAdmission) Reserve(_ context.Context, apiKey, _ string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.maxPerKey > 0 && a.perKey[apiKey] >= a.maxPerKey {
		return "", contract.ErrConcurrencyFull
	}
	a.perKey[apiKey]++
	a.seq++
	slotID := "slot-" + strconv.FormatInt(a.seq, 10)
	a.slots[slotID] = apiKey
	return slotID, nil
}

// Release implements Admission. It is safe to call with an unknown/empty slotID
// (idempotent) so duplicate Settles do not underflow the counter.
func (a *MemAdmission) Release(_ context.Context, slotID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	apiKey, ok := a.slots[slotID]
	if !ok {
		return
	}
	delete(a.slots, slotID)
	if a.perKey[apiKey] > 0 {
		a.perKey[apiKey]--
	}
}

// InFlight reports current in-flight count for an api key (test/observability).
func (a *MemAdmission) InFlight(apiKey string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.perKey[apiKey]
}

// MemSticky is an in-memory session -> account binding store.
type MemSticky struct {
	mu sync.RWMutex
	m  map[string]string
}

// NewMemSticky builds an empty sticky store.
func NewMemSticky() *MemSticky {
	return &MemSticky{m: make(map[string]string)}
}

// Lookup implements StickyStore.
func (s *MemSticky) Lookup(_ context.Context, key string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.m[key]
	return id, ok
}

// Bind implements StickyStore.
func (s *MemSticky) Bind(_ context.Context, key, accountID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[key] = accountID
}

// UsageRecord is one settled usage row captured by MemUsageSink.
type UsageRecord struct {
	contract.SettleRequest
	Charged float64
}

// MemUsageSink records settled usage in memory and computes a trivial quota
// charge (1 unit per 1K total tokens). Production backs this with the real
// usage worker pool + quota flusher.
type MemUsageSink struct {
	mu      sync.Mutex
	records []UsageRecord
}

// NewMemUsageSink builds an empty sink.
func NewMemUsageSink() *MemUsageSink {
	return &MemUsageSink{}
}

// Record implements UsageSink.
func (u *MemUsageSink) Record(_ context.Context, s contract.SettleRequest) (float64, error) {
	charged := float64(s.InputTokens+s.OutputTokens) / 1000.0
	u.mu.Lock()
	defer u.mu.Unlock()
	u.records = append(u.records, UsageRecord{SettleRequest: s, Charged: charged})
	return charged, nil
}

// Records returns a copy of recorded usage (test/observability).
func (u *MemUsageSink) Records() []UsageRecord {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]UsageRecord(nil), u.records...)
}

// HMACMinter mints short-lived capability tokens that wrap the account's real
// upstream token. The envelope is "<base64(accountID|exp)>.<base64(hmac)>" and
// the registry resolves the real upstream token at mint time so the edge
// receives a usable bearer credential bound to a deadline. In production the
// minted value would be the provider access token under an mTLS channel; the
// signature lets the center (or a verifier) detect tampering/expiry.
type HMACMinter struct {
	registry *MemRegistry
	secret   []byte
	now      Clock
}

// NewHMACMinter builds a minter over the registry's real upstream tokens.
func NewHMACMinter(registry *MemRegistry, secret []byte, now Clock) *HMACMinter {
	if now == nil {
		now = time.Now
	}
	return &HMACMinter{registry: registry, secret: secret, now: now}
}

// Mint implements TokenMinter. The returned token carries the real upstream
// bearer plus a signed expiry the edge passes through; an expired token is
// rejectable without a registry lookup.
func (m *HMACMinter) Mint(accountID string, ttl time.Duration) (string, int64) {
	exp := m.now().Add(ttl).Unix()
	acc, _ := m.registry.Account(accountID)
	payload := fmt.Sprintf("%s|%d|%s", accountID, exp, acc.UpstreamToken)
	mac := hmac.New(sha256.New, m.secret)
	_, _ = mac.Write([]byte(payload))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	token := base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + sig
	return token, exp
}

// UpstreamBearer + splitN moved to the shared contract package (cchub mints,
// ccdirect extracts — see contract/upstream.go).
