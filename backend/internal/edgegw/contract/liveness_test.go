//go:build unit

package contract

import (
	"crypto/ed25519"
	"testing"
	"time"
)

func mustKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	return pub, priv
}

func TestLiveness_RoundTrip(t *testing.T) {
	pub, priv := mustKey(t)
	tok := SignLiveness(priv, "edge-u1", time.Minute, time.Now)
	if err := VerifyLiveness(pub, tok, "edge-u1", time.Now); err != nil {
		t.Fatalf("verify ok token: %v", err)
	}
}

func TestLiveness_WrongEdgeRejected(t *testing.T) {
	pub, priv := mustKey(t)
	tok := SignLiveness(priv, "edge-u1", time.Minute, time.Now)
	if err := VerifyLiveness(pub, tok, "edge-u2", time.Now); err != ErrLivenessWrongEdge {
		t.Fatalf("want wrong-edge, got %v", err)
	}
}

func TestLiveness_WrongKeyRejected(t *testing.T) {
	_, priv := mustKey(t)
	otherPub, _ := mustKey(t)
	tok := SignLiveness(priv, "edge-u1", time.Minute, time.Now)
	if err := VerifyLiveness(otherPub, tok, "edge-u1", time.Now); err != ErrLivenessBadSig {
		t.Fatalf("want bad-sig, got %v", err)
	}
}

func TestLiveness_ExpiredRejected(t *testing.T) {
	pub, priv := mustKey(t)
	base := time.Unix(1_700_000_000, 0)
	tok := SignLiveness(priv, "edge-u1", time.Minute, func() time.Time { return base })
	later := func() time.Time { return base.Add(2 * time.Minute) }
	if err := VerifyLiveness(pub, tok, "edge-u1", later); err != ErrLivenessExpired {
		t.Fatalf("want expired, got %v", err)
	}
}

func TestLiveness_TamperedExpRejected(t *testing.T) {
	pub, priv := mustKey(t)
	tok := SignLiveness(priv, "edge-u1", time.Minute, time.Now)
	tok.ExpiresAt += 100000 // extend exp without re-signing
	if err := VerifyLiveness(pub, tok, "edge-u1", time.Now); err != ErrLivenessBadSig {
		t.Fatalf("want bad-sig on tampered exp, got %v", err)
	}
}

func TestLiveness_MalformedSig(t *testing.T) {
	pub, _ := mustKey(t)
	tok := LivenessToken{EdgeID: "edge-u1", ExpiresAt: time.Now().Add(time.Minute).Unix(), Signature: "!!!not-base64!!!"}
	if err := VerifyLiveness(pub, tok, "edge-u1", time.Now); err != ErrLivenessMalformed {
		t.Fatalf("want malformed, got %v", err)
	}
}

func TestLivenessPubKey_EncodeDecode(t *testing.T) {
	pub, _ := mustKey(t)
	s := EncodeLivenessPubKey(pub)
	got, err := DecodeLivenessPubKey(s)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !pub.Equal(got) {
		t.Fatalf("roundtrip pubkey mismatch")
	}
	// empty => nil, no error (liveness disabled)
	if k, err := DecodeLivenessPubKey(""); err != nil || k != nil {
		t.Fatalf("empty pubkey should be (nil,nil), got (%v,%v)", k, err)
	}
	// wrong length => error
	if _, err := DecodeLivenessPubKey("YWJj"); err == nil {
		t.Fatalf("short pubkey should error")
	}
}
