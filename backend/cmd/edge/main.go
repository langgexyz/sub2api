// Command edge is the standalone distributed-edge data-plane CLI.
//
// Onboarding is one token:
//
//	edge enroll <token>     # token (from the center login) embeds center URL + key;
//	                        # the edge fetches its config from the center and saves it
//	edge                    # runs using the saved, center-issued config
//
// The edge registers with the center, accepts client prompts on the sub2api
// gateway surface, leases an account, performs the upstream request itself from
// its stable egress IP/proxy, streams the response back, and settles usage.
// Advanced flags exist as overrides but are rarely needed. See
// docs/tech/distributed-edge.md.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/edgegw"
	"github.com/Wei-Shaw/sub2api/internal/edgegw/edgetls"
	"github.com/Wei-Shaw/sub2api/internal/edgegw/enroll"
)

// Version is set via -ldflags "-X main.Version=...".
var Version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "enroll" {
		runEnroll(os.Args[2:])
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Printf("edge %s\n", Version)
		return
	}
	runServe(os.Args[1:])
}

// runEnroll exchanges the user's token for center-issued config and saves it.
func runEnroll(args []string) {
	fs := flag.NewFlagSet("enroll", flag.ExitOnError)
	statePath := fs.String("state", "", "state file path (default: per-user config dir)")
	_ = fs.Parse(args)
	if fs.NArg() < 1 {
		log.Fatalf("usage: edge enroll <token>")
	}
	tok, err := enroll.DecodeToken(fs.Arg(0))
	if err != nil {
		log.Fatalf("edge: invalid token: %v", err)
	}

	body, _ := json.Marshal(edgegw.EnrollRequest{Key: tok.Key})
	resp, err := http.Post(strings.TrimRight(tok.Center, "/")+"/v1/enroll", "application/json", bytes.NewReader(body))
	if err != nil {
		log.Fatalf("edge: enroll request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		log.Fatalf("edge: enroll rejected (%d): %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	var er edgegw.EnrollResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		log.Fatalf("edge: decode enroll response: %v", err)
	}

	centerURL := er.CenterURL
	if centerURL == "" {
		centerURL = tok.Center
	}
	e := enroll.Enrolled{
		CenterURL:        centerURL,
		EdgeID:           er.EdgeID,
		EnrollKey:        tok.Key,
		HeartbeatSeconds: er.HeartbeatSeconds,
		MaxFailover:      er.MaxFailover,
		Platforms:        er.Platforms,
	}
	path := *statePath
	if path == "" {
		path = os.Getenv("EDGE_STATE")
	}
	if path == "" {
		path, err = enroll.DefaultStatePath()
		if err != nil {
			log.Fatalf("edge: resolve state path: %v", err)
		}
	}
	if err := enroll.Save(path, e); err != nil {
		log.Fatalf("edge: save state: %v", err)
	}
	log.Printf("edge: enrolled as %q (center=%s); saved to %s. run `edge` to start.", e.EdgeID, e.CenterURL, path)
}

// runServe loads saved state (if any) as defaults, applies flag/env overrides,
// and runs the relay.
func runServe(args []string) {
	// Seed defaults from saved enrollment state when present.
	st := loadState("")

	fs := flag.NewFlagSet("edge", flag.ExitOnError)
	addr := fs.String("addr", env("EDGE_ADDR", ":8088"), "listen address for client traffic [EDGE_ADDR]")
	center := fs.String("center", orDefault(st.CenterURL, env("EDGE_CENTER_URL", "http://localhost:9000")), "center base URL [EDGE_CENTER_URL]")
	edgeID := fs.String("edge-id", orDefault(st.EdgeID, env("EDGE_ID", "edge-local")), "edge identifier [EDGE_ID]")
	enrollKey := fs.String("enroll-key", orDefault(st.EnrollKey, env("EDGE_ENROLL_KEY", "")), "enroll key [EDGE_ENROLL_KEY]")
	platforms := fs.String("platforms", env("EDGE_PLATFORMS", strings.Join(st.Platforms, ",")), "comma-separated platforms [EDGE_PLATFORMS]")
	egressIP := fs.String("egress-ip", env("EDGE_EGRESS_IP", ""), "stable egress IP reported to center (auto-detected if empty) [EDGE_EGRESS_IP]")
	maxFailover := fs.Int("max-failover", intOrDefault(st.MaxFailover, envInt("EDGE_MAX_FAILOVER", 3)), "max candidates to try locally [EDGE_MAX_FAILOVER]")
	upstreamProxy := fs.String("upstream-proxy", env("EDGE_UPSTREAM_PROXY", ""), "egress proxy: http/https/socks5 (empty=direct) [EDGE_UPSTREAM_PROXY]")
	upstreamTimeout := fs.Duration("upstream-timeout", envDuration("EDGE_UPSTREAM_TIMEOUT", 5*time.Minute), "upstream timeout [EDGE_UPSTREAM_TIMEOUT]")
	heartbeat := fs.Duration("heartbeat", heartbeatDefault(st), "heartbeat interval [EDGE_HEARTBEAT]")
	tlsCert := fs.String("tls-cert", env("EDGE_TLS_CERT", ""), "client cert PEM for mTLS to center [EDGE_TLS_CERT]")
	tlsKey := fs.String("tls-key", env("EDGE_TLS_KEY", ""), "client key PEM for mTLS to center [EDGE_TLS_KEY]")
	tlsServerCA := fs.String("tls-server-ca", env("EDGE_TLS_SERVER_CA", ""), "CA PEM that signs the center cert [EDGE_TLS_SERVER_CA]")
	internalKey := fs.String("internal-key", env("EDGE_INTERNAL_KEY", ""), "shared secret enabling /internal/egress (empty = disabled) [EDGE_INTERNAL_KEY]")
	_ = fs.Parse(args)

	upstreamClient, err := edgegw.NewUpstreamClient(*upstreamProxy, *upstreamTimeout)
	if err != nil {
		log.Fatalf("edge: %v", err)
	}

	centerClient := &http.Client{Timeout: 15 * time.Second}
	if *tlsCert != "" {
		cfg, err := edgetls.ClientTLSConfig(*tlsCert, *tlsKey, *tlsServerCA)
		if err != nil {
			log.Fatalf("edge: mTLS config: %v", err)
		}
		centerClient.Transport = &http.Transport{TLSClientConfig: cfg.Clone(), ForceAttemptHTTP2: true}
		_ = tls.VersionTLS12
	}

	relay := edgegw.NewEdgeRelay(edgegw.EdgeConfig{
		EdgeID:      *edgeID,
		EnrollKey:   *enrollKey,
		InternalKey: *internalKey,
		CenterURL:   *center,
		CenterHTTP:  centerClient,
		Upstream:    upstreamClient,
		MaxFailover: *maxFailover,
	})

	egressLabel := *upstreamProxy
	if egressLabel == "" {
		egressLabel = "direct"
	}
	log.Printf("edge %q (%s): listening on %s, center=%s, egress=%s", *edgeID, Version, *addr, *center, egressLabel)

	srv := &http.Server{Addr: *addr, Handler: relay.Handler(), ReadHeaderTimeout: 15 * time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go relay.RunHeartbeat(ctx, *egressIP, splitCSV(*platforms), *heartbeat)
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("edge: %v", err)
		}
	}()
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Printf("edge %q: shutting down", *edgeID)
	cancel()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("edge: shutdown error: %v", err)
	}
}

func loadState(path string) enroll.Enrolled {
	if path == "" {
		path = os.Getenv("EDGE_STATE")
	}
	if path == "" {
		p, err := enroll.DefaultStatePath()
		if err != nil {
			return enroll.Enrolled{}
		}
		path = p
	}
	e, err := enroll.Load(path)
	if err != nil {
		return enroll.Enrolled{}
	}
	return e
}

func heartbeatDefault(st enroll.Enrolled) time.Duration {
	if st.HeartbeatSeconds > 0 {
		return time.Duration(st.HeartbeatSeconds) * time.Second
	}
	return envDuration("EDGE_HEARTBEAT", 10*time.Second)
}

func orDefault(primary, fallback string) string {
	if primary != "" {
		return primary
	}
	return fallback
}

func intOrDefault(primary, fallback int) int {
	if primary > 0 {
		return primary
	}
	return fallback
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

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
