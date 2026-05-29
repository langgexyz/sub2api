// Command mockupstream is a stand-in AI provider for the distributed-edge demo.
// It accepts any bearer token, echoes the (mapped) model back, emits usage via
// response headers, and supports both JSON and SSE streaming responses so the
// edge can be exercised end to end without real provider credentials.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

func main() {
	addr := flag.String("addr", ":9100", "listen address")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/", handle)
	log.Printf("mockupstream: listening on %s", *addr)
	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 15 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("mockupstream: %v", err)
	}
}

func handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var parsed struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	_ = json.Unmarshal(body, &parsed)
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	log.Printf("mockupstream: %s model=%s stream=%v bearer=%q edge=%s",
		r.URL.Path, parsed.Model, parsed.Stream, bearer, r.Header.Get("X-Edge-Id"))

	w.Header().Set("X-Usage-Input-Tokens", "11")
	w.Header().Set("X-Usage-Output-Tokens", "22")

	if parsed.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		for i := 0; i < 3; i++ {
			_, _ = fmt.Fprintf(w, "data: {\"chunk\":%d,\"model\":%q}\n\n", i, parsed.Model)
			if fl != nil {
				fl.Flush()
			}
			time.Sleep(50 * time.Millisecond)
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, `{"model":%q,"content":"echo from mock upstream"}`, parsed.Model)
}
