// Package contract holds the wire types and crypto shared between the two halves
// of the distributed gateway: ccdirect (data plane / edge) and cchub (control
// plane / center). Both import this package; neither depends on the other. It
// has no dependency on the heavy service/Ent layer.
package contract

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"time"
)

// Sealed lease tokens: instead of returning the raw upstream credential in the
// lease response, cchub AEAD-encrypts it bound to {ccdirectID, expiry}. ccdirect
// decrypts it with the same shared key just before use, then discards it.
//
// What this buys (defense-in-depth on top of mTLS):
//   - a captured lease response is useless to a DIFFERENT edge (AAD = ccdirectID),
//   - and useless after it expires (short TTL), bounding replay,
//   - and opaque to any mTLS-terminating intermediary (it stays encrypted at the
//     application layer).
//
// It does NOT stop the legitimate holding edge from using the decrypted token
// out-of-band — that is inherent to edge-egress with static upstream keys and is
// contained by edge trust (mTLS), usage reconciliation, and key rotation.

func deriveSealKey(secret []byte) []byte {
	sum := sha256.Sum256(secret)
	return sum[:]
}

// SealLeaseToken AEAD-encrypts token bound to ccdirectID with a TTL-derived expiry.
func SealLeaseToken(token, ccdirectID string, ttl time.Duration, secret []byte, now func() time.Time) (string, error) {
	if now == nil {
		now = time.Now
	}
	gcm, err := newSealGCM(secret)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	exp := now().Add(ttl).Unix()
	plain := make([]byte, 8+len(token))
	binary.BigEndian.PutUint64(plain[:8], uint64(exp))
	copy(plain[8:], token)
	sealed := gcm.Seal(nonce, nonce, plain, []byte(ccdirectID))
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

// OpenLeaseToken decrypts a sealed token, verifying the ccdirectID binding and that
// it has not expired. Any tampering / wrong key / wrong edge / expiry fails.
func OpenLeaseToken(sealed, ccdirectID string, secret []byte, now func() time.Time) (string, error) {
	if now == nil {
		now = time.Now
	}
	raw, err := base64.RawURLEncoding.DecodeString(sealed)
	if err != nil {
		return "", errors.New("contract: sealed token not base64")
	}
	gcm, err := newSealGCM(secret)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("contract: sealed token too short")
	}
	nonce, ct := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, ct, []byte(ccdirectID))
	if err != nil {
		return "", errors.New("contract: sealed token invalid (wrong key/edge or tampered)")
	}
	if len(plain) < 8 {
		return "", errors.New("contract: sealed token malformed")
	}
	exp := int64(binary.BigEndian.Uint64(plain[:8]))
	if now().Unix() > exp {
		return "", errors.New("contract: sealed token expired")
	}
	return string(plain[8:]), nil
}

func newSealGCM(secret []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(deriveSealKey(secret))
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
