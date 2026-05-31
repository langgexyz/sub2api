package edgegw

import "github.com/Wei-Shaw/sub2api/internal/edgegw/contract"

// Sealed lease tokens moved to the shared contract package (both ccdirect and
// cchub need them: cchub seals, ccdirect opens). These aliases keep existing
// edgegw.SealLeaseToken / edgegw.OpenLeaseToken call sites working during the
// ccdirect/cchub split; call sites migrate to contract.* incrementally.
var (
	SealLeaseToken = contract.SealLeaseToken
	OpenLeaseToken = contract.OpenLeaseToken
)
