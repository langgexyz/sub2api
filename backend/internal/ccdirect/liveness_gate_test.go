//go:build unit

package ccdirect

import (
	"crypto/ed25519"
	"testing"
	"time"
)

// livenessClock returns a controllable Clock for the gate tests.
func livenessClock(t *time.Time) Clock { return func() time.Time { return *t } }

func TestLivenessGate_DisabledWhenNoPubKey(t *testing.T) {
	r := &Relay{now: time.Now} // cchubPubKey nil
	if !r.livenessHealthy() {
		t.Fatal("with no pubkey, liveness enforcement must be disabled (healthy)")
	}
}

func TestLivenessGate_HealthyWhileTokenValid(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	now := time.Unix(1_700_000_000, 0)
	r := &Relay{now: livenessClock(&now), cchubPubKey: pub}

	// No token yet -> not healthy (must be vouched before serving).
	if r.livenessHealthy() {
		t.Fatal("before any liveness token, gate must be unhealthy")
	}
	// Record a token expiring 1 minute out -> healthy.
	r.recordLiveness(now.Add(time.Minute).Unix())
	if !r.livenessHealthy() {
		t.Fatal("with a fresh token, gate must be healthy")
	}
	// Advance past expiry -> drains.
	now = now.Add(2 * time.Minute)
	if r.livenessHealthy() {
		t.Fatal("after token expiry, gate must drain (unhealthy)")
	}
	// A new token revives it.
	r.recordLiveness(now.Add(time.Minute).Unix())
	if !r.livenessHealthy() {
		t.Fatal("a renewed token must make the gate healthy again")
	}
}
