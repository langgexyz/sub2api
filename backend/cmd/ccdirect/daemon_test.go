//go:build unit

package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/ccgw/contract"
)

// releaseServer serves a signed manifest at /v1/release and the binary at /bin.
func releaseServer(t *testing.T, priv ed25519.PrivateKey, version string, binary []byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/bin", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(binary) })
	sum := sha256.Sum256(binary)
	var srv *httptest.Server
	mux.HandleFunc("/v1/release", func(w http.ResponseWriter, _ *http.Request) {
		man := contract.SignRelease(priv, contract.ReleaseManifest{
			Version: version, OS: runtime.GOOS, Arch: runtime.GOARCH,
			URL: srv.URL + "/bin", SHA256: hex.EncodeToString(sum[:]),
		})
		_ = json.NewEncoder(w).Encode(man)
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func withReleaseKey(t *testing.T, pub ed25519.PublicKey) {
	t.Helper()
	prev := cchubReleasePubKey
	if pub == nil {
		cchubReleasePubKey = ""
	} else {
		cchubReleasePubKey = contract.EncodeReleasePubKey(pub)
	}
	t.Cleanup(func() { cchubReleasePubKey = prev })
}

func TestCheckAndUpgradeSwapsNewerSignedRelease(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	withReleaseKey(t, pub)

	newBin := []byte("brand-new-edge-binary-bytes")
	srv := releaseServer(t, priv, "v9.9.9", newBin)

	self := filepath.Join(t.TempDir(), "edge")
	if err := os.WriteFile(self, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := checkAndUpgrade(context.Background(), edgeFlags{center: srv.URL}, self)
	if err != nil {
		t.Fatalf("checkAndUpgrade: %v", err)
	}
	if got != "v9.9.9" {
		t.Fatalf("new version = %q, want v9.9.9", got)
	}
	swapped, _ := os.ReadFile(self)
	if !bytes.Equal(swapped, newBin) {
		t.Fatalf("binary not swapped: got %q", swapped)
	}
}

func TestCheckAndUpgradeNoopOnSameVersion(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	withReleaseKey(t, pub)

	// Manifest advertises the running Version -> nothing to do, binary untouched.
	srv := releaseServer(t, priv, Version, []byte("whatever"))
	self := filepath.Join(t.TempDir(), "edge")
	orig := []byte("unchanged")
	if err := os.WriteFile(self, orig, 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := checkAndUpgrade(context.Background(), edgeFlags{center: srv.URL}, self)
	if err != nil {
		t.Fatalf("checkAndUpgrade: %v", err)
	}
	if got != "" {
		t.Fatalf("expected no-op, got version %q", got)
	}
	cur, _ := os.ReadFile(self)
	if !bytes.Equal(cur, orig) {
		t.Fatalf("binary changed on no-op: %q", cur)
	}
}

func TestCheckAndUpgradeRefusesWithoutEmbeddedKey(t *testing.T) {
	withReleaseKey(t, nil) // no embedded release key
	self := filepath.Join(t.TempDir(), "edge")
	_ = os.WriteFile(self, []byte("x"), 0o755)
	if _, err := checkAndUpgrade(context.Background(), edgeFlags{center: "http://127.0.0.1:1"}, self); err == nil {
		t.Fatal("expected refusal without embedded release key")
	}
}

// newTestApp builds an edgeApp with state confined to a temp dir and a center that
// resolves to a refused loopback port (best-effort logout calls fail fast).
func newTestApp(t *testing.T) *edgeApp {
	t.Helper()
	app, err := newEdgeApp(edgeFlags{
		center:          "http://127.0.0.1:1/edge",
		addr:            ":0",
		statePath:       filepath.Join(t.TempDir(), "session.json"),
		upstreamTimeout: time.Minute,
	})
	if err != nil {
		t.Fatalf("newEdgeApp: %v", err)
	}
	return app
}

func TestStatusSnapshotLoggedOut(t *testing.T) {
	app := newTestApp(t)
	s := app.statusSnapshot()
	if s.LoggedIn {
		t.Fatal("fresh app should be logged out")
	}
	if s.CCHub == "" || s.Listen == "" || s.Version == "" {
		t.Fatalf("status missing static fields: %+v", s)
	}
}

func TestControlHandlerDispatch(t *testing.T) {
	app := newTestApp(t)

	// status while logged out
	if resp := app.controlHandler(controlRequest{Cmd: cmdStatus}); !resp.OK || resp.Status == nil || resp.Status.LoggedIn {
		t.Fatalf("status logged-out: %+v", resp)
	}

	// login installs tokens (seal secret hex-encoded on the wire)
	resp := app.controlHandler(controlRequest{
		Cmd: cmdLogin, Access: "acc", Refresh: "ref", CCDirectID: "edge-7",
		Secret: hex.EncodeToString([]byte{0x00, 0x01, 0xff}),
	})
	if !resp.OK || resp.Status == nil || !resp.Status.LoggedIn || resp.Status.CCDirectID != "edge-7" {
		t.Fatalf("login: %+v (status %+v)", resp, resp.Status)
	}
	if !app.relay.LoggedIn() {
		t.Fatal("relay not logged in after login command")
	}

	// login missing tokens -> error
	if resp := app.controlHandler(controlRequest{Cmd: cmdLogin}); resp.OK {
		t.Fatalf("login without tokens should fail: %+v", resp)
	}

	// bad hex secret -> error
	if resp := app.controlHandler(controlRequest{Cmd: cmdLogin, Access: "a", Refresh: "r", Secret: "zz"}); resp.OK {
		t.Fatalf("login with bad hex secret should fail: %+v", resp)
	}

	// unknown command -> error
	if resp := app.controlHandler(controlRequest{Cmd: "frobnicate"}); resp.OK {
		t.Fatalf("unknown command should fail: %+v", resp)
	}
}
