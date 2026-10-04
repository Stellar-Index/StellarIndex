package completeness_test

import (
	"math"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/completeness"
)

func TestNetworkTipLowerBound(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name       string
		last       uint32
		ingestedAt time.Time
		want       uint32
	}{
		{"just ingested", 63_000_000, now, 63_000_000},
		{"under one slow close", 63_000_000, now.Add(-5 * time.Second), 63_000_000},
		{"one hour frozen", 63_000_000, now.Add(-time.Hour), 63_000_600},
		{"three days frozen", 63_000_000, now.Add(-72 * time.Hour), 63_043_200},
		{"unknown ingest time", 63_000_000, time.Time{}, 63_000_000},
		{"ingest time in the future (clock skew)", 63_000_000, now.Add(time.Minute), 63_000_000},
		{"saturates instead of wrapping", math.MaxUint32 - 10, now.Add(-time.Hour), math.MaxUint32},
	} {
		if got := completeness.NetworkTipLowerBound(tc.last, tc.ingestedAt, now); got != tc.want {
			t.Errorf("%s: NetworkTipLowerBound = %d, want %d", tc.name, got, tc.want)
		}
	}
}
