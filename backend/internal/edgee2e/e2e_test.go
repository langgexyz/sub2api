//go:build unit

package edgee2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/ccdirect"
	"github.com/Wei-Shaw/sub2api/internal/cchub"
)

// mockUpstream emulates an upstream provider (Anthropic-shaped). Behavior is
// keyed on the bearer token so a single server can act as several accounts.
type mockUpstream struct {
	mu        sync.Mutex
	statusFor map[string]int // bearer -> status code (default 200)
	seen      []upstreamHit
}

type upstreamHit struct {
	bearer string
	model  string
	path   string
	stream bool
}

func newMockUpstream() *mockUpstream {
	return &mockUpstream{statusFor: make(map[string]int)}
}

func (m *mockUpstream) hits() []upstreamHit {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]upstreamHit(nil), m.seen...)
}

func (m *mockUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	model, stream := ccdirect.ParseModelStream(body)
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")

	m.mu.Lock()
	m.seen = append(m.seen, upstreamHit{bearer: bearer, model: model, path: r.URL.Path, stream: stream})
	status := m.statusFor[bearer]
	m.mu.Unlock()
	if status == 0 {
		status = http.StatusOK
	}

	if status >= 500 {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":"upstream boom"}`))
		return
	}

	// Report usage out-of-band via headers (works for stream + non-stream).
	w.Header().Set("X-Usage-Input-Tokens", "11")
	w.Header().Set("X-Usage-Output-Tokens", "22")

	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(status)
		fl, _ := w.(http.Flusher)
		for i := 0; i < 3; i++ {
			_, _ = fmt.Fprintf(w, "data: {\"chunk\":%d,\"model\":%q}\n\n", i, model)
			if fl != nil {
				fl.Flush()
			}
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(fmt.Sprintf(`{"model":%q,"content":"echo"}`, model)))
}

// testSystem holds a running center + edge + their backing components.
type testSystem struct {
	center    *httptest.Server
	edge      *httptest.Server
	usage     *cchub.MemUsageSink
	admission *cchub.MemAdmission
	registry  *cchub.MemRegistry
}

func (s *testSystem) close() {
	s.edge.Close()
	s.center.Close()
}

func newTestSystem(accounts []cchub.AccountConfig, maxPerKey int) *testSystem {
	registry := cchub.NewMemRegistry(accounts)
	admission := cchub.NewMemAdmission(maxPerKey)
	usage := cchub.NewMemUsageSink()
	minter := cchub.NewHMACMinter(registry, []byte("test-secret"), fixedClock())
	coord := cchub.NewCoordinator(cchub.Config{
		Admission: admission,
		Scheduler: registry,
		Sticky:    cchub.NewMemSticky(),
		Usage:     usage,
		Minter:    minter,
		Now:       fixedClock(),
	})
	center := httptest.NewServer(cchub.NewServer(coord, registry, nil).Handler())
	edge := httptest.NewServer(ccdirect.NewRelay(ccdirect.Config{
		CCDirectID: "edge-test",
		CCHubURL:   center.URL,
		Now:        time.Now,
	}).Handler())
	return &testSystem{center: center, edge: edge, usage: usage, admission: admission, registry: registry}
}

// postPrompt sends a client prompt to the edge and returns status + body.
func postPrompt(t *testing.T, edgeURL, apiKey, model string, stream bool) (int, string, http.Header) {
	t.Helper()
	reqBody, _ := json.Marshal(map[string]any{
		"model":    model,
		"stream":   stream,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	req, _ := http.NewRequest(http.MethodPost, edgeURL+"/v1/messages", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post prompt: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out), resp.Header
}

// waitSettle polls until at least n usage records are present (Settle is async
// relative to the client response on a without-cancel context).
func waitSettle(t *testing.T, sink *cchub.MemUsageSink, n int) []cchub.UsageRecord {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if recs := sink.Records(); len(recs) >= n {
			return recs
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d settle record(s); got %d", n, len(sink.Records()))
	return nil
}

func TestE2E_FullFlow_NonStream(t *testing.T) {
	up := newMockUpstream()
	upstream := httptest.NewServer(up)
	defer upstream.Close()

	sys := newTestSystem([]cchub.AccountConfig{{
		ID:              "acc-1",
		HomeCCDirectID:  "edge-test",
		UpstreamBaseURL: upstream.URL,
		UpstreamToken:   "real-upstream-token-1",
		ModelMapping:    map[string]string{"claude-x": "upstream-y"},
	}}, 0)
	defer sys.close()

	status, body, _ := postPrompt(t, sys.edge.URL, "key-1", "claude-x", false)
	if status != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", status, body)
	}
	// The edge must have rewritten the requested model to the mapped upstream model.
	if !strings.Contains(body, "upstream-y") {
		t.Fatalf("response should echo mapped model, got %s", body)
	}

	hits := up.hits()
	if len(hits) != 1 {
		t.Fatalf("upstream should be hit once, got %d", len(hits))
	}
	// The edge presented the account's real upstream token (unwrapped from the lease).
	if hits[0].bearer != "real-upstream-token-1" {
		t.Fatalf("upstream saw wrong bearer: %q", hits[0].bearer)
	}
	if hits[0].model != "upstream-y" {
		t.Fatalf("upstream saw unmapped model: %q", hits[0].model)
	}

	recs := waitSettle(t, sys.usage, 1)
	if recs[0].InputTokens != 11 || recs[0].OutputTokens != 22 {
		t.Fatalf("usage not settled from upstream headers: %+v", recs[0])
	}
	if recs[0].AccountID != "acc-1" {
		t.Fatalf("settle recorded wrong account: %s", recs[0].AccountID)
	}
	// Slot must be released after settle.
	if inflight := sys.admission.InFlight("key-1"); inflight != 0 {
		t.Fatalf("admission slot leaked: in-flight=%d", inflight)
	}
}

func TestE2E_Streaming(t *testing.T) {
	up := newMockUpstream()
	upstream := httptest.NewServer(up)
	defer upstream.Close()

	sys := newTestSystem([]cchub.AccountConfig{{
		ID: "acc-1", HomeCCDirectID: "edge-test", UpstreamBaseURL: upstream.URL, UpstreamToken: "tok-1",
	}}, 0)
	defer sys.close()

	status, body, hdr := postPrompt(t, sys.edge.URL, "key-1", "claude-x", true)
	if status != http.StatusOK {
		t.Fatalf("want 200, got %d", status)
	}
	if ct := hdr.Get("Content-Type"); !strings.Contains(ct, "event-stream") {
		t.Fatalf("expected SSE content-type, got %q", ct)
	}
	if strings.Count(body, "data:") < 3 {
		t.Fatalf("expected streamed SSE chunks, got %s", body)
	}
	recs := waitSettle(t, sys.usage, 1)
	if recs[0].OutputTokens != 22 {
		t.Fatalf("streaming usage not settled: %+v", recs[0])
	}
}

func TestE2E_LocalFailover(t *testing.T) {
	up := newMockUpstream()
	up.statusFor["bad-token"] = 503 // first account's upstream is down
	upstream := httptest.NewServer(up)
	defer upstream.Close()

	// Two accounts, both for key-1/claude-x. Equal load => registry order =>
	// acc-bad is primary, acc-good is failover.
	sys := newTestSystem([]cchub.AccountConfig{
		{ID: "acc-bad", HomeCCDirectID: "edge-test", UpstreamBaseURL: upstream.URL, UpstreamToken: "bad-token"},
		{ID: "acc-good", HomeCCDirectID: "edge-test", UpstreamBaseURL: upstream.URL, UpstreamToken: "good-token"},
	}, 0)
	defer sys.close()

	status, body, _ := postPrompt(t, sys.edge.URL, "key-1", "claude-x", false)
	if status != http.StatusOK {
		t.Fatalf("failover should yield 200, got %d body=%s", status, body)
	}
	hits := up.hits()
	if len(hits) != 2 {
		t.Fatalf("expected primary(500)+failover(200) = 2 upstream hits, got %d", len(hits))
	}
	if hits[0].bearer != "bad-token" || hits[1].bearer != "good-token" {
		t.Fatalf("unexpected failover order: %q then %q", hits[0].bearer, hits[1].bearer)
	}
	recs := waitSettle(t, sys.usage, 1)
	if recs[0].AccountID != "acc-good" {
		t.Fatalf("settle should record the account that served: %s", recs[0].AccountID)
	}
}

func TestE2E_NoAccount_Propagated(t *testing.T) {
	sys := newTestSystem([]cchub.AccountConfig{{
		ID: "acc-1", UpstreamBaseURL: "http://unused", UpstreamToken: "t",
		GroupAPIKeys: []string{"other-key"}, // key-1 is not allowed
	}}, 0)
	defer sys.close()

	status, _, _ := postPrompt(t, sys.edge.URL, "key-1", "claude-x", false)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("want 503 no-account propagated, got %d", status)
	}
	if len(sys.usage.Records()) != 0 {
		t.Fatalf("no settle should occur when lease fails")
	}
	if inflight := sys.admission.InFlight("key-1"); inflight != 0 {
		t.Fatalf("rejected lease must not leak a slot: in-flight=%d", inflight)
	}
}
