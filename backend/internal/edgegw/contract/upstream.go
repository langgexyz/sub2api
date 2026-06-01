package contract

import "encoding/base64"

// UpstreamBearer extracts the real upstream bearer from a minted lease token. It
// is what the edge (ccdirect) sends upstream. cchub mints the token; ccdirect
// extracts the bearer, so this codec is shared and lives in contract. (In a
// stricter build the edge would treat the whole envelope as opaque; here the
// bearer is exposed so the demo edge can set a normal Authorization header.)
func UpstreamBearer(minted string) string {
	dot := -1
	for i := 0; i < len(minted); i++ {
		if minted[i] == '.' {
			dot = i
			break
		}
	}
	if dot < 0 {
		return minted
	}
	raw, err := base64.RawURLEncoding.DecodeString(minted[:dot])
	if err != nil {
		return minted
	}
	// payload = accountID|exp|upstreamToken
	parts := splitN(string(raw), '|', 3)
	if len(parts) == 3 {
		return parts[2]
	}
	return minted
}

func splitN(s string, sep byte, n int) []string {
	out := make([]string, 0, n)
	start := 0
	for i := 0; i < len(s) && len(out) < n-1; i++ {
		if s[i] == sep {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out
}
