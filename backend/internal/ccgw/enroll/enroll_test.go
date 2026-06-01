//go:build unit

package enroll

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestTokenRoundTrip verifies that EncodeToken then DecodeToken preserves both
// the CCHub and Key fields.
func TestTokenRoundTrip(t *testing.T) {
	in := Token{CCHub: "https://center.example.com", Key: "secret-enroll-key"}
	s := EncodeToken(in)
	if s == "" {
		t.Fatal("EncodeToken returned an empty string")
	}
	out, err := DecodeToken(s)
	if err != nil {
		t.Fatalf("DecodeToken returned unexpected error: %v", err)
	}
	if out.CCHub != in.CCHub {
		t.Errorf("CCHub mismatch: got %q want %q", out.CCHub, in.CCHub)
	}
	if out.Key != in.Key {
		t.Errorf("Key mismatch: got %q want %q", out.Key, in.Key)
	}
}

// TestDecodeTokenRejects checks that DecodeToken rejects malformed input:
// non-base64 garbage, valid base64 of non-JSON, and JSON missing CCHub or Key.
func TestDecodeTokenRejects(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{
			name:  "non-base64 garbage",
			input: "!!! not base64 @@@",
		},
		{
			name:  "valid base64 of non-JSON",
			input: base64.RawURLEncoding.EncodeToString([]byte("this is not json")),
		},
		{
			name:  "JSON missing center",
			input: base64.RawURLEncoding.EncodeToString([]byte(`{"key":"k"}`)),
		},
		{
			name:  "JSON missing key",
			input: base64.RawURLEncoding.EncodeToString([]byte(`{"center":"https://c"}`)),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DecodeToken(tc.input); err == nil {
				t.Fatalf("DecodeToken(%q) = nil error, want error", tc.input)
			}
		})
	}
}

// TestSaveLoadRoundTrip writes an Enrolled with Save and reads it back with
// Load, asserting equality (including the Platforms slice) and that the file is
// written with mode 0600.
func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "edge.json")
	in := Enrolled{
		CCHubURL:         "https://center.example.com",
		CCDirectID:       "edge-42",
		EnrollKey:        "secret-enroll-key",
		HeartbeatSeconds: 30,
		MaxFailover:      3,
		Platforms:        []string{"openai", "anthropic"},
	}
	if err := Save(path, in); err != nil {
		t.Fatalf("Save returned unexpected error: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat returned unexpected error: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("file mode = %o, want 0600", mode)
	}

	out, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned unexpected error: %v", err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Errorf("round-trip mismatch:\n got  %+v\n want %+v", out, in)
	}
}

// TestLoadMissingFile checks that Load returns an error for a path that does
// not exist.
func TestLoadMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.json")
	if _, err := Load(path); err == nil {
		t.Fatal("Load of a missing file = nil error, want error")
	}
}

// TestDefaultStatePath checks that DefaultStatePath returns a non-empty path
// ending in "edge.json". It is skipped if os.UserConfigDir fails in the
// sandbox.
func TestDefaultStatePath(t *testing.T) {
	if _, err := os.UserConfigDir(); err != nil {
		t.Skipf("os.UserConfigDir unavailable in this environment: %v", err)
	}
	path, err := DefaultStatePath()
	if err != nil {
		t.Fatalf("DefaultStatePath returned unexpected error: %v", err)
	}
	if path == "" {
		t.Fatal("DefaultStatePath returned an empty path")
	}
	if base := filepath.Base(path); base != "edge.json" {
		t.Errorf("path base = %q, want %q", base, "edge.json")
	}
}
