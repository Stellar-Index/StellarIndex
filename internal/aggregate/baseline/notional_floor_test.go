package baseline_test

import (
	"context"
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

// Two accounts cycling 0.01 USDC every minute for the whole 30-day window
// ($864 of flow) buy one baseline point per ~$6.94, not one per print, so
// the pair stays deep under the bootstrap gate (#1108).
func TestRefresher_PennySelfTradesEveryMinuteStayCapped(t *testing.T) {
	pair := mustPair(t, "native", "fiat:USD")
	now := time.Now().UTC()
	src := newStubSource()
	src.set(pair, withUSD(stableTimedSeries(now, 30*1440), big.NewRat(2, 100)))
	sink := newStubSink()

	r := baseline.NewRefresher(src, sink, 30*24*time.Hour, nil).WithMinuteNotionalFloor(productionFloor())
	if outcome, err := r.RefreshPair(context.Background(), pair); err != nil || outcome != baseline.OutcomeOK {
		t.Fatalf("RefreshPair = (%v, %v), want (ok, nil)", outcome, err)
	}
	d30 := sink.byPair[pair.String()].Day30
	// 348 penny-pair minutes ($6.96) per point: 43,200/348 = 124 points.
	if d30 == nil || d30.N != 123 {
		t.Fatalf("Day30 = %+v, want N=123 (one return per $6.94 of flow)", d30)
	}
	if days := float64(d30.N+1) / 1440; days >= confidence.BootstrapDensityDays {
		t.Errorf("density = %.3f days, want < %.1f (bootstrap cap stays engaged)", days, confidence.BootstrapDensityDays)
	}
}

// The issue's pair: mature, ~200 real minutes a day, with a self-trader
// filling every other minute at a penny. The pennies add one point a day
// ($12.40 of flow), so density stays where the real flow puts it.
func TestRefresher_PennyPaddingDoesNotLiftDensity(t *testing.T) {
	pair := mustPair(t, "native", "fiat:USD")
	now := time.Now().UTC()
	series := withUSD(stableTimedSeries(now, 30*1440), big.NewRat(1, 100))
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
	if want := realMinutes + 30 - 1; d30.N != want {
		t.Errorf("Day30.N = %d, want %d (real minutes + one penny point a day)", d30.N, want)
	}
	// Same measure orchestrator.baselineAgeDays reads.
	if days := float64(d30.N+1) / 1440; days >= confidence.BootstrapDensityDays {
		t.Errorf("density = %.2f days, want < %.1f (bootstrap cap stays engaged)", days, confidence.BootstrapDensityDays)
	}
}

// Sub-floor minutes merge into one point priced by USD weight, so a dust
// print at a wild price moves it only in proportion to its USD share.
func TestRefresher_VolumeBarPriceIsUSDWeighted(t *testing.T) {
	pair := mustPair(t, "native", "fiat:USD")
	now := time.Now().UTC()
	var series []baseline.TimedVWAP
	for i := 0; i < 10; i++ {
		end := now.Add(-time.Duration(20-2*i) * time.Minute)
		series = append(series,
			baseline.TimedVWAP{VWAP: 1000, BucketEnd: end.Add(-time.Minute), USDVolume: big.NewRat(1, 100)},
			baseline.TimedVWAP{VWAP: 2 + float64(i)*0.001, BucketEnd: end, USDVolume: big.NewRat(9_99, 100)})
	}
	src := newStubSource()
	src.set(pair, series)
	sink := newStubSink()

	r := baseline.NewRefresher(src, sink, 30*24*time.Hour, nil).WithMinuteNotionalFloor(big.NewRat(10, 1))
	if outcome, err := r.RefreshPair(context.Background(), pair); err != nil || outcome != baseline.OutcomeOK {
		t.Fatalf("RefreshPair = (%v, %v), want (ok, nil)", outcome, err)
	}
	d30 := sink.byPair[pair.String()].Day30
	// Each point is (1000*0.01 + p*9.99)/10 ≈ p+1, where an unweighted
	// mean would put it at ~500; the median return stays near zero.
	if d30 == nil || d30.N != 9 || d30.Median > 0.001 {
		t.Errorf("Day30 = %+v, want N=9 with a small median return", d30)
	}
}

// A pair with no USD-valued minute cannot be measured; it keeps a baseline
// over every minute so the z-score freeze stays live, under its own outcome.
func TestRefresher_UnvaluedPairKeepsGuardUnderDistinctOutcome(t *testing.T) {
	pair := mustPair(t, "native", "fiat:EUR")
	now := time.Now().UTC()
	src := newStubSource()
	src.set(pair, withUSD(stableTimedSeries(now, 2*1440), nil))
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

// A valued pair's unpriced minutes prove no notional and are dropped; a pair
// too thin for any baseline stays not_enough_samples; a pair with minutes
// but too little flow for three bars falls back to a per-minute baseline.
func TestRefresher_NotionalOutcomesAreDistinct(t *testing.T) {
	now := time.Now().UTC()
	mixed := mustPair(t, "native", "fiat:USD")
	series := stableTimedSeries(now, 1440)
	for i := range series {
		if i%2 == 1 {
			series[i].USDVolume = nil
		}
	}
	thin := mustPair(t, "native", "fiat:GBP")
	dust := mustPair(t, "native", "fiat:JPY")
	src := newStubSource()
	src.set(mixed, series)
	src.set(thin, withUSD(stableTimedSeries(now, 1), big.NewRat(1, 100)))
	src.set(dust, withUSD(stableTimedSeries(now, 100), big.NewRat(1, 1000)))
	sink := newStubSink()

	r := baseline.NewRefresher(src, sink, 30*24*time.Hour, nil).WithMinuteNotionalFloor(productionFloor())
	if outcome, err := r.RefreshPair(context.Background(), mixed); err != nil || outcome != baseline.OutcomeOK {
		t.Fatalf("mixed = (%v, %v), want (ok, nil)", outcome, err)
	}
	if d30 := sink.byPair[mixed.String()].Day30; d30 == nil || d30.N != 720-1 {
		t.Errorf("mixed Day30 = %+v, want N=719 (priced minutes only)", d30)
	}
	if outcome, err := r.RefreshPair(context.Background(), dust); err != nil || outcome != baseline.OutcomeOKPerMinuteFallback {
		t.Errorf("dust = (%v, %v), want ok_per_minute_fallback ($0.10 of flow is under one bar)", outcome, err)
	}
	if d30 := sink.byPair[dust.String()].Day30; d30 == nil || d30.N != 99 {
		t.Errorf("dust Day30 = %+v, want the per-minute N=99", d30)
	}
	sum := r.RefreshAll(context.Background(), []canonical.Pair{mixed, thin, dust}, 2)
	if sum.OK != 1 || sum.NotEnoughSamples != 1 || sum.OKPerMinuteFallback != 1 || sum.OKUnvalued != 0 {
		t.Errorf("summary = %+v, want OK=1 NotEnoughSamples=1 OKPerMinuteFallback=1", sum)
	}
}
