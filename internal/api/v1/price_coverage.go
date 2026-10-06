package v1

import (
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
)

// CachedVWAP is one aggregator-published VWAP read from the Redis
// cache.
type CachedVWAP struct {
	// Value is the decimal-string VWAP.
	Value string
	// Triangulated is true when the provenance marker says the
	// triangulation worker wrote the value; false is a direct VWAP.
	Triangulated bool
	// ObservedAt is when the aggregator observed the value — the end of
	// the closed bucket its window ends at. A freeze keeps a value
	// served long after this, so it is the only honest observed_at.
	ObservedAt time.Time
	// Coverage is how much of its window the value read; nil when
	// unknown (a composite, or no readable coverage stamp).
	Coverage *cachekeys.WindowCoverage
}

// withCoverage stamps a cached VWAP's window coverage onto snap; unknown
// coverage leaves both fields absent.
func withCoverage(snap PriceSnapshot, c *cachekeys.WindowCoverage) PriceSnapshot {
	if c == nil {
		return snap
	}
	truncated := c.Truncated
	snap.Truncated = &truncated
	if truncated {
		from := WireTime(c.CoveredFrom)
		snap.CoveredFrom = &from
	}
	return snap
}
