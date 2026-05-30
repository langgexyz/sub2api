//go:build unit

package handler

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// ---- PKCE verification ----

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func TestVerifyPKCES256_Match(t *testing.T) {
	verifier := "this-is-a-43-char-ish-code-verifier-abcdef12345"
	challenge := pkceChallenge(verifier)
	if !verifyPKCES256(verifier, challenge) {
		t.Fatalf("expected PKCE match for correct verifier")
	}
}

func TestVerifyPKCES256_Mismatch(t *testing.T) {
	verifier := "correct-verifier-value"
	challenge := pkceChallenge(verifier)
	if verifyPKCES256("wrong-verifier-value", challenge) {
		t.Fatalf("expected PKCE mismatch for wrong verifier")
	}
	// Also reject an empty challenge.
	if verifyPKCES256(verifier, "") {
		t.Fatalf("expected mismatch against empty challenge")
	}
}

// ---- loopback redirect_uri validation ----

func TestIsLoopbackRedirectURI(t *testing.T) {
	cases := []struct {
		uri  string
		want bool
	}{
		{"http://127.0.0.1:54321/callback", true},
		{"http://localhost:8080/callback", true},
		{"https://127.0.0.1:443/callback", true},
		{"http://[::1]:9000/callback", true},
		{"http://evil.com/callback", false},
		{"http://127.0.0.1.evil.com/callback", false},
		{"https://example.com:127/callback", false},
		{"ftp://127.0.0.1/callback", false},
		{"not a url at all ::::", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isLoopbackRedirectURI(tc.uri); got != tc.want {
			t.Errorf("isLoopbackRedirectURI(%q) = %v, want %v", tc.uri, got, tc.want)
		}
	}
}

// ---- grant store: one-shot consume, expiry ----

func TestCLIGrantStore_ConsumeOnce(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	st := newCLIGrantStore(120*time.Second, 30*time.Second, func() time.Time { return now })

	code := st.create(42, "challenge-x", "pubkey-x", "http://127.0.0.1:1/callback")
	if code == "" {
		t.Fatalf("expected non-empty code")
	}

	g, ok := st.consume(code)
	if !ok {
		t.Fatalf("expected first consume to succeed")
	}
	if g.userID != 42 || g.codeChallenge != "challenge-x" || g.devicePubKey != "pubkey-x" {
		t.Fatalf("grant fields not preserved: %+v", g)
	}

	// Second consume must fail (single-use).
	if _, ok := st.consume(code); ok {
		t.Fatalf("expected second consume to fail (already consumed)")
	}
}

func TestCLIGrantStore_Expired(t *testing.T) {
	now := time.Unix(2_000_000, 0)
	clock := now
	st := newCLIGrantStore(120*time.Second, 30*time.Second, func() time.Time { return clock })

	code := st.create(7, "c", "p", "http://localhost:1/callback")
	// Advance past TTL.
	clock = now.Add(121 * time.Second)
	if _, ok := st.consume(code); ok {
		t.Fatalf("expected expired grant consume to fail")
	}
}

func TestCLIGrantStore_UnknownCode(t *testing.T) {
	st := newCLIGrantStore(0, 0, nil)
	if _, ok := st.consume("does-not-exist"); ok {
		t.Fatalf("expected unknown code consume to fail")
	}
}

// ---- device binding store: bind / lookup / rebind on rotation ----

func TestDeviceBindingStore_BindLookup(t *testing.T) {
	st := newDeviceBindingStore()
	if _, ok := st.lookup("rt-unbound"); ok {
		t.Fatalf("expected unbound token lookup to report not bound")
	}
	st.bind("rt-1", "pub-1")
	pub, ok := st.lookup("rt-1")
	if !ok || pub != "pub-1" {
		t.Fatalf("expected bound pubkey pub-1, got %q ok=%v", pub, ok)
	}
}

func TestDeviceBindingStore_RebindPreservesPubkey(t *testing.T) {
	st := newDeviceBindingStore()
	st.bind("rt-old", "pub-1")
	st.rebind("rt-old", "rt-new")

	if _, ok := st.lookup("rt-old"); ok {
		t.Fatalf("expected old token binding to be removed after rotation")
	}
	pub, ok := st.lookup("rt-new")
	if !ok || pub != "pub-1" {
		t.Fatalf("expected rotated token to keep pub-1, got %q ok=%v", pub, ok)
	}
}

func TestDeviceBindingStore_RebindUnboundNoop(t *testing.T) {
	st := newDeviceBindingStore()
	st.rebind("rt-old", "rt-new")
	if _, ok := st.lookup("rt-new"); ok {
		t.Fatalf("expected no binding created when old token was unbound")
	}
}

// ---- device signature verification ----

func signCanonical(t *testing.T, priv ed25519.PrivateKey, method, path, ts string, body []byte) string {
	t.Helper()
	canonical := canonicalSignString(method, path, ts, body)
	sig := ed25519.Sign(priv, []byte(canonical))
	return base64.StdEncoding.EncodeToString(sig)
}

func newSignedRefreshContext(method, path, ts, sig string, body []byte) *gin.Context {
	gin.SetMode(gin.TestMode)
	req := httptest.NewRequest(method, path, strings.NewReader(string(body)))
	if ts != "" {
		req.Header.Set(headerCcdirectTimestamp, ts)
	}
	if sig != "" {
		req.Header.Set(headerCcdirectSignature, sig)
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = req
	return c
}

func TestVerifyDeviceRefreshSignature_Unbound(t *testing.T) {
	h := &AuthHandler{deviceBindings: newDeviceBindingStore()}
	c := newSignedRefreshContext("POST", "/api/v1/auth/refresh", "", "", []byte(`{"refresh_token":"rt"}`))

	ok, bound := h.verifyDeviceRefreshSignature(c, "rt", []byte(`{"refresh_token":"rt"}`))
	if bound {
		t.Fatalf("unbound token must report bound=false")
	}
	if !ok {
		t.Fatalf("unbound token must pass (ok=true)")
	}
}

func TestVerifyDeviceRefreshSignature_Valid(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	pubB64 := base64.StdEncoding.EncodeToString(pub)

	h := &AuthHandler{deviceBindings: newDeviceBindingStore()}
	h.deviceBindings.bind("rt-bound", pubB64)

	body := []byte(`{"refresh_token":"rt-bound"}`)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig := signCanonical(t, priv, "POST", "/api/v1/auth/refresh", ts, body)

	c := newSignedRefreshContext("POST", "/api/v1/auth/refresh", ts, sig, body)
	ok, bound := h.verifyDeviceRefreshSignature(c, "rt-bound", body)
	if !bound {
		t.Fatalf("expected bound=true")
	}
	if !ok {
		t.Fatalf("expected valid signature to pass")
	}
}

func TestVerifyDeviceRefreshSignature_BadSignature(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	pubB64 := base64.StdEncoding.EncodeToString(pub)
	// Sign with a DIFFERENT key.
	_, otherPriv, _ := ed25519.GenerateKey(nil)

	h := &AuthHandler{deviceBindings: newDeviceBindingStore()}
	h.deviceBindings.bind("rt-bound", pubB64)

	body := []byte(`{"refresh_token":"rt-bound"}`)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig := signCanonical(t, otherPriv, "POST", "/api/v1/auth/refresh", ts, body)

	c := newSignedRefreshContext("POST", "/api/v1/auth/refresh", ts, sig, body)
	ok, bound := h.verifyDeviceRefreshSignature(c, "rt-bound", body)
	if !bound {
		t.Fatalf("expected bound=true")
	}
	if ok {
		t.Fatalf("expected wrong-key signature to fail")
	}
}

func TestVerifyDeviceRefreshSignature_MissingHeaders(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	h := &AuthHandler{deviceBindings: newDeviceBindingStore()}
	h.deviceBindings.bind("rt-bound", base64.StdEncoding.EncodeToString(pub))

	body := []byte(`{"refresh_token":"rt-bound"}`)
	c := newSignedRefreshContext("POST", "/api/v1/auth/refresh", "", "", body)
	ok, bound := h.verifyDeviceRefreshSignature(c, "rt-bound", body)
	if !bound {
		t.Fatalf("expected bound=true")
	}
	if ok {
		t.Fatalf("expected missing headers on a bound token to fail")
	}
}

func TestVerifyDeviceRefreshSignature_TimestampSkew(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	h := &AuthHandler{deviceBindings: newDeviceBindingStore()}
	h.deviceBindings.bind("rt-bound", base64.StdEncoding.EncodeToString(pub))

	body := []byte(`{"refresh_token":"rt-bound"}`)
	// Timestamp 10 minutes in the past -> beyond the +/-120s window.
	staleTs := strconv.FormatInt(time.Now().Add(-10*time.Minute).Unix(), 10)
	sig := signCanonical(t, priv, "POST", "/api/v1/auth/refresh", staleTs, body)

	c := newSignedRefreshContext("POST", "/api/v1/auth/refresh", staleTs, sig, body)
	ok, bound := h.verifyDeviceRefreshSignature(c, "rt-bound", body)
	if !bound {
		t.Fatalf("expected bound=true")
	}
	if ok {
		t.Fatalf("expected stale timestamp to fail even with a valid signature")
	}
}

func TestVerifyDeviceRefreshSignature_BodyTampered(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	h := &AuthHandler{deviceBindings: newDeviceBindingStore()}
	h.deviceBindings.bind("rt-bound", base64.StdEncoding.EncodeToString(pub))

	signedBody := []byte(`{"refresh_token":"rt-bound"}`)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig := signCanonical(t, priv, "POST", "/api/v1/auth/refresh", ts, signedBody)

	// Verify against a DIFFERENT body than what was signed.
	tamperedBody := []byte(`{"refresh_token":"rt-bound","x":1}`)
	c := newSignedRefreshContext("POST", "/api/v1/auth/refresh", ts, sig, tamperedBody)
	ok, _ := h.verifyDeviceRefreshSignature(c, "rt-bound", tamperedBody)
	if ok {
		t.Fatalf("expected tampered body to invalidate the signature")
	}
}

// ---- canonical string + pubkey decoding helpers ----

func TestCanonicalSignString(t *testing.T) {
	body := []byte(`{"a":1}`)
	sum := sha256.Sum256(body)
	want := strings.Join([]string{
		"POST",
		"/api/v1/auth/refresh",
		"1700000000",
		hex.EncodeToString(sum[:]),
	}, "\n")
	got := canonicalSignString("POST", "/api/v1/auth/refresh", "1700000000", body)
	if got != want {
		t.Fatalf("canonical mismatch:\n got=%q\nwant=%q", got, want)
	}
	if strings.HasSuffix(got, "\n") {
		t.Fatalf("canonical string must not have a trailing newline")
	}
}

func TestDecodeDevicePubKey(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	// Standard base64 (the contract's encoding).
	if got, err := decodeDevicePubKey(base64.StdEncoding.EncodeToString(pub)); err != nil || len(got) != ed25519.PublicKeySize {
		t.Fatalf("std base64 decode failed: err=%v len=%d", err, len(got))
	}
	// Wrong length must be rejected.
	if _, err := decodeDevicePubKey(base64.StdEncoding.EncodeToString([]byte("too-short"))); err == nil {
		t.Fatalf("expected error for wrong-length pubkey")
	}
}
