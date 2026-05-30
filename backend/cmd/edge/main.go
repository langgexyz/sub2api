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

	"github.com/Wei-Shaw/sub2api/internal/edgegw"
	"github.com/Wei-Shaw/sub2api/internal/edgegw/enroll"
)

// Version is set via -ldflags "-X main.Version=...".
var Version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Printf("edge %s\n", Version)
		return
	}
	runServe(os.Args[1:])
}

// edgeApp holds the running relay + the bits the console needs to log in/out and
// persist credentials.
type edgeApp struct {
	relay       *edgegw.EdgeRelay
	centerEdge  string // center edge control-plane base, e.g. http://host:8080/edge
	authBase    string // sub2api API root, e.g. http://host:8080
	httpClient  *http.Client
	sessionPath string
	addr        string
	deviceKey   deviceKey
}

func runServe(args []string) {
	cfg := parseFlags(args)

	upstreamClient, err := edgegw.NewUpstreamClient(cfg.upstreamProxy, cfg.upstreamTimeout)
	if err != nil {
		log.Fatalf("edge: %v", err)
	}
	centerClient := &http.Client{Timeout: 15 * time.Second}

	// Load any saved session (owner token pair). Everything else is fetched at
	// login/startup.
	sessionPath := cfg.statePath
	if sessionPath == "" {
		if p, perr := enroll.DefaultSessionPath(); perr == nil {
			sessionPath = p
		}
	}
	sess, _ := enroll.LoadSession(sessionPath)

	// Load (or create) the per-machine device key. The center binds the refresh
	// token to its public key; refreshOwner signs bound refresh requests with the
	// private key. A failure here is non-fatal: refresh just runs unsigned (and
	// any bound token will be rejected until a successful /login rebinds).
	dkPath, _ := deviceKeyPath(sessionPath)
	dk, dkErr := loadOrCreateDeviceKey(dkPath)
	if dkErr != nil {
		log.Printf("edge: warning: device key unavailable (%v); refresh will be unsigned", dkErr)
	}

	relay := edgegw.NewEdgeRelay(edgegw.EdgeConfig{
		InternalKey:       cfg.internalKey,
		OwnerAccessToken:  sess.OwnerAccess,
		OwnerRefreshToken: sess.OwnerRefresh,
		CenterURL:         cfg.center,
		CenterHTTP:        centerClient,
		Upstream:          upstreamClient,
		MaxFailover:       cfg.maxFailover,
		DeviceKey:         dk.priv,
	})

	app := &edgeApp{
		relay:       relay,
		centerEdge:  cfg.center,
		authBase:    authBaseFromCenter(cfg.center),
		httpClient:  centerClient,
		sessionPath: sessionPath,
		addr:        cfg.addr,
		deviceKey:   dk,
	}
	// Persist rotated tokens (login + auto-refresh) so a restart keeps the
	// session. Empty pair clears the file (logout).
	relay.SetOnRefresh(func(access, refresh string) {
		if serr := enroll.SaveSession(app.sessionPath, enroll.Session{OwnerAccess: access, OwnerRefresh: refresh}); serr != nil {
			log.Printf("edge: warning: could not save session: %v", serr)
		}
	})

	egressLabel := cfg.upstreamProxy
	if egressLabel == "" {
		egressLabel = "direct"
	}
	log.Printf("edge %s: listening on %s, center=%s, egress=%s", Version, cfg.addr, cfg.center, egressLabel)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// If we already have a session on disk, bring the relay online (fetch config
	// using the saved/refreshed access token) before serving.
	if relay.LoggedIn() {
		if err := app.activate(ctx); err != nil {
			log.Printf("edge: saved session not usable (%v); run /login", err)
		}
	}

	srv := &http.Server{Addr: cfg.addr, Handler: relay.Handler(), ReadHeaderTimeout: 15 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("edge: %v", err)
		}
	}()
	go relay.RunHeartbeat(ctx, cfg.egressIP, splitCSV(cfg.platforms), cfg.heartbeat)

	// If not logged in, kick off login immediately (like claude code's first run).
	if !relay.LoggedIn() {
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
	log.Printf("edge: shutting down")
	cancel()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("edge: shutdown error: %v", err)
	}
}

// activate fetches the edge config with the current access token (refreshing
// once if it is expired) and installs edge id + seal secret into the relay,
// WITHOUT touching the owner token pair (so the saved refresh token survives).
func (app *edgeApp) activate(ctx context.Context) error {
	access := app.relay.OwnerAccess()
	conf, err := fetchConfig(ctx, app.httpClient, app.centerEdge, access)
	if err != nil {
		// Access may have expired while the edge was off; refresh via the relay
		// (which persists the rotated pair) and retry once.
		if app.relay.RefreshOwner(ctx) {
			access = app.relay.OwnerAccess()
			conf, err = fetchConfig(ctx, app.httpClient, app.centerEdge, access)
		}
		if err != nil {
			return err
		}
	}
	app.relay.SetSeal(conf.EdgeID, []byte(conf.TokenSecret))
	return nil
}

// doLogin runs the loopback + PKCE + device-key login and installs the result
// into the relay. authBase is the sub2api API root; it is also the web origin
// that serves the /cli/authorize SPA route.
func (app *edgeApp) doLogin(ctx context.Context) error {
	res, err := loopbackLogin(ctx, app.httpClient, app.authBase, app.authBase, app.centerEdge, app.deviceKey)
	if err != nil {
		return err
	}
	app.relay.Login(res.access, res.refresh, res.edgeID, res.secret)
	if claims, ok := parseJWTUnverified(res.access); ok && claims.Email != "" {
		fmt.Printf("logged in as %s (edge %s)\n", claims.Email, res.edgeID)
	} else {
		fmt.Printf("logged in (edge %s)\n", res.edgeID)
	}
	return nil
}

// repl reads console commands until /quit or EOF.
func (app *edgeApp) repl(ctx context.Context, cancel context.CancelFunc) {
	sc := bufio.NewScanner(os.Stdin)
	fmt.Println("Type /login, /logout, /status, or /quit.")
	prompt := func() { fmt.Print("edge> ") }
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
	if !app.relay.LoggedIn() {
		fmt.Println("  status: logged out (run /login)")
		fmt.Printf("  center: %s\n", app.centerEdge)
		fmt.Printf("  listen: %s\n", app.addr)
		return
	}
	access := app.relay.OwnerAccess()
	who := "unknown"
	expiry := "?"
	if claims, ok := parseJWTUnverified(access); ok {
		if claims.Email != "" {
			who = fmt.Sprintf("%s (uid %d)", claims.Email, claims.UserID)
		}
		expiry = time.Until(claims.expiresAt()).Round(time.Second).String()
	}
	fmt.Printf("  status: logged in\n")
	fmt.Printf("  owner:  %s\n", who)
	fmt.Printf("  edge:   %s\n", app.relay.EdgeID())
	fmt.Printf("  center: %s\n", app.centerEdge)
	fmt.Printf("  listen: %s\n", app.addr)
	fmt.Printf("  access expires in: %s\n", expiry)
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
