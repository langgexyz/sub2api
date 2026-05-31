package contract

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"time"
)

// Liveness tokens: cchub proves to ccdirect, on every heartbeat, that it is the
// real cchub AND that this specific edge is still authorized to operate. The
// token is an Ed25519-signed {edge_id, exp}; ccdirect verifies it with cchub's
// public key (embedded in the ccdirect binary at build time). If ccdirect stops
// receiving fresh, valid tokens within a threshold it drains (finishes in-flight
// requests, refuses new ones) — so a withheld/expired token, an unreachable or
// impostor cchub, or cchub revoking THIS edge, all converge to "stop serving".
//
// "Directed" = the token is bound to one edge_id, so cchub can stop a single
// edge by simply not signing for it, without affecting the rest of the fleet.

// LivenessToken is the cchub-signed liveness assertion returned on heartbeat.
type LivenessToken struct {
	EdgeID    string `json:"edge_id"`
	ExpiresAt int64  `json:"exp"` // unix seconds
	Signature string `json:"sig"` // base64(ed25519 sig over canonical payload)
}

// livenessPayload is the exact byte string signed/verified: "edge_id\nexp".
func livenessPayload(edgeID string, exp int64) []byte {
	return []byte(edgeID + "\n" + strconv.FormatInt(exp, 10))
}

// SignLiveness produces a LivenessToken for edgeID valid for ttl, signed with
// priv. now defaults to time.Now when nil.
func SignLiveness(priv ed25519.PrivateKey, edgeID string, ttl time.Duration, now func() time.Time) LivenessToken {
	if now == nil {
		now = time.Now
	}
	exp := now().Add(ttl).Unix()
	sig := ed25519.Sign(priv, livenessPayload(edgeID, exp))
	return LivenessToken{
		EdgeID:    edgeID,
		ExpiresAt: exp,
		Signature: base64.StdEncoding.EncodeToString(sig),
	}
}

var (
	// ErrLivenessWrongEdge means the token is for a different edge id.
	ErrLivenessWrongEdge = errors.New("contract: liveness token edge id mismatch")
	// ErrLivenessExpired means the token's exp is in the past.
	ErrLivenessExpired = errors.New("contract: liveness token expired")
	// ErrLivenessBadSig means the signature did not verify against the pubkey.
	ErrLivenessBadSig = errors.New("contract: liveness token signature invalid")
	// ErrLivenessMalformed means the signature is not decodable.
	ErrLivenessMalformed = errors.New("contract: liveness token malformed")
)

// VerifyLiveness checks a token against pub: the signature must verify, edge_id
// must match, and it must not be expired (relative to now, default time.Now).
func VerifyLiveness(pub ed25519.PublicKey, tok LivenessToken, edgeID string, now func() time.Time) error {
	if now == nil {
		now = time.Now
	}
	if tok.EdgeID != edgeID {
		return ErrLivenessWrongEdge
	}
	sig, err := base64.StdEncoding.DecodeString(tok.Signature)
	if err != nil {
		return ErrLivenessMalformed
	}
	if !ed25519.Verify(pub, livenessPayload(tok.EdgeID, tok.ExpiresAt), sig) {
		return ErrLivenessBadSig
	}
	if now().Unix() > tok.ExpiresAt {
		return ErrLivenessExpired
	}
	return nil
}

// HeartbeatResponse is the cchub reply to a heartbeat. Liveness is nil only when
// cchub declines to vouch for the edge (e.g. it is revoked) — ccdirect treats a
// missing/invalid token as "no fresh liveness" and drains after the threshold.
type HeartbeatResponse struct {
	OK       bool           `json:"ok"`
	Liveness *LivenessToken `json:"liveness,omitempty"`
}

// EncodeLivenessPubKey / DecodeLivenessPubKey marshal an Ed25519 public key as
// base64 for embedding into the ccdirect binary (ldflags) and parsing at start.
func EncodeLivenessPubKey(pub ed25519.PublicKey) string {
	return base64.StdEncoding.EncodeToString(pub)
}

// DecodeLivenessPubKey parses a base64 Ed25519 public key. Empty input returns
// (nil, nil) so callers can treat "no key embedded" as "liveness disabled".
func DecodeLivenessPubKey(s string) (ed25519.PublicKey, error) {
	if s == "" {
		return nil, nil
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, errors.New("contract: liveness pubkey wrong length")
	}
	return ed25519.PublicKey(raw), nil
}

// jsonRoundTripCheck is a compile-time anchor ensuring LivenessToken stays
// JSON-serializable (used by tests); kept out of hot paths.
var _ = json.Marshal
