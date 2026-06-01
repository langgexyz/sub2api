//go:build unit

package main

import (
	"bufio"
	"bytes"
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"
)

func TestControlMessageRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	in := controlRequest{Cmd: cmdLogin, Access: "a", Refresh: "r", EdgeID: "e1", Secret: "s"}
	if err := writeControlMessage(&buf, in); err != nil {
		t.Fatalf("write: %v", err)
	}
	var out controlRequest
	if err := readControlMessage(bufio.NewReader(&buf), &out); err != nil {
		t.Fatalf("read: %v", err)
	}
	if out != in {
		t.Fatalf("round-trip mismatch: got %+v want %+v", out, in)
	}
}

// startTestControl spins a control server on a temp socket with handler h and
// returns its path. The server is torn down when the test ends.
func startTestControl(t *testing.T, h controlHandler) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), controlSockName)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if _, err := serveControl(ctx, sock, h); err != nil {
		t.Fatalf("serveControl: %v", err)
	}
	// Wait until the socket answers so the test isn't racy.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("unix", sock, 100*time.Millisecond); err == nil {
			_ = c.Close()
			return sock
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("control socket never came up")
	return ""
}

func TestServeControlStatusRoundTrip(t *testing.T) {
	want := &statusInfo{LoggedIn: true, Owner: "me@x", EdgeID: "edge-1", Center: "c", Listen: ":9", Version: "test"}
	sock := startTestControl(t, func(req controlRequest) controlResponse {
		if req.Cmd != cmdStatus {
			return controlResponse{OK: false, Error: "unexpected cmd"}
		}
		return controlResponse{OK: true, Status: want}
	})

	resp, err := controlRoundTrip(sock, controlRequest{Cmd: cmdStatus})
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if !resp.OK || resp.Status == nil || *resp.Status != *want {
		t.Fatalf("got %+v (status %+v), want OK with %+v", resp, resp.Status, want)
	}
}

func TestServeControlLoginDispatch(t *testing.T) {
	var got controlRequest
	sock := startTestControl(t, func(req controlRequest) controlResponse {
		got = req
		return controlResponse{OK: true}
	})
	in := controlRequest{Cmd: cmdLogin, Access: "acc", Refresh: "ref", EdgeID: "e9", Secret: "sec"}
	resp, err := controlRoundTrip(sock, in)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if !resp.OK {
		t.Fatalf("login not ok: %+v", resp)
	}
	if got != in {
		t.Fatalf("handler saw %+v, want %+v", got, in)
	}
}

func TestServeControlMalformedRequest(t *testing.T) {
	sock := startTestControl(t, func(controlRequest) controlResponse {
		t.Fatal("handler must not run for malformed input")
		return controlResponse{}
	})
	conn, err := net.DialTimeout("unix", sock, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write([]byte("not json\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	var resp controlResponse
	if err := readControlMessage(bufio.NewReader(conn), &resp); err != nil {
		t.Fatalf("read: %v", err)
	}
	if resp.OK {
		t.Fatalf("malformed request should fail, got %+v", resp)
	}
}

func TestDaemonRunning(t *testing.T) {
	// No daemon at a nonexistent socket.
	missing := filepath.Join(t.TempDir(), controlSockName)
	if daemonRunning(missing) {
		t.Fatal("daemonRunning true for missing socket")
	}
	// Live daemon answers status OK.
	sock := startTestControl(t, func(controlRequest) controlResponse {
		return controlResponse{OK: true, Status: &statusInfo{}}
	})
	if !daemonRunning(sock) {
		t.Fatal("daemonRunning false for live socket")
	}
}
