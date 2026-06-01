//go:build unit

package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what it
// wrote. Used to assert the console's human-facing output.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()
	fn()
	_ = w.Close()
	out, _ := io.ReadAll(r)
	return string(out)
}

func TestRenderStatusInfo(t *testing.T) {
	loggedOut := captureStdout(t, func() {
		renderStatusInfo(&statusInfo{Center: "http://c/edge", Listen: ":8088"})
	})
	if !strings.Contains(loggedOut, "logged out") || !strings.Contains(loggedOut, ":8088") {
		t.Fatalf("logged-out render missing fields:\n%s", loggedOut)
	}

	loggedIn := captureStdout(t, func() {
		renderStatusInfo(&statusInfo{LoggedIn: true, Owner: "me@x (uid 1)", EdgeID: "edge-2", Center: "http://c/edge", Listen: ":8088", AccessExpires: "1h0m0s"})
	})
	for _, want := range []string{"logged in", "me@x", "edge-2", "1h0m0s"} {
		if !strings.Contains(loggedIn, want) {
			t.Fatalf("logged-in render missing %q:\n%s", want, loggedIn)
		}
	}
}

func TestClientLogoutRoundTrip(t *testing.T) {
	sock := startTestControl(t, func(req controlRequest) controlResponse {
		if req.Cmd != cmdLogout {
			return controlResponse{OK: false, Error: "unexpected cmd"}
		}
		return controlResponse{OK: true, Status: &statusInfo{}}
	})
	out := captureStdout(t, func() { clientLogout(sock) })
	if !strings.Contains(out, "logged out") {
		t.Fatalf("expected logout confirmation, got: %q", out)
	}
}

func TestClientLogoutDaemonUnreachable(t *testing.T) {
	missing := filepath.Join(t.TempDir(), controlSockName)
	out := captureStdout(t, func() { clientLogout(missing) })
	if !strings.Contains(out, "could not reach daemon") {
		t.Fatalf("expected unreachable message, got: %q", out)
	}
}

func TestPrintClientStatusRendersDaemonReply(t *testing.T) {
	sock := startTestControl(t, func(controlRequest) controlResponse {
		return controlResponse{OK: true, Status: &statusInfo{LoggedIn: true, EdgeID: "edge-9", Center: "c", Listen: ":1"}}
	})
	out := captureStdout(t, func() { printClientStatus(sock) })
	if !strings.Contains(out, "logged in") || !strings.Contains(out, "edge-9") {
		t.Fatalf("client status render: %q", out)
	}
}
