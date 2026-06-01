// Command edge is the standalone distributed-edge data-plane CLI.
//
// Running `edge` starts the relay immediately and drops you into an interactive
// console:
//
//	edge                    # start serving + console; if not logged in, prompts login
//	  /login                # browser loopback + PKCE login; creds saved locally
//	  /logout               # revoke + clear local creds (relay stops serving)
//	  /status               # owner, center, edge id, listen addr, token validity
//	  /quit                 # graceful shutdown
//
// For an unattended, long-lived service, run it as a daemon under launchd (#16):
//
//	edge daemon install     # write + load a KeepAlive LaunchAgent (macOS)
//	edge daemon status      # launchd load state + relay status over the socket
//	edge daemon uninstall   # unload + remove the LaunchAgent
//	edge daemon [run]       # run headless in the foreground (what launchd execs)
//
// When a daemon is running, a plain `edge` attaches to it as a thin client: the
// console's /login, /logout and /status are routed to the daemon over a Unix
// control socket next to the session file; the daemon periodically self-updates
// (verifying cchub's signed release) and exits so launchd restarts it on the new
// binary.
//
// Login uses loopback + PKCE + a per-machine device key: the console opens the
// browser to the center's /cli/authorize page (already logged in there); on
// approval the authorization code is delivered to a localhost callback server,
// exchanged for tokens, and saved to ~/.config/sub2api-edge/session.json (0600).
// The device key (~/.config/sub2api-edge/device_key, 0600) binds the refresh
// token to this machine. Only the owner token pair is stored — the edge id and
// lease-token seal secret are fetched from the center at runtime. See
// docs/tech/ccdirect-auth-contract.md and docs/tech/distributed-edge.md.
package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/ccdirect"
	"github.com/Wei-Shaw/sub2api/ccgw/contract"
	"github.com/Wei-Shaw/sub2api/ccgw/enroll"
)

// Version is set via -ldflags "-X main.Version=...".
var Version = "dev"

// cchubLivenessPubKey is cchub's base64 Ed25519 liveness public key, baked into
// the binary at build time via -ldflags "-X main.cchubLivenessPubKey=...". When
// empty, liveness enforcement is disabled (dev builds). Get the value from
// cchub's LivenessPublicKey() and embed it for production.
var cchubLivenessPubKey = ""

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Printf("ccdirect %s\n", Version)
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "upgrade" {
		if err := runUpgrade(os.Args[2:]); err != nil {
			log.Fatalf("edge upgrade: %v", err)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "daemon" {
		runDaemonCommand(os.Args[2:])
		return
	}
	runServe(os.Args[1:])
}

// edgeApp holds the running relay + the bits the console needs to log in/out and
// persist credentials.
type edgeApp struct {
	relay       *ccdirect.Relay
	cchubBase   string // center edge control-plane base, e.g. http://host:8080/edge
	authBase    string // sub2api API root, e.g. http://host:8080
	httpClient  *http.Client
	sessionPath string
	addr        string
	deviceKey   deviceKey
	cfg         edgeFlags
}

// resolveSessionPath returns the session file path: the explicit override if set,
// else the per-user default. Shared so the console, daemon, and the daemon-detect
// path all agree on where the session (and thus the control socket) lives.
func resolveSessionPath(cfg edgeFlags) string {
	if cfg.statePath != "" {
		return cfg.statePath
	}
	if p, err := enroll.DefaultSessionPath(); err == nil {
		return p
	}
	return ""
}

// newEdgeApp builds the relay + app from cfg, loading any saved session (owner
// token pair) and the per-machine device key. It does not start serving. Shared
// by the interactive console (runServe) and the headless daemon (runDaemon).
func newEdgeApp(cfg edgeFlags) (*edgeApp, error) {
	upstreamClient, err := ccdirect.NewUpstreamClient(cfg.upstreamProxy, cfg.upstreamTimeout)
	if err != nil {
		return nil, err
	}
	centerClient := &http.Client{Timeout: 15 * time.Second}

	sessionPath := resolveSessionPath(cfg)
	sess, _ := enroll.LoadSession(sessionPath)

	// Load (or create) the per-machine device key. The center binds the refresh
	// token to its public key; refreshOwner signs bound refresh requests with the
	// private key. A failure here is non-fatal: refresh just runs unsigned (and
	// any bound token will be rejected until a successful /login rebinds).
	dkPath, _ := deviceKeyPath(sessionPath)
	dk, dkErr := loadOrCreateDeviceKey(dkPath)
	if dkErr != nil {
		log.Printf("ccdirect: warning: device key unavailable (%v); refresh will be unsigned", dkErr)
	}

	cchubPubKey, err := contract.DecodeLivenessPubKey(cchubLivenessPubKey)
	if err != nil {
		return nil, fmt.Errorf("invalid embedded cchub liveness pubkey: %w", err)
	}
	if cchubPubKey == nil {
		log.Printf("ccdirect: WARNING: no cchub liveness key embedded — liveness enforcement disabled (dev build)")
	}

	relay := ccdirect.NewRelay(ccdirect.Config{
		InternalKey:       cfg.internalKey,
		OwnerAccessToken:  sess.OwnerAccess,
		OwnerRefreshToken: sess.OwnerRefresh,
		CCHubURL:          cfg.center,
		CCHubHTTP:         centerClient,
		Upstream:          upstreamClient,
		MaxFailover:       cfg.maxFailover,
		DeviceKey:         dk.priv,
		CCHubPubKey:       cchubPubKey,
	})

	app := &edgeApp{
		relay:       relay,
		cchubBase:   cfg.center,
		authBase:    authBaseFromCenter(cfg.center),
		httpClient:  centerClient,
		sessionPath: sessionPath,
		addr:        cfg.addr,
		deviceKey:   dk,
		cfg:         cfg,
	}
	// Persist rotated tokens (login + auto-refresh) so a restart keeps the
	// session. Empty pair clears the file (logout).
	relay.SetOnRefresh(func(access, refresh string) {
		if serr := enroll.SaveSession(app.sessionPath, enroll.Session{OwnerAccess: access, OwnerRefresh: refresh}); serr != nil {
			log.Printf("ccdirect: warning: could not save session: %v", serr)
		}
	})
	return app, nil
}

// startServing brings a logged-in relay online (fetch config), starts the HTTP
// server and heartbeat loop, and returns the server for later shutdown. Neither
// goroutine blocks the caller. Shared by console and daemon.
func (app *edgeApp) startServing(ctx context.Context) *http.Server {
	// If we already have a session on disk, bring the relay online (fetch config
	// using the saved/refreshed access token) before serving.
	if app.relay.LoggedIn() {
		if err := app.activate(ctx); err != nil {
			log.Printf("ccdirect: saved session not usable (%v); run /login", err)
		}
	}
	srv := &http.Server{Addr: app.addr, Handler: app.relay.Handler(), ReadHeaderTimeout: 15 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("ccdirect: %v", err)
		}
	}()
	go app.relay.RunHeartbeat(ctx, app.cfg.egressIP, splitCSV(app.cfg.platforms), app.cfg.heartbeat)
	return srv
}

// shutdownServer drains in-flight requests up to a timeout. Used on console quit,
// daemon SIGTERM, and before a self-update binary swap.
func shutdownServer(srv *http.Server) {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("ccdirect: shutdown error: %v", err)
	}
}

func runServe(args []string) {
	cfg := parseFlags(args)

	// If a daemon is already running, attach to it as a thin client instead of
	// starting a second relay on the same listen address. The console then drives
	// the daemon over the control socket.
	if sockPath, perr := controlSocketPath(resolveSessionPath(cfg)); perr == nil && daemonRunning(sockPath) {
		runConsoleClient(cfg, resolveSessionPath(cfg), sockPath)
		return
	}

	app, err := newEdgeApp(cfg)
	if err != nil {
		log.Fatalf("ccdirect: %v", err)
	}

	egressLabel := cfg.upstreamProxy
	if egressLabel == "" {
		egressLabel = "direct"
	}
	log.Printf("ccdirect %s: listening on %s, center=%s, egress=%s", Version, cfg.addr, cfg.center, egressLabel)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := app.startServing(ctx)

	// If not logged in, kick off login immediately (like claude code's first run).
	if !app.relay.LoggedIn() {
		fmt.Println("Not logged in. Starting login…")
		if err := app.doLogin(ctx); err != nil {
			fmt.Printf("login failed: %v\n", err)
		}
	}

	// Interactive console alongside the running relay.
	go app.repl(ctx, cancel)

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-quit:
	case <-ctx.Done():
	}
	log.Printf("ccdirect: shutting down")
	cancel()
	shutdownServer(srv)
}

// activate fetches the edge config with the current access token (refreshing
// once if it is expired) and installs edge id + seal secret into the relay,
// WITHOUT touching the owner token pair (so the saved refresh token survives).
func (app *edgeApp) activate(ctx context.Context) error {
	access := app.relay.OwnerAccess()
	conf, err := fetchConfig(ctx, app.httpClient, app.cchubBase, access)
	if err != nil {
		// Access may have expired while the edge was off; refresh via the relay
		// (which persists the rotated pair) and retry once.
		if app.relay.RefreshOwner(ctx) {
			access = app.relay.OwnerAccess()
			conf, err = fetchConfig(ctx, app.httpClient, app.cchubBase, access)
		}
		if err != nil {
			return err
		}
	}
	app.relay.SetSeal(conf.CCDirectID, []byte(conf.TokenSecret))
	return nil
}

// doLogin runs the loopback + PKCE + device-key login and installs the result
// into the relay. authBase is the sub2api API root; it is also the web origin
// that serves the /cli/authorize SPA route.
func (app *edgeApp) doLogin(ctx context.Context) error {
	res, err := loopbackLogin(ctx, app.httpClient, app.authBase, app.authBase, app.cchubBase, app.deviceKey)
	if err != nil {
		return err
	}
	app.relay.Login(res.access, res.refresh, res.ccdirectID, res.secret)
	if claims, ok := parseJWTUnverified(res.access); ok && claims.Email != "" {
		fmt.Printf("logged in as %s (edge %s)\n", claims.Email, res.ccdirectID)
	} else {
		fmt.Printf("logged in (edge %s)\n", res.ccdirectID)
	}
	return nil
}

// repl reads console commands until /quit or EOF.
func (app *edgeApp) repl(ctx context.Context, cancel context.CancelFunc) {
	sc := bufio.NewScanner(os.Stdin)
	fmt.Println("Type /login, /logout, /status, or /quit.")
	prompt := func() { fmt.Print("ccdirect> ") }
	prompt()
	for sc.Scan() {
		switch strings.TrimSpace(sc.Text()) {
		case "":
		case "/login":
			if err := app.doLogin(ctx); err != nil {
				fmt.Printf("login failed: %v\n", err)
			}
		case "/logout":
			logoutCenter(ctx, app.httpClient, app.authBase, app.relay.OwnerRefresh())
			app.relay.Logout()
			fmt.Println("logged out")
		case "/status":
			app.printStatus()
		case "/quit", "/exit":
			fmt.Println("bye")
			cancel()
			return
		default:
			fmt.Println("unknown command; try /login, /logout, /status, /quit")
		}
		prompt()
	}
	// EOF on stdin (e.g. piped/headless): keep serving, just stop reading.
}

func (app *edgeApp) printStatus() {
	renderStatusInfo(app.statusSnapshot())
}

func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}
