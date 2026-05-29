package edgegw

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// Edge -> center registration + heartbeat. On startup the edge announces itself
// and its stable egress IP; a heartbeat loop keeps it marked live so the center
// can route account->edge bindings only to healthy edges. Re-registration is
// automatic if a heartbeat is rejected as "unknown" (e.g. after a center
// restart). Uses the relay's center HTTP client, so it inherits mTLS in prod.

// Register announces this edge to the center once.
func (e *EdgeRelay) Register(ctx context.Context, egressIP string, platforms []string) error {
	body, _ := json.Marshal(RegisterRequest{EdgeID: e.edgeID, EnrollKey: e.enrollKey, EgressIP: egressIP, Platforms: platforms})
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
func (e *EdgeRelay) heartbeat(ctx context.Context) (known bool, err error) {
	body, _ := json.Marshal(HeartbeatRequest{EdgeID: e.edgeID})
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
	return true, nil
}

// RunHeartbeat registers once, then heartbeats every interval until ctx is done,
// re-registering whenever the center reports this edge as unknown. Intended to
// run in its own goroutine.
func (e *EdgeRelay) RunHeartbeat(ctx context.Context, egressIP string, platforms []string, interval time.Duration) {
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
