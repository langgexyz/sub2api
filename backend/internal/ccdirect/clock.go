package ccdirect

import "time"

// Clock returns the current time. Injected in tests; defaults to time.Now in the
// relay. ccdirect keeps its own (the edge and center planes never pass clocks
// across the wire), mirroring edgereg.Clock.
type Clock func() time.Time
