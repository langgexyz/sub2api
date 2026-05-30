package edgegw

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"sync"
)

// ownerToken holds the edge owner's sub2api JWT (access) + refresh token and
// refreshes the access token via sub2api's own /api/v1/auth/refresh when the
// center rejects it as expired. This reuses sub2api's auth system end to end:
// the edge is just a client holding a user session, no bespoke edge credential.
type ownerToken struct {
	mu      sync.Mutex
	access  string
	refresh string
}

func (t *ownerToken) accessToken() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.access
}

func (t *ownerToken) refreshToken() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.refresh
}

// set replaces both tokens (used by device login at runtime).
func (t *ownerToken) set(access, refresh string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.access = access
	t.refresh = refresh
}

// clear wipes both tokens (logout).
func (t *ownerToken) clear() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.access = ""
	t.refresh = ""
}

// authHeader sets Authorization: Bearer <access jwt> on a center request.
func (e *EdgeRelay) authHeader(req *http.Request) {
	if e.owner == nil {
		return
	}
	if tok := e.owner.accessToken(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
}

// refreshOwner exchanges the refresh token for a fresh access JWT via the
// center's sub2api auth endpoint. Returns true on success.
func (e *EdgeRelay) refreshOwner(ctx context.Context) bool {
	if e.owner == nil {
		return false
	}
	e.owner.mu.Lock()
	refresh := e.owner.refresh
	e.owner.mu.Unlock()
	if refresh == "" {
		return false
	}

	body, _ := json.Marshal(map[string]string{"refresh_token": refresh})
	// The auth API lives under the center host at /api/v1/auth/refresh (the edge
	// is configured with the center base ending in /edge; strip it to reach /api).
	base := authBaseFromCenter(e.centerURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/v1/auth/refresh", bytes.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.centerHTTP.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var out struct {
		Data struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.Data.AccessToken == "" {
		return false
	}
	e.owner.mu.Lock()
	e.owner.access = out.Data.AccessToken
	if out.Data.RefreshToken != "" {
		e.owner.refresh = out.Data.RefreshToken
	}
	access, refresh := e.owner.access, e.owner.refresh
	e.owner.mu.Unlock()
	// Persist the rotated pair so a restart doesn't lose the renewed session.
	// Without this the edge keeps only the in-memory tokens and falls back to the
	// (possibly stale) on-disk pair after a restart.
	if e.onRefresh != nil {
		e.onRefresh(access, refresh)
	}
	return true
}

// authBaseFromCenter strips a trailing "/edge" path segment from the center URL
// so auth calls reach the sub2api API root.
func authBaseFromCenter(centerURL string) string {
	const suffix = "/edge"
	if len(centerURL) >= len(suffix) && centerURL[len(centerURL)-len(suffix):] == suffix {
		return centerURL[:len(centerURL)-len(suffix)]
	}
	return centerURL
}
