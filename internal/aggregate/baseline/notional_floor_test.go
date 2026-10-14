package baseline_test

import (
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/baseline"
)

// productionFloor is the floor the aggregator wires on defaults:
// min_usd_volume 10,000 over the 24h window.
func productionFloor() *big.Rat { return baseline.MinuteNotionalFloor(10_000, 24*time.Hour) }

// withUSD re-stamps a series' per-minute USD notional.
func withUSD(in []baseline.TimedVWAP, usd *big.Rat) []baseline.TimedVWAP {
	out := make([]baseline.TimedVWAP, len(in))
	for i := range in {
		out[i] = in[i]
		out[i].USDVolume = usd
	}
	return out
}

func TestMinuteNotionalFloor(t *testing.T) {
	if got, want := productionFloor(), big.NewRat(10_000, 1440); got.Cmp(want) != 0 {
		t.Errorf("floor(10000, 24h) = %s, want %s", got.RatString(), want.RatString())
	}
	if got := baseline.MinuteNotionalFloor(0, 24*time.Hour); got.Sign() != 0 {
		t.Errorf("floor(0, 24h) = %s, want 0 (publish floor disabled)", got.RatString())
	}
	if got, want := baseline.MinuteNotionalFloor(10_000, 5*time.Minute), big.NewRat(2_000, 1); got.Cmp(want) != 0 {
		t.Errorf("floor(10000, 5m) = %s, want %s", got.RatString(), want.RatString())
	}
}
