package orchestrator

import (
	"math/big"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/baseline"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/freeze"
	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
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

// TestFreezeLifecycle_UnpricedBucketsKeepTheFreezeAlive — a frozen window
// whose buckets go empty or fall under MinUSDVolume must keep advancing its
// lifecycle. Those buckets used to return before the lifecycle step, so
// nothing refreshed the marker or the durable ladder; hold + grace later both
// had lapsed, the next priced bucket read the absence as the operator
// override, released with mode="operator" and published the manipulated
// print the freeze was withholding.
func TestFreezeLifecycle_UnpricedBucketsKeepTheFreezeAlive(t *testing.T) {
	cases := []struct {
		name         string
		thin, priced func(t *testing.T, f *freezeFixture)
	}{
		{
			name:   "empty",
			thin:   func(_ *testing.T, f *freezeFixture) { f.store.trades = nil },
			priced: func(t *testing.T, f *freezeFixture) { f.feed(t, manipQuoteAmount, "soroswap") },
		},
		{
			name:   "below_min_usd_volume",
			thin:   func(_ *testing.T, f *freezeFixture) { f.orch.cfg.MinUSDVolume = 1e9 },
			priced: func(_ *testing.T, f *freezeFixture) { f.orch.cfg.MinUSDVolume = 0 },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newWiredFreezeFixture(t)
			operator := obs.AnomalyFreezeReleasedTotal.WithLabelValues("operator")
			before := testutil.ToFloat64(operator)

			f.feed(t, manipQuoteAmount, "soroswap")
			f.advance(t, closedBucket)
			if !f.state().Active() {
				t.Fatal("setup: freeze did not fire")
			}

			// Twenty unpriced buckets: well past the 10-minute hold plus the
			// 5-minute marker and ladder grace.
			tc.thin(t, f)
			for range 20 {
				f.advance(t, closedBucket)
			}
			if !f.mr.Exists(cachekeys.Freeze(f.pair.Base, f.pair.Quote).String()) {
				t.Error("freeze marker lapsed during unpriced buckets: flags.frozen is off for a live freeze")
			}

			tc.priced(t, f)
			f.advance(t, closedBucket)
			st := f.state()
			if !st.Active() {
				t.Fatalf("freeze ended after unpriced buckets: %+v", st)
			}
			if st.ExtensionsUsed != 0 {
				t.Errorf("ExtensionsUsed = %d; unscored buckets must slide the hold, not climb the ladder", st.ExtensionsUsed)
			}
			if got := f.served(t); got != lkgFormatted {
				t.Errorf("served %q, want the held LKG %q", got, lkgFormatted)
			}
			if d := testutil.ToFloat64(operator) - before; d != 0 {
				t.Errorf("AnomalyFreezeReleasedTotal{operator} delta = %v with no operator action", d)
			}
		})
	}
}
