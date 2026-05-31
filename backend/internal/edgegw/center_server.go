package edgegw

import (
	"encoding/json"
	"errors"
	"net/http"
	"sync"

	"github.com/Wei-Shaw/sub2api/internal/edgegw/edgereg"
)

// CenterServer exposes the coordinator over HTTP: POST /v1/lease and
// POST /v1/settle, plus the edge fleet endpoints (/v1/register, /v1/heartbeat,
// /v1/edges). It also keeps per-account in-flight accounting balanced against
// the registry so the load-aware scheduler spreads concurrent requests.
type CenterServer struct {
	coord    *Coordinator
	registry *MemRegistry
	edges    *edgereg.Registry

	mu         sync.Mutex
	slotAccts  map[string]string   // slotID -> accountID acquired at lease, released at settle
	enrollKeys map[string]struct{} // valid edge enroll keys; empty = accept any (dev)

	// Config the center issues to edges at enroll time (so edges need almost no
	// local flags). Guarded by mu.
	issuedCenterURL   string
	issuedHeartbeat   int
	issuedMaxFailover int
	issuedPlatforms   []string
	enrollSeq         int64
}

// SetEnrollKeys restricts edge registration to the given enroll keys. An empty
// list (the default) accepts any edge (dev mode).
func (s *CenterServer) SetEnrollKeys(keys []string) {
	m := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		if k != "" {
			m[k] = struct{}{}
		}
	}
	s.mu.Lock()
	s.enrollKeys = m
	s.mu.Unlock()
}

func (s *CenterServer) enrollKeyAllowed(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.enrollKeys) == 0 {
		return true
	}
	_, ok := s.enrollKeys[key]
	return ok
}

// NewCenterServer wires a coordinator and registry into an HTTP server. edges
// may be nil to disable edge-fleet tracking.
func NewCenterServer(coord *Coordinator, registry *MemRegistry, edges *edgereg.Registry) *CenterServer {
	return &CenterServer{
		coord:     coord,
		registry:  registry,
		edges:     edges,
		slotAccts: make(map[string]string),
	}
}

// Handler returns the center's HTTP mux.
func (s *CenterServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/v1/lease", s.handleLease)
	mux.HandleFunc("/v1/settle", s.handleSettle)
	mux.HandleFunc("/v1/enroll", s.handleEnroll)
	mux.HandleFunc("/v1/register", s.handleRegister)
	mux.HandleFunc("/v1/heartbeat", s.handleHeartbeat)
	mux.HandleFunc("/v1/edges", s.handleEdges)
	return mux
}

// RegisterRequest / HeartbeatRequest moved to the shared contract package
// (aliased in contract.go) — both ccdirect and cchub use them.

func (s *CenterServer) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.edges == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	var req RegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.EdgeID == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "edge_id required")
		return
	}
	if !s.enrollKeyAllowed(req.EnrollKey) {
		writeJSONError(w, http.StatusUnauthorized, "enroll_denied", "invalid enroll key")
		return
	}
	egressIP := req.EgressIP
	if egressIP == "" {
		// Auto-detect from the connection so the edge need not configure it.
		egressIP = clientIPFromRemoteAddr(r.RemoteAddr)
	}
	s.edges.Register(req.EdgeID, egressIP, req.Platforms)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *CenterServer) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.edges == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	var req HeartbeatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.EdgeID == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "edge_id required")
		return
	}
	if !s.edges.Heartbeat(req.EdgeID) {
		// Unknown edge: ask it to re-register.
		writeJSONError(w, http.StatusNotFound, "unknown_edge", "edge not registered")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *CenterServer) handleEdges(w http.ResponseWriter, _ *http.Request) {
	if s.edges == nil {
		writeJSON(w, http.StatusOK, []edgereg.EdgeInfo{})
		return
	}
	writeJSON(w, http.StatusOK, s.edges.Live())
}

func (s *CenterServer) handleLease(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req LeaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "decode lease request: "+err.Error())
		return
	}
	res, err := s.coord.Lease(r.Context(), req)
	if err != nil {
		status, code := leaseErrorStatus(err)
		writeJSONError(w, status, code, err.Error())
		return
	}
	if primary, ok := res.Primary(); ok {
		s.acquire(res.SlotID, primary.AccountID)
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *CenterServer) handleSettle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req SettleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "decode settle request: "+err.Error())
		return
	}
	res, err := s.coord.Settle(r.Context(), req)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "settle_failed", err.Error())
		return
	}
	if !res.Duplicate {
		s.release(req.SlotID)
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *CenterServer) acquire(slotID, accountID string) {
	s.mu.Lock()
	s.slotAccts[slotID] = accountID
	s.mu.Unlock()
	s.registry.acquireLoad(accountID)
}

func (s *CenterServer) release(slotID string) {
	s.mu.Lock()
	accountID, ok := s.slotAccts[slotID]
	if ok {
		delete(s.slotAccts, slotID)
	}
	s.mu.Unlock()
	if ok {
		s.registry.releaseLoad(accountID)
	}
}

// leaseErrorStatus maps coordinator sentinels to HTTP status + error code.
func leaseErrorStatus(err error) (int, string) {
	switch {
	case errors.Is(err, ErrWaitQueueFull), errors.Is(err, ErrConcurrencyFull):
		return http.StatusTooManyRequests, "rate_limited"
	case errors.Is(err, ErrBillingIneligible):
		return http.StatusPaymentRequired, "billing_ineligible"
	case errors.Is(err, ErrNoAccount):
		return http.StatusServiceUnavailable, "no_account"
	case errors.Is(err, ErrInvalidRequest):
		return http.StatusBadRequest, "invalid_request"
	default:
		return http.StatusInternalServerError, "lease_failed"
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{"code": code, "message": message},
	})
}
