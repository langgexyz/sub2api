// Package enroll implements the single-token enrollment flow for sub2api
// distributed-edge gateways. A user installs an edge and supplies exactly one
// token that embeds the center base URL and an enroll key. The edge decodes the
// token, enrolls with the center, and persists the center-issued configuration
// locally so subsequent runs require no flags.
package enroll

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Token is the single user-facing credential. It embeds the center base URL
// and an enroll key, so the user copy-pastes exactly one string.
type Token struct {
	CCHub string `json:"cchub"` // center base URL, e.g. https://center.example.com
	Key   string `json:"key"`   // enroll key issued by the center login
}

// EncodeToken serializes a Token to a compact, copy-pasteable string
// (base64url(JSON), no padding).
func EncodeToken(t Token) string {
	// json.Marshal of a struct with only string fields never fails, so the
	// error is intentionally discarded here.
	data, _ := json.Marshal(t)
	return base64.RawURLEncoding.EncodeToString(data)
}

// DecodeToken parses a string produced by EncodeToken. Returns an error for
// malformed input or when CCHub/Key is empty.
func DecodeToken(s string) (Token, error) {
	data, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return Token{}, fmt.Errorf("enroll: token is not valid base64url: %w", err)
	}
	var t Token
	if err := json.Unmarshal(data, &t); err != nil {
		return Token{}, fmt.Errorf("enroll: token payload is not valid JSON: %w", err)
	}
	if t.CCHub == "" {
		return Token{}, errors.New("enroll: token is missing center URL")
	}
	if t.Key == "" {
		return Token{}, errors.New("enroll: token is missing enroll key")
	}
	return t, nil
}

// Enrolled is the persisted edge configuration: the center URL + enroll key
// the edge keeps, plus the parameters the center issued at enroll time.
type Enrolled struct {
	CCHubURL         string   `json:"cchub_url"`
	CCDirectID       string   `json:"ccdirect_id"`
	EnrollKey        string   `json:"enroll_key"`
	HeartbeatSeconds int      `json:"heartbeat_seconds"`
	MaxFailover      int      `json:"max_failover"`
	Platforms        []string `json:"platforms,omitempty"`
}

// DefaultStatePath returns the per-user state file path, using
// os.UserConfigDir() joined with "sub2api-edge/edge.json". Returns an error
// only if os.UserConfigDir fails.
func DefaultStatePath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("enroll: cannot resolve user config dir: %w", err)
	}
	return filepath.Join(dir, "sub2api-edge", "edge.json"), nil
}

// Save writes e as indented JSON to path, creating parent dirs (0700) and
// writing the file 0600 (it can contain the enroll key).
func Save(path string, e Enrolled) error {
	data, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return fmt.Errorf("enroll: cannot marshal enrolled config: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("enroll: cannot create state dir: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("enroll: cannot write state file: %w", err)
	}
	return nil
}

// Load reads an Enrolled previously written by Save.
func Load(path string) (Enrolled, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Enrolled{}, fmt.Errorf("enroll: cannot read state file: %w", err)
	}
	var e Enrolled
	if err := json.Unmarshal(data, &e); err != nil {
		return Enrolled{}, fmt.Errorf("enroll: state file is not valid JSON: %w", err)
	}
	return e, nil
}
