package edgegw

import (
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"sync/atomic"
)

// Enrollment turns one user-supplied token into a fully-configured edge: the
// center validates the enroll key, assigns an edge ID, and issues the operating
// parameters (heartbeat interval, failover count, platforms) so the edge needs
// no local flags beyond the token. See cmd/edge and internal/edgegw/enroll.

// EnrollRequest is what an edge sends to /v1/enroll (key from the user's token).
type EnrollRequest struct {
	Key    string `json:"key"`
	EdgeID string `json:"edge_id,omitempty"` // optional preferred id; the center assigns one if empty
}

// EnrollResponse is the center-issued edge configuration.
type EnrollResponse struct {
	EdgeID           string   `json:"edge_id"`
	CenterURL        string   `json:"center_url,omitempty"`
	HeartbeatSeconds int      `json:"heartbeat_seconds"`
	MaxFailover      int      `json:"max_failover"`
	Platforms        []string `json:"platforms,omitempty"`
}

// SetEnrollConfig sets the parameters the center issues to edges at enroll time.
func (s *CenterServer) SetEnrollConfig(centerURL string, heartbeatSeconds, maxFailover int, platforms []string) {
	s.mu.Lock()
	s.issuedCenterURL = centerURL
	s.issuedHeartbeat = heartbeatSeconds
	s.issuedMaxFailover = maxFailover
	s.issuedPlatforms = append([]string(nil), platforms...)
	s.mu.Unlock()
}

func (s *CenterServer) handleEnroll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req EnrollRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "decode enroll request: "+err.Error())
		return
	}
	if !s.enrollKeyAllowed(req.Key) {
		writeJSONError(w, http.StatusUnauthorized, "enroll_denied", "invalid enroll key")
		return
	}

	edgeID := req.EdgeID
	if edgeID == "" {
		edgeID = "edge-" + strconv.FormatInt(atomic.AddInt64(&s.enrollSeq, 1), 10)
	}

	s.mu.Lock()
	resp := EnrollResponse{
		EdgeID:           edgeID,
		CenterURL:        s.issuedCenterURL,
		HeartbeatSeconds: s.issuedHeartbeat,
		MaxFailover:      s.issuedMaxFailover,
		Platforms:        append([]string(nil), s.issuedPlatforms...),
	}
	s.mu.Unlock()
	if resp.HeartbeatSeconds <= 0 {
		resp.HeartbeatSeconds = 10
	}
	if resp.MaxFailover <= 0 {
		resp.MaxFailover = 3
	}
	writeJSON(w, http.StatusOK, resp)
}

// clientIPFromRemoteAddr extracts the host portion of an http RemoteAddr.
func clientIPFromRemoteAddr(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}
