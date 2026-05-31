package edgegw

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"github.com/Wei-Shaw/sub2api/internal/edgegw/contract"
)

// anomalyReporter aggregates operational anomalies by kind so ccdirect can ship
// a single batched report to cchub on an interval instead of one HTTP call per
// event. Because the data plane never transits cchub, this (plus heartbeats) is
// cchub's only fleet service-quality signal: lease failures, upstream error
// spikes, heartbeat loss, liveness drains and recovered panics all surface here.
type anomalyReporter struct {
	now Clock

	mu    sync.Mutex
	items map[string]*contract.ReportItem
}

func newAnomalyReporter(now Clock) *anomalyReporter {
	return &anomalyReporter{now: now, items: make(map[string]*contract.ReportItem)}
}

// record buckets one anomaly under its kind, counting occurrences and keeping
// the first/last timestamps and the latest sample message for the window.
func (a *anomalyReporter) record(kind, message string) {
	if a == nil {
		return
	}
	ts := a.now().Unix()
	a.mu.Lock()
	defer a.mu.Unlock()
	if it, ok := a.items[kind]; ok {
		it.Count++
		it.Message = message
		it.LastAt = ts
		return
	}
	a.items[kind] = &contract.ReportItem{
		Kind:    kind,
		Message: message,
		Count:   1,
		FirstAt: ts,
		LastAt:  ts,
	}
}

// drain returns the buffered items and resets the buffer. Returns nil when empty.
func (a *anomalyReporter) drain() []contract.ReportItem {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.items) == 0 {
		return nil
	}
	out := make([]contract.ReportItem, 0, len(a.items))
	for _, it := range a.items {
		out = append(out, *it)
	}
	a.items = make(map[string]*contract.ReportItem)
	return out
}

// reportAnomaly records one anomaly for the next batched report to cchub.
func (e *EdgeRelay) reportAnomaly(kind, message string) {
	e.reporter.record(kind, message)
}

// reportPanic records a recovered panic; kept separate so the panic-recovery
// defer in relay() needs no fmt import of its own.
func (e *EdgeRelay) reportPanic(rec any) {
	e.reporter.record("panic_recovered", fmt.Sprintf("%v", rec))
}

// flushReport ships accumulated anomalies to cchub's /edge/v1/report and clears
// the buffer. No-op when nothing is buffered or the edge is logged out (no edge
// id yet). Best-effort: a failed send drops this window rather than blocking the
// heartbeat loop. centerURL already includes the /edge prefix (see Register).
func (e *EdgeRelay) flushReport(ctx context.Context) {
	items := e.reporter.drain()
	if len(items) == 0 {
		return
	}
	edgeID := e.EdgeID()
	if edgeID == "" {
		return
	}
	body, _ := json.Marshal(contract.ErrorReport{EdgeID: edgeID, Items: items})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.centerURL+"/v1/report", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.centerHTTP.Do(req)
	if err != nil {
		return
	}
	_ = resp.Body.Close()
}
