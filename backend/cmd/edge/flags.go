package main

import (
	"flag"
	"os"
	"strconv"
	"time"
)

// edgeFlags holds the edge's startup configuration. Credentials are NOT here —
// they come from the device-login flow and the saved session. These are just
// the operational knobs (listen address, center URL, egress).
type edgeFlags struct {
	addr            string
	center          string // center edge control-plane base, e.g. http://host:8080/edge
	platforms       string
	egressIP        string
	maxFailover     int
	upstreamProxy   string
	upstreamTimeout time.Duration
	heartbeat       time.Duration
	internalKey     string
	statePath       string // session file override (default: per-user config dir)
}

func parseFlags(args []string) edgeFlags {
	fs := flag.NewFlagSet("edge", flag.ExitOnError)
	addr := fs.String("addr", env("EDGE_ADDR", ":8088"), "listen address for client traffic [EDGE_ADDR]")
	center := fs.String("center", env("EDGE_CENTER_URL", "http://localhost:8080/edge"), "center edge base URL, ending in /edge [EDGE_CENTER_URL]")
	platforms := fs.String("platforms", env("EDGE_PLATFORMS", ""), "comma-separated platforms to advertise [EDGE_PLATFORMS]")
	egressIP := fs.String("egress-ip", env("EDGE_EGRESS_IP", ""), "stable egress IP reported to center (auto-detected if empty) [EDGE_EGRESS_IP]")
	maxFailover := fs.Int("max-failover", envInt("EDGE_MAX_FAILOVER", 3), "max candidates to try locally [EDGE_MAX_FAILOVER]")
	upstreamProxy := fs.String("upstream-proxy", env("EDGE_UPSTREAM_PROXY", ""), "egress proxy: http/https/socks5 (empty=direct) [EDGE_UPSTREAM_PROXY]")
	upstreamTimeout := fs.Duration("upstream-timeout", envDuration("EDGE_UPSTREAM_TIMEOUT", 5*time.Minute), "upstream timeout [EDGE_UPSTREAM_TIMEOUT]")
	heartbeat := fs.Duration("heartbeat", envDuration("EDGE_HEARTBEAT", 10*time.Second), "heartbeat interval [EDGE_HEARTBEAT]")
	internalKey := fs.String("internal-key", env("EDGE_INTERNAL_KEY", ""), "shared secret enabling /internal/egress (empty = disabled) [EDGE_INTERNAL_KEY]")
	statePath := fs.String("session", env("EDGE_SESSION", ""), "session file path (default: per-user config dir) [EDGE_SESSION]")
	_ = fs.Parse(args)

	return edgeFlags{
		addr:            *addr,
		center:          *center,
		platforms:       *platforms,
		egressIP:        *egressIP,
		maxFailover:     *maxFailover,
		upstreamProxy:   *upstreamProxy,
		upstreamTimeout: *upstreamTimeout,
		heartbeat:       *heartbeat,
		internalKey:     *internalKey,
		statePath:       *statePath,
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
		if n, err := strconv.Atoi(v); err == nil {
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
