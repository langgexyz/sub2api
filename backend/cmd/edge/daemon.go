package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

// runDaemonCommand dispatches the `daemon` subcommands. Bare `edge daemon` and
// `edge daemon run` start the headless service; install/uninstall/status manage
// the process-manager integration (added in part 4).
func runDaemonCommand(args []string) {
	sub := "run"
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "run":
		runDaemon(args)
	case "install":
		runDaemonInstall(args)
	case "uninstall":
		runDaemonUninstall(args)
	case "status":
		runDaemonStatus(args)
	default:
		log.Fatalf("edge daemon: unknown subcommand %q (want: run|install|uninstall|status)", sub)
	}
}

// runDaemon is the headless service mode (`edge daemon` / `edge daemon run`). It
// serves the relay with NO interactive console: an interactive `edge` becomes a
// thin client and drives it over the control socket. The daemon is meant to run
// under a process manager (launchd; see `edge daemon install`) which restarts it
// on crash and after a self-update exit. Login is NOT initiated here — the daemon
// cannot open a browser; the console performs loopback+PKCE and pushes the token
// pair over the control socket.
func runDaemon(args []string) {
	cfg := parseFlags(args)
	app, err := newEdgeApp(cfg)
	if err != nil {
		log.Fatalf("edge daemon: %v", err)
	}

	sockPath, err := controlSocketPath(app.sessionPath)
	if err != nil {
		log.Fatalf("edge daemon: control socket path: %v", err)
	}

	egressLabel := cfg.upstreamProxy
	if egressLabel == "" {
		egressLabel = "direct"
	}
	log.Printf("edge daemon %s: listening on %s, center=%s, egress=%s, control=%s",
		Version, cfg.addr, cfg.center, egressLabel, sockPath)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := app.startServing(ctx)

	if _, err := serveControl(ctx, sockPath, app.controlHandler); err != nil {
		log.Fatalf("edge daemon: control socket: %v", err)
	}

	go app.selfUpgradeLoop(ctx, srv, cancel)

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-quit:
	case <-ctx.Done():
	}
	log.Printf("edge daemon: shutting down")
	cancel()
	shutdownServer(srv)
}

// controlHandler answers console requests against the live relay. login installs a
// token pair the console obtained from its browser loopback+PKCE flow (the seal
// secret is hex-encoded on the wire to survive JSON); the daemon never opens a
// browser itself.
func (app *edgeApp) controlHandler(req controlRequest) controlResponse {
	switch req.Cmd {
	case cmdStatus:
		return controlResponse{OK: true, Status: app.statusSnapshot()}
	case cmdLogin:
		if req.Access == "" || req.Refresh == "" {
			return controlResponse{OK: false, Error: "login requires access and refresh tokens"}
		}
		secret, err := hex.DecodeString(req.Secret)
		if err != nil {
			return controlResponse{OK: false, Error: "invalid seal secret encoding: " + err.Error()}
		}
		app.relay.Login(req.Access, req.Refresh, req.EdgeID, secret)
		return controlResponse{OK: true, Status: app.statusSnapshot()}
	case cmdLogout:
		logoutCenter(context.Background(), app.httpClient, app.authBase, app.relay.OwnerRefresh())
		app.relay.Logout()
		return controlResponse{OK: true, Status: app.statusSnapshot()}
	default:
		return controlResponse{OK: false, Error: "unknown command: " + req.Cmd}
	}
}

// statusSnapshot captures the relay state for a control status reply (mirrors the
// console's printStatus fields).
func (app *edgeApp) statusSnapshot() *statusInfo {
	s := &statusInfo{Center: app.centerEdge, Listen: app.addr, Version: Version}
	if !app.relay.LoggedIn() {
		return s
	}
	s.LoggedIn = true
	s.EdgeID = app.relay.EdgeID()
	if claims, ok := parseJWTUnverified(app.relay.OwnerAccess()); ok {
		if claims.Email != "" {
			s.Owner = fmt.Sprintf("%s (uid %d)", claims.Email, claims.UserID)
		}
		s.AccessExpires = time.Until(claims.expiresAt()).Round(time.Second).String()
	}
	return s
}

// selfUpgradeLoop ticks every cfg.upgradeInterval and self-updates when cchub
// publishes a newer signed release. On a successful swap it drains in-flight
// requests and exits(0) so the process manager restarts on the new binary
// (chosen design: replace-and-exit, not in-place re-exec). Disabled when the
// interval is <= 0 or the binary path can't be resolved; a failed check is logged
// and retried next tick (never bricks the running service).
func (app *edgeApp) selfUpgradeLoop(ctx context.Context, srv *http.Server, cancel context.CancelFunc) {
	if app.cfg.upgradeInterval <= 0 {
		log.Printf("edge daemon: self-update disabled (upgrade-interval <= 0)")
		return
	}
	self, err := os.Executable()
	if err != nil {
		log.Printf("edge daemon: self-update disabled: cannot locate binary: %v", err)
		return
	}
	self, _ = filepath.EvalSymlinks(self)

	t := time.NewTicker(app.cfg.upgradeInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			newVer, err := checkAndUpgrade(ctx, app.cfg, self)
			if err != nil {
				log.Printf("edge daemon: self-update check failed: %v", err)
				continue
			}
			if newVer == "" {
				continue // already current / nothing published
			}
			log.Printf("edge daemon: self-updated %s -> %s; draining and exiting for restart", Version, newVer)
			shutdownServer(srv)
			cancel()
			os.Exit(0)
		}
	}
}
