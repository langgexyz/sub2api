// Command center runs the distributed-edge control plane: account registry,
// admission, scheduling, sticky sessions, usage ledger, exposed as POST
// /v1/lease and POST /v1/settle. See docs/tech/distributed-edge.md.
//
// This is a runnable proof-of-concept backed by in-memory implementations; it
// shares the edgegw contract with the production gateway, which will back the
// same interfaces with the real GatewayService / BillingCacheService.
package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/cchub"
	"github.com/Wei-Shaw/sub2api/internal/edgegw/edgereg"
	"github.com/Wei-Shaw/sub2api/internal/edgegw/edgetls"
)

func main() {
	addr := flag.String("addr", ":9000", "listen address")
	accountsPath := flag.String("accounts", "accounts.json", "path to accounts JSON file")
	maxPerKey := flag.Int("max-per-key", 0, "max concurrent in-flight leases per api key (0 = unlimited)")
	leaseTTL := flag.Duration("lease-ttl", 2*time.Minute, "lease token TTL")
	secret := flag.String("token-secret", "dev-secret-change-me", "HMAC secret for minting lease tokens")
	edgeTTL := flag.Duration("edge-ttl", 30*time.Second, "edge liveness window (no heartbeat within this = not live)")
	tlsCert := flag.String("tls-cert", "", "server cert PEM for mTLS to edges (enables mTLS)")
	tlsKey := flag.String("tls-key", "", "server key PEM for mTLS to edges")
	tlsClientCA := flag.String("tls-client-ca", "", "CA PEM that signs edge client certs (mTLS)")
	enrollKeys := flag.String("enroll-keys", "", "comma-separated valid edge enroll keys (empty = accept any)")
	publicURL := flag.String("public-url", "", "center URL issued to edges at enroll (default: derived from -addr)")
	issuePlatforms := flag.String("issue-platforms", "", "comma-separated platforms issued to enrolling edges")
	issueHeartbeat := flag.Int("issue-heartbeat", 10, "heartbeat seconds issued to enrolling edges")
	issueMaxFailover := flag.Int("issue-max-failover", 3, "max-failover issued to enrolling edges")
	flag.Parse()

	accounts, err := loadAccounts(*accountsPath)
	if err != nil {
		log.Fatalf("load accounts: %v", err)
	}
	if len(accounts) == 0 {
		log.Fatalf("no accounts configured in %s", *accountsPath)
	}

	registry := cchub.NewMemRegistry(accounts)
	edges := edgereg.New(*edgeTTL, time.Now)
	coord := cchub.NewCoordinator(cchub.Config{
		Admission: cchub.NewMemAdmission(*maxPerKey),
		Scheduler: registry,
		Sticky:    cchub.NewMemSticky(),
		Usage:     cchub.NewMemUsageSink(),
		Minter:    cchub.NewHMACMinter(registry, []byte(*secret), time.Now),
		LeaseTTL:  *leaseTTL,
	})
	server := cchub.NewServer(coord, registry, edges)
	if *enrollKeys != "" {
		server.SetEnrollKeys(splitCSV(*enrollKeys))
	}
	issuedURL := *publicURL
	if issuedURL == "" {
		issuedURL = "http://" + *addr
	}
	server.SetEnrollConfig(issuedURL, *issueHeartbeat, *issueMaxFailover, splitCSV(*issuePlatforms))

	srv := &http.Server{
		Addr:              *addr,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
	}

	if *tlsCert != "" {
		tlsCfg, err := edgetls.ServerTLSConfig(*tlsCert, *tlsKey, *tlsClientCA)
		if err != nil {
			log.Fatalf("center: mTLS config: %v", err)
		}
		srv.TLSConfig = tlsCfg
		log.Printf("center: listening on %s (mTLS) with %d account(s), max-per-key=%d", *addr, len(accounts), *maxPerKey)
		if err := srv.ListenAndServeTLS("", ""); err != nil {
			log.Fatalf("center: %v", err)
		}
		return
	}

	log.Printf("center: listening on %s with %d account(s), max-per-key=%d", *addr, len(accounts), *maxPerKey)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("center: %v", err)
	}
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func loadAccounts(path string) ([]cchub.AccountConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var accounts []cchub.AccountConfig
	if err := json.Unmarshal(data, &accounts); err != nil {
		return nil, err
	}
	return accounts, nil
}
