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
	"time"

	"github.com/Wei-Shaw/sub2api/internal/edgegw"
)

func main() {
	addr := flag.String("addr", ":9000", "listen address")
	accountsPath := flag.String("accounts", "accounts.json", "path to accounts JSON file")
	maxPerKey := flag.Int("max-per-key", 0, "max concurrent in-flight leases per api key (0 = unlimited)")
	leaseTTL := flag.Duration("lease-ttl", 2*time.Minute, "lease token TTL")
	secret := flag.String("token-secret", "dev-secret-change-me", "HMAC secret for minting lease tokens")
	flag.Parse()

	accounts, err := loadAccounts(*accountsPath)
	if err != nil {
		log.Fatalf("load accounts: %v", err)
	}
	if len(accounts) == 0 {
		log.Fatalf("no accounts configured in %s", *accountsPath)
	}

	registry := edgegw.NewMemRegistry(accounts)
	coord := edgegw.NewCoordinator(edgegw.Config{
		Admission: edgegw.NewMemAdmission(*maxPerKey),
		Scheduler: registry,
		Sticky:    edgegw.NewMemSticky(),
		Usage:     edgegw.NewMemUsageSink(),
		Minter:    edgegw.NewHMACMinter(registry, []byte(*secret), time.Now),
		LeaseTTL:  *leaseTTL,
	})
	server := edgegw.NewCenterServer(coord, registry)

	log.Printf("center: listening on %s with %d account(s), max-per-key=%d", *addr, len(accounts), *maxPerKey)
	srv := &http.Server{
		Addr:              *addr,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("center: %v", err)
	}
}

func loadAccounts(path string) ([]edgegw.AccountConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var accounts []edgegw.AccountConfig
	if err := json.Unmarshal(data, &accounts); err != nil {
		return nil, err
	}
	return accounts, nil
}
