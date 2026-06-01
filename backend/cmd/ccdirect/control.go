package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/ccgw/enroll"
)

// control.go is the local control-plane between the interactive `edge` console
// and a running `edge daemon` (#16). They are separate processes that coordinate
// over a Unix socket next to the session file. The console becomes a thin client
// when a daemon is running: /status, /login, /logout are sent over the socket so
// the long-lived daemon stays the single owner of the relay and credentials.
//
// The browser loopback+PKCE login still runs in the console (where the human is);
// only the resulting token pair is pushed to the daemon via the "login" command.

const controlSockName = "control.sock"

// control command names.
const (
	cmdStatus = "status"
	cmdLogin  = "login"
	cmdLogout = "logout"
)

// controlSocketPath returns the daemon control socket path, placed next to the
// session file. sessionPath may be empty, in which case the default session
// directory is used.
func controlSocketPath(sessionPath string) (string, error) {
	if sessionPath == "" {
		p, err := enroll.DefaultSessionPath()
		if err != nil {
			return "", err
		}
		sessionPath = p
	}
	return filepath.Join(filepath.Dir(sessionPath), controlSockName), nil
}

// controlRequest is one console->daemon command. login carries the token pair the
// console obtained from a loopback+PKCE login so the daemon can bring the relay
// online without doing the browser dance itself.
type controlRequest struct {
	Cmd     string `json:"cmd"`
	Access  string `json:"access,omitempty"`
	Refresh string `json:"refresh,omitempty"`
	EdgeID  string `json:"edge_id,omitempty"`
	Secret  string `json:"secret,omitempty"`
}

// controlResponse is the daemon's single reply. Status is populated for cmdStatus.
type controlResponse struct {
	OK     bool        `json:"ok"`
	Error  string      `json:"error,omitempty"`
	Status *statusInfo `json:"status,omitempty"`
}

// statusInfo mirrors what the standalone console's /status prints, but is produced
// by the daemon and rendered by the console.
type statusInfo struct {
	LoggedIn      bool   `json:"logged_in"`
	Owner         string `json:"owner,omitempty"`
	EdgeID        string `json:"edge_id,omitempty"`
	Center        string `json:"center"`
	Listen        string `json:"listen"`
	Version       string `json:"version"`
	AccessExpires string `json:"access_expires,omitempty"`
}

// writeControlMessage writes v as a single newline-delimited JSON frame.
func writeControlMessage(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = w.Write(b)
	return err
}

// readControlMessage reads one newline-delimited JSON frame into v.
func readControlMessage(r *bufio.Reader, v any) error {
	line, err := r.ReadBytes('\n')
	if err != nil {
		return err
	}
	return json.Unmarshal(line, v)
}

// controlHandler turns a request into a response. The daemon supplies one bound to
// its live relay; tests supply a fake.
type controlHandler func(controlRequest) controlResponse

// serveControl listens on the Unix socket at path and dispatches each connection's
// single request to h. A stale socket file (from an unclean prior exit) is removed
// first. The listener is closed when ctx is cancelled. The accept loop runs in a
// goroutine; the returned listener lets the caller close it eagerly too.
func serveControl(ctx context.Context, path string, h controlHandler) (net.Listener, error) {
	_ = os.Remove(path) // clear a stale socket from a prior unclean shutdown
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("chmod control socket: %w", err)
	}
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed (ctx done) or fatal accept error
			}
			go handleControlConn(conn, h)
		}
	}()
	return ln, nil
}

// handleControlConn reads one request, runs the handler, writes one response, and
// closes the connection. A malformed request yields an error response rather than
// a crash, and never wedges the accept loop (each conn is its own goroutine).
func handleControlConn(conn net.Conn, h controlHandler) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	br := bufio.NewReader(conn)
	var req controlRequest
	if err := readControlMessage(br, &req); err != nil {
		_ = writeControlMessage(conn, controlResponse{OK: false, Error: "bad request: " + err.Error()})
		return
	}
	_ = writeControlMessage(conn, h(req))
}

// controlRoundTrip dials the daemon socket, sends req, and reads one response. Used
// by the console thin client.
func controlRoundTrip(path string, req controlRequest) (controlResponse, error) {
	var resp controlResponse
	conn, err := net.DialTimeout("unix", path, 2*time.Second)
	if err != nil {
		return resp, err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	if err := writeControlMessage(conn, req); err != nil {
		return resp, err
	}
	br := bufio.NewReader(conn)
	if err := readControlMessage(br, &resp); err != nil {
		return resp, err
	}
	return resp, nil
}

// daemonRunning reports whether a daemon is answering on the control socket. A
// refused dial / missing socket / bad reply all mean "no daemon" (the console
// falls back to standalone mode).
func daemonRunning(path string) bool {
	resp, err := controlRoundTrip(path, controlRequest{Cmd: cmdStatus})
	return err == nil && resp.OK
}
