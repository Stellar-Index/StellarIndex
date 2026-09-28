package baseline_test

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/baseline"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/confidence"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// productionFloor is the floor the aggregator wires on defaults:
// min_usd_volume 10,000 over the 24h window.
func productionFloor() *big.Rat { return baseline.MinuteNotionalFloor(10_000, 24*time.Hour) }

// withUSD re-stamps a series' per-minute USD notional.
func withUSD(in []baseline.TimedVWAP, usd *big.Rat, notional int64) []baseline.TimedVWAP {
	out := make([]baseline.TimedVWAP, len(in))
	for i := range in {
		out[i] = in[i]
		out[i].USDVolume = usd
		out[i].NotionalTrades = notional
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

// Two accounts cycling 0.01 USDC every minute for the whole 30-day window
// must not author a baseline: nothing is persisted, so the pair stays in
// bootstrap and its confidence stays capped (#1108).
func TestRefresher_PennySelfTradesEveryMinuteStayCapped(t *testing.T) {
	pair := mustPair(t, "native", "fiat:USD")
	now := time.Now().UTC()
	src := newStubSource()
	src.set(pair, withUSD(stableTimedSeries(now, 30*1440), big.NewRat(2, 100), 2))
	sink := newStubSink()

	r := baseline.NewRefresher(src, sink, 30*24*time.Hour, nil).WithMinuteNotionalFloor(productionFloor())
	outcome, err := r.RefreshPair(context.Background(), pair)
	if !errors.Is(err, baseline.ErrBelowNotionalFloor) {
		t.Fatalf("err = %v, want ErrBelowNotionalFloor", err)
	}
	if outcome != baseline.OutcomeBelowNotionalFloor {
		t.Errorf("outcome = %v, want below_notional_floor", outcome)
	}
	if sink.calls != 0 {
		t.Errorf("sink.calls = %d, want 0: a penny-authored baseline was persisted", sink.calls)
	}
}

// The issue's pair: mature, ~200 real minutes a day, with a self-trader
// filling every other minute at a penny. Only the real minutes count, so
// density stays under the bootstrap gate.
func TestRefresher_PennyPaddingDoesNotLiftDensity(t *testing.T) {
	pair := mustPair(t, "native", "fiat:USD")
	now := time.Now().UTC()
	series := withUSD(stableTimedSeries(now, 30*1440), big.NewRat(1, 100), 1)
	realMinutes := 0
	for i := range series {
		if i%1440 < 200 {
			series[i].USDVolume = big.NewRat(50, 1)
			realMinutes++
		}
	}
	src := newStubSource()
	src.set(pair, series)
	sink := newStubSink()

	r := baseline.NewRefresher(src, sink, 30*24*time.Hour, nil).WithMinuteNotionalFloor(productionFloor())
	outcome, err := r.RefreshPair(context.Background(), pair)
	if err != nil || outcome != baseline.OutcomeOK {
		t.Fatalf("RefreshPair = (%v, %v), want (ok, nil)", outcome, err)
	}
	d30 := sink.byPair[pair.String()].Day30
	if d30 == nil {
		t.Fatal("Day30 nil")
	}
	if d30.N != realMinutes-1 {
		t.Errorf("Day30.N = %d, want %d (real minutes only)", d30.N, realMinutes-1)
	}
	// Same measure orchestrator.baselineAgeDays reads.
	if days := float64(d30.N+1) / 1440; days >= confidence.BootstrapDensityDays {
		t.Errorf("density = %.2f days, want < %.1f (bootstrap cap stays engaged)", days, confidence.BootstrapDensityDays)
	}
}

// A pair with no USD-valued minute cannot be measured; it keeps a baseline
// over every minute so the z-score freeze stays live, under its own outcome.
func TestRefresher_UnvaluedPairKeepsGuardUnderDistinctOutcome(t *testing.T) {
	pair := mustPair(t, "native", "fiat:EUR")
	now := time.Now().UTC()
	src := newStubSource()
	src.set(pair, withUSD(stableTimedSeries(now, 2*1440), nil, 0))
	sink := newStubSink()

	r := baseline.NewRefresher(src, sink, 30*24*time.Hour, nil).WithMinuteNotionalFloor(productionFloor())
	outcome, err := r.RefreshPair(context.Background(), pair)
	if err != nil || outcome != baseline.OutcomeOKUnvalued {
		t.Fatalf("RefreshPair = (%v, %v), want (ok_unvalued, nil)", outcome, err)
	}
	if d30 := sink.byPair[pair.String()].Day30; d30 == nil || d30.N != 2*1440-1 {
		t.Errorf("Day30 = %+v, want N=%d from every minute", d30, 2*1440-1)
	}
}

// A valued pair's unpriced minutes prove no notional and are dropped, and a
// pair too thin for any baseline stays not_enough_samples, not below-floor.
func TestRefresher_NotionalOutcomesAreDistinct(t *testing.T) {
	now := time.Now().UTC()
	mixed := mustPair(t, "native", "fiat:USD")
	series := stableTimedSeries(now, 1440)
	for i := range series {
		if i%2 == 1 {
			series[i].USDVolume = nil
			series[i].NotionalTrades = 0
		}
	}
	thin := mustPair(t, "native", "fiat:GBP")
	src := newStubSource()
	src.set(mixed, series)
	src.set(thin, withUSD(stableTimedSeries(now, 1), big.NewRat(1, 100), 1))
	sink := newStubSink()

	r := baseline.NewRefresher(src, sink, 30*24*time.Hour, nil).WithMinuteNotionalFloor(productionFloor())
	if outcome, err := r.RefreshPair(context.Background(), mixed); err != nil || outcome != baseline.OutcomeOK {
		t.Fatalf("mixed = (%v, %v), want (ok, nil)", outcome, err)
	}
	if d30 := sink.byPair[mixed.String()].Day30; d30 == nil || d30.N != 720-1 {
		t.Errorf("mixed Day30 = %+v, want N=719 (priced minutes only)", d30)
	}
	sum := r.RefreshAll(context.Background(), []canonical.Pair{mixed, thin}, 2)
	if sum.OK != 1 || sum.NotEnoughSamples != 1 || sum.BelowNotionalFloor != 0 || sum.OKUnvalued != 0 {
		t.Errorf("summary = %+v, want OK=1 NotEnoughSamples=1", sum)
	}
}
