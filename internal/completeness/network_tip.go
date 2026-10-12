package completeness

import (
	"math"
	"time"
)

// SlowestLedgerClose is the slowest sustained ledger cadence assumed when extrapolating the tip, so dividing elapsed time by it under-counts ledgers actually closed.
const SlowestLedgerClose = 6 * time.Second

// NetworkTipLowerBound is the least ledger the network can have closed by now given lastLedger ingested at ingestedAt, growing with wall-clock time even when ingest stalls; a zero or future ingestedAt yields lastLedger.
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
