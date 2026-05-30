package main

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

// ownerClaims is the subset of the sub2api JWT payload the edge displays. The
// edge never VERIFIES this token (only the center does) — it decodes the payload
// purely to show "logged in as <email>" and the expiry in /status. Trusting it
// for display is fine; it is never used for an authorization decision.
type ownerClaims struct {
	UserID int64  `json:"user_id"`
	Email  string `json:"email"`
	Exp    int64  `json:"exp"`
}

func (c ownerClaims) expiresAt() time.Time { return time.Unix(c.Exp, 0) }

// parseJWTUnverified decodes the payload of a JWT without verifying its
// signature. For display only.
func parseJWTUnverified(token string) (ownerClaims, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ownerClaims{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		payload, err = base64.StdEncoding.DecodeString(parts[1])
		if err != nil {
			return ownerClaims{}, false
		}
	}
	var c ownerClaims
	if err := json.Unmarshal(payload, &c); err != nil {
		return ownerClaims{}, false
	}
	return c, true
}
