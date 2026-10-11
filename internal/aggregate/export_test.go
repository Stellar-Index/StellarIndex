package aggregate

import (
	"math/big"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// VWAPOf prices a raw fixture slice without lifting it, for tests whose trades share one scale.
func VWAPOf(trades []canonical.Trade) (*big.Rat, error) {
	return VWAP(ScaledWindow{trades: trades})
}

// RejectAggregatorOutliers exposes the oracle-aggregator band to external tests; it exists only in test binaries.
var RejectAggregatorOutliers = rejectAggregatorOutliers
