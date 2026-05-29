// Command edge is a standalone distributed-edge data-plane CLI. It runs on a
// VPS with a stable egress IP (optionally via a local proxy): it registers with
// the center, accepts client prompts on the sub2api gateway surface, leases an
// account from the center, performs the upstream request itself through its
// egress proxy/IP, streams the response back, and reports usage via Settle.
// See docs/tech/distributed-edge.md.
//
// Onboarding: a user installs an edge, obtains an enroll key from the center's
// web login, and passes it via -enroll-key (or EDGE_ENROLL_KEY). The edge then
// registers and heartbeats automatically.
//
// Every flag can also be set via an environment variable (shown in -help).
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/edgegw"
	"github.com/Wei-Shaw/sub2api/internal/edgegw/edgetls"
)

// Version is set via -ldflags "-X main.Version=...".
var Version = "dev"

func main() {
	addr := flag.String("addr", env("EDGE_ADDR", ":8088"), "listen address for client traffic [EDGE_ADDR]")
	center := flag.String("center", env("EDGE_CENTER_URL", "http://localhost:9000"), "center base URL [EDGE_CENTER_URL]")
	edgeID := flag.String("edge-id", env("EDGE_ID", "edge-local"), "this edge node's identifier [EDGE_ID]")
	enrollKey := flag.String("enroll-key", env("EDGE_ENROLL_KEY", ""), "enroll key obtained from the center login [EDGE_ENROLL_KEY]")
	egressIP := flag.String("egress-ip", env("EDGE_EGRESS_IP", ""), "this edge's stable egress IP (reported to center) [EDGE_EGRESS_IP]")
	platforms := flag.String("platforms", env("EDGE_PLATFORMS", ""), "comma-separated platforms this edge serves [EDGE_PLATFORMS]")
	maxFailover := flag.Int("max-failover", envInt("EDGE_MAX_FAILOVER", 3), "max candidates to try locally [EDGE_MAX_FAILOVER]")
	upstreamProxy := flag.String("upstream-proxy", env("EDGE_UPSTREAM_PROXY", ""), "egress proxy: http/https/socks5 (empty = direct) [EDGE_UPSTREAM_PROXY]")
	upstreamTimeout := flag.Duration("upstream-timeout", envDuration("EDGE_UPSTREAM_TIMEOUT", 5*time.Minute), "upstream request timeout [EDGE_UPSTREAM_TIMEOUT]")
	heartbeat := flag.Duration("heartbeat", envDuration("EDGE_HEARTBEAT", 10*time.Second), "heartbeat interval to center [EDGE_HEARTBEAT]")
	tlsCert := flag.String("tls-cert", env("EDGE_TLS_CERT", ""), "client cert PEM for mTLS to center [EDGE_TLS_CERT]")
	tlsKey := flag.String("tls-key", env("EDGE_TLS_KEY", ""), "client key PEM for mTLS to center [EDGE_TLS_KEY]")
	tlsServerCA := flag.String("tls-server-ca", env("EDGE_TLS_SERVER_CA", ""), "CA PEM that signs the center cert [EDGE_TLS_SERVER_CA]")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("edge %s\n", Version)
		return
	}

	upstreamClient, err := edgegw.NewUpstreamClient(*upstreamProxy, *upstreamTimeout)
	if err != nil {
		log.Fatalf("edge: %v", err)
	}

	// Center HTTP client: mTLS when certs are configured.
	centerClient := &http.Client{Timeout: 15 * time.Second}
	if *tlsCert != "" {
		cfg, err := edgetls.ClientTLSConfig(*tlsCert, *tlsKey, *tlsServerCA)
		if err != nil {
			log.Fatalf("edge: mTLS config: %v", err)
		}
		centerClient.Transport = &http.Transport{TLSClientConfig: cfg.Clone(), ForceAttemptHTTP2: true}
		_ = tls.VersionTLS12 // mTLS min version is enforced inside edgetls
	}

	relay := edgegw.NewEdgeRelay(edgegw.EdgeConfig{
		EdgeID:      *edgeID,
		CenterURL:   *center,
		CenterHTTP:  centerClient,
		Upstream:    upstreamClient,
		MaxFailover: *maxFailover,
		EnrollKey:   *enrollKey,
	})

	egressLabel := *upstreamProxy
	if egressLabel == "" {
		egressLabel = "direct"
	}
	log.Printf("edge %q (%s): listening on %s, center=%s, egress=%s", *edgeID, Version, *addr, *center, egressLabel)

	srv := &http.Server{
		Addr:              *addr,
		Handler:           relay.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Register + heartbeat with the center.
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
