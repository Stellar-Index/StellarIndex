package domain

import (
	"math/big"
	"time"
)

// BaselineTimedVWAP is one prices_1m VWAP bucket read by the volatility-baseline
// refresher (origin: internal/aggregate/baseline.TimedVWAP).
type BaselineTimedVWAP struct {
	VWAP      float64
	BucketEnd time.Time
	// USDVolume is the minute's summed trades.usd_volume across both stored
	// directions; nil when no trade in the minute carried a USD valuation.
	USDVolume *big.Rat
}
