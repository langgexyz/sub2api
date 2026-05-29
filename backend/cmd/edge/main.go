// Command edge is a standalone distributed-edge data-plane CLI. It is meant to
// run on a VPS with a stable egress IP (optionally via a local proxy): it
// accepts client prompts, leases an account from the center, performs the
// upstream request itself through its egress proxy/IP, streams the response
// back, and reports usage via Settle. See docs/tech/distributed-edge.md.
//
// Every flag can also be set via an environment variable (shown in -help), so
// the edge deploys cleanly as a 12-factor process on each VPS.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/edgegw"
)

// Version is set via -ldflags "-X main.Version=...".
var Version = "dev"

func main() {
	addr := flag.String("addr", env("EDGE_ADDR", ":8088"), "listen address for client traffic [EDGE_ADDR]")
	center := flag.String("center", env("EDGE_CENTER_URL", "http://localhost:9000"), "center (control plane) base URL [EDGE_CENTER_URL]")
	edgeID := flag.String("edge-id", env("EDGE_ID", "edge-local"), "this edge node's identifier [EDGE_ID]")
	maxFailover := flag.Int("max-failover", envInt("EDGE_MAX_FAILOVER", 3), "max candidates to try locally before giving up [EDGE_MAX_FAILOVER]")
	upstreamProxy := flag.String("upstream-proxy", env("EDGE_UPSTREAM_PROXY", ""), "egress proxy for upstream calls: http://, https://, socks5:// (empty = direct) [EDGE_UPSTREAM_PROXY]")
	upstreamTimeout := flag.Duration("upstream-timeout", envDuration("EDGE_UPSTREAM_TIMEOUT", 5*time.Minute), "upstream request timeout [EDGE_UPSTREAM_TIMEOUT]")
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

	relay := edgegw.NewEdgeRelay(edgegw.EdgeConfig{
		EdgeID:      *edgeID,
		CenterURL:   *center,
		Upstream:    upstreamClient,
		MaxFailover: *maxFailover,
	})

	egress := *upstreamProxy
	if egress == "" {
		egress = "direct"
	}
	log.Printf("edge %q (%s): listening on %s, center=%s, egress=%s", *edgeID, Version, *addr, *center, egress)

	srv := &http.Server{
		Addr:    *addr,
		Handler: relay.Handler(),
		// No write timeout: streaming responses are long-lived.
		ReadHeaderTimeout: 15 * time.Second,
	}

	// Graceful shutdown.
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("edge: %v", err)
		}
	}()
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Printf("edge %q: shutting down", *edgeID)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("edge: shutdown error: %v", err)
	}
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
