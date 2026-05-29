package edgegw

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
)

// Egress lets the CENTER perform an outbound HTTP request from an EDGE's stable
// IP. Its primary use is OAuth token refresh: the center remains the single
// writer (it holds the refresh token and decides when to refresh), but the
// actual refresh request must leave from the account's home-edge IP so the
// upstream provider always sees a consistent source IP for that account -- the
// same IP the data-plane uses. The center never has to guess whether a provider
// pins refresh to an IP; routing refresh through the home edge makes it
// consistent by construction.
//
// The edge replays the request through its own egress proxy/IP (the same
// http.Client used for prompt forwarding) and returns the response.

// EgressRequest is an outbound request the center asks an edge to perform.
type EgressRequest struct {
	Method string            `json:"method"`
	URL    string            `json:"url"`
	Header map[string]string `json:"header,omitempty"`
	Body   []byte            `json:"body,omitempty"`
}

// EgressResponse is the upstream response captured by the edge.
type EgressResponse struct {
	StatusCode int               `json:"status_code"`
	Header     map[string]string `json:"header,omitempty"`
	Body       []byte            `json:"body,omitempty"`
}

const maxEgressResponseBytes = 1 << 20 // 1 MiB cap for control-plane responses (token endpoints are tiny)

// handleEgress executes the requested outbound call through this edge's egress
// client and returns the captured response. Mounted at POST /internal/egress
// (mTLS-restricted to the center in production).
func (e *EdgeRelay) handleEgress(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req EgressRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "decode egress request: "+err.Error())
		return
	}
	if req.Method == "" || req.URL == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "method and url are required")
		return
	}

	var bodyReader io.Reader
	if len(req.Body) > 0 {
		bodyReader = bytes.NewReader(req.Body)
	}
	outReq, err := http.NewRequestWithContext(r.Context(), req.Method, req.URL, bodyReader)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "build egress request: "+err.Error())
		return
	}
	for k, v := range req.Header {
		outReq.Header.Set(k, v)
	}
	outReq.Header.Set("X-Edge-Id", e.edgeID)

	// Use the edge's egress client (proxied -> the VPS's stable IP).
	resp, err := e.upstream.Do(outReq)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "egress_failed", err.Error())
		return
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxEgressResponseBytes))
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "egress_read_failed", err.Error())
		return
	}
	out := EgressResponse{
		StatusCode: resp.StatusCode,
		Header:     map[string]string{},
		Body:       body,
	}
	for k := range resp.Header {
		out.Header[k] = resp.Header.Get(k)
	}
	writeJSON(w, http.StatusOK, out)
}

// EgressVia asks the edge at edgeBaseURL to perform req from its stable IP and
// returns the captured response. The center uses this to run OAuth refresh (and
// any other account-bound control call) through the account's home edge.
func EgressVia(ctx context.Context, httpClient *http.Client, edgeBaseURL string, req EgressRequest) (*EgressResponse, error) {
	buf, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, edgeBaseURL+"/internal/egress", bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, decodeAPIError(resp.Body)
	}
	var out EgressResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}
