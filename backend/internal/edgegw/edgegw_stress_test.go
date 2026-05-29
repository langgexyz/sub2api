//go:build unit

package edgegw

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// flakyUpstream simulates network turbulence per account (keyed on bearer):
// some accounts return 5xx, some drop the connection mid-handshake, some add
// latency. "good" tokens always return 200 with usage.
type flakyUpstream struct {
	mu        sync.Mutex
	statusFor map[string]int           // bearer -> status (default 200)
	dropFor   map[string]bool          // bearer -> hijack + close conn (transport error)
	delayFor  map[string]time.Duration // bearer -> pre-response delay
	hits      int64
}

func (f *flakyUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	atomic.AddInt64(&f.hits, 1)
	bearer := authBearer(r)

	f.mu.Lock()
	status := f.statusFor[bearer]
	drop := f.dropFor[bearer]
	delay := f.delayFor[bearer]
	f.mu.Unlock()

	if delay > 0 {
		time.Sleep(delay)
	}
	if drop {
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, err := hj.Hijack()
			if err == nil {
				_ = conn.Close() // client sees a transport error -> edge fails over
				return
			}
		}
		// Fallback if hijack unsupported: behave like a 5xx.
		status = http.StatusBadGateway
	}
	if status == 0 {
		status = http.StatusOK
	}
	if status >= 500 {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("X-Usage-Input-Tokens", "11")
	w.Header().Set("X-Usage-Output-Tokens", "22")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `{"content":"ok"}`)
}

func authBearer(r *http.Request) string {
	const p = "Bearer "
	if v := r.Header.Get("Authorization"); len(v) > len(p) {
		return v[len(p):]
	}
	return ""
}

// fire sends one prompt to the edge and returns the status code (0 on client
// error). Safe to call from many goroutines (no t.Fatalf).
func fire(edgeURL, apiKey string) int {
	reqBody, _ := json.Marshal(map[string]any{
		"model": "claude-x", "stream": false,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	req, _ := http.NewRequest(http.MethodPost, edgeURL+"/v1/messages", bytes.NewReader(reqBody))
	req.Header.Set("x-api-key", apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode
}

// waitNoInflight polls until the api key has zero in-flight slots or times out.
func waitNoInflight(t *testing.T, adm *MemAdmission, apiKey string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if adm.InFlight(apiKey) == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("admission slots leaked: %d still in-flight for %q", adm.InFlight(apiKey), apiKey)
}

// TestConcurrency_NoSlotLeak hammers the edge with many concurrent requests and
// asserts every one succeeds, every one settles, and no admission slot leaks.
func TestConcurrency_NoSlotLeak(t *testing.T) {
	up := httptest.NewServer(&flakyUpstream{})
	defer up.Close()
	sys := newTestSystem([]AccountConfig{{
		ID: "acc-1", Platform: "openai", UpstreamBaseURL: up.URL, UpstreamToken: "good", MaxConcurrency: 0,
	}}, 0)
	defer sys.close()

	const n = 100
	var ok int64
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if fire(sys.edge.URL, "key-1") == http.StatusOK {
				atomic.AddInt64(&ok, 1)
			}
		}()
	}
	wg.Wait()

	if ok != n {
		t.Fatalf("want %d successful, got %d", n, ok)
	}
	waitNoInflight(t, sys.admission, "key-1")
	if recs := waitSettle(t, sys.usage, n); len(recs) != n {
		t.Fatalf("want %d settle records, got %d", n, len(recs))
	}
}

// TestStability_SustainedSequential runs a long sequential stream of requests to
// catch slow leaks / state drift.
func TestStability_SustainedSequential(t *testing.T) {
	up := httptest.NewServer(&flakyUpstream{})
	defer up.Close()
	sys := newTestSystem([]AccountConfig{{
		ID: "acc-1", Platform: "anthropic", UpstreamBaseURL: up.URL, UpstreamToken: "good",
	}}, 0)
	defer sys.close()

	const n = 300
	for i := 0; i < n; i++ {
		if got := fire(sys.edge.URL, "key-1"); got != http.StatusOK {
			t.Fatalf("request %d: status %d", i, got)
		}
		if inf := sys.admission.InFlight("key-1"); inf != 0 {
			t.Fatalf("request %d left %d slots in-flight (sequential must settle before returning)", i, inf)
		}
	}
	if recs := waitSettle(t, sys.usage, n); len(recs) != n {
		t.Fatalf("want %d settle records, got %d", n, len(recs))
	}
}

// TestNetworkFluctuation_FailoverReachesGood mixes a healthy account with flaky
// ones (5xx + connection drops + latency). With failover covering all
// candidates, every request must still succeed and no slot may leak.
func TestNetworkFluctuation_FailoverReachesGood(t *testing.T) {
	fu := &flakyUpstream{
		statusFor: map[string]int{"bad1": 503},
		dropFor:   map[string]bool{"bad2": true},
		delayFor:  map[string]time.Duration{"good": 5 * time.Millisecond},
	}
	up := httptest.NewServer(fu)
	defer up.Close()

	sys := newTestSystem([]AccountConfig{
		{ID: "acc-bad1", Platform: "openai", UpstreamBaseURL: up.URL, UpstreamToken: "bad1"},
		{ID: "acc-bad2", Platform: "openai", UpstreamBaseURL: up.URL, UpstreamToken: "bad2"},
		{ID: "acc-good", Platform: "openai", UpstreamBaseURL: up.URL, UpstreamToken: "good"},
	}, 0)
	defer sys.close()

	const n = 60
	var ok int64
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if fire(sys.edge.URL, "key-1") == http.StatusOK {
				atomic.AddInt64(&ok, 1)
			}
		}()
	}
	wg.Wait()

	if ok != n {
		t.Fatalf("failover should make all %d succeed, got %d", n, ok)
	}
	waitNoInflight(t, sys.admission, "key-1")
	// Every served request settled against the healthy account.
	recs := waitSettle(t, sys.usage, n)
	for _, r := range recs {
		if r.AccountID != "acc-good" {
			t.Fatalf("served account should be acc-good, got %s", r.AccountID)
		}
	}
}

// TestNetworkFluctuation_AllDown_CleanFailNoLeak asserts that when every upstream
// is failing, requests fail cleanly (no 200, no panic) and slots still release.
func TestNetworkFluctuation_AllDown_CleanFailNoLeak(t *testing.T) {
	fu := &flakyUpstream{statusFor: map[string]int{"d1": 503, "d2": 502, "d3": 500}}
	up := httptest.NewServer(fu)
	defer up.Close()

	sys := newTestSystem([]AccountConfig{
		{ID: "a1", Platform: "openai", UpstreamBaseURL: up.URL, UpstreamToken: "d1"},
		{ID: "a2", Platform: "openai", UpstreamBaseURL: up.URL, UpstreamToken: "d2"},
		{ID: "a3", Platform: "openai", UpstreamBaseURL: up.URL, UpstreamToken: "d3"},
	}, 0)
	defer sys.close()

	const n = 40
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got := fire(sys.edge.URL, "key-1")
			if got == http.StatusOK {
				// can't happen: every upstream is 5xx
				t.Errorf("unexpected success with all upstreams down")
			}
		}()
	}
	wg.Wait()
	// Even though every request failed upstream, the edge must have settled each
	// lease and released its slot.
	waitNoInflight(t, sys.admission, "key-1")
}
