package orchestrator

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/baseline"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/confidence"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

type probeSource struct{ series []baseline.TimedVWAP }

func (s probeSource) TimedVWAPsForPair1m(context.Context, canonical.Pair, time.Time, time.Time) ([]baseline.TimedVWAP, error) {
	return s.series, nil
}

type probeSink struct{ multi *baseline.MultiBaseline }

func (s *probeSink) UpsertBaseline(_ context.Context, _ canonical.Pair, _, _, _ time.Time, m baseline.MultiBaseline) error {
	s.multi = &m
	return nil
}

// probeFreezes runs series through the refresher with the floor wired on
// defaults, then reports whether a 1 -> 5 minute return fires the Phase 2
// freeze against the persisted baseline.
func probeFreezes(t *testing.T, series []baseline.TimedVWAP, wantOutcome baseline.RefreshOutcome) {
	t.Helper()
	usd, err := canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatal(err)
	}
	pair, err := canonical.NewPair(canonical.NativeAsset(), usd)
	if err != nil {
		t.Fatal(err)
	}
	sink := &probeSink{}
	r := baseline.NewRefresher(probeSource{series}, sink, baseline.DefaultWindow, nil).
		WithMinuteNotionalFloor(baseline.MinuteNotionalFloor(10_000, LongestWindow(nil)))
	outcome, err := r.RefreshPair(context.Background(), pair)
	if err != nil || outcome != wantOutcome || sink.multi == nil {
		t.Fatalf("RefreshPair = (%v, %v), want (%v, nil) and a persisted baseline", outcome, err, wantOutcome)
	}
	multi := *sink.multi

	age := baselineAgeDays(multi)
	if age < 0 || age >= confidence.BootstrapDensityDays {
		t.Errorf("baselineAgeDays = %.3f, want in [0, %.1f): thin flow must stay capped", age, confidence.BootstrapDensityDays)
	}
	ret, ok := baseline.NewBucketReturn(1, 5, time.Minute)
	if !ok {
		t.Fatal("NewBucketReturn")
	}
	z, _, valid := maxBucketZ(multi, []baseline.BucketReturn{ret})
	if !valid {
		t.Fatal("no baseline window can score the spike")
	}
	score := confidence.Compute(confidence.Inputs{
		ZScore:           z,
		SourceCount:      1,
		SourceClassCount: 1,
		LiquidityUSD:     10_000,
		BaselineAgeDays:  age,
	}, confidence.DefaultWeights())
	in := confidenceWithSourceCount{Confidence: score.Confidence, ZScore: z, SourceCount: 1}
	if !phase2FreezeFires(in, Phase2Thresholds{}) {
		t.Errorf("freeze did not fire on the 5x print: z=%.1f confidence=%.3f", z, score.Confidence)
	}
}

// A thin pair that can publish must keep a Phase 2 guard under the notional
// floor. 200 organic $3 minutes a day for 30 days, then two
// $5,000 minutes at 5x on the last day ($10,000 clears the 24h publish
// floor): the persisted baseline stays under the bootstrap gate and the 5x
// print fires the freeze.
func TestNotionalFloor_ThinPublishablePairKeepsFreeze(t *testing.T) {
	now := time.Now().UTC()
	start := now.Add(-baseline.Window30d)
	var series []baseline.TimedVWAP
	for day := 0; day < 30; day++ {
		for k := 0; k < 200; k++ {
			i := day*1440 + k*7
			series = append(series, baseline.TimedVWAP{
				VWAP:      1 + float64(i%11)*0.0001,
				BucketEnd: start.Add(time.Duration(i+1) * time.Minute),
				USDVolume: big.NewRat(3, 1),
			})
		}
	}
	spikeAt := start.Add(29*24*time.Hour + 1430*time.Minute)
	for m := 0; m < 2; m++ {
		series = append(series, baseline.TimedVWAP{
			VWAP:      5,
			BucketEnd: spikeAt.Add(time.Duration(m) * time.Minute),
			USDVolume: big.NewRat(5_000, 1),
		})
	}
	probeFreezes(t, series, baseline.OutcomeOK)
}

// Never less protective than main: 100 minutes at $0.20 ($20 of flow, under
// three bars) and no spike yet in the baseline. Main trains a per-minute
// baseline that freezes the next 5x print; so must this.
func TestNotionalFloor_SubBarPairFallsBackToPerMinuteFreeze(t *testing.T) {
	now := time.Now().UTC()
	series := make([]baseline.TimedVWAP, 0, 100)
	for i := 0; i < 100; i++ {
		series = append(series, baseline.TimedVWAP{
			VWAP:      1 + float64(i%11)*0.0001,
			BucketEnd: now.Add(-time.Duration(100-i) * time.Minute),
			USDVolume: big.NewRat(20, 100),
		})
	}
	probeFreezes(t, series, baseline.OutcomeOKPerMinuteFallback)
}

// The fallback must not lift the cap: $0.0004 of dust every
// minute for 30 days ($17.28, two bars) trains the per-minute freeze stats,
// but the density stays that of the bars, so the cap holds.
func TestNotionalFloor_DustFallbackKeepsCap(t *testing.T) {
	now := time.Now().UTC()
	start := now.Add(-baseline.Window30d)
	series := make([]baseline.TimedVWAP, 0, 30*1440)
	for i := 0; i < 30*1440; i++ {
		series = append(series, baseline.TimedVWAP{
			VWAP:      1 + float64(i%11)*0.0001,
			BucketEnd: start.Add(time.Duration(i+1) * time.Minute),
			USDVolume: big.NewRat(4, 10_000),
		})
	}
	probeFreezes(t, series, baseline.OutcomeOKPerMinuteFallback)
}
