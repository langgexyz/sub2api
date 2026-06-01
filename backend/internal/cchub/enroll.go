package cchub

import (
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"sync/atomic"

	"github.com/Wei-Shaw/sub2api/internal/ccgw/contract"
)

// Enrollment turns one user-supplied token into a fully-configured edge: the
// center validates the enroll key, assigns an edge ID, and issues the operating
// parameters (heartbeat interval, failover count, platforms) so the edge needs
// no local flags beyond the token. See cmd/ccdirect and internal/ccgw/enroll.

// contract.EnrollRequest / contract.EnrollResponse moved to the shared contract package (aliased
// in contract.go) — both ccdirect and cchub use them.

// SetEnrollConfig sets the parameters the center issues to edges at enroll time.
func (s *Server) SetEnrollConfig(cchubURL string, heartbeatSeconds, maxFailover int, platforms []string) {
	s.mu.Lock()
	s.issuedCenterURL = cchubURL
	s.issuedHeartbeat = heartbeatSeconds
	s.issuedMaxFailover = maxFailover
	s.issuedPlatforms = append([]string(nil), platforms...)
	s.mu.Unlock()
}

func (s *Server) handleEnroll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req contract.EnrollRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "decode enroll request: "+err.Error())
		return
	}
	if !s.enrollKeyAllowed(req.Key) {
		writeJSONError(w, http.StatusUnauthorized, "enroll_denied", "invalid enroll key")
		return
	}

	ccdirectID := req.CCDirectID
	if ccdirectID == "" {
		ccdirectID = "edge-" + strconv.FormatInt(atomic.AddInt64(&s.enrollSeq, 1), 10)
	}

	s.mu.Lock()
	resp := contract.EnrollResponse{
		CCDirectID:       ccdirectID,
		CCHubURL:         s.issuedCenterURL,
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
