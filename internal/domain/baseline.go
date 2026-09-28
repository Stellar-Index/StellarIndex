package domain

import (
	"math/big"
	"time"
)

// BaselineTimedVWAP is one bucketed VWAP with its window-end
// timestamp, as read from the prices_1m served tier for the
// volatility-baseline refresher. Canonical home of
// internal/aggregate/baseline.TimedVWAP — see doc.go.
type BaselineTimedVWAP struct {
	VWAP      float64
	BucketEnd time.Time
	// USDVolume is the minute's summed trades.usd_volume across both stored
	// directions; nil when no trade in the minute carried a USD valuation.
	USDVolume *big.Rat
}
