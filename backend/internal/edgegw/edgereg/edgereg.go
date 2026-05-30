// Package edgereg tracks live edge nodes for the distributed-edge gateway.
//
// The center uses a Registry to record edges as they register and send
// heartbeats, and to answer liveness queries. An edge is considered live
// while the time since its last heartbeat stays within the configured TTL.
// All operations are safe for concurrent use.
package edgereg

import (
	"sort"
	"sync"
	"time"
)

// defaultTTL is the liveness window used when New is given a non-positive ttl.
const defaultTTL = 30 * time.Second

// EdgeInfo describes a single edge node tracked by the center.
type EdgeInfo struct {
	ID           string    `json:"id"`
	EgressIP     string    `json:"egress_ip"`
	Platforms    []string  `json:"platforms"`
	RegisteredAt time.Time `json:"registered_at"`
	LastSeen     time.Time `json:"last_seen"`
}

// Clock returns the current time. It is injectable for deterministic tests;
// a nil Clock passed to New means time.Now is used.
type Clock func() time.Time

// Registry is a concurrency-safe store of edge nodes and their liveness.
type Registry struct {
	ttl time.Duration
	now Clock

	mu    sync.RWMutex
	edges map[string]EdgeInfo
}

// New builds a registry. ttl is the liveness window: an edge is live when
// now-LastSeen <= ttl. If ttl <= 0 the 30s default is used. If now is nil
// time.Now is used.
func New(ttl time.Duration, now Clock) *Registry {
	if ttl <= 0 {
		ttl = defaultTTL
	}
	if now == nil {
		now = time.Now
	}
	return &Registry{
		ttl:   ttl,
		now:   now,
		edges: make(map[string]EdgeInfo),
	}
}

// copyPlatforms returns a defensive copy of the given slice. A nil input
// yields a nil result.
func copyPlatforms(platforms []string) []string {
	if platforms == nil {
		return nil
	}
	out := make([]string, len(platforms))
	copy(out, platforms)
	return out
}

// Register upserts an edge. On first sight RegisteredAt is set to now; on
// subsequent calls RegisteredAt is preserved. LastSeen is always bumped to
// now, and EgressIP and Platforms are refreshed. Platforms is copied so the
// caller cannot mutate stored state.
func (r *Registry) Register(id, egressIP string, platforms []string) {
	t := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()

	e, ok := r.edges[id]
	if !ok {
		e.ID = id
		e.RegisteredAt = t
	}
	e.EgressIP = egressIP
	e.Platforms = copyPlatforms(platforms)
	e.LastSeen = t
	r.edges[id] = e
}

// Heartbeat bumps LastSeen for a known edge. It returns false if id is
// unknown.
func (r *Registry) Heartbeat(id string) bool {
	t := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()

	e, ok := r.edges[id]
	if !ok {
		return false
	}
	e.LastSeen = t
	r.edges[id] = e
	return true
}

// isLive reports whether the edge was last seen within the TTL relative to t.
func (r *Registry) isLive(e EdgeInfo, t time.Time) bool {
	return t.Sub(e.LastSeen) <= r.ttl
}

// cloneEdge returns a deep copy of e so callers cannot mutate stored state.
func cloneEdge(e EdgeInfo) EdgeInfo {
	e.Platforms = copyPlatforms(e.Platforms)
	return e
}

// Live returns all currently-live edges (now-LastSeen <= ttl), sorted by ID
// ascending. The returned values are copies.
func (r *Registry) Live() []EdgeInfo {
	t := r.now()
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]EdgeInfo, 0, len(r.edges))
	for _, e := range r.edges {
		if r.isLive(e, t) {
			out = append(out, cloneEdge(e))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].ID < out[j].ID
	})
	return out
}

// IsLive reports whether id is known and live.
func (r *Registry) IsLive(id string) bool {
	t := r.now()
	r.mu.RLock()
	defer r.mu.RUnlock()

	e, ok := r.edges[id]
	if !ok {
		return false
	}
	return r.isLive(e, t)
}

// Get returns an edge by id (copy) and whether it exists.
func (r *Registry) Get(id string) (EdgeInfo, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	e, ok := r.edges[id]
	if !ok {
		return EdgeInfo{}, false
	}
	return cloneEdge(e), true
}

// Prune deletes expired (non-live) edges and returns how many were removed.
func (r *Registry) Prune() int {
	t := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()

	removed := 0
	for id, e := range r.edges {
		if !r.isLive(e, t) {
			delete(r.edges, id)
			removed++
		}
	}
	return removed
}
