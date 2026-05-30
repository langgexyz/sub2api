//go:build unit

package edgereg

import (
	"sync"
	"testing"
	"time"
)

// fakeClock is a manually-advanced clock for deterministic tests.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock(start time.Time) *fakeClock {
	return &fakeClock{t: start}
}

// now returns the current fake time and is usable as a Clock.
func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// advance moves the fake clock forward by d.
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestRegisterAndGet(t *testing.T) {
	clk := newFakeClock(time.Unix(1000, 0))
	r := New(time.Minute, clk.now)

	r.Register("edge-a", "10.0.0.1", []string{"openai", "anthropic"})

	got, ok := r.Get("edge-a")
	if !ok {
		t.Fatalf("Get(edge-a): want ok, got missing")
	}
	if got.ID != "edge-a" {
		t.Errorf("ID: want edge-a, got %q", got.ID)
	}
	if got.EgressIP != "10.0.0.1" {
		t.Errorf("EgressIP: want 10.0.0.1, got %q", got.EgressIP)
	}
	if got.RegisteredAt != clk.now() {
		t.Errorf("RegisteredAt: want %v, got %v", clk.now(), got.RegisteredAt)
	}
	if got.LastSeen != clk.now() {
		t.Errorf("LastSeen: want %v, got %v", clk.now(), got.LastSeen)
	}
	if len(got.Platforms) != 2 || got.Platforms[0] != "openai" || got.Platforms[1] != "anthropic" {
		t.Errorf("Platforms: want [openai anthropic], got %v", got.Platforms)
	}

	live := r.Live()
	if len(live) != 1 || live[0].ID != "edge-a" {
		t.Fatalf("Live: want [edge-a], got %v", live)
	}

	if _, ok := r.Get("missing"); ok {
		t.Errorf("Get(missing): want missing, got ok")
	}
}

func TestRegisterPlatformsCopied(t *testing.T) {
	clk := newFakeClock(time.Unix(1000, 0))
	r := New(time.Minute, clk.now)

	in := []string{"openai"}
	r.Register("edge-a", "10.0.0.1", in)
	in[0] = "mutated"

	got, ok := r.Get("edge-a")
	if !ok {
		t.Fatalf("Get(edge-a): want ok, got missing")
	}
	if got.Platforms[0] != "openai" {
		t.Errorf("stored Platforms mutated via caller slice: got %q", got.Platforms[0])
	}

	// Mutating a returned copy must not affect stored state either.
	got.Platforms[0] = "mutated-again"
	got2, _ := r.Get("edge-a")
	if got2.Platforms[0] != "openai" {
		t.Errorf("stored Platforms mutated via returned copy: got %q", got2.Platforms[0])
	}
}

func TestRegisterPreservesRegisteredAt(t *testing.T) {
	clk := newFakeClock(time.Unix(1000, 0))
	r := New(time.Minute, clk.now)

	r.Register("edge-a", "10.0.0.1", nil)
	first, _ := r.Get("edge-a")

	clk.advance(10 * time.Second)
	r.Register("edge-a", "10.0.0.2", nil)
	second, _ := r.Get("edge-a")

	if second.RegisteredAt != first.RegisteredAt {
		t.Errorf("RegisteredAt changed on re-register: was %v, now %v",
			first.RegisteredAt, second.RegisteredAt)
	}
	if !second.LastSeen.After(first.LastSeen) {
		t.Errorf("LastSeen not bumped on re-register: was %v, now %v",
			first.LastSeen, second.LastSeen)
	}
	if second.EgressIP != "10.0.0.2" {
		t.Errorf("EgressIP not refreshed: got %q", second.EgressIP)
	}
}

func TestHeartbeat(t *testing.T) {
	clk := newFakeClock(time.Unix(1000, 0))
	r := New(time.Minute, clk.now)

	if r.Heartbeat("unknown") {
		t.Errorf("Heartbeat(unknown): want false, got true")
	}

	r.Register("edge-a", "10.0.0.1", nil)
	before, _ := r.Get("edge-a")

	clk.advance(5 * time.Second)
	if !r.Heartbeat("edge-a") {
		t.Fatalf("Heartbeat(edge-a): want true, got false")
	}

	after, _ := r.Get("edge-a")
	if !after.LastSeen.After(before.LastSeen) {
		t.Errorf("LastSeen not updated by Heartbeat: was %v, now %v",
			before.LastSeen, after.LastSeen)
	}
}

func TestLivenessExpiry(t *testing.T) {
	clk := newFakeClock(time.Unix(1000, 0))
	ttl := 30 * time.Second
	r := New(ttl, clk.now)

	r.Register("edge-a", "10.0.0.1", nil)

	// Exactly at the TTL boundary the edge is still live (<= ttl).
	clk.advance(ttl)
	if !r.IsLive("edge-a") {
		t.Errorf("at ttl boundary: want live")
	}
	if len(r.Live()) != 1 {
		t.Errorf("at ttl boundary: want 1 live edge, got %d", len(r.Live()))
	}

	// One tick past the TTL the edge is no longer live.
	clk.advance(time.Nanosecond)
	if r.IsLive("edge-a") {
		t.Errorf("past ttl: want not live")
	}
	if got := r.Live(); len(got) != 0 {
		t.Errorf("past ttl: want 0 live edges, got %v", got)
	}

	// IsLive on an unknown edge is always false.
	if r.IsLive("unknown") {
		t.Errorf("IsLive(unknown): want false")
	}
}

func TestPrune(t *testing.T) {
	clk := newFakeClock(time.Unix(1000, 0))
	ttl := 30 * time.Second
	r := New(ttl, clk.now)

	r.Register("old-1", "10.0.0.1", nil)
	r.Register("old-2", "10.0.0.2", nil)

	clk.advance(ttl + time.Second)

	// A fresh edge registered after the others have expired.
	r.Register("fresh", "10.0.0.3", nil)

	removed := r.Prune()
	if removed != 2 {
		t.Errorf("Prune removed count: want 2, got %d", removed)
	}

	if _, ok := r.Get("old-1"); ok {
		t.Errorf("old-1 should have been pruned")
	}
	if _, ok := r.Get("old-2"); ok {
		t.Errorf("old-2 should have been pruned")
	}
	if _, ok := r.Get("fresh"); !ok {
		t.Errorf("fresh should remain after Prune")
	}

	// Pruning again removes nothing.
	if again := r.Prune(); again != 0 {
		t.Errorf("second Prune: want 0, got %d", again)
	}
}

func TestLiveSortedByID(t *testing.T) {
	clk := newFakeClock(time.Unix(1000, 0))
	r := New(time.Minute, clk.now)

	for _, id := range []string{"c", "a", "b", "d"} {
		r.Register(id, "10.0.0.1", nil)
	}

	live := r.Live()
	want := []string{"a", "b", "c", "d"}
	if len(live) != len(want) {
		t.Fatalf("Live length: want %d, got %d", len(want), len(live))
	}
	for i, id := range want {
		if live[i].ID != id {
			t.Errorf("Live[%d]: want %q, got %q", i, id, live[i].ID)
		}
	}
}

func TestDefaultsApplied(t *testing.T) {
	// ttl <= 0 falls back to the default; nil clock falls back to time.Now.
	r := New(0, nil)
	if r.ttl != defaultTTL {
		t.Errorf("ttl default: want %v, got %v", defaultTTL, r.ttl)
	}
	r.Register("edge-a", "10.0.0.1", nil)
	if !r.IsLive("edge-a") {
		t.Errorf("freshly registered edge should be live with default clock")
	}
}

func TestConcurrentAccess(t *testing.T) {
	// Uses the real clock; this test exercises the locking under -race and
	// does not make timing-deterministic assertions.
	r := New(time.Minute, nil)

	const workers = 16
	const iters = 500

	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func(w int) {
			defer wg.Done()
			ids := []string{"a", "b", "c", "d"}
			for i := 0; i < iters; i++ {
				id := ids[i%len(ids)]
				switch i % 4 {
				case 0:
					r.Register(id, "10.0.0.1", []string{"openai", "anthropic"})
				case 1:
					_ = r.Heartbeat(id)
				case 2:
					_ = r.Live()
				case 3:
					_ = r.Prune()
				}
				_, _ = r.Get(id)
				_ = r.IsLive(id)
			}
		}(w)
	}
	wg.Wait()
}
