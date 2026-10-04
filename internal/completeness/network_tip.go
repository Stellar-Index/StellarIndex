package completeness

import (
	"math"
	"time"
)

// SlowestLedgerClose is the slowest sustained network ledger cadence assumed
// when extrapolating the tip: pubnet averages ~5.6 s and Protocol 28 pins 5 s,
// so dividing elapsed time by it under-counts the ledgers actually closed.
const SlowestLedgerClose = 6 * time.Second

// NetworkTipLowerBound is the least ledger the network can have closed by now,
// given that lastLedger was ingested at ingestedAt. A ledger is ingested only
// after it closes, so at least elapsed/[SlowestLedgerClose] more have closed
// since; the bound grows with wall-clock time, which a stalled ingest frontier
// cannot hold still. A zero or future ingestedAt yields lastLedger.
func NetworkTipLowerBound(lastLedger uint32, ingestedAt, now time.Time) uint32 {
	if ingestedAt.IsZero() || !now.After(ingestedAt) {
		return lastLedger
	}
	closed := uint64(now.Sub(ingestedAt) / SlowestLedgerClose)
	if closed > uint64(math.MaxUint32-lastLedger) {
		return math.MaxUint32
	}
	return lastLedger + uint32(closed)
}
