//go:build unit

package cchub

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/ccgw/contract"
	"github.com/Wei-Shaw/sub2api/internal/ccgw/edgereg"
)

func newEnrollTestServer(t *testing.T) (*httptest.Server, *edgereg.Registry) {
	t.Helper()
	registry := NewMemRegistry(nil)
	coord := NewCoordinator(Config{Admission: NewMemAdmission(0), Scheduler: registry, Usage: NewMemUsageSink()})
	edges := edgereg.New(time.Minute, time.Now)
	cs := NewServer(coord, registry, edges)
	cs.SetEnrollKeys([]string{"k"})
	cs.SetEnrollConfig("http://center.example", 7, 2, []string{"anthropic"})
	return httptest.NewServer(cs.Handler()), edges
}

func TestCenterEnroll_IssuesConfig(t *testing.T) {
	srv, _ := newEnrollTestServer(t)
	defer srv.Close()

	body, _ := json.Marshal(contract.EnrollRequest{Key: "k"})
	resp, err := http.Post(srv.URL+"/v1/enroll", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var er contract.EnrollResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if er.EdgeID == "" {
		t.Fatalf("center must assign an edge id")
	}
	if er.CenterURL != "http://center.example" || er.HeartbeatSeconds != 7 || er.MaxFailover != 2 {
		t.Fatalf("issued config wrong: %+v", er)
	}
	if len(er.Platforms) != 1 || er.Platforms[0] != "anthropic" {
		t.Fatalf("issued platforms wrong: %+v", er.Platforms)
	}
}

func TestCenterEnroll_RejectsBadKey(t *testing.T) {
	srv, _ := newEnrollTestServer(t)
	defer srv.Close()

	body, _ := json.Marshal(contract.EnrollRequest{Key: "wrong"})
	resp, err := http.Post(srv.URL+"/v1/enroll", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad key must be 401, got %d", resp.StatusCode)
	}
}

func TestCenterRegister_AutoDetectsEgressIP(t *testing.T) {
	srv, edges := newEnrollTestServer(t)
	defer srv.Close()

	// EgressIP intentionally empty: the center should fill it from the connection.
	body, _ := json.Marshal(contract.RegisterRequest{EdgeID: "e1", EnrollKey: "k", Platforms: []string{"anthropic"}})
	resp, err := http.Post(srv.URL+"/v1/register", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	_ = resp.Body.Close()

	info, ok := edges.Get("e1")
	if !ok {
		t.Fatalf("edge not registered")
	}
	if info.EgressIP == "" {
		t.Fatalf("center must auto-detect egress IP when the edge omits it")
	}
}

func TestClientIPFromRemoteAddr(t *testing.T) {
	if got := clientIPFromRemoteAddr("203.0.113.7:54321"); got != "203.0.113.7" {
		t.Fatalf("host extraction wrong: %q", got)
	}
	if got := clientIPFromRemoteAddr("no-port"); got != "no-port" {
		t.Fatalf("fallback wrong: %q", got)
	}
}
