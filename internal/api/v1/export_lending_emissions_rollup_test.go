package v1

import (
	"context"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// EmissionUSDFromRowForTest renders a stored emissions row as /emissions/history serves its day's usd.
func EmissionUSDFromRowForTest(d timescale.LendingEmissionsUSDDay) (LendingEmissionUSD, error) {
	return emissionUSDFromRow(d)
}

// RunCycleForTest is one cycle as Run runs it, gauges included.
func (r *LendingEmissionsRollup) RunCycleForTest(ctx context.Context) { r.rollup().runCycle(ctx) }
