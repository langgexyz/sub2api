package contract

// Enrollment + fleet registration wire types, shared between ccdirect and cchub.

// RegisterRequest announces a ccdirect node to cchub.
type RegisterRequest struct {
	EdgeID    string   `json:"edge_id"`
	EnrollKey string   `json:"enroll_key"`
	EgressIP  string   `json:"egress_ip"`
	Platforms []string `json:"platforms"`
}

// HeartbeatRequest keeps a ccdirect node marked live.
type HeartbeatRequest struct {
	EdgeID string `json:"edge_id"`
}

// RegisterResponse acknowledges a register/heartbeat.
type RegisterResponse struct {
	OK bool `json:"ok"`
}

// EnrollRequest is what ccdirect sends to /v1/enroll (key from the user's token).
type EnrollRequest struct {
	EdgeID string `json:"edge_id"`
	Key    string `json:"key"`
}

// EnrollResponse is the cchub-issued ccdirect configuration.
type EnrollResponse struct {
	EdgeID           string   `json:"edge_id"`
	CenterURL        string   `json:"center_url"`
	TokenSecret      string   `json:"token_secret"`
	HeartbeatSeconds int      `json:"heartbeat_seconds"`
	MaxFailover      int      `json:"max_failover"`
	Platforms        []string `json:"platforms"`
	UpstreamProxy    string   `json:"upstream_proxy"`
	UpstreamTimeout  int      `json:"upstream_timeout"`
}
