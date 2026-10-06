package aggregate

import (
	"math/big"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// VWAPOf prices a raw fixture slice without lifting it, for tests whose
// trades share one scale or that pin what an unlifted sum would serve.
func VWAPOf(trades []canonical.Trade) (*big.Rat, error) {
	return VWAP(ScaledWindow{trades: trades})
}

// RejectAggregatorOutliers exposes the oracle-aggregator band to the external
// symmetry table in band_symmetry_test.go. It exists only in test binaries.
var RejectAggregatorOutliers = rejectAggregatorOutliers
