package orchestrator

import (
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/baseline"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/freeze"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// newWiredFreezeFixture is newFreezeFixture with the production writer — a
// real freeze.Writer with a ladder store on the fixture's miniredis — so
// marker and ladder expiry are real. Drive it with advance, not tick.
func newWiredFreezeFixture(t *testing.T) *freezeFixture {
	t.Helper()
	f := newFreezeFixture(t)
	w, err := freeze.NewWriter(f.rdb, 0,
		freeze.WithLadderStore(newSinkShapedLadderStore(), 0),
		freeze.WithClock(func() time.Time { return f.now }))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	f.orch = New(f.store, f.rdb, Config{
		Pairs:        []canonical.Pair{f.pair},
		Windows:      []time.Duration{freezeTestWindow},
		Interval:     time.Hour,
		FreezeWriter: w,
		Baselines: stubBaselineSource{multi: baseline.MultiBaseline{
			Day30: &baseline.Baseline{Median: 0, MAD: 0.001, N: maxDay30Returns},
		}},
	})
	f.orch.clock = func() time.Time { return f.now }
	f.orch.prevVWAPs[f.stateKey()] = big.NewRat(lkgQuoteAmount, lkgBaseAmount)
	// Outlive the first advance; from then on only the freeze keeps it.
	f.mr.SetTTL(f.vwapKey(), 2*closedBucket)
	return f
}

// advance moves both the fixture clock and miniredis's TTL clock by d, then
// runs one Tick.
func (f *freezeFixture) advance(t *testing.T, d time.Duration) {
	t.Helper()
	f.mr.FastForward(d)
	f.tick(t, d)
}
