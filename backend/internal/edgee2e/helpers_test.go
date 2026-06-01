//go:build unit

package edgee2e

import "time"

// fixedClock returns a deterministic clock for the cross-plane tests. It returns
// an unnamed func() time.Time so it is assignable to both cchub.Clock and
// ccdirect.Clock (cchub's coordinator_test has its own copy; this package is
// external so it can't reuse that unexported helper).
func fixedClock() func() time.Time {
	t := time.Unix(1_700_000_000, 0)
	return func() time.Time { return t }
}
