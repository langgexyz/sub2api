package handler

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

// In-memory stores backing the loopback+PKCE CLI login (ccdirect).
//
// Two pieces of state, both single-process and in-memory (mirroring the prior
// device-code store / lease-slot map): a short-TTL authorization-code grant
// store, and a refresh-token -> device-pubkey binding map. Multi-replica
// centers would need a shared store; for MVP losing this on restart is
// acceptable (the ccdirect re-logs-in). See docs/tech/ccdirect-auth-contract.md.

const (
	// cliGrantTTL is the authorization_code lifetime (contract: 120s).
	cliGrantTTL = 120 * time.Second
	// cliGrantPruneInterval bounds how often expired grants are swept on write.
	cliGrantPruneInterval = 30 * time.Second
)

// cliGrant is a pending authorization-code grant created by the authenticated
// /cli/authorize path and consumed once by the public /cli/token path.
type cliGrant struct {
	userID        int64
	codeChallenge string // PKCE S256 challenge (base64url(sha256(verifier)))
	devicePubKey  string // base64(raw 32-byte Ed25519 pubkey)
	redirectURI   string
	expiresAt     time.Time
	consumed      bool // one-shot: tokens minted exactly once per code
}

// cliGrantStore tracks pending authorization-code grants. Safe for concurrent
// use.
type cliGrantStore struct {
	ttl      time.Duration
	interval time.Duration
	now      func() time.Time

	mu        sync.Mutex
	byCode    map[string]*cliGrant // authorization_code -> grant
	lastPrune time.Time
}

func newCLIGrantStore(ttl, interval time.Duration, now func() time.Time) *cliGrantStore {
	if ttl <= 0 {
		ttl = cliGrantTTL
	}
	if interval <= 0 {
		interval = cliGrantPruneInterval
	}
	if now == nil {
		now = time.Now
	}
	return &cliGrantStore{
		ttl:      ttl,
		interval: interval,
		now:      now,
		byCode:   make(map[string]*cliGrant),
	}
}

// create mints a new grant for the given parameters and returns its
// authorization_code (32 random bytes hex, single-use). Only the authenticated
// authorize path calls this.
func (s *cliGrantStore) create(userID int64, codeChallenge, devicePubKey, redirectURI string) string {
	t := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(t)

	code := cliRandHex(32)
	s.byCode[code] = &cliGrant{
		userID:        userID,
		codeChallenge: codeChallenge,
		devicePubKey:  devicePubKey,
		redirectURI:   redirectURI,
		expiresAt:     t.Add(s.ttl),
	}
	return code
}

// consume looks up the grant for code, marks it consumed, and returns it. It
// returns (nil, false) if the code is unknown, expired, or already consumed.
// One-shot: a second call for the same code always fails.
func (s *cliGrantStore) consume(code string) (*cliGrant, bool) {
	t := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()

	g, ok := s.byCode[code]
	if !ok {
		return nil, false
	}
	if t.After(g.expiresAt) {
		delete(s.byCode, code)
		return nil, false
	}
	if g.consumed {
		return nil, false
	}
	g.consumed = true
	// Single-use: drop it once consumed so it can never be replayed.
	delete(s.byCode, code)
	return g, true
}

func (s *cliGrantStore) pruneLocked(t time.Time) {
	if !s.lastPrune.IsZero() && t.Sub(s.lastPrune) < s.interval {
		return
	}
	s.lastPrune = t
	for code, g := range s.byCode {
		if t.After(g.expiresAt) {
			delete(s.byCode, code)
		}
	}
}

// deviceBindingStore binds a refresh token (keyed by a hash of the token, never
// the raw token) to a device Ed25519 public key. Survives rotation because the
// token handler re-binds the rotated token; lost on process restart (MVP).
type deviceBindingStore struct {
	mu      sync.RWMutex
	byToken map[string]string // sha256-hex(refresh_token) -> base64(pubkey)
}

func newDeviceBindingStore() *deviceBindingStore {
	return &deviceBindingStore{byToken: make(map[string]string)}
}

// bind records that refreshToken is bound to devicePubKey.
func (s *deviceBindingStore) bind(refreshToken, devicePubKey string) {
	key := hashRefreshToken(refreshToken)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byToken[key] = devicePubKey
}

// lookup returns the device pubkey bound to refreshToken, and whether it is
// bound at all.
func (s *deviceBindingStore) lookup(refreshToken string) (string, bool) {
	key := hashRefreshToken(refreshToken)
	s.mu.RLock()
	defer s.mu.RUnlock()
	pub, ok := s.byToken[key]
	return pub, ok
}

// rebind moves a binding from an old refresh token to a new one (rotation),
// preserving the same device pubkey. No-op if the old token was not bound.
func (s *deviceBindingStore) rebind(oldRefreshToken, newRefreshToken string) {
	oldKey := hashRefreshToken(oldRefreshToken)
	newKey := hashRefreshToken(newRefreshToken)
	s.mu.Lock()
	defer s.mu.Unlock()
	pub, ok := s.byToken[oldKey]
	if !ok {
		return
	}
	if oldKey != newKey {
		delete(s.byToken, oldKey)
	}
	s.byToken[newKey] = pub
}

// hashRefreshToken returns sha256-hex of the refresh token so the raw token is
// never used as a map key (defense in depth against accidental logging/leaks).
func hashRefreshToken(refreshToken string) string {
	sum := sha256.Sum256([]byte(refreshToken))
	return hex.EncodeToString(sum[:])
}

// cliRandHex returns n random bytes hex-encoded (2n chars). Falls back to a
// fixed constant only if the OS RNG fails (extremely unlikely); a duplicate
// code would simply be rejected as already-consumed.
func cliRandHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "00000000000000000000000000000000"
	}
	return hex.EncodeToString(b)
}
