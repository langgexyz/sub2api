package ccdirect

import (
	"encoding/json"
	"net/http"
)

// writeJSON / writeJSONError are the edge relay's HTTP JSON response helpers.
// They are duplicated from cchub's center server (a few trivial lines) rather
// than shared via contract, which is intentionally net/http-free.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{"code": code, "message": message},
	})
}
