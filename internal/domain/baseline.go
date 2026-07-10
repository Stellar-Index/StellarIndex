package domain

import "time"

// BaselineTimedVWAP is one bucketed VWAP with its window-end
// timestamp, as read from the prices_1m served tier for the
// volatility-baseline refresher. Canonical home of
// internal/aggregate/baseline.TimedVWAP — see doc.go.
type BaselineTimedVWAP struct {
	VWAP      float64
	BucketEnd time.Time
}
