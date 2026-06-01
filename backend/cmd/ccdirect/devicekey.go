package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Wei-Shaw/sub2api/ccgw/enroll"
)

// deviceKey is the edge's per-machine Ed25519 identity. The private key never
// leaves the machine; the public key is sent to the center at login and the
// center binds the refresh token to it, so refreshing requires a signature from
// this private key (token-theft mitigation, see ccdirect-auth-contract.md).
type deviceKey struct {
	priv ed25519.PrivateKey
}

// publicKeyB64 returns base64(raw 32-byte public key) — the wire form sent to
// the center as the device_pubkey login param.
func (k deviceKey) publicKeyB64() string {
	pub, _ := k.priv.Public().(ed25519.PublicKey)
	return base64.StdEncoding.EncodeToString(pub)
}

// loadOrCreateDeviceKey loads the raw 64-byte Ed25519 private key from path, or
// generates and persists a new one (file 0600, parent dir 0700) if absent.
func loadOrCreateDeviceKey(path string) (deviceKey, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		if len(data) != ed25519.PrivateKeySize {
			return deviceKey{}, fmt.Errorf("device key %s: expected %d bytes, got %d", path, ed25519.PrivateKeySize, len(data))
		}
		return deviceKey{priv: ed25519.PrivateKey(data)}, nil
	}
	if !os.IsNotExist(err) {
		return deviceKey{}, fmt.Errorf("read device key: %w", err)
	}

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return deviceKey{}, fmt.Errorf("generate device key: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return deviceKey{}, fmt.Errorf("create device key dir: %w", err)
	}
	if err := os.WriteFile(path, priv, 0o600); err != nil {
		return deviceKey{}, fmt.Errorf("write device key: %w", err)
	}
	return deviceKey{priv: priv}, nil
}

// deviceKeyPath returns the device key path. If statePath is set (custom session
// location) the device key sits next to it; otherwise it uses the per-user
// default alongside the default session file.
func deviceKeyPath(sessionPath string) (string, error) {
	if sessionPath != "" {
		return filepath.Join(filepath.Dir(sessionPath), "device_key"), nil
	}
	return enroll.DefaultDeviceKeyPath()
}
