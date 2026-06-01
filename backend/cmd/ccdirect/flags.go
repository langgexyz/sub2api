package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/url"
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
	statePath       string        // session file override (default: per-user config dir)
	upgradeInterval time.Duration // daemon self-update check cadence (0 = disabled)
}

func parseFlags(args []string) edgeFlags {
	fs := flag.NewFlagSet("edge", flag.ExitOnError)
	addr := fs.String("addr", env("CCDIRECT_ADDR", ":8088"), "listen address for client traffic [CCDIRECT_ADDR]")
	center := fs.String("center", env("CCDIRECT_CCHUB_URL", "http://localhost:8080/edge"), "center edge base URL, ending in /edge [CCDIRECT_CCHUB_URL]")
	platforms := fs.String("platforms", env("CCDIRECT_PLATFORMS", ""), "comma-separated platforms to advertise [CCDIRECT_PLATFORMS]")
	egressIP := fs.String("egress-ip", env("CCDIRECT_EGRESS_IP", ""), "stable egress IP reported to center (auto-detected if empty) [CCDIRECT_EGRESS_IP]")
	maxFailover := fs.Int("max-failover", envInt("CCDIRECT_MAX_FAILOVER", 3), "max candidates to try locally [CCDIRECT_MAX_FAILOVER]")
	upstreamProxy := fs.String("upstream-proxy", env("CCDIRECT_UPSTREAM_PROXY", ""), "egress proxy: http/https/socks5 (empty=direct) [CCDIRECT_UPSTREAM_PROXY]")
	upstreamTimeout := fs.Duration("upstream-timeout", envDuration("CCDIRECT_UPSTREAM_TIMEOUT", 5*time.Minute), "upstream timeout [CCDIRECT_UPSTREAM_TIMEOUT]")
	heartbeat := fs.Duration("heartbeat", envDuration("CCDIRECT_HEARTBEAT", 10*time.Second), "heartbeat interval [CCDIRECT_HEARTBEAT]")
	internalKey := fs.String("internal-key", env("CCDIRECT_INTERNAL_KEY", ""), "shared secret enabling /internal/egress (empty = disabled) [CCDIRECT_INTERNAL_KEY]")
	statePath := fs.String("session", env("CCDIRECT_SESSION", ""), "session file path (default: per-user config dir) [CCDIRECT_SESSION]")
	upgradeInterval := fs.Duration("upgrade-interval", envDuration("CCDIRECT_UPGRADE_INTERVAL", 6*time.Hour), "daemon self-update check cadence, 0 to disable [CCDIRECT_UPGRADE_INTERVAL]")
	_ = fs.Parse(args)

	// Enforce HTTPS to the center: ccdirect↔cchub carries owner tokens, sealed
	// lease tokens and usage — it must not run in cleartext. A loopback center
	// (local dev / a local cchub) is exempt; CCDIRECT_INSECURE=1 is an explicit
	// escape hatch for non-loopback plaintext (testing only).
	if err := requireSecureCenter(*center, os.Getenv("CCDIRECT_INSECURE") == "1"); err != nil {
		log.Fatalf("edge: %v", err)
	}

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
		upgradeInterval: *upgradeInterval,
	}
}

// requireSecureCenter rejects a non-HTTPS center URL unless it targets loopback
// (127.0.0.1/::1/localhost) or insecure is explicitly allowed. Returns an error
// describing why; nil means the center URL is acceptable.
func requireSecureCenter(centerURL string, allowInsecure bool) error {
	u, err := url.Parse(centerURL)
	if err != nil {
		return fmt.Errorf("invalid center URL %q: %w", centerURL, err)
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme != "http" {
		return fmt.Errorf("center URL must be http(s): got scheme %q in %q", u.Scheme, centerURL)
	}
	// scheme == http from here.
	if isLoopbackHost(u.Hostname()) {
		return nil // local dev / local cchub
	}
	if allowInsecure {
		log.Printf("edge: WARNING: using plaintext HTTP to a non-loopback center (%s) — CCDIRECT_INSECURE=1 set; do NOT use in production", centerURL)
		return nil
	}
	return fmt.Errorf("center URL must use HTTPS for a non-loopback host: %q (set CCDIRECT_INSECURE=1 to override for testing)", centerURL)
}

// isLoopbackHost reports whether host is a loopback address or "localhost".
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
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
