package main

import (
	"bufio"
	"context"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// runConsoleClient is the thin-client console used when an `edge daemon` is already
// running: /status, /login and /logout are routed to the daemon over the control
// socket so the daemon stays the single owner of the relay and credentials. /quit
// exits the console only — the daemon keeps serving.
//
// The browser loopback+PKCE login still runs HERE (where the human is): the console
// completes the flow, then pushes the resulting token pair to the daemon via the
// login command (seal secret hex-encoded to survive JSON).
func runConsoleClient(cfg edgeFlags, sessionPath, sockPath string) {
	httpClient := &http.Client{Timeout: 15 * time.Second}
	authBase := authBaseFromCenter(cfg.center)
	dkPath, _ := deviceKeyPath(sessionPath)
	dk, dkErr := loadOrCreateDeviceKey(dkPath)
	if dkErr != nil {
		log.Printf("ccdirect: warning: device key unavailable (%v); login may fail", dkErr)
	}

	fmt.Printf("ccdirect %s: attached to running daemon (control=%s)\n", Version, sockPath)
	printClientStatus(sockPath)

	sc := bufio.NewScanner(os.Stdin)
	fmt.Println("Type /login, /logout, /status, or /quit (the daemon keeps running).")
	prompt := func() { fmt.Print("ccdirect> ") }
	prompt()
	for sc.Scan() {
		switch strings.TrimSpace(sc.Text()) {
		case "":
		case "/login":
			clientLogin(httpClient, authBase, cfg.center, dk, sockPath)
		case "/logout":
			clientLogout(sockPath)
		case "/status":
			printClientStatus(sockPath)
		case "/quit", "/exit":
			fmt.Println("bye (daemon keeps running; use `edge daemon` controls to stop it)")
			return
		default:
			fmt.Println("unknown command; try /login, /logout, /status, /quit")
		}
		prompt()
	}
	// EOF on stdin (piped/headless): nothing to do; the daemon keeps serving.
}

// clientLogin runs the local loopback+PKCE browser login and pushes the resulting
// token pair to the daemon.
func clientLogin(hc *http.Client, authBase, cchubBase string, dk deviceKey, sockPath string) {
	ctx := context.Background()
	res, err := loopbackLogin(ctx, hc, authBase, authBase, cchubBase, dk)
	if err != nil {
		fmt.Printf("login failed: %v\n", err)
		return
	}
	resp, err := controlRoundTrip(sockPath, controlRequest{
		Cmd:        cmdLogin,
		Access:     res.access,
		Refresh:    res.refresh,
		CCDirectID: res.ccdirectID,
		Secret:     hex.EncodeToString(res.secret),
	})
	if err != nil {
		fmt.Printf("login: could not reach daemon: %v\n", err)
		return
	}
	if !resp.OK {
		fmt.Printf("login rejected by daemon: %s\n", resp.Error)
		return
	}
	fmt.Printf("logged in (pushed to daemon, edge %s)\n", res.ccdirectID)
}

// clientLogout asks the daemon to revoke + clear credentials (the daemon performs
// the server-side revocation itself).
func clientLogout(sockPath string) {
	resp, err := controlRoundTrip(sockPath, controlRequest{Cmd: cmdLogout})
	if err != nil {
		fmt.Printf("logout: could not reach daemon: %v\n", err)
		return
	}
	if !resp.OK {
		fmt.Printf("logout rejected: %s\n", resp.Error)
		return
	}
	fmt.Println("logged out")
}

// printClientStatus queries the daemon's status over the socket and renders it.
func printClientStatus(sockPath string) {
	resp, err := controlRoundTrip(sockPath, controlRequest{Cmd: cmdStatus})
	if err != nil {
		fmt.Printf("  status: could not reach daemon: %v\n", err)
		return
	}
	if !resp.OK || resp.Status == nil {
		fmt.Printf("  status: daemon error: %s\n", resp.Error)
		return
	}
	renderStatusInfo(resp.Status)
}

// renderStatusInfo prints a statusInfo in the same layout the standalone console
// uses, so console and daemon-attached output match.
func renderStatusInfo(s *statusInfo) {
	if !s.LoggedIn {
		fmt.Println("  status: logged out (run /login)")
		fmt.Printf("  center: %s\n", s.CCHub)
		fmt.Printf("  listen: %s\n", s.Listen)
		return
	}
	fmt.Println("  status: logged in")
	owner := s.Owner
	if owner == "" {
		owner = "unknown"
	}
	fmt.Printf("  owner:  %s\n", owner)
	fmt.Printf("  edge:   %s\n", s.CCDirectID)
	fmt.Printf("  center: %s\n", s.CCHub)
	fmt.Printf("  listen: %s\n", s.Listen)
	expiry := s.AccessExpires
	if expiry == "" {
		expiry = "?"
	}
	fmt.Printf("  access expires in: %s\n", expiry)
}
