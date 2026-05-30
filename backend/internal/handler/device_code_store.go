package handler

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// Device-authorization (RFC 8628) pending-grant store.
//
// The edge CLI starts a login by asking the center for a device_code + a short
// human user_code. The user opens the web frontend (already authenticated) and
// approves the user_code, which binds the grant to their user id. The edge polls
// with the device_code until approved, then the center mints a JWT+refresh pair.
//
// This is in-memory and single-process (like the lease-slot map in
// EdgeCenterHandler): a grant lives only for its short TTL and the whole flow
// completes in seconds, so there is nothing to persist. Multi-replica centers
// would need a shared store; see docs/tech/distributed-edge.md.

type deviceStatus int

const (
	devicePending deviceStatus = iota
	deviceApproved
	deviceDenied
)

type deviceGrant struct {
	userCode  string
	status    deviceStatus
	userID    int64
	expiresAt time.Time
	lastPoll  time.Time
	consumed  bool // tokens already issued for this grant (one-shot)
}

// deviceCodeStore tracks pending device authorizations. Safe for concurrent use.
type deviceCodeStore struct {
	ttl      time.Duration
	interval time.Duration
	now      func() time.Time

	mu       sync.Mutex
	byDevice map[string]*deviceGrant // device_code -> grant
	byUser   map[string]string       // user_code  -> device_code
}

func newDeviceCodeStore(ttl, interval time.Duration, now func() time.Time) *deviceCodeStore {
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	if interval <= 0 {
		interval = 5 * time.Second
	}
	if now == nil {
		now = time.Now
	}
	return &deviceCodeStore{
		ttl:      ttl,
		interval: interval,
		now:      now,
		byDevice: make(map[string]*deviceGrant),
		byUser:   make(map[string]string),
	}
}

// create mints a new pending grant and returns (deviceCode, userCode).
func (s *deviceCodeStore) create() (deviceCode, userCode string) {
	t := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(t)

	// device_code: opaque 32-byte hex (same pattern as the lease seal secret).
	deviceCode = randHex(32)
	// user_code: short, typed-by-hand, ambiguity-free; retry on the rare clash.
	for {
		userCode = randUserCode()
		if _, exists := s.byUser[userCode]; !exists {
			break
		}
	}
	s.byDevice[deviceCode] = &deviceGrant{
		userCode:  userCode,
		status:    devicePending,
		expiresAt: t.Add(s.ttl),
	}
	s.byUser[userCode] = deviceCode
	return deviceCode, userCode
}

// userCodeExists reports whether a user_code maps to a live pending grant. Used
// by the frontend to validate the code before showing the approve button.
func (s *deviceCodeStore) userCodeExists(userCode string) bool {
	t := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.grantByUserLocked(userCode, t)
	return g != nil && g.status == devicePending
}

// approve binds a pending grant to userID. Returns false if the user_code is
// unknown or expired.
func (s *deviceCodeStore) approve(userCode string, userID int64) bool {
	t := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.grantByUserLocked(userCode, t)
	if g == nil {
		return false
	}
	g.status = deviceApproved
	g.userID = userID
	return true
}

// pollResult is what the edge's poll observes.
type pollResult struct {
	status string // "pending" | "approved" | "denied" | "expired" | "slow_down"
	userID int64
}

// poll reports the grant state for a device_code and enforces the min polling
// interval (RFC 8628 slow_down). On "approved" it marks the grant consumed so
// tokens are only minted once.
func (s *deviceCodeStore) poll(deviceCode string) pollResult {
	t := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()

	g, ok := s.byDevice[deviceCode]
	if !ok {
		return pollResult{status: "expired"}
	}
	if t.After(g.expiresAt) {
		s.deleteLocked(deviceCode, g)
		return pollResult{status: "expired"}
	}
	// Enforce polling cadence: a poll sooner than `interval` since the last one
	// gets slow_down (does not advance state).
	if !g.lastPoll.IsZero() && t.Sub(g.lastPoll) < s.interval {
		return pollResult{status: "slow_down"}
	}
	g.lastPoll = t

	switch g.status {
	case deviceDenied:
		return pollResult{status: "denied"}
	case deviceApproved:
		if g.consumed {
			// Already handed out once; treat as expired to prevent token reissue.
			return pollResult{status: "expired"}
		}
		g.consumed = true
		uid := g.userID
		s.deleteLocked(deviceCode, g)
		return pollResult{status: "approved", userID: uid}
	default:
		return pollResult{status: "pending"}
	}
}

func (s *deviceCodeStore) grantByUserLocked(userCode string, t time.Time) *deviceGrant {
	dc, ok := s.byUser[userCode]
	if !ok {
		return nil
	}
	g, ok := s.byDevice[dc]
	if !ok {
		delete(s.byUser, userCode)
		return nil
	}
	if t.After(g.expiresAt) {
		s.deleteLocked(dc, g)
		return nil
	}
	return g
}

func (s *deviceCodeStore) deleteLocked(deviceCode string, g *deviceGrant) {
	delete(s.byDevice, deviceCode)
	if g != nil {
		delete(s.byUser, g.userCode)
	}
}

func (s *deviceCodeStore) pruneLocked(t time.Time) {
	for dc, g := range s.byDevice {
		if t.After(g.expiresAt) {
			s.deleteLocked(dc, g)
		}
	}
}

// randHex returns n random bytes hex-encoded (2n chars). Falls back to a
// time-independent constant only if the OS RNG fails (extremely unlikely).
func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "00000000000000000000000000000000"
	}
	return hex.EncodeToString(b)
}

// userCodeAlphabet excludes visually ambiguous characters (0/O, 1/I/L) so the
// code is easy to read off a terminal and type into a browser.
const userCodeAlphabet = "BCDFGHJKMNPQRSTVWXZ23456789"

// randUserCode returns a short code formatted XXXX-XXXX from the ambiguity-free
// alphabet, using crypto/rand with rejection sampling for an unbiased pick. It
// draws exactly n letters and inserts the dash positionally, so the dash never
// affects how many letters are produced.
func randUserCode() string {
	const n = 8 // letters, not counting the dash
	letters := make([]byte, 0, n)
	buf := make([]byte, 1)
	max := byte(len(userCodeAlphabet))
	limit := byte(256 - (256 % int(max)))
	for len(letters) < n {
		if _, err := rand.Read(buf); err != nil {
			letters = append(letters, userCodeAlphabet[0])
			continue
		}
		if buf[0] >= limit {
			continue // reject to avoid modulo bias
		}
		letters = append(letters, userCodeAlphabet[buf[0]%max])
	}
	return string(letters[:4]) + "-" + string(letters[4:])
}
