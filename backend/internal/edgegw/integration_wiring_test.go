//go:build unit

package edgegw

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/ccdirect"
	"github.com/Wei-Shaw/sub2api/internal/edgegw/edgereg"
	"github.com/Wei-Shaw/sub2api/internal/edgegw/quota"
)

// TestEgressViaEdge proves the center can run an outbound call (e.g. OAuth
// refresh) THROUGH an edge, so it egresses from the edge's stable IP.
func TestEgressViaEdge(t *testing.T) {
	var sawAuth string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(append([]byte("refreshed:"), body...))
	}))
	defer target.Close()

	edge := httptest.NewServer(ccdirect.NewRelay(ccdirect.Config{EdgeID: "edge-1", CenterURL: "http://unused", InternalKey: "secret"}).Handler())
	defer edge.Close()

	// Wrong/missing internal key is rejected (SSRF gate).
	if _, err := ccdirect.EgressVia(context.Background(), http.DefaultClient, edge.URL, "wrong", ccdirect.EgressRequest{Method: http.MethodPost, URL: target.URL}); err == nil {
		t.Fatalf("egress with wrong internal key must be rejected")
	}

	resp, err := ccdirect.EgressVia(context.Background(), http.DefaultClient, edge.URL, "secret", ccdirect.EgressRequest{
		Method: http.MethodPost,
		URL:    target.URL + "/oauth/token",
		Header: map[string]string{"Authorization": "Bearer refresh-tok"},
		Body:   []byte("grant_type=refresh_token"),
	})
	if err != nil {
		t.Fatalf("egress: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("egress status %d", resp.StatusCode)
	}
	if string(resp.Body) != "refreshed:grant_type=refresh_token" {
		t.Fatalf("egress body roundtrip wrong: %q", resp.Body)
	}
	if sawAuth != "Bearer refresh-tok" {
		t.Fatalf("target did not see forwarded auth: %q", sawAuth)
	}
}

// TestCenterRegisterHeartbeatEnroll covers the edge-fleet endpoints and the
// enroll-key gate.
func TestCenterRegisterHeartbeatEnroll(t *testing.T) {
	registry := NewMemRegistry(nil)
	coord := NewCoordinator(Config{Admission: NewMemAdmission(0), Scheduler: registry, Usage: NewMemUsageSink()})
	edges := edgereg.New(time.Minute, time.Now)
	cs := NewCenterServer(coord, registry, edges)
	cs.SetEnrollKeys([]string{"good-key"})
	srv := httptest.NewServer(cs.Handler())
	defer srv.Close()

	post := func(path string, v any) int {
		buf, _ := json.Marshal(v)
		resp, err := http.Post(srv.URL+path, "application/json", bytes.NewReader(buf))
		if err != nil {
			t.Fatalf("post %s: %v", path, err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	// Wrong enroll key is rejected.
	if code := post("/v1/register", RegisterRequest{EdgeID: "e1", EnrollKey: "bad", EgressIP: "1.2.3.4"}); code != http.StatusUnauthorized {
		t.Fatalf("bad enroll key: want 401, got %d", code)
	}
	// Correct enroll key registers.
	if code := post("/v1/register", RegisterRequest{EdgeID: "e1", EnrollKey: "good-key", EgressIP: "1.2.3.4", Platforms: []string{"openai"}}); code != http.StatusOK {
		t.Fatalf("good enroll key: want 200, got %d", code)
	}
	if !edges.IsLive("e1") {
		t.Fatalf("e1 should be live after register")
	}
	// Heartbeat known/unknown.
	if code := post("/v1/heartbeat", HeartbeatRequest{EdgeID: "e1"}); code != http.StatusOK {
		t.Fatalf("heartbeat known: want 200, got %d", code)
	}
	if code := post("/v1/heartbeat", HeartbeatRequest{EdgeID: "ghost"}); code != http.StatusNotFound {
		t.Fatalf("heartbeat unknown: want 404, got %d", code)
	}
	// /v1/edges lists the live edge.
	resp, err := http.Get(srv.URL + "/v1/edges")
	if err != nil {
		t.Fatalf("get edges: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var live []edgereg.EdgeInfo
	_ = json.NewDecoder(resp.Body).Decode(&live)
	if len(live) != 1 || live[0].ID != "e1" {
		t.Fatalf("expected [e1] live, got %+v", live)
	}
}

// TestCoordinatorQuotaReserveReconcile checks the pre-debit/reconcile path
// through the coordinator with the real quota ledger.
func TestCoordinatorQuotaReserveReconcile(t *testing.T) {
	ledger := quota.New()
	ledger.SetBalance("k", 100)
	registry := NewMemRegistry([]AccountConfig{{ID: "a", Platform: "openai", UpstreamBaseURL: "http://x", UpstreamToken: "t"}})
	co := NewCoordinator(Config{
		Admission:     NewMemAdmission(0),
		Scheduler:     registry,
		Usage:         NewMemUsageSink(),
		Quota:         ledger,
		LeaseEstimate: 5,
		CostFunc:      func(SettleRequest) float64 { return 2 },
		Now:           fixedClock(),
	})

	lease, err := co.Lease(context.Background(), LeaseRequest{APIKey: "k", Model: "m", RequestID: "r1", EdgeID: "e"})
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	if ledger.Balance("k") != 95 {
		t.Fatalf("after reserve want balance 95, got %v", ledger.Balance("k"))
	}
	if _, err := co.Settle(context.Background(), SettleRequest{RequestID: "r1", APIKey: "k", AccountID: "a", SlotID: lease.SlotID, StatusCode: 200}); err != nil {
		t.Fatalf("settle: %v", err)
	}
	// reserved 5, actual 2 -> refund 3 -> balance 98.
	if ledger.Balance("k") != 98 {
		t.Fatalf("after reconcile want balance 98, got %v", ledger.Balance("k"))
	}
	if ledger.Reserved("k") != 0 {
		t.Fatalf("reservation must be released, reserved=%v", ledger.Reserved("k"))
	}
}

// TestCoordinatorQuotaRefundOnFailedLease ensures a lease that fails after the
// pre-debit refunds the reservation.
func TestCoordinatorQuotaRefundOnFailedLease(t *testing.T) {
	ledger := quota.New()
	ledger.SetBalance("k", 100)
	co := NewCoordinator(Config{
		Admission:     NewMemAdmission(0),
		Scheduler:     NewMemRegistry(nil), // no accounts -> ErrNoAccount
		Usage:         NewMemUsageSink(),
		Quota:         ledger,
		LeaseEstimate: 7,
	})
	if _, err := co.Lease(context.Background(), LeaseRequest{APIKey: "k", Model: "m", RequestID: "r1"}); err == nil {
		t.Fatalf("expected lease to fail with no account")
	}
	if ledger.Balance("k") != 100 {
		t.Fatalf("failed lease must refund the pre-debit, balance=%v", ledger.Balance("k"))
	}
}

// TestSchedulerEdgeAffinity verifies accounts homed on the requesting edge are
// ranked first.
func TestSchedulerEdgeAffinity(t *testing.T) {
	registry := NewMemRegistry([]AccountConfig{
		{ID: "a-edgeA", Platform: "openai", HomeEdgeID: "edgeA", UpstreamBaseURL: "http://x", UpstreamToken: "t"},
		{ID: "a-edgeB", Platform: "openai", HomeEdgeID: "edgeB", UpstreamBaseURL: "http://x", UpstreamToken: "t"},
	})
	cands, err := registry.Select(context.Background(), LeaseRequest{APIKey: "k", Model: "m", EdgeID: "edgeB"})
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if len(cands) != 2 || cands[0].AccountID != "a-edgeB" {
		t.Fatalf("edge affinity should rank edgeB-homed first, got %+v", cands)
	}
}
