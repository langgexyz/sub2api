package edgegw

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"hash/fnv"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// EdgeRelay is the data plane. It runs on a VPS with a stable egress IP, holds
// the client connection, leases an account from the center, performs the
// upstream request itself (from this node's IP), streams the response back, and
// reports usage via Settle. It carries no durable state.
type EdgeRelay struct {
	edgeID      string
	centerURL   string
	centerHTTP  *http.Client
	upstream    *http.Client
	maxFailover int
	now         Clock
}

// EdgeConfig configures an EdgeRelay.
type EdgeConfig struct {
	EdgeID      string
	CenterURL   string // base URL of the center, e.g. http://center:9000
	CenterHTTP  *http.Client
	Upstream    *http.Client
	MaxFailover int
	Now         Clock
}

// NewEdgeRelay builds an edge relay.
func NewEdgeRelay(cfg EdgeConfig) *EdgeRelay {
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
	return &EdgeRelay{
		edgeID:      cfg.EdgeID,
		centerURL:   strings.TrimRight(cfg.CenterURL, "/"),
		centerHTTP:  ch,
		upstream:    up,
		maxFailover: mf,
		now:         now,
	}
}

// Handler returns the edge's HTTP mux. It accepts the upstream-compatible paths
// and relays them.
func (e *EdgeRelay) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/", e.relay)
	return mux
}

func (e *EdgeRelay) relay(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}

	apiKey := extractAPIKey(r)
	model, stream := parseModelStream(body)
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
	lease, status, err := e.callLease(r.Context(), LeaseRequest{
		APIKey:      apiKey,
		Model:       model,
		SessionHash: sessionHash,
		RequestID:   requestID,
		EdgeID:      e.edgeID,
		Stream:      stream,
	})
	if err != nil {
		// Propagate the center's rejection status (rate limit / no account / ...).
		if status == 0 {
			status = http.StatusBadGateway
		}
		writeJSONError(w, status, "lease_failed", err.Error())
		return
	}

	// 2. Forward upstream, failing over locally down the ranked candidates.
	start := e.now()
	used, code, inTok, outTok, streamed, ferr := e.forward(r, w, body, model, lease)
	latency := e.now().Sub(start).Milliseconds()

	// 3. Settle: report usage so the center reconciles quota + releases the slot.
	settle := SettleRequest{
		RequestID:    requestID,
		AccountID:    used.AccountID,
		SlotID:       lease.SlotID,
		SessionHash:  sessionHash,
		InputTokens:  inTok,
		OutputTokens: outTok,
		StatusCode:   code,
		LatencyMS:    latency,
		Partial:      ferr != nil && streamed,
	}
	// Settle on a fresh context: the client request context may be done once we
	// finish streaming, but the slot still must be released.
	e.callSettle(context.WithoutCancel(r.Context()), settle)

	if ferr != nil && !streamed {
		writeJSONError(w, http.StatusBadGateway, "upstream_failed", ferr.Error())
	}
}

// forward tries each candidate in order until one succeeds or the stream has
// already started (after which failover is unsafe). Returns the used candidate,
// the upstream status code, token usage, whether bytes were written to the
// client, and any terminal error.
func (e *EdgeRelay) forward(r *http.Request, w http.ResponseWriter, body []byte, reqModel string, lease *LeaseResult) (used Candidate, code, inTok, outTok int, streamed bool, err error) {
	limit := e.maxFailover
	if limit > len(lease.Candidates) {
		limit = len(lease.Candidates)
	}
	for i := 0; i < limit; i++ {
		cand := lease.Candidates[i]
		used = cand

		upstreamBody := rewriteModel(body, cand.MappedModel(reqModel))
		upReq, buildErr := http.NewRequestWithContext(r.Context(), http.MethodPost,
			cand.UpstreamBaseURL+r.URL.Path, bytes.NewReader(upstreamBody))
		if buildErr != nil {
			err = buildErr
			continue
		}
		copyForwardHeaders(r.Header, upReq.Header)
		// The edge presents the leased credential. UpstreamBearer unwraps the
		// minted envelope to the real upstream token.
		upReq.Header.Set("Authorization", "Bearer "+UpstreamBearer(cand.LeaseToken))
		upReq.Header.Set("X-Edge-Id", e.edgeID)

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
		inTok, outTok = readUsageHeaders(resp.Header)
		// Relay status + headers, then stream the body to the client.
		copyResponseHeaders(resp.Header, w.Header())
		w.WriteHeader(resp.StatusCode)
		streamed = true
		_, copyErr := io.Copy(newFlushWriter(w), resp.Body)
		_ = resp.Body.Close()
		if copyErr != nil {
			err = copyErr
			return used, code, inTok, outTok, streamed, err
		}
		return used, code, inTok, outTok, streamed, nil
	}
	if err == nil {
		err = ErrNoAccount
	}
	return used, code, inTok, outTok, streamed, err
}

func (e *EdgeRelay) callLease(ctx context.Context, req LeaseRequest) (*LeaseResult, int, error) {
	buf, _ := json.Marshal(req)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, e.centerURL+"/v1/lease", bytes.NewReader(buf))
	if err != nil {
		return nil, 0, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := e.centerHTTP.Do(httpReq)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, decodeAPIError(resp.Body)
	}
	var out LeaseResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, resp.StatusCode, err
	}
	return &out, resp.StatusCode, nil
}

func (e *EdgeRelay) callSettle(ctx context.Context, req SettleRequest) {
	buf, _ := json.Marshal(req)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, e.centerURL+"/v1/settle", bytes.NewReader(buf))
	if err != nil {
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")
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

func parseModelStream(body []byte) (model string, stream bool) {
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

func readUsageHeaders(h http.Header) (in, out int) {
	in, _ = strconv.Atoi(h.Get("X-Usage-Input-Tokens"))
	out, _ = strconv.Atoi(h.Get("X-Usage-Output-Tokens"))
	return in, out
}

var hopByHopHeaders = map[string]struct{}{
	"Connection": {}, "Keep-Alive": {}, "Proxy-Authenticate": {},
	"Proxy-Authorization": {}, "Te": {}, "Trailer": {}, "Transfer-Encoding": {},
	"Upgrade": {}, "Authorization": {}, "Host": {}, "Content-Length": {},
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
