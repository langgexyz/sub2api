package ccdirect

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/edgegw/contract"
)

// Edge -> center registration + heartbeat. On startup the edge announces itself
// and its stable egress IP; a heartbeat loop keeps it marked live so the center
// can route account->edge bindings only to healthy edges. Re-registration is
// automatic if a heartbeat is rejected as "unknown" (e.g. after a center
// restart). Uses the relay's center HTTP client, so it inherits mTLS in prod.

// Register announces this edge to the center once.
func (e *Relay) Register(ctx context.Context, egressIP string, platforms []string) error {
	body, _ := json.Marshal(contract.RegisterRequest{EdgeID: e.EdgeID(), EnrollKey: e.enrollKey, EgressIP: egressIP, Platforms: platforms})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.centerURL+"/v1/register", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.centerHTTP.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}

// heartbeat pings the center once; returns false if the center does not know
// this edge (caller should re-register).
func (e *Relay) heartbeat(ctx context.Context) (known bool, err error) {
	body, _ := json.Marshal(contract.HeartbeatRequest{EdgeID: e.EdgeID()})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.centerURL+"/v1/heartbeat", bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.centerHTTP.Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	// Parse + verify the cchub-signed liveness token; record its expiry so the
	// relay keeps serving. A missing/invalid token (revoked edge, impostor, clock
	// skew) is simply not recorded — the relay drains once the last valid token
	// expires. Verification is skipped when no cchub pubkey is embedded.
	var hbResp contract.HeartbeatResponse
	if derr := json.NewDecoder(resp.Body).Decode(&hbResp); derr == nil && hbResp.Liveness != nil && e.cchubPubKey != nil {
		if verr := contract.VerifyLiveness(e.cchubPubKey, *hbResp.Liveness, e.EdgeID(), e.now); verr == nil {
			e.recordLiveness(hbResp.Liveness.ExpiresAt)
		}
	}
	return true, nil
}

// RunHeartbeat registers once, then heartbeats every interval until ctx is done,
// re-registering whenever the center reports this edge as unknown. Intended to
// run in its own goroutine.
func (e *Relay) RunHeartbeat(ctx context.Context, egressIP string, platforms []string, interval time.Duration) {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	_ = e.Register(ctx, egressIP, platforms)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			known, err := e.heartbeat(ctx)
			if err == nil && !known {
				_ = e.Register(ctx, egressIP, platforms)
			}
		}
	}
}
