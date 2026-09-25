package orchestrator

import (
	"context"
	"encoding/json"
	"math"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/freeze"
	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/divergence"
)

// fixedReference quotes one price for every pair.
type fixedReference struct {
	name  string
	price float64
}

func (r fixedReference) Name() string { return r.name }

func (r fixedReference) LookupQuote(_ context.Context, _ canonical.Pair, at time.Time) (divergence.Quote, error) {
	return divergence.Quote{Price: r.price, AsOf: at}, nil
}

const (
	pinnedLKG   = 0.100 // the last-known-good a freeze keeps serving
	marketPrice = 0.108 // where the market settled: +8%, past the 5% threshold
)

// pinnedDivergenceRig is an orchestrator driving a real divergence
// service whose references all quote marketPrice, with pinnedLKG cached
// as the pair's shortest-window VWAP.
func pinnedDivergenceRig(t *testing.T) (*Orchestrator, *redis.Client, canonical.Pair, *atomic.Int32) {
	t.Helper()
	rdb, _ := newTestRedis(t)
	pair := pairXLMUSD(t)
	window := 5 * time.Minute
	if err := rdb.Set(context.Background(), cachekeys.VWAP(pair.Base, pair.Quote, window).String(),
		"0.1", time.Hour).Err(); err != nil {
		t.Fatalf("seed vwap: %v", err)
	}
	var hooks atomic.Int32
	svc, err := divergence.NewService(divergence.ServiceOptions{
		References: []divergence.Reference{
			fixedReference{"coingecko", marketPrice},
			fixedReference{"chainlink", marketPrice},
			fixedReference{"coinmarketcap", marketPrice},
		},
		Cache:                rdb,
		Threshold:            5,
		MinSourcesForWarning: 2,
		WarningPersistence:   5 * time.Minute,
		Logger:               silentLogger(),
		OnWarningFired: func(context.Context, canonical.Pair, divergence.CachedResult) {
			hooks.Add(1)
		},
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	o := New(nil, rdb, Config{
		Pairs:               []canonical.Pair{pair},
		Windows:             []time.Duration{window},
		DivergenceRefresher: svc,
		Logger:              silentLogger(),
	})
	return o, rdb, pair, &hooks
}

func readDivergenceEntry(t *testing.T, rdb *redis.Client, pair canonical.Pair) divergence.CachedResult {
	t.Helper()
	raw, err := rdb.Get(context.Background(), cachekeys.Divergence(pair).String()).Bytes()
	if err != nil {
		t.Fatalf("read div entry: %v", err)
	}
	var cached divergence.CachedResult
	if err := json.Unmarshal(raw, &cached); err != nil {
		t.Fatalf("decode div entry: %v", err)
	}
	return cached
}

// TestRefreshDivergenceAll_FrozenPairIsNotJudgedOnItsPinnedPrice: a pair
// frozen on a genuine repricing keeps serving its last-known-good while
// the market settles 8% higher. Across two divergence passes past the
// warning persistence — the first in the freeze's own tick, the second
// later in its hold — the pinned value must not raise warning_fired or the
// divergence.firing hook, and must not reach the confidence score as a
// cross-oracle disagreement. The reference median must still reach the
// release path.
func TestRefreshDivergenceAll_FrozenPairIsNotJudgedOnItsPinnedPrice(t *testing.T) {
	t.Parallel()
	o, rdb, pair, hooks := pinnedDivergenceRig(t)
	window := 5 * time.Minute
	t0 := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)

	// Tick 1: the freeze fires this tick.
	o.markFrozenThisTick(pair, window)
	o.refreshDivergenceAll(context.Background(), t0)

	// Tick 2, ten minutes on: a new tick's set is empty, the hold is live.
	t1 := t0.Add(10 * time.Minute)
	o.frozenThisTick = make(map[string]struct{})
	o.freezeStates[pair.String()+":"+window.String()] = freeze.State{FiredAt: t0, HoldUntil: t0.Add(30 * time.Minute)}
	o.clock = func() time.Time { return t1 }
	o.refreshDivergenceAll(context.Background(), t1)

	if n := hooks.Load(); n != 0 {
		t.Errorf("divergence.firing hook fired %d time(s) for a pair serving its pinned LKG", n)
	}
	if got := readDivergenceEntry(t, rdb, pair); got.WarningFired {
		t.Errorf("warning_fired = true on a frozen pair: the pinned %v was judged against the %v market",
			pinnedLKG, marketPrice)
	}
	xo := o.lookupCrossOracle(context.Background(), pair)
	if xo.divergencePct != -1 || xo.agreementCount != -1 {
		t.Errorf("confidence cross-oracle input = {pct %v, agree %d}, want unchecked {-1, -1}: "+
			"a divergence measured against the pinned LKG would depress the score that is itself a freeze leg",
			xo.divergencePct, xo.agreementCount)
	}
	if math.Abs(xo.median-marketPrice) > 1e-12 {
		t.Errorf("reference median = %v, want %v — the release path corroborates the candidate against it",
			xo.median, marketPrice)
	}
}

// TestRefreshDivergenceAll_UnfrozenDivergenceStillFires is the control:
// the same rig with no freeze does fire, so the frozen case above is
// suppressed by the freeze and not by the rig.
func TestRefreshDivergenceAll_UnfrozenDivergenceStillFires(t *testing.T) {
	t.Parallel()
	o, rdb, pair, hooks := pinnedDivergenceRig(t)
	t0 := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	o.refreshDivergenceAll(context.Background(), t0)
	o.refreshDivergenceAll(context.Background(), t0.Add(10*time.Minute))

	if !readDivergenceEntry(t, rdb, pair).WarningFired {
		t.Error("warning_fired = false for an unfrozen pair 8% off the references for 10 minutes")
	}
	if n := hooks.Load(); n != 1 {
		t.Errorf("divergence.firing hook fired %d time(s), want 1", n)
	}
	if xo := o.lookupCrossOracle(context.Background(), pair); xo.agreementCount != 0 || xo.divergencePct <= 5 {
		t.Errorf("confidence cross-oracle input = {pct %v, agree %d}, want a checked disagreement",
			xo.divergencePct, xo.agreementCount)
	}
}
