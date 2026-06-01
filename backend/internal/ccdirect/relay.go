package ccdirect

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"hash/fnv"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/ccgw/contract"
)

// Relay is the data plane. It runs on a VPS with a stable egress IP, holds
// the client connection, leases an account from the center, performs the
// upstream request itself (from this node's IP), streams the response back, and
// reports usage via Settle. It carries no durable state.
type Relay struct {
	enrollKey   string
	internalKey string
	owner       *ownerToken // always non-nil; empty tokens => logged out
	centerURL   string
	centerHTTP  *http.Client
	upstream    *http.Client
	maxFailover int
	maxBodyByte int64
	now         Clock

	// deviceKey, if non-nil, signs bound refresh requests with the per-machine
	// Ed25519 private key (see owner_token.go signDeviceRequest). Nil => unbound.
	deviceKey ed25519.PrivateKey

	// onRefresh, if set, is called whenever the owner access/refresh pair is
	// rotated (login or auto-refresh) so the caller can persist it. Set via
	// SetOnRefresh before serving.
	onRefresh func(access, refresh string)

	// secMu guards edgeID + tokenSecret, both set at login time (fetched from the
	// center's /edge/v1/config) and changed across login/logout, concurrently
	// with the relay/heartbeat goroutines reading them. The seal token is bound
	// to edgeID, so they must move together.
	secMu       sync.RWMutex
	edgeID      string
	tokenSecret []byte // shared secret to OPEN sealed lease tokens; empty = tokens are raw

	// Liveness: cchubPubKey verifies cchub-signed heartbeat liveness tokens. When
	// nil, liveness enforcement is disabled (no key embedded — dev). livenessExp
	// holds the unix-seconds expiry of the latest valid token; the relay drains
	// (refuses new requests) once it passes.
	cchubPubKey ed25519.PublicKey
	livenessExp atomic.Int64

	// reporter aggregates operational anomalies (lease failures, upstream errors,
	// heartbeat loss, liveness drains, recovered panics) for batched reporting to
	// cchub on the heartbeat interval. Always non-nil.
	reporter *anomalyReporter
}

// Config configures an Relay.
type Config struct {
	EdgeID    string
	EnrollKey string // presented to the center at registration
	// InternalKey gates /internal/egress (center-only control egress). When
	// empty, /internal/egress is disabled (denied) -- it must be explicitly
	// enabled with a shared secret to avoid an SSRF/credential-relay hole.
	InternalKey string
	// TokenSecret is the shared secret used to OPEN sealed lease tokens. Must
	// match the center's seal secret. Empty means the center returns raw tokens.
	TokenSecret []byte
	// OwnerAccessToken / OwnerRefreshToken are the edge owner's sub2api JWT and
	// refresh token. The edge presents the access token to the center (proving
	// ownership) and refreshes it via /api/v1/auth/refresh when it expires.
	OwnerAccessToken  string
	OwnerRefreshToken string
	CenterURL         string // base URL of the center, e.g. http://center:9000
	CenterHTTP        *http.Client
	Upstream          *http.Client
	MaxFailover       int
	MaxBodyByte       int64 // max client request body; 0 => 32 MiB default
	Now               Clock

	// DeviceKey, if set, is the per-machine Ed25519 private key the edge signs
	// bound refresh requests with. Nil => unbound (refresh sent unsigned).
	DeviceKey ed25519.PrivateKey

	// CchubPubKey, when set, enables liveness enforcement: the relay verifies
	// each heartbeat's signed liveness token with this key and drains when no
	// fresh valid token has arrived. Empty/nil = enforcement disabled (dev).
	CchubPubKey ed25519.PublicKey
}

// NewRelay builds an edge relay.
func NewRelay(cfg Config) *Relay {
	ch := cfg.CenterHTTP
	if ch == nil {
		ch = &http.Client{Timeout: 10 * time.Second}
	}
	up := cfg.Upstream
	if up == nil {
		up = &http.Client{Timeout: 5 * time.Minute}
	}
	mf := cfg.MaxFailover
	if mf <= 0 {
		mf = 3
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	maxBody := cfg.MaxBodyByte
	if maxBody <= 0 {
		maxBody = 32 << 20 // 32 MiB
	}
	// owner is always non-nil; empty tokens simply mean "logged out" and the
	// relay rejects requests until Login sets them. This lets device login
	// populate tokens at runtime without rebuilding the relay.
	owner := &ownerToken{access: cfg.OwnerAccessToken, refresh: cfg.OwnerRefreshToken}
	return &Relay{
		edgeID:      cfg.EdgeID,
		enrollKey:   cfg.EnrollKey,
		internalKey: cfg.InternalKey,
		tokenSecret: cfg.TokenSecret,
		owner:       owner,
		centerURL:   strings.TrimRight(cfg.CenterURL, "/"),
		centerHTTP:  ch,
		upstream:    up,
		maxFailover: mf,
		maxBodyByte: maxBody,
		deviceKey:   cfg.DeviceKey,
		cchubPubKey: cfg.CchubPubKey,
		now:         now,
		reporter:    newAnomalyReporter(now),
	}
}

// recordLiveness stores the expiry of a freshly-verified liveness token.
func (e *Relay) recordLiveness(exp int64) {
	e.livenessExp.Store(exp)
}

// livenessHealthy reports whether the relay may serve new requests. True when
// liveness enforcement is disabled (no cchub pubkey embedded) or a currently
// valid liveness token is held. Once the last token expires — cchub unreachable,
// impostor, or this edge revoked — it returns false and the relay drains.
func (e *Relay) livenessHealthy() bool {
	if e.cchubPubKey == nil {
		return true // enforcement disabled (dev / no key embedded)
	}
	return e.now().Unix() <= e.livenessExp.Load()
}

// SetOnRefresh registers a callback invoked whenever the owner token pair is
// rotated (login or auto-refresh), so the caller can persist it to disk. Not
// safe to call concurrently with serving; set it once before Handler runs.
func (e *Relay) SetOnRefresh(fn func(access, refresh string)) {
	e.onRefresh = fn
}

// Login installs the owner token pair plus the edge id + lease-token seal secret
// obtained from the center (GET /edge/v1/config), making the relay able to
// serve. Safe to call while serving. It also fires onRefresh so the new pair is
// persisted.
func (e *Relay) Login(access, refresh, edgeID string, tokenSecret []byte) {
	e.owner.set(access, refresh)
	e.secMu.Lock()
	e.edgeID = edgeID
	e.tokenSecret = tokenSecret
	e.secMu.Unlock()
	if e.onRefresh != nil {
		e.onRefresh(access, refresh)
	}
}

// Logout clears the owner tokens, edge id and seal secret. Subsequent lease
// calls fail auth (401) so the relay stops serving until the next Login. Fires
// onRefresh with empty strings so persisted creds are cleared too.
func (e *Relay) Logout() {
	e.owner.clear()
	e.secMu.Lock()
	e.edgeID = ""
	e.tokenSecret = nil
	e.secMu.Unlock()
	if e.onRefresh != nil {
		e.onRefresh("", "")
	}
}

// EdgeID returns the current edge id (empty until login).
func (e *Relay) EdgeID() string {
	e.secMu.RLock()
	defer e.secMu.RUnlock()
	return e.edgeID
}

// LoggedIn reports whether the relay currently holds an owner access token.
func (e *Relay) LoggedIn() bool {
	return e.owner.accessToken() != ""
}

// OwnerAccess returns the current owner access JWT (for status / JWT parsing).
func (e *Relay) OwnerAccess() string {
	return e.owner.accessToken()
}

// OwnerRefresh returns the current owner refresh token (for server-side logout).
func (e *Relay) OwnerRefresh() string {
	return e.owner.refreshToken()
}

// RefreshOwner forces an owner access-token refresh via the center and persists
// the rotated pair (through onRefresh). Returns true on success. Exposed so the
// CLI can bring a relay online from a saved session whose access token expired
// while the edge was stopped.
func (e *Relay) RefreshOwner(ctx context.Context) bool {
	return e.refreshOwner(ctx)
}

// SetSeal installs the edge id + seal secret without touching the owner tokens.
// Used to bring the relay online from an existing session (after fetching
// /edge/v1/config) without overwriting the token pair.
func (e *Relay) SetSeal(edgeID string, tokenSecret []byte) {
	e.secMu.Lock()
	e.edgeID = edgeID
	e.tokenSecret = tokenSecret
	e.secMu.Unlock()
}

// sealState returns the current edge id + seal secret together under one read
// lock, so the seal token is always opened against the matching edge id.
func (e *Relay) sealState() (edgeID string, secret []byte) {
	e.secMu.RLock()
	defer e.secMu.RUnlock()
	return e.edgeID, e.tokenSecret
}

// unwrapToken resolves the real upstream credential from a lease token. When a
// token secret is configured the token is an AEAD-sealed envelope bound to this
// edge + an expiry (contract.OpenLeaseToken); otherwise it is raw (or a PoC HMAC
// envelope handled by contract.UpstreamBearer).
func (e *Relay) unwrapToken(leaseToken string) (string, error) {
	if edgeID, secret := e.sealState(); len(secret) > 0 {
		return contract.OpenLeaseToken(leaseToken, edgeID, secret, e.now)
	}
	return contract.UpstreamBearer(leaseToken), nil
}

// Handler returns the edge's HTTP mux. It accepts the upstream-compatible paths
// and relays them.
func (e *Relay) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	// Control-plane egress: the center runs account-bound outbound calls (e.g.
	// OAuth refresh) through this edge so they leave from the edge's stable IP.
	mux.HandleFunc("/internal/egress", e.handleEgress)
	mux.HandleFunc("/", e.relay)
	return mux
}

func (e *Relay) relay(w http.ResponseWriter, r *http.Request) {
	// Recover panics so one bad request cannot crash a long-lived daemon; the
	// recovered panic is reported to cchub for service-quality visibility.
	defer func() {
		if rec := recover(); rec != nil {
			e.reportPanic(rec)
			writeJSONError(w, http.StatusInternalServerError, "internal_error", "edge internal error")
		}
	}()
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Liveness drain: refuse NEW requests when cchub has not vouched for this edge
	// recently (unreachable / impostor / this edge revoked). Requests already past
	// this point finish normally — graceful drain.
	if !e.livenessHealthy() {
		e.reportAnomaly("liveness_drain", "edge draining: no current cchub liveness")
		writeJSONError(w, http.StatusServiceUnavailable, "draining", "edge is draining: no current cchub liveness")
		return
	}
	defer func() { _ = r.Body.Close() }()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, e.maxBodyByte))
	if err != nil {
		writeJSONError(w, http.StatusRequestEntityTooLarge, "invalid_request", "read body: "+err.Error())
		return
	}

	apiKey := extractAPIKey(r)
	model, stream := ParseModelStream(body)
	if model == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "model is required")
		return
	}
	requestID := r.Header.Get("X-Request-Id")
	if requestID == "" {
		requestID = newID()
	}
	sessionHash := sessionHashFor(apiKey, model, body)

	// 1. Lease an account from the center.
	lease, status, err := e.callLease(r.Context(), contract.LeaseRequest{
		APIKey:      apiKey,
		Model:       model,
		SessionHash: sessionHash,
		RequestID:   requestID,
		EdgeID:      e.EdgeID(),
		Stream:      stream,
	})
	if err != nil {
		// Propagate the center's rejection status (rate limit / no account / ...).
		if status == 0 {
			status = http.StatusBadGateway
		}
		e.reportAnomaly("lease_failed", err.Error())
		writeJSONError(w, status, "lease_failed", err.Error())
		return
	}

	// 2. Forward upstream, failing over locally down the ranked candidates.
	start := e.now()
	used, code, usage, streamed, ferr := e.forward(r, w, body, model, lease)
	latency := e.now().Sub(start).Milliseconds()

	// 3. Settle: report usage so the center reconciles quota + releases the slot.
	settle := contract.SettleRequest{
		RequestID:           requestID,
		APIKey:              apiKey,
		AccountID:           used.AccountID,
		SlotID:              lease.SlotID,
		SessionHash:         sessionHash,
		InputTokens:         usage.Input,
		OutputTokens:        usage.Output,
		CacheReadTokens:     usage.CacheRead,
		CacheCreationTokens: usage.CacheCreation,
		StatusCode:          code,
		LatencyMS:           latency,
		Partial:             ferr != nil && streamed,
	}
	// Settle on a fresh context: the client request context may be done once we
	// finish streaming, but the slot still must be released.
	e.callSettle(context.WithoutCancel(r.Context()), settle)

	if ferr != nil && !streamed {
		e.reportAnomaly("upstream_failed", ferr.Error())
		writeJSONError(w, http.StatusBadGateway, "upstream_failed", ferr.Error())
	}
}

// forward tries each candidate in order until one succeeds or the stream has
// already started (after which failover is unsafe). Returns the used candidate,
// the upstream status code, token usage, whether bytes were written to the
// client, and any terminal error.
func (e *Relay) forward(r *http.Request, w http.ResponseWriter, body []byte, reqModel string, lease *contract.LeaseResult) (used contract.Candidate, code int, usage Usage, streamed bool, err error) {
	limit := e.maxFailover
	if limit > len(lease.Candidates) {
		limit = len(lease.Candidates)
	}
	_, stream := ParseModelStream(body)
	for i := 0; i < limit; i++ {
		cand := lease.Candidates[i]
		used = cand
		provider := ProviderFor(cand.Platform)

		// Provider-aware request prep: model mapping lands in the body
		// (Anthropic/OpenAI) or the URL path (Gemini).
		upstreamPath, upstreamBody := provider.PrepareRequest(r.URL.Path, body, cand.MappedModel(reqModel))
		// Protocol-uniform join: handles bases that already include a version
		// segment (OpenAI ".../v1") without doubling /v1.
		upstreamURL := JoinUpstreamURL(cand.UpstreamBaseURL, upstreamPath)
		// Preserve the client's query string (e.g. ?beta=true for Anthropic OAuth).
		if r.URL.RawQuery != "" {
			upstreamURL += "?" + r.URL.RawQuery
		}
		upReq, buildErr := http.NewRequestWithContext(r.Context(), http.MethodPost,
			upstreamURL, bytes.NewReader(upstreamBody))
		if buildErr != nil {
			err = buildErr
			continue
		}
		// Resolve the real upstream credential from the lease token (AEAD-sealed
		// when a token secret is configured, else raw/HMAC-enveloped). A token
		// that fails to open (wrong edge / expired / tampered) drops this candidate.
		bearer, uerr := e.unwrapToken(cand.LeaseToken)
		if uerr != nil {
			err = uerr
			continue
		}
		copyForwardHeaders(r.Header, upReq.Header)
		// Present the leased credential per the account's auth scheme.
		applyAuthScheme(cand.AuthScheme, upReq, bearer)
		upReq.Header.Set("X-Edge-Id", e.EdgeID())

		resp, doErr := e.upstream.Do(upReq)
		if doErr != nil {
			err = doErr
			continue // transport failure: try next candidate (nothing written yet)
		}

		// 5xx with no body streamed yet: fail over to the next candidate.
		if resp.StatusCode >= 500 && i < limit-1 {
			_ = resp.Body.Close()
			err = &upstreamStatusError{code: resp.StatusCode}
			continue
		}

		code = resp.StatusCode
		// Tee the response through the provider's usage parser while streaming
		// it to the client, then fall back to usage headers if the provider
		// reported nothing (keeps header-only upstreams working).
		parser := provider.NewUsageParser(stream)
		copyResponseHeaders(resp.Header, w.Header())
		w.WriteHeader(resp.StatusCode)
		streamed = true
		_, copyErr := io.Copy(newFlushWriter(w), io.TeeReader(resp.Body, parser))
		_ = resp.Body.Close()
		usage = parser.Usage()
		if !usage.any() {
			usage = readUsageHeaders(resp.Header)
		}
		if copyErr != nil {
			err = copyErr
			return used, code, usage, streamed, err
		}
		return used, code, usage, streamed, nil
	}
	if err == nil {
		err = contract.ErrNoAccount
	}
	return used, code, usage, streamed, err
}

func (e *Relay) callLease(ctx context.Context, req contract.LeaseRequest) (*contract.LeaseResult, int, error) {
	buf, _ := json.Marshal(req)
	do := func() (*http.Response, error) {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, e.centerURL+"/v1/lease", bytes.NewReader(buf))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		e.authHeader(httpReq)
		return e.centerHTTP.Do(httpReq)
	}
	resp, err := do()
	if err != nil {
		return nil, 0, err
	}
	// Owner JWT expired -> refresh once via sub2api's auth endpoint and retry.
	if resp.StatusCode == http.StatusUnauthorized && e.refreshOwner(ctx) {
		_ = resp.Body.Close()
		if resp, err = do(); err != nil {
			return nil, 0, err
		}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, decodeAPIError(resp.Body)
	}
	var out contract.LeaseResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, resp.StatusCode, err
	}
	return &out, resp.StatusCode, nil
}

func (e *Relay) callSettle(ctx context.Context, req contract.SettleRequest) {
	buf, _ := json.Marshal(req)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, e.centerURL+"/v1/settle", bytes.NewReader(buf))
	if err != nil {
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")
	e.authHeader(httpReq)
	resp, err := e.centerHTTP.Do(httpReq)
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

// --- helpers ---

type upstreamStatusError struct{ code int }

func (e *upstreamStatusError) Error() string { return "upstream status " + strconv.Itoa(e.code) }

func extractAPIKey(r *http.Request) string {
	if v := r.Header.Get("x-api-key"); v != "" {
		return v
	}
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		return strings.TrimSpace(auth[len("bearer "):])
	}
	return auth
}

func ParseModelStream(body []byte) (model string, stream bool) {
	var m struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return "", false
	}
	return m.Model, m.Stream
}

func rewriteModel(body []byte, newModel string) []byte {
	var generic map[string]any
	if err := json.Unmarshal(body, &generic); err != nil {
		return body
	}
	if _, ok := generic["model"]; !ok {
		return body
	}
	generic["model"] = newModel
	out, err := json.Marshal(generic)
	if err != nil {
		return body
	}
	return out
}

func sessionHashFor(apiKey, model string, body []byte) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(apiKey))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(model))
	_, _ = h.Write([]byte{0})
	// First user turn dominates the conversation identity; hashing the whole
	// body keeps the demo simple and deterministic.
	_, _ = h.Write(body)
	return strconv.FormatUint(h.Sum64(), 16)
}

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "req-fallback"
	}
	return hex.EncodeToString(b[:])
}

func readUsageHeaders(h http.Header) Usage {
	in, _ := strconv.Atoi(h.Get("X-Usage-Input-Tokens"))
	out, _ := strconv.Atoi(h.Get("X-Usage-Output-Tokens"))
	cr, _ := strconv.Atoi(h.Get("X-Usage-Cache-Read-Tokens"))
	cc, _ := strconv.Atoi(h.Get("X-Usage-Cache-Creation-Tokens"))
	return Usage{Input: in, Output: out, CacheRead: cr, CacheCreation: cc}
}

// hopByHopHeaders are dropped when forwarding upstream. Besides the standard
// hop-by-hop set, this includes every client-side credential header: the edge
// authenticates the client to the CENTER (sub2api API key), and authenticates
// itself to the UPSTREAM with the leased provider token. The client's sub2api
// credential must never reach the provider.
var hopByHopHeaders = map[string]struct{}{
	"Connection": {}, "Keep-Alive": {}, "Proxy-Authenticate": {},
	"Proxy-Authorization": {}, "Te": {}, "Trailer": {}, "Transfer-Encoding": {},
	"Upgrade": {}, "Host": {}, "Content-Length": {},
	// client credentials — stripped, replaced by the leased provider auth:
	"Authorization": {}, "X-Api-Key": {}, "X-Goog-Api-Key": {}, "Api-Key": {},
}

func copyForwardHeaders(src, dst http.Header) {
	for k, vv := range src {
		if _, skip := hopByHopHeaders[http.CanonicalHeaderKey(k)]; skip {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func copyResponseHeaders(src, dst http.Header) {
	for k, vv := range src {
		if _, skip := hopByHopHeaders[http.CanonicalHeaderKey(k)]; skip {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func decodeAPIError(body io.Reader) error {
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(body).Decode(&e); err != nil {
		return errBadGateway
	}
	if e.Error.Message != "" {
		return &apiError{msg: e.Error.Message}
	}
	return errBadGateway
}

type apiError struct{ msg string }

func (a *apiError) Error() string { return a.msg }

var errBadGateway = &apiError{msg: "center returned non-OK status"}

// flushWriter flushes after every write so SSE chunks reach the client
// promptly during streaming.
type flushWriter struct {
	w http.ResponseWriter
	f http.Flusher
}

func newFlushWriter(w http.ResponseWriter) io.Writer {
	if f, ok := w.(http.Flusher); ok {
		return &flushWriter{w: w, f: f}
	}
	return w
}

func (fw *flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	if fw.f != nil {
		fw.f.Flush()
	}
	return n, err
}
