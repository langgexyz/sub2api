//go:build unit

package handler

import (
	"strings"
	"testing"
	"time"
)

// fakeClock is a manually-advanced clock for deterministic TTL/interval tests.
type fakeClock struct{ t time.Time }

func (f *fakeClock) now() time.Time { return f.t }
func (f *fakeClock) add(d time.Duration) { f.t = f.t.Add(d) }

func newTestStore(clk *fakeClock) *deviceCodeStore {
	return newDeviceCodeStore(10*time.Minute, 5*time.Second, clk.now)
}

func TestDeviceStore_HappyPath(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	s := newTestStore(clk)

	dc, uc := s.create()
	if dc == "" || uc == "" {
		t.Fatalf("empty codes: dc=%q uc=%q", dc, uc)
	}
	if !s.userCodeExists(uc) {
		t.Fatalf("user_code should exist while pending")
	}

	// First poll: pending.
	if r := s.poll(dc); r.status != "pending" {
		t.Fatalf("want pending, got %q", r.status)
	}

	// Approve, then poll (after interval) -> approved with the bound user id.
	if !s.approve(uc, 42) {
		t.Fatalf("approve failed")
	}
	clk.add(6 * time.Second)
	r := s.poll(dc)
	if r.status != "approved" || r.userID != 42 {
		t.Fatalf("want approved/42, got %q/%d", r.status, r.userID)
	}

	// One-shot: a second poll must not re-issue (grant consumed -> expired).
	clk.add(6 * time.Second)
	if r := s.poll(dc); r.status != "expired" {
		t.Fatalf("want expired after consume, got %q", r.status)
	}
}

func TestDeviceStore_SlowDown(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	s := newTestStore(clk)
	dc, _ := s.create()

	if r := s.poll(dc); r.status != "pending" {
		t.Fatalf("first poll want pending, got %q", r.status)
	}
	// Immediate second poll (< interval) -> slow_down.
	if r := s.poll(dc); r.status != "slow_down" {
		t.Fatalf("want slow_down, got %q", r.status)
	}
	// After the interval, polling resumes normally.
	clk.add(6 * time.Second)
	if r := s.poll(dc); r.status != "pending" {
		t.Fatalf("want pending after interval, got %q", r.status)
	}
}

func TestDeviceStore_Expiry(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	s := newTestStore(clk)
	dc, uc := s.create()

	clk.add(11 * time.Minute) // past the 10m TTL
	if r := s.poll(dc); r.status != "expired" {
		t.Fatalf("want expired, got %q", r.status)
	}
	if s.userCodeExists(uc) {
		t.Fatalf("expired user_code must not exist")
	}
	if s.approve(uc, 1) {
		t.Fatalf("approve of expired code must fail")
	}
}

func TestDeviceStore_UnknownCodes(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	s := newTestStore(clk)
	if r := s.poll("nonexistent"); r.status != "expired" {
		t.Fatalf("unknown device_code want expired, got %q", r.status)
	}
	if s.approve("NOPE-NOPE", 1) {
		t.Fatalf("approve of unknown user_code must fail")
	}
	if s.userCodeExists("NOPE-NOPE") {
		t.Fatalf("unknown user_code must not exist")
	}
}

func TestRandUserCode_Format(t *testing.T) {
	for i := 0; i < 200; i++ {
		c := randUserCode()
		if len(c) != 9 || c[4] != '-' {
			t.Fatalf("bad format: %q", c)
		}
		// No ambiguous characters anywhere.
		for _, bad := range []string{"0", "O", "1", "I", "L"} {
			if strings.Contains(c, bad) {
				t.Fatalf("ambiguous char %q in %q", bad, c)
			}
		}
	}
}
