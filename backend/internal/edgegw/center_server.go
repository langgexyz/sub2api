package edgegw

import (
	"encoding/json"
	"errors"
	"net/http"
	"sync"
)

// CenterServer exposes the coordinator over HTTP: POST /v1/lease and
// POST /v1/settle. It also keeps per-account in-flight accounting balanced
// against the registry so the load-aware scheduler spreads concurrent requests.
type CenterServer struct {
	coord    *Coordinator
	registry *MemRegistry

	mu        sync.Mutex
	slotAccts map[string]string // slotID -> accountID acquired at lease, released at settle
}

// NewCenterServer wires a coordinator and registry into an HTTP server.
func NewCenterServer(coord *Coordinator, registry *MemRegistry) *CenterServer {
	return &CenterServer{
		coord:     coord,
		registry:  registry,
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
	return mux
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
