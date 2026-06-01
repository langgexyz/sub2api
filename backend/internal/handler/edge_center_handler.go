package handler

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/ccgw/contract"
	"github.com/Wei-Shaw/sub2api/internal/ccgw/edgereg"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// CCHubHandler turns sub2api into the control plane for the distributed
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
type CCHubHandler struct {
	apiKeyService  *service.APIKeyService
	gatewayService *service.GatewayService
	billingService *service.BillingCacheService
	authService    *service.AuthService

	edges *edgereg.Registry

	// Lease tokens are ALWAYS AEAD-sealed bound to {ccdirectID, exp}; tokenSecret is
	// derived automatically (never operator-configured) and handed to each edge
	// at enroll, so the edge can open what the center sealed. seal is mandatory.
	tokenSecret []byte
	tokenTTL    time.Duration

	// livenessKey signs per-heartbeat liveness tokens (Ed25519, derived from
	// JWT_SECRET so all replicas agree and the public key is reproducible for
	// embedding into ccdirect). livenessTTL bounds each token's validity.
	livenessKey ed25519.PrivateKey
	livenessTTL time.Duration
	// revokedEdges: edge ids cchub refuses to vouch for — heartbeat returns no
	// liveness token, so those edges drain. Guarded by mu.
	revokedEdges map[string]struct{}

	// releaseKey signs ccdirect self-update release manifests (Ed25519, derived
	// from JWT_SECRET like livenessKey so the public key is reproducible for
	// embedding into ccdirect). releaseManifests holds the latest published
	// release per "os/arch"; the upgrade endpoint returns the signed manifest for
	// the requesting node's platform. Guarded by mu.
	releaseKey       ed25519.PrivateKey
	releaseManifests map[string]contract.ReleaseManifest

	// Config the center ISSUES to edges at enroll (so the edge needs no local
	// config beyond a token): egress proxy URL, heartbeat, failover, platforms.
	issuedProxy       string
	issuedCenterURL   string
	issuedHeartbeat   int
	issuedMaxFailover int
	issuedPlatforms   []string
	enrollKeys        map[string]struct{}
	enrollSeq         int64

	mu    sync.Mutex
	slots map[string]*leasedSlot
}

// SetEnrollConfig sets what the center issues to enrolling edges (egress proxy
// is included so the edge's stable-IP egress is center-controlled, not local).
func (h *CCHubHandler) SetEnrollConfig(cchubURL, upstreamProxy string, heartbeatSeconds, maxFailover int, platforms []string) {
	h.issuedCenterURL = cchubURL
	h.issuedProxy = upstreamProxy
	h.issuedHeartbeat = heartbeatSeconds
	h.issuedMaxFailover = maxFailover
	h.issuedPlatforms = append([]string(nil), platforms...)
}

// SetEnrollKeys restricts enroll/register to these keys (empty = accept any).
func (h *CCHubHandler) SetEnrollKeys(keys []string) {
	m := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		if k != "" {
			m[k] = struct{}{}
		}
	}
	h.enrollKeys = m
}

func (h *CCHubHandler) enrollKeyAllowed(key string) bool {
	if len(h.enrollKeys) == 0 {
		return true
	}
	_, ok := h.enrollKeys[key]
	return ok
}

type leasedSlot struct {
	release   func()
	accountID int64
	released  bool

	// Captured at lease so Settle can record usage via sub2api's existing
	// accounting (per-account + per-apikey + pricing/quota).
	apiKey        *service.APIKey
	user          *service.User
	account       *service.Account
	model         string
	upstreamModel string
	stream        bool
	quotaPlatform string
}

// NewCCHubHandler builds the edge control-plane handler.
func NewCCHubHandler(
	apiKeyService *service.APIKeyService,
	gatewayService *service.GatewayService,
	billingService *service.BillingCacheService,
	authService *service.AuthService,
) *CCHubHandler {
	h := &CCHubHandler{
		apiKeyService:  apiKeyService,
		gatewayService: gatewayService,
		billingService: billingService,
		authService:    authService,
		edges:          edgereg.New(60*time.Second, time.Now),
		slots:          make(map[string]*leasedSlot),
		// Seal is MANDATORY and zero-config: the secret is auto-derived (from
		// JWT_SECRET if present, else a random per-process key) and handed to each
		// edge at enroll. Lease tokens are never returned raw.
		tokenSecret: deriveSealSecret(),
		tokenTTL:    2 * time.Minute,
		livenessKey: deriveLivenessKey(),
		// Each token outlives ~3 heartbeats so brief network hiccups don't drain a
		// healthy edge; ccdirect drains only after the token actually expires.
		livenessTTL:      3 * time.Minute,
		revokedEdges:     make(map[string]struct{}),
		releaseKey:       deriveReleaseKey(),
		releaseManifests: make(map[string]contract.ReleaseManifest),
	}
	return h
}

// deriveSealSecret produces the center's lease-token seal secret without
// operator configuration: from JWT_SECRET when available (stable across
// restarts, shared by all replicas of the same deploy), else a random key.
func deriveSealSecret() []byte {
	if s := os.Getenv("JWT_SECRET"); s != "" {
		sum := sha256.Sum256([]byte("edge-lease-seal/" + s))
		// hex-encode so the secret is a printable ASCII string: it is JSON-safe
		// to hand to the edge at enroll, and both sides treat []byte(hexString)
		// as the seal key identically.
		return []byte(hex.EncodeToString(sum[:]))
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return []byte("edge-lease-seal-fallback-key-0001")
	}
	return []byte(hex.EncodeToString(b))
}

// deriveLivenessKey derives cchub's Ed25519 liveness keypair from JWT_SECRET, so
// every replica produces the same key and operators can reproduce the public key
// to embed into ccdirect. Dev fallback: a random per-process key.
func deriveLivenessKey() ed25519.PrivateKey {
	if s := os.Getenv("JWT_SECRET"); s != "" {
		seed := sha256.Sum256([]byte("ccdirect-liveness/" + s))
		return ed25519.NewKeyFromSeed(seed[:])
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		seed := sha256.Sum256([]byte("ccdirect-liveness-fallback"))
		return ed25519.NewKeyFromSeed(seed[:])
	}
	return priv
}

// LivenessPublicKey returns the base64 Ed25519 public key ccdirect must embed
// (via ldflags) to verify liveness tokens.
func (h *CCHubHandler) LivenessPublicKey() string {
	pub, ok := h.livenessKey.Public().(ed25519.PublicKey)
	if !ok {
		return ""
	}
	return contract.EncodeLivenessPubKey(pub)
}

// SetEdgeRevoked marks (or clears) an edge id as revoked. A revoked edge gets no
// liveness token on heartbeat and drains within the liveness TTL.
func (h *CCHubHandler) SetEdgeRevoked(ccdirectID string, revoked bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if revoked {
		h.revokedEdges[ccdirectID] = struct{}{}
	} else {
		delete(h.revokedEdges, ccdirectID)
	}
}

func (h *CCHubHandler) isEdgeRevoked(ccdirectID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.revokedEdges[ccdirectID]
	return ok
}

// deriveReleaseKey derives cchub's Ed25519 release-signing keypair from
// JWT_SECRET (distinct domain separator from the liveness key), so every replica
// produces the same key and operators can reproduce the public key to embed into
// ccdirect. Dev fallback: a random per-process key.
func deriveReleaseKey() ed25519.PrivateKey {
	if s := os.Getenv("JWT_SECRET"); s != "" {
		seed := sha256.Sum256([]byte("ccdirect-release/" + s))
		return ed25519.NewKeyFromSeed(seed[:])
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		seed := sha256.Sum256([]byte("ccdirect-release-fallback"))
		return ed25519.NewKeyFromSeed(seed[:])
	}
	return priv
}

// ReleasePublicKey returns the base64 Ed25519 public key ccdirect must embed
// (via ldflags) to verify release manifests.
func (h *CCHubHandler) ReleasePublicKey() string {
	pub, ok := h.releaseKey.Public().(ed25519.PublicKey)
	if !ok {
		return ""
	}
	return contract.EncodeReleasePubKey(pub)
}

// SetReleaseManifest publishes the latest ccdirect release for one os/arch. The
// manifest is signed here with the release key, so the upgrade endpoint can serve
// it without re-signing per request. version/url/sha256 come from the release
// pipeline (operator-controlled).
func (h *CCHubHandler) SetReleaseManifest(version, goos, goarch, url, sha256hex string) {
	m := contract.SignRelease(h.releaseKey, contract.ReleaseManifest{
		Version: version, OS: goos, Arch: goarch, URL: url, SHA256: sha256hex,
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	h.releaseManifests[goos+"/"+goarch] = m
}

// Release returns the signed release manifest for the requesting node's os/arch
// (query params os= and arch=). When no release is published for that platform
// it returns an empty (unsigned) manifest with 200, which ccdirect treats as
// "nothing to upgrade to". ccdirect verifies the signature with its embedded
// release public key before trusting the url+checksum.
// GET /edge/v1/release?os=&arch=
func (h *CCHubHandler) Release(c *gin.Context) {
	goos := c.Query("os")
	goarch := c.Query("arch")
	if goos == "" || goarch == "" {
		edgeCenterError(c, http.StatusBadRequest, "invalid_request", "os and arch are required")
		return
	}
	h.mu.Lock()
	m := h.releaseManifests[goos+"/"+goarch]
	h.mu.Unlock()
	c.JSON(http.StatusOK, m)
}

// Enroll exchanges an enroll key for the edge's full operating config: an
// assigned edge id, the seal secret (so it can open sealed lease tokens), the
// center-controlled egress proxy, and runtime params. This is what lets an edge
// run with ZERO local config beyond the enroll token.
func (h *CCHubHandler) Enroll(c *gin.Context) {
	var req contract.EnrollRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		edgeCenterError(c, http.StatusBadRequest, "invalid_request", "decode enroll request")
		return
	}
	if !h.enrollKeyAllowed(req.Key) {
		edgeCenterError(c, http.StatusUnauthorized, "enroll_denied", "invalid enroll key")
		return
	}
	ccdirectID := req.CCDirectID
	if ccdirectID == "" {
		ccdirectID = "edge-" + strconv.FormatInt(atomic.AddInt64(&h.enrollSeq, 1), 10)
	}
	hb := h.issuedHeartbeat
	if hb <= 0 {
		hb = 10
	}
	mf := h.issuedMaxFailover
	if mf <= 0 {
		mf = 3
	}
	c.JSON(http.StatusOK, contract.EnrollResponse{
		CCDirectID:       ccdirectID,
		CCHubURL:         h.issuedCenterURL,
		TokenSecret:      string(h.tokenSecret),
		UpstreamProxy:    h.issuedProxy,
		HeartbeatSeconds: hb,
		MaxFailover:      mf,
		Platforms:        append([]string(nil), h.issuedPlatforms...),
	})
}

// Config returns the edge's operating config to an owner-authenticated edge.
// It is the device-login counterpart of Enroll: instead of presenting an enroll
// key, the edge presents its owner's sub2api JWT (obtained via the device flow),
// and the center returns the seal secret + runtime params, with the edge id
// derived from the owner's user id (one logical edge per user). No enroll key
// needed — the JWT IS the credential.
// GET /edge/v1/config
func (h *CCHubHandler) Config(c *gin.Context) {
	uid, err := h.edgeOwnerUserID(c)
	if err != nil {
		edgeCenterError(c, http.StatusUnauthorized, "edge_unauthenticated", err.Error())
		return
	}
	if uid == 0 {
		edgeCenterError(c, http.StatusUnauthorized, "edge_unauthenticated", "owner JWT required")
		return
	}
	hb := h.issuedHeartbeat
	if hb <= 0 {
		hb = 10
	}
	mf := h.issuedMaxFailover
	if mf <= 0 {
		mf = 3
	}
	c.JSON(http.StatusOK, contract.EnrollResponse{
		CCDirectID:       "edge-u" + strconv.FormatInt(uid, 10),
		CCHubURL:         h.issuedCenterURL,
		TokenSecret:      string(h.tokenSecret),
		UpstreamProxy:    h.issuedProxy,
		HeartbeatSeconds: hb,
		MaxFailover:      mf,
		Platforms:        append([]string(nil), h.issuedPlatforms...),
	})
}

// Register records an edge in the fleet (auto-detecting its egress IP).
func (h *CCHubHandler) Register(c *gin.Context) {
	var req contract.RegisterRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.CCDirectID == "" {
		edgeCenterError(c, http.StatusBadRequest, "invalid_request", "edge_id required")
		return
	}
	ip := req.EgressIP
	if ip == "" {
		ip = c.ClientIP()
	}
	h.edges.Register(req.CCDirectID, ip, req.Platforms)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// Heartbeat keeps an edge marked live; 404 tells it to re-register.
func (h *CCHubHandler) Heartbeat(c *gin.Context) {
	var req contract.HeartbeatRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.CCDirectID == "" {
		edgeCenterError(c, http.StatusBadRequest, "invalid_request", "edge_id required")
		return
	}
	if !h.edges.Heartbeat(req.CCDirectID) {
		edgeCenterError(c, http.StatusNotFound, "unknown_edge", "edge not registered")
		return
	}
	resp := contract.HeartbeatResponse{OK: true}
	// Vouch for the edge with a fresh signed liveness token, UNLESS it is revoked
	// — a revoked edge gets ok:true but no token, so it drains within the TTL.
	if h.livenessKey != nil && !h.isEdgeRevoked(req.CCDirectID) {
		tok := contract.SignLiveness(h.livenessKey, req.CCDirectID, h.livenessTTL, time.Now)
		resp.Liveness = &tok
	}
	c.JSON(http.StatusOK, resp)
}

// Edges lists the live edge fleet (admin/observability).
func (h *CCHubHandler) Edges(c *gin.Context) {
	c.JSON(http.StatusOK, h.edges.Live())
}

// Report ingests a batched anomaly report from a ccdirect node and logs each
// aggregated item for service-quality observability. Because the data plane
// never transits cchub, these reports (plus heartbeats) are cchub's only
// fleet-health signal — lease failures, upstream error spikes, heartbeat loss
// and recovered panics surface here without any end-user prompt passing through.
func (h *CCHubHandler) Report(c *gin.Context) {
	var req contract.ErrorReport
	if err := c.ShouldBindJSON(&req); err != nil || req.CCDirectID == "" {
		edgeCenterError(c, http.StatusBadRequest, "invalid_request", "edge_id required")
		return
	}
	l := logger.L().With(zap.String("component", "handler.edge_center"))
	for _, it := range req.Items {
		l.Warn("edge_center.anomaly",
			zap.String("edge_id", req.CCDirectID),
			zap.String("kind", it.Kind),
			zap.Int("count", it.Count),
			zap.String("message", it.Message),
			zap.Int64("first_at", it.FirstAt),
			zap.Int64("last_at", it.LastAt))
	}
	c.JSON(http.StatusOK, contract.ReportResponse{OK: true, Accepted: len(req.Items)})
}

// edgeOwnerUserID validates the edge owner's sub2api JWT (presented as
// Authorization: Bearer <jwt>) and returns the owner user id. The edge holds the
// user's JWT + refresh token (sub2api's own auth system) and refreshes via
// /api/v1/auth/refresh when the access token expires — no bespoke edge
// credential. When no auth service is wired (PoC), ownership is not enforced.
func (h *CCHubHandler) edgeOwnerUserID(c *gin.Context) (int64, error) {
	if h.authService == nil {
		return 0, nil
	}
	auth := c.GetHeader("Authorization")
	const p = "Bearer "
	if len(auth) <= len(p) || !strings.EqualFold(auth[:len(p)], p) {
		return 0, errEdgeNoJWT
	}
	claims, err := h.authService.ValidateToken(strings.TrimSpace(auth[len(p):]))
	if err != nil {
		return 0, errEdgeBadJWT
	}
	return claims.UserID, nil
}

var (
	errEdgeNoJWT  = edgeCenterErr("edge owner JWT required (Authorization: Bearer)")
	errEdgeBadJWT = edgeCenterErr("edge owner JWT invalid or expired")
)

// Lease validates the sub2api API key, checks billing, selects a real account
// (acquiring its concurrency slot), and returns the account + upstream token +
// endpoint for the edge to use.
func (h *CCHubHandler) Lease(c *gin.Context) {
	var req contract.LeaseRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		edgeCenterError(c, http.StatusBadRequest, "invalid_request", "decode lease request")
		return
	}
	if req.APIKey == "" || req.Model == "" {
		edgeCenterError(c, http.StatusBadRequest, "invalid_request", "api_key and model are required")
		return
	}

	ctx := c.Request.Context()

	// 0. Edge owner: the edge presents its owner's sub2api JWT. We verify it
	// (sub2api's own JWT system — no bespoke edge credential) and bind ownership.
	ownerUserID, err := h.edgeOwnerUserID(c)
	if err != nil {
		edgeCenterError(c, http.StatusUnauthorized, "edge_unauthenticated", err.Error())
		return
	}

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

	// 1b. Owner enforcement: an edge only serves its OWNER's api keys. A key
	// belonging to a different user is rejected — the edge is a per-user egress,
	// not a shared data plane. (ownerUserID == 0 means enforcement disabled, e.g.
	// no auth service wired in the PoC.)
	if ownerUserID != 0 && apiKey.UserID != ownerUserID {
		edgeCenterError(c, http.StatusForbidden, "not_owner", "api key does not belong to this edge's owner")
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

	cand, err := h.candidateFromAccount(ctx, sel.Account)
	if err != nil {
		release()
		edgeCenterError(c, http.StatusServiceUnavailable, "no_account", err.Error())
		return
	}

	// Seal the upstream token bound to this edge + a short TTL (MANDATORY: the
	// lease token is never returned raw). The edge opens it with the seal secret
	// it received at enroll.
	if cand.LeaseToken != "" {
		sealed, sErr := contract.SealLeaseToken(cand.LeaseToken, req.CCDirectID, h.tokenTTL, h.tokenSecret, time.Now)
		if sErr != nil {
			release()
			edgeCenterError(c, http.StatusInternalServerError, "seal_failed", sErr.Error())
			return
		}
		cand.LeaseToken = sealed
	}

	slotID := newSlotID()
	h.mu.Lock()
	h.slots[slotID] = &leasedSlot{
		release:       release,
		accountID:     sel.Account.ID,
		apiKey:        apiKey,
		user:          user,
		account:       sel.Account,
		model:         req.Model,
		upstreamModel: sel.Account.GetMappedModel(req.Model),
		stream:        req.Stream,
		quotaPlatform: service.QuotaPlatform(ctx, apiKey),
	}
	h.mu.Unlock()

	c.JSON(http.StatusOK, contract.LeaseResult{
		RequestID:  req.RequestID,
		SlotID:     slotID,
		Candidates: []contract.Candidate{cand},
	})
}

// Settle releases the concurrency slot held since Lease. Idempotent on slotID.
func (h *CCHubHandler) Settle(c *gin.Context) {
	var req contract.SettleRequest
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

	// Record usage via sub2api's existing accounting (per-account + per-apikey,
	// pricing/quota) — the SAME entry the central gateway uses. Only on success
	// with real tokens; failures consume no usage.
	if !duplicate && slot != nil && slot.apiKey != nil && req.StatusCode < 400 && (req.InputTokens > 0 || req.OutputTokens > 0) {
		h.recordSettleUsage(slot, req)
	}

	c.JSON(http.StatusOK, contract.SettleResult{
		RequestID: req.RequestID,
		Accepted:  true,
		Duplicate: duplicate,
	})
}

// recordSettleUsage builds a ForwardResult from the edge-reported tokens and
// records it through sub2api's GatewayService.RecordUsage (per-account + per-
// apikey usage, pricing, quota deduction). Runs synchronously on a background
// context so it is independent of the settle request's lifetime.
func (h *CCHubHandler) recordSettleUsage(slot *leasedSlot, req contract.SettleRequest) {
	result := &service.ForwardResult{
		RequestID: req.RequestID,
		Usage: service.ClaudeUsage{
			InputTokens:              req.InputTokens,
			OutputTokens:             req.OutputTokens,
			CacheReadInputTokens:     req.CacheReadTokens,
			CacheCreationInputTokens: req.CacheCreationTokens,
		},
		Model:         slot.model,
		UpstreamModel: slot.upstreamModel,
		Stream:        slot.stream,
		Duration:      time.Duration(req.LatencyMS) * time.Millisecond,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h.gatewayService.RecordUsage(ctx, &service.RecordUsageInput{
		Result:        result,
		APIKey:        slot.apiKey,
		User:          slot.user,
		Account:       slot.account,
		QuotaPlatform: slot.quotaPlatform,
		APIKeyService: h.apiKeyService,
	}); err != nil {
		logger.L().With(zap.String("component", "handler.edge_center")).
			Error("edge_center.record_usage_failed", zap.Error(err))
	}
}

// candidateFromAccount maps a real sub2api Account to an edge Candidate. It
// resolves the access token the SAME way the gateway does — via the existing
// public GatewayService.GetAccessToken — so it is agnostic to whether the
// account is a fixed API key (token == the key, tokenType "apikey") or OAuth
// (refreshed token, tokenType "oauth"). The edge never perceives that
// difference; it only consumes the resolved access_token + endpoint + auth.
//
// Note: this calls only EXISTING public sub2api APIs and lives entirely in the
// edge extension — no sub2api core file is modified, so the fork stays
// upstream-upgradeable.
func (h *CCHubHandler) candidateFromAccount(ctx context.Context, acc *service.Account) (contract.Candidate, error) {
	// Resolve the access token via sub2api's existing public method — agnostic to
	// fixed-key vs OAuth (for a fixed-key account the token IS the api key; for
	// OAuth it is the refreshed access token).
	token, _, err := h.gatewayService.GetAccessToken(ctx, acc)
	if err != nil {
		return contract.Candidate{}, err
	}
	if token == "" {
		// Empty token => not a bearer-style credential (e.g. bedrock /
		// service_account): not servable through the edge.
		return contract.Candidate{}, errEdgeUnsupportedType
	}

	// The upstream base URL is account configuration provided when the account is
	// created (MiMo exposes one base URL per protocol: .../v1 for OpenAI,
	// .../anthropic for Anthropic; the account is created on one platform with the
	// matching base_url). Read the configured base via the protocol's own accessor
	// — this is the AnthropicProtocolProvider / OpenAIProtocolProvider distinction
	// (sub2api's two gateway services), NOT base-URL guessing.
	var base string
	switch acc.Platform {
	case service.PlatformOpenAI:
		base = acc.GetOpenAIBaseURL()
	default:
		base = acc.GetBaseURL()
	}
	if base == "" {
		return contract.Candidate{}, errEdgeNoBaseURL
	}

	// No new auth "scheme": the edge presents the credential as Authorization:
	// Bearer <token> (the protocol sub2api / MiMo-compatible upstreams + OpenAI +
	// OAuth all use) and otherwise relays the client request unchanged. The edge
	// is just one more way to execute an account; the only new surface is
	// lease/settle. (AuthScheme zero value == Authorization: Bearer.)
	return contract.Candidate{
		AccountID:       strconv.FormatInt(acc.ID, 10),
		Platform:        acc.Platform,
		UpstreamBaseURL: base,
		LeaseToken:      token,
		ModelMapping:    acc.GetModelMapping(),
	}, nil
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
