package handler

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/edgegw"
	"github.com/Wei-Shaw/sub2api/internal/edgegw/edgereg"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// EdgeCenterHandler turns sub2api into the control plane for the distributed
// edge: it exposes /v1/lease and /v1/settle backed by the REAL services
// (APIKeyService for auth, GatewayService for load-aware account selection,
// BillingCacheService for eligibility, ConcurrencyService for slots). A data
// -plane edge calls Lease to obtain a real account + its upstream token +
// endpoint, performs the upstream request itself from its stable IP, then calls
// Settle to release the slot. This reuses sub2api's accounts, keys, scheduling
// and concurrency without the edge holding any of that state.
//
// Slot lifecycle: GatewayService hands back an in-process ReleaseFunc closure
// that cannot cross the Lease/Settle HTTP boundary, so we keep slotID ->
// ReleaseFunc in memory here (correct for a single center process) and release
// it at Settle. Multi-replica centers need a Redis-backed release-by-key; see
// docs/tech/distributed-edge.md.
type EdgeCenterHandler struct {
	apiKeyService  *service.APIKeyService
	gatewayService *service.GatewayService
	billingService *service.BillingCacheService

	edges *edgereg.Registry

	mu    sync.Mutex
	slots map[string]*leasedSlot
}

type leasedSlot struct {
	release   func()
	accountID int64
	released  bool
}

// NewEdgeCenterHandler builds the edge control-plane handler.
func NewEdgeCenterHandler(
	apiKeyService *service.APIKeyService,
	gatewayService *service.GatewayService,
	billingService *service.BillingCacheService,
) *EdgeCenterHandler {
	return &EdgeCenterHandler{
		apiKeyService:  apiKeyService,
		gatewayService: gatewayService,
		billingService: billingService,
		edges:          edgereg.New(60*time.Second, time.Now),
		slots:          make(map[string]*leasedSlot),
	}
}

// Register records an edge in the fleet (auto-detecting its egress IP).
func (h *EdgeCenterHandler) Register(c *gin.Context) {
	var req edgegw.RegisterRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.EdgeID == "" {
		edgeCenterError(c, http.StatusBadRequest, "invalid_request", "edge_id required")
		return
	}
	ip := req.EgressIP
	if ip == "" {
		ip = c.ClientIP()
	}
	h.edges.Register(req.EdgeID, ip, req.Platforms)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// Heartbeat keeps an edge marked live; 404 tells it to re-register.
func (h *EdgeCenterHandler) Heartbeat(c *gin.Context) {
	var req edgegw.HeartbeatRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.EdgeID == "" {
		edgeCenterError(c, http.StatusBadRequest, "invalid_request", "edge_id required")
		return
	}
	if !h.edges.Heartbeat(req.EdgeID) {
		edgeCenterError(c, http.StatusNotFound, "unknown_edge", "edge not registered")
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// Edges lists the live edge fleet (admin/observability).
func (h *EdgeCenterHandler) Edges(c *gin.Context) {
	c.JSON(http.StatusOK, h.edges.Live())
}

// Lease validates the sub2api API key, checks billing, selects a real account
// (acquiring its concurrency slot), and returns the account + upstream token +
// endpoint for the edge to use.
func (h *EdgeCenterHandler) Lease(c *gin.Context) {
	var req edgegw.LeaseRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		edgeCenterError(c, http.StatusBadRequest, "invalid_request", "decode lease request")
		return
	}
	if req.APIKey == "" || req.Model == "" {
		edgeCenterError(c, http.StatusBadRequest, "invalid_request", "api_key and model are required")
		return
	}

	ctx := c.Request.Context()

	// 1. Authenticate the sub2api key -> user + group (same as the gateway).
	apiKey, user, err := h.apiKeyService.ValidateKey(ctx, req.APIKey)
	if err != nil {
		edgeCenterError(c, http.StatusUnauthorized, "authentication_error", "invalid api key")
		return
	}
	if apiKey.GroupID == nil {
		edgeCenterError(c, http.StatusForbidden, "forbidden", "api key has no group assigned")
		return
	}

	// 2. Billing eligibility (balance / quota / subscription / rate limits).
	platform := ""
	if apiKey.Group != nil {
		platform = apiKey.Group.Platform
	}
	if err := h.billingService.CheckBillingEligibility(ctx, user, apiKey, apiKey.Group, nil, platform); err != nil {
		edgeCenterError(c, http.StatusPaymentRequired, "billing_ineligible", err.Error())
		return
	}

	// 3. Load-aware account selection (acquires a concurrency slot).
	sel, err := h.gatewayService.SelectAccountWithLoadAwareness(ctx, apiKey.GroupID, req.SessionHash, req.Model, nil, "", user.ID)
	if err != nil || sel == nil || sel.Account == nil {
		edgeCenterError(c, http.StatusServiceUnavailable, "no_account", "no schedulable account")
		return
	}

	release := sel.ReleaseFunc
	if release == nil {
		release = func() {}
	}
	if !sel.Acquired {
		// We do not wait at the center; the edge can retry. Release any
		// background wait reservation and report capacity exhaustion.
		release()
		edgeCenterError(c, http.StatusTooManyRequests, "rate_limited", "account at capacity")
		return
	}

	cand, err := candidateFromAccount(sel.Account)
	if err != nil {
		release()
		edgeCenterError(c, http.StatusServiceUnavailable, "no_account", err.Error())
		return
	}

	slotID := newSlotID()
	h.mu.Lock()
	h.slots[slotID] = &leasedSlot{release: release, accountID: sel.Account.ID}
	h.mu.Unlock()

	c.JSON(http.StatusOK, edgegw.LeaseResult{
		RequestID:  req.RequestID,
		SlotID:     slotID,
		Candidates: []edgegw.Candidate{cand},
	})
}

// Settle releases the concurrency slot held since Lease. Idempotent on slotID.
func (h *EdgeCenterHandler) Settle(c *gin.Context) {
	var req edgegw.SettleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		edgeCenterError(c, http.StatusBadRequest, "invalid_request", "decode settle request")
		return
	}

	duplicate := false
	h.mu.Lock()
	slot, ok := h.slots[req.SlotID]
	if !ok || slot.released {
		duplicate = true
	} else {
		slot.released = true
		delete(h.slots, req.SlotID)
	}
	h.mu.Unlock()

	if !duplicate && slot != nil && slot.release != nil {
		slot.release()
	}
	// NOTE: detailed usage/billing recording is a follow-up; sub2api's usage
	// task carries pricing/cache fields built in the gateway path. The slot
	// release (the correctness-critical part) is done here.

	c.JSON(http.StatusOK, edgegw.SettleResult{
		RequestID: req.RequestID,
		Accepted:  true,
		Duplicate: duplicate,
	})
}

// candidateFromAccount maps a real sub2api Account to an edge Candidate: the
// upstream base URL, the credential the edge presents, the auth scheme, and the
// model mapping. Mirrors how GatewayService.buildUpstreamRequest authenticates.
func candidateFromAccount(acc *service.Account) (edgegw.Candidate, error) {
	cand := edgegw.Candidate{
		AccountID:    strconv.FormatInt(acc.ID, 10),
		Platform:     acc.Platform,
		ModelMapping: acc.GetModelMapping(),
	}

	switch acc.Type {
	case service.AccountTypeAPIKey:
		base := acc.GetBaseURL()
		if base == "" {
			return edgegw.Candidate{}, errEdgeNoBaseURL
		}
		cand.UpstreamBaseURL = base
		cand.LeaseToken = acc.GetCredential("api_key")
		// sub2api uses x-api-key for apikey accounts; anthropic-version required.
		cand.AuthScheme = edgegw.AuthScheme{
			Header: "x-api-key",
			Extra:  map[string]string{"anthropic-version": "2023-06-01"},
		}
	default:
		// OAuth and other types need their own token resolution / refresh; not
		// yet supported through the edge.
		return edgegw.Candidate{}, errEdgeUnsupportedType
	}
	return cand, nil
}

var (
	errEdgeNoBaseURL       = edgeCenterErr("account has no upstream base url")
	errEdgeUnsupportedType = edgeCenterErr("account type not yet supported via edge")
)

type edgeCenterErr string

func (e edgeCenterErr) Error() string { return string(e) }

func edgeCenterError(c *gin.Context, status int, code, msg string) {
	logger.L().With(zap.String("component", "handler.edge_center")).Debug("edge_center.error",
		zap.Int("status", status), zap.String("code", code), zap.String("message", msg))
	c.JSON(status, gin.H{"error": gin.H{"code": code, "message": msg}})
}

func newSlotID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "slot-fallback"
	}
	return "slot-" + hex.EncodeToString(b[:])
}
