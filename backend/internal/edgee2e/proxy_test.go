//go:build unit

package edgee2e

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/ccdirect"
	"github.com/Wei-Shaw/sub2api/internal/cchub"
)

// recordingProxy is a minimal forward proxy: for plaintext (http) targets the
// edge's Transport sends an absolute-URI request here, which we forward to the
// real target. It records how many requests transited it so the test can prove
// upstream egress actually went through the proxy (the mechanism behind
// per-edge stable IPs).
type recordingProxy struct {
	mu     sync.Mutex
	hits   int
	client *http.Client
}

func (p *recordingProxy) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hits
}

func (p *recordingProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.hits++
	p.mu.Unlock()

	outReq, err := http.NewRequestWithContext(r.Context(), r.Method, r.URL.String(), r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	for k, vv := range r.Header {
		for _, v := range vv {
			outReq.Header.Add(k, v)
		}
	}
	outReq.Header.Set("X-Via-Proxy", "1")
	resp, err := p.client.Do(outReq)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// TestE2E_EgressThroughProxy proves the edge routes its upstream call through
// the configured egress proxy rather than connecting directly.
func TestE2E_EgressThroughProxy(t *testing.T) {
	up := newMockUpstream()
	upstream := httptest.NewServer(up)
	defer upstream.Close()

	prox := &recordingProxy{client: &http.Client{Timeout: 10 * time.Second}}
	proxySrv := httptest.NewServer(prox)
	defer proxySrv.Close()

	// Center with one account whose upstream is the mock.
	registry := cchub.NewMemRegistry([]cchub.AccountConfig{{
		ID: "acc-1", HomeEdgeID: "edge-test", UpstreamBaseURL: upstream.URL, UpstreamToken: "tok-1",
	}})
	usage := cchub.NewMemUsageSink()
	coord := cchub.NewCoordinator(cchub.Config{
		Admission: cchub.NewMemAdmission(0),
		Scheduler: registry,
		Sticky:    cchub.NewMemSticky(),
		Usage:     usage,
		Minter:    cchub.NewHMACMinter(registry, []byte("s"), fixedClock()),
		Now:       fixedClock(),
	})
	center := httptest.NewServer(cchub.NewServer(coord, registry, nil).Handler())
	defer center.Close()

	// Edge whose upstream client egresses through the recording proxy.
	upstreamClient, err := ccdirect.NewUpstreamClient(proxySrv.URL, 10*time.Second)
	if err != nil {
		t.Fatalf("build upstream client: %v", err)
	}
	edge := httptest.NewServer(ccdirect.NewRelay(ccdirect.Config{
		EdgeID:    "edge-test",
		CenterURL: center.URL,
		Upstream:  upstreamClient,
		Now:       time.Now,
	}).Handler())
	defer edge.Close()

	status, body, _ := postPrompt(t, edge.URL, "key-1", "claude-x", false)
	if status != http.StatusOK {
		t.Fatalf("want 200 via proxy, got %d body=%s", status, body)
	}
	if prox.count() != 1 {
		t.Fatalf("upstream must transit the egress proxy exactly once, got %d", prox.count())
	}
	hits := up.hits()
	if len(hits) != 1 {
		t.Fatalf("upstream should be hit once, got %d", len(hits))
	}
	recs := waitSettle(t, usage, 1)
	if recs[0].OutputTokens != 22 {
		t.Fatalf("usage not settled through proxied path: %+v", recs[0])
	}
}
