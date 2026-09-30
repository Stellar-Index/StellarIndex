package orchestrator

import (
	"context"
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/baseline"
	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// TestConfidence_ServedFactorsCarryBootstrapState — the cached score the
// API serves names the baseline density the bootstrap cap gated on and
// whether the cap bounded the score, and the per-pair gauges agree. A
// thin baseline (1,001 buckets) is capped; a fully-observed 30d window
// (43,200 buckets = 30.0 days-equivalent) is not.
func TestConfidence_ServedFactorsCarryBootstrapState(t *testing.T) {
	cases := []struct {
		name       string
		returns    int
		wantAge    float64
		wantCapped bool
	}{
		{"thin baseline stays capped", 1000, 1001.0 / 1440.0, true},
		{"full 30d density releases the cap", 43_199, 30.0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			factors := servedFactorsFor(t, c.returns)
			age, ok := factors["baseline_age_days"].(float64)
			if !ok || math.Abs(age-c.wantAge) > 1e-9 {
				t.Errorf("served baseline_age_days = %v (present=%v), want %v", factors["baseline_age_days"], ok, c.wantAge)
			}
			if capped, ok := factors["bootstrap_capped"].(bool); !ok || capped != c.wantCapped {
				t.Errorf("served bootstrap_capped = %v, want %v", factors["bootstrap_capped"], c.wantCapped)
			}

			pair := xlmUSDPair(t).String()
			wantGauge := 0.0
			if c.wantCapped {
				wantGauge = 1
			}
			if got, ok := gaugeValue(t, "stellarindex_aggregator_bootstrap_capped", "pair", pair); !ok || got != wantGauge {
				t.Errorf("bootstrap_capped gauge = %v (exported=%v), want %v", got, ok, wantGauge)
			}
			if got, ok := gaugeValue(t, "stellarindex_aggregator_baseline_density_days", "pair", pair); !ok || math.Abs(got-c.wantAge) > 1e-9 {
				t.Errorf("baseline_density_days gauge = %v (exported=%v), want %v", got, ok, c.wantAge)
			}
		})
	}
}

// servedFactorsFor runs the orchestrator for two ticks against a 30d
// baseline built from n returns and decodes the cached factors as the
// API reads them, keyed by wire name.
func servedFactorsFor(t *testing.T, n int) map[string]any {
	t.Helper()
	pair := xlmUSDPair(t)
	now := time.Now().UTC()
	store := &mockStore{trades: []canonical.Trade{
		makeXLMUSDTrade(t, "soroswap", 1_000_000, 1_242_000, now.Add(-30*time.Second)),
		makeXLMUSDTrade(t, "phoenix", 1_000_000, 1_245_000, now.Add(-20*time.Second)),
	}}
	rdb, _ := newTestRedis(t)
	orch := New(store, rdb, Config{
		Pairs:    []canonical.Pair{pair},
		Windows:  []time.Duration{time.Minute},
		Interval: time.Hour,
		Baselines: stubBaselineSource{multi: baseline.MultiBaseline{
			Day30: &baseline.Baseline{Median: 0.0001, MAD: 0.001, N: n},
		}},
	})
	if err := orch.Tick(context.Background()); err != nil {
		t.Fatalf("first tick: %v", err)
	}
	nextBucket(orch)
	if err := orch.Tick(context.Background()); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	body, err := rdb.Get(context.Background(), cachekeys.Confidence(pair.Base, pair.Quote, time.Minute).String()).Bytes()
	if err != nil {
		t.Fatalf("confidence key missing from cache: %v", err)
	}
	var score struct {
		Factors map[string]any `json:"factors"`
	}
	if err := json.Unmarshal(body, &score); err != nil {
		t.Fatalf("cached score not JSON: %v\nraw: %s", err, body)
	}
	return score.Factors
}

// densityBaselineSource serves a 30d baseline whose return count the
// test moves between ticks.
type densityBaselineSource struct{ returns *int }

func (s densityBaselineSource) LatestBaseline(context.Context, canonical.Pair) (baseline.MultiBaseline, time.Time, error) {
	return baseline.MultiBaseline{
		Day30: &baseline.Baseline{Median: 0.0001, MAD: 0.001, N: *s.returns},
	}, time.Now(), nil
}

// TestConfidence_BootstrapGateHoldsStateAcrossTicks — the orchestrator
// carries each pair's gate state into its next score, so a released pair
// whose density dips into the 27.0–28.5 band keeps its score, and a
// re-capped pair needs the full 28.5 to release again.
func TestConfidence_BootstrapGateHoldsStateAcrossTicks(t *testing.T) {
	pair := xlmUSDPair(t)
	now := time.Now().UTC()
	store := &mockStore{trades: []canonical.Trade{
		makeXLMUSDTrade(t, "soroswap", 1_000_000, 1_242_000, now.Add(-30*time.Second)),
		makeXLMUSDTrade(t, "phoenix", 1_000_000, 1_245_000, now.Add(-20*time.Second)),
	}}
	rdb, _ := newTestRedis(t)
	returns := 0
	orch := New(store, rdb, Config{
		Pairs:     []canonical.Pair{pair},
		Windows:   []time.Duration{time.Minute},
		Interval:  time.Hour,
		Baselines: densityBaselineSource{returns: &returns},
	})
	if err := orch.Tick(context.Background()); err != nil {
		t.Fatalf("first tick: %v", err)
	}

	// Bucket counts behind each step; days-equivalent = buckets / 1440.
	steps := []struct {
		buckets    int
		wantCapped float64
	}{
		{40_896, 1}, // 28.4: never released
		{42_912, 0}, // 29.8: clears 28.5
		{40_320, 0}, // 28.0: a shared gap pulls it into the band, held
		{38_736, 1}, // 26.9: below 27.0, re-capped
		{40_320, 1}, // 28.0: back into the band from below, held capped
		{41_040, 0}, // 28.5: released at the upper gate
	}
	for i, s := range steps {
		returns = s.buckets - 1
		nextBucket(orch)
		if err := orch.Tick(context.Background()); err != nil {
			t.Fatalf("step %d tick: %v", i, err)
		}
		// The density gauge proves this tick re-scored; a held gauge alone could be stale.
		if d, ok := gaugeValue(t, "stellarindex_aggregator_baseline_density_days", "pair", pair.String()); !ok || d != float64(s.buckets)/1440 {
			t.Fatalf("step %d: baseline_density_days gauge = %v (exported=%v), want %v", i, d, ok, float64(s.buckets)/1440)
		}
		got, ok := gaugeValue(t, "stellarindex_aggregator_bootstrap_capped", "pair", pair.String())
		if !ok || got != s.wantCapped {
			t.Errorf("step %d (%d buckets): bootstrap_capped gauge = %v (exported=%v), want %v",
				i, s.buckets, got, ok, s.wantCapped)
		}
	}
}
