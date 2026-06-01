//go:build unit

package edgee2e

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/ccdirect"
	"github.com/Wei-Shaw/sub2api/internal/cchub"
)

// TestE2E_ForwardsQueryString proves the edge preserves the client's query
// string to upstream (e.g. ?beta=true, required by Anthropic OAuth).
func TestE2E_ForwardsQueryString(t *testing.T) {
	var gotQuery string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("X-Usage-Input-Tokens", "1")
		w.Header().Set("X-Usage-Output-Tokens", "1")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	registry := cchub.NewMemRegistry([]cchub.AccountConfig{{
		ID: "a", Platform: "anthropic", UpstreamBaseURL: upstream.URL, UpstreamToken: "t",
	}})
	coord := cchub.NewCoordinator(cchub.Config{
		Admission: cchub.NewMemAdmission(0), Scheduler: registry, Sticky: cchub.NewMemSticky(),
		Usage: cchub.NewMemUsageSink(), Minter: cchub.NewHMACMinter(registry, []byte("s"), fixedClock()), Now: fixedClock(),
	})
	center := httptest.NewServer(cchub.NewServer(coord, registry, nil).Handler())
	defer center.Close()
	edge := httptest.NewServer(ccdirect.NewRelay(ccdirect.Config{CCDirectID: "e", CCHubURL: center.URL, Now: time.Now}).Handler())
	defer edge.Close()

	req, _ := http.NewRequest(http.MethodPost, edge.URL+"/v1/messages?beta=true",
		bytes.NewReader([]byte(`{"model":"claude-x","messages":[]}`)))
	req.Header.Set("x-api-key", "k")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	if gotQuery != "beta=true" {
		t.Fatalf("upstream must see forwarded query string, got %q", gotQuery)
	}
}
