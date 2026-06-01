//go:build unit

package contract

import (
	"crypto/ed25519"
	"testing"
)

func TestRelease_SignVerifyRoundTrip(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	m := SignRelease(priv, ReleaseManifest{
		Version: "0.2.0", OS: "darwin", Arch: "arm64",
		URL: "https://cchub/dl/ccdirect-0.2.0-darwin-arm64", SHA256: "abc123",
	})
	if m.Signature == "" {
		t.Fatal("signature not set")
	}
	if err := VerifyRelease(pub, m); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func TestRelease_TamperedURLFailsVerify(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	m := SignRelease(priv, ReleaseManifest{Version: "0.2.0", OS: "linux", Arch: "amd64",
		URL: "https://cchub/good", SHA256: "deadbeef"})
	m.URL = "https://evil/bad" // tamper after signing
	if err := VerifyRelease(pub, m); err != ErrReleaseBadSig {
		t.Fatalf("want ErrReleaseBadSig, got %v", err)
	}
}

func TestRelease_TamperedSHAFailsVerify(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	m := SignRelease(priv, ReleaseManifest{Version: "0.2.0", OS: "linux", Arch: "amd64",
		URL: "https://cchub/good", SHA256: "deadbeef"})
	m.SHA256 = "0000" // tamper the checksum
	if err := VerifyRelease(pub, m); err != ErrReleaseBadSig {
		t.Fatalf("want ErrReleaseBadSig, got %v", err)
	}
}

func TestRelease_WrongKeyFailsVerify(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	otherPub, _, _ := ed25519.GenerateKey(nil)
	m := SignRelease(priv, ReleaseManifest{Version: "1", OS: "linux", Arch: "amd64", URL: "u", SHA256: "s"})
	if err := VerifyRelease(otherPub, m); err != ErrReleaseBadSig {
		t.Fatalf("want ErrReleaseBadSig, got %v", err)
	}
}

func TestRelease_NilKeyDisablesVerify(t *testing.T) {
	m := ReleaseManifest{Version: "1", URL: "u"}
	if err := VerifyRelease(nil, m); err != nil {
		t.Fatalf("nil key should disable verification, got %v", err)
	}
}

func TestRelease_MalformedSig(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	m := ReleaseManifest{Version: "1", OS: "linux", Arch: "amd64", URL: "u", SHA256: "s", Signature: "!!!not-base64!!!"}
	if err := VerifyRelease(pub, m); err != ErrReleaseMalformed {
		t.Fatalf("want ErrReleaseMalformed, got %v", err)
	}
}

func TestRelease_PubKeyCodec(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	enc := EncodeReleasePubKey(pub)
	got, err := DecodeReleasePubKey(enc)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Equal(pub) {
		t.Fatal("round-trip pubkey mismatch")
	}
	if n, err := DecodeReleasePubKey(""); err != nil || n != nil {
		t.Fatalf("empty should decode to nil,nil; got %v,%v", n, err)
	}
}

func TestRelease_Empty(t *testing.T) {
	if !(ReleaseManifest{}).Empty() {
		t.Fatal("zero manifest should be Empty")
	}
	if (ReleaseManifest{Version: "1"}).Empty() {
		t.Fatal("manifest with version is not Empty")
	}
}
