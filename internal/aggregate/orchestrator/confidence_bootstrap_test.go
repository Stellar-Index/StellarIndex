package orchestrator

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/baseline"
	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

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
