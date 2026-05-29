// Command edge runs a distributed-edge data-plane node. It is meant to run on a
// VPS with a stable egress IP: it accepts client prompts, leases an account
// from the center, performs the upstream request itself (from this node's IP),
// streams the response back, and reports usage via Settle.
// See docs/tech/distributed-edge.md.
package main

import (
	"flag"
	"log"
	"net/http"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/edgegw"
)

func main() {
	addr := flag.String("addr", ":8088", "listen address for client traffic")
	center := flag.String("center", "http://localhost:9000", "center (control plane) base URL")
	edgeID := flag.String("edge-id", "edge-local", "this edge node's identifier")
	maxFailover := flag.Int("max-failover", 3, "max candidates to try locally before giving up")
	flag.Parse()

	relay := edgegw.NewEdgeRelay(edgegw.EdgeConfig{
		EdgeID:      *edgeID,
		CenterURL:   *center,
		MaxFailover: *maxFailover,
	})

	log.Printf("edge %q: listening on %s, center=%s", *edgeID, *addr, *center)
	srv := &http.Server{
		Addr:    *addr,
		Handler: relay.Handler(),
		// No write timeout: streaming responses can be long-lived.
		ReadHeaderTimeout: 15 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("edge: %v", err)
	}
}
