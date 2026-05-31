package contract

// Operational anomaly reporting, ccdirect -> cchub. ccdirect aggregates
// anomalies (lease failures, upstream errors, heartbeat loss, recovered panics)
// by kind and ships a batched report on an interval so cchub has service-quality
// visibility into the fleet without the data-plane request ever transiting it.

// ReportItem is one aggregated anomaly kind over a reporting window.
type ReportItem struct {
	Kind    string `json:"kind"`    // stable category, e.g. "lease_unavailable"
	Message string `json:"message"` // latest human-readable sample for this kind
	Count   int    `json:"count"`   // occurrences in this window
	FirstAt int64  `json:"first_at"`
	LastAt  int64  `json:"last_at"`
}

// ErrorReport is a batched anomaly report from one ccdirect node.
type ErrorReport struct {
	EdgeID string       `json:"edge_id"`
	Items  []ReportItem `json:"items"`
}

// ReportResponse acknowledges an anomaly report.
type ReportResponse struct {
	OK       bool `json:"ok"`
	Accepted int  `json:"accepted"`
}
