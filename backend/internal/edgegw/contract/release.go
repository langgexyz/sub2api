package contract

// Signed release manifests, cchub -> ccdirect. ccdirect's `upgrade` command asks
// cchub for the latest release for its os/arch; cchub returns a manifest signed
// with its Ed25519 release key. ccdirect verifies the signature with the release
// public key embedded at build time, then downloads the artifact and checks its
// SHA-256 against the (signed) manifest before swapping the binary. Signing the
// manifest means a compromised mirror or a MITM cannot point ccdirect at a
// malicious binary: the URL + checksum are covered by cchub's signature.

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
)

// ReleaseManifest describes one downloadable ccdirect release for a given
// os/arch. Signature covers Version+OS+Arch+URL+SHA256 (see releasePayload).
type ReleaseManifest struct {
	Version   string `json:"version"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	URL       string `json:"url"`
	SHA256    string `json:"sha256"`
	Signature string `json:"signature"`
}

// Empty reports whether the manifest carries no release (cchub has none
// configured for this os/arch). An empty manifest is never signed/verified.
func (m ReleaseManifest) Empty() bool {
	return m.Version == "" && m.URL == ""
}

// releasePayload is the canonical signed byte sequence for a manifest. Field
// order is fixed and newline-separated so signer and verifier agree exactly.
func releasePayload(version, os, arch, url, sha256 string) []byte {
	return []byte(version + "\n" + os + "\n" + arch + "\n" + url + "\n" + sha256)
}

// SignRelease returns a copy of m with Signature set, signed by cchub's release
// private key. The Signature field on the input is ignored.
func SignRelease(priv ed25519.PrivateKey, m ReleaseManifest) ReleaseManifest {
	sig := ed25519.Sign(priv, releasePayload(m.Version, m.OS, m.Arch, m.URL, m.SHA256))
	m.Signature = base64.StdEncoding.EncodeToString(sig)
	return m
}

var (
	// ErrReleaseBadSig means the manifest signature did not verify against the
	// embedded release public key — refuse the download.
	ErrReleaseBadSig = errors.New("release manifest signature invalid")
	// ErrReleaseMalformed means the signature was not decodable base64.
	ErrReleaseMalformed = errors.New("release manifest signature malformed")
)

// VerifyRelease checks m's signature against cchub's release public key. A nil
// pub disables verification (dev builds with no embedded key); callers that want
// enforcement must pass a non-nil key (and refuse upgrades when it is nil).
func VerifyRelease(pub ed25519.PublicKey, m ReleaseManifest) error {
	if pub == nil {
		return nil
	}
	sig, err := base64.StdEncoding.DecodeString(m.Signature)
	if err != nil {
		return ErrReleaseMalformed
	}
	if !ed25519.Verify(pub, releasePayload(m.Version, m.OS, m.Arch, m.URL, m.SHA256), sig) {
		return ErrReleaseBadSig
	}
	return nil
}

// EncodeReleasePubKey / DecodeReleasePubKey mirror the liveness key codec so the
// release public key can be embedded into ccdirect via ldflags as base64. An
// empty string decodes to nil (verification disabled).
func EncodeReleasePubKey(pub ed25519.PublicKey) string {
	return base64.StdEncoding.EncodeToString(pub)
}

// DecodeReleasePubKey decodes a base64 Ed25519 release public key. Empty => nil.
func DecodeReleasePubKey(s string) (ed25519.PublicKey, error) {
	if s == "" {
		return nil, nil
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	if len(b) != ed25519.PublicKeySize {
		return nil, errors.New("release public key wrong size")
	}
	return ed25519.PublicKey(b), nil
}
