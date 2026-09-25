package orchestrator

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/anomaly"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/baseline"
	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// bucketReturnCase ticks a single-source market with one steady print per
// minute across the whole window, then closes a minute holding one steady
// print and one equal-sized print at 3x. That minute's VWAP is +100% over the
// previous minute's — past Phase 1's crypto FreezePct (50%) and far past a
// 1%-MAD baseline — while the rolling window moves only 1/(window minutes+1)
// as much (+33% on 5m, +3.3% on 1h, +0.14% on 24h). It returns whether the
// second bucket published and whether the window's freeze is active.
func bucketReturnCase(t *testing.T, window time.Duration, cfg Config) (published, frozen bool) {
	t.Helper()
	pair := xlmUSDPair(t)
	bucketEnd := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	store := &rangeStore{}
	for m := time.Duration(1); m <= window/time.Minute; m++ {
		store.all = append(store.all, makeXLMUSDTrade(t, "soroswap", 1_000_000, 1_000_000, bucketEnd.Add(-m*time.Minute+10*time.Second)))
	}
	rdb, mr := newTestRedis(t)
	cfg.Pairs = []canonical.Pair{pair}
	cfg.Windows = []time.Duration{window}
	cfg.Interval = time.Hour
	o := New(store, rdb, cfg)
	now := bucketEnd.Add(5 * time.Second)
	o.clock = func() time.Time { return now }
	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	vwapKey := cachekeys.VWAP(pair.Base, pair.Quote, window).String()
	lkg, err := mr.Get(vwapKey)
	if err != nil {
		t.Fatalf("tick 1 did not publish: %v", err)
	}

	store.all = append(store.all,
		makeXLMUSDTrade(t, "soroswap", 1_000_000, 1_000_000, bucketEnd.Add(10*time.Second)),
		makeXLMUSDTrade(t, "soroswap", 1_000_000, 3_000_000, bucketEnd.Add(20*time.Second)),
	)
	nextBucket(o)
	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	got, err := mr.Get(vwapKey)
	if err != nil {
		t.Fatalf("read VWAP after tick 2: %v", err)
	}
	return got != lkg, o.freezeStates[pair.String()+":"+window.String()].Active()
}

// Phase 1 compares the newest closed minute against the previous one — the
// basis its class thresholds are set on — at every window length.
func TestPhase1_ScoresTheNewestBucketNotTheRollingDelta(t *testing.T) {
	for _, window := range DefaultWindows {
		t.Run(window.String(), func(t *testing.T) {
			checker, err := anomaly.NewChecker(anomaly.DefaultThresholds(), anomaly.NewClassifier(map[string]anomaly.AssetClass{
				xlmUSDPair(t).Base.String(): anomaly.ClassCrypto,
			}))
			if err != nil {
				t.Fatalf("NewChecker: %v", err)
			}
			published, frozen := bucketReturnCase(t, window, Config{Anomaly: checker})
			if published || !frozen {
				t.Errorf("%s: a +100%% single-source minute published=%v frozen=%v, want held and frozen", window, published, frozen)
			}
		})
	}
}

// Phase 2 z-scores the same minute-on-minute return the baseline is trained
// on, so a 1h or 24h window cannot dilute a spike below the freeze.
func TestPhase2_ScoresTheNewestBucketNotTheRollingDelta(t *testing.T) {
	for _, window := range DefaultWindows {
		t.Run(window.String(), func(t *testing.T) {
			bsrc := stubBaselineSource{
				multi:      baseline.MultiBaseline{Day30: &baseline.Baseline{Median: 0, MAD: 0.01, N: 1000}},
				computedAt: time.Date(2026, 7, 25, 11, 0, 0, 0, time.UTC),
			}
			published, frozen := bucketReturnCase(t, window, Config{Baselines: bsrc})
			if published || !frozen {
				t.Errorf("%s: a +100%% single-source minute published=%v frozen=%v, want held and frozen", window, published, frozen)
			}
		})
	}
}

// A window holding one non-empty minute has no in-window predecessor; the
// minute is then measured against the last published VWAP.
func TestMarginalBuckets_FallsBackToPublishedWhenWindowHoldsOneMinute(t *testing.T) {
	o := New(&mockStore{}, nil, Config{})
	pair := xlmUSDPair(t)
	at := time.Date(2026, 7, 25, 12, 0, 30, 0, time.UTC)
	mb := o.scoreMinutes([]canonical.Trade{
		makeXLMUSDTrade(t, "soroswap", 1_000_000, 2_000_000, at),
		makeXLMUSDTrade(t, "soroswap", 1_000_000, 2_000_000, at.Add(10*time.Second)),
	}, pair, "k", nil)
	if mb.pred != nil {
		t.Fatalf("pred = %v, want nil for a single-minute window", mb.pred)
	}
	fallback := o.bucketVWAP([]canonical.Trade{makeXLMUSDTrade(t, "soroswap", 1_000_000, 1_000_000, at)}, pair)
	rets := mb.returns(fallback)
	if len(rets) != 1 || rets[0].Fraction() != 1 {
		t.Errorf("returns = %v, want one +1.0 against the published price", rets)
	}
	if got := mb.returns(nil); len(got) != 0 {
		t.Errorf("returns scored a lone minute with no comparator: %v", got)
	}
}

// A minute whose predecessor printed k minutes earlier is scored in the
// baseline's one-minute unit — the raw move over sqrt(k) — whether the
// predecessor is in the window or is the previous decision's minute.
func TestMarginalBuckets_ScalesAReturnAcrossEmptyMinutes(t *testing.T) {
	o := New(&mockStore{}, nil, Config{})
	pair := xlmUSDPair(t)
	at := time.Date(2026, 7, 25, 12, 0, 30, 0, time.UTC)
	first := makeXLMUSDTrade(t, "soroswap", 1_000_000, 1_000_000, at)
	second := makeXLMUSDTrade(t, "soroswap", 1_000_000, 1_210_000, at.Add(10*time.Minute))
	rets := o.scoreMinutes([]canonical.Trade{first, second}, pair, "k", nil).returns(nil)
	if want := 0.21 / math.Sqrt(10); len(rets) != 1 || math.Abs(rets[0].Fraction()-want) > 1e-12 {
		t.Errorf("in-window predecessor 10m back: returns = %v, want one %v", rets, want)
	}

	third := makeXLMUSDTrade(t, "soroswap", 1_000_000, 1_331_000, at.Add(25*time.Minute))
	rets = o.scoreMinutes([]canonical.Trade{third}, pair, "k", nil).returns(nil)
	if want := 0.1 / math.Sqrt(15); len(rets) != 1 || math.Abs(rets[0].Fraction()-want) > 1e-12 {
		t.Errorf("previous decision's minute 15m back: returns = %v, want one %v", rets, want)
	}
}

// backlogCase ticks a single-source market at 1.0 with one print per minute
// across the window, decides three more buckets while ingest stalls, then
// lands four minutes at 2.0 in one batch: the batch's two newest minutes
// agree, and only the level before the batch shows the +100% move. With
// unpriceableNewest the batch's newest minute holds only a zero-quote print,
// which survives the filters but has no VWAP. It returns whether the catch-up
// bucket published and whether the window's freeze is active.
func backlogCase(t *testing.T, window time.Duration, cfg Config, unpriceableNewest bool) (published, frozen bool) {
	t.Helper()
	pair := xlmUSDPair(t)
	bucketEnd := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	store := &rangeStore{}
	for m := time.Duration(1); m <= window/time.Minute; m++ {
		store.all = append(store.all, makeXLMUSDTrade(t, "soroswap", 1_000_000, 1_000_000, bucketEnd.Add(-m*time.Minute+10*time.Second)))
	}
	rdb, mr := newTestRedis(t)
	cfg.Pairs = []canonical.Pair{pair}
	cfg.Windows = []time.Duration{window}
	cfg.Interval = time.Hour
	o := New(store, rdb, cfg)
	now := bucketEnd.Add(5 * time.Second)
	o.clock = func() time.Time { return now }
	for i := 0; i < 4; i++ {
		if i > 0 {
			nextBucket(o)
		}
		if err := o.Tick(context.Background()); err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
	}
	vwapKey := cachekeys.VWAP(pair.Base, pair.Quote, window).String()
	lkg, err := mr.Get(vwapKey)
	if err != nil {
		t.Fatalf("stall ticks did not publish: %v", err)
	}
	for m := time.Duration(0); m < 4; m++ {
		quote := int64(2_000_000)
		if unpriceableNewest && m == 3 {
			quote = 0
		}
		store.all = append(store.all, makeXLMUSDTrade(t, "soroswap", 1_000_000, quote, bucketEnd.Add(m*time.Minute+10*time.Second)))
	}
	nextBucket(o)
	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("catch-up tick: %v", err)
	}
	got, err := mr.Get(vwapKey)
	if err != nil {
		t.Fatalf("read VWAP after catch-up: %v", err)
	}
	return got != lkg, o.freezeStates[pair.String()+":"+window.String()].Active()
}

// A move that lands in one batch is scored against the level before the
// batch, not only between the batch's two newest (agreeing) minutes; and a
// newest minute with no VWAP hands scoring to the newest priceable minute
// instead of skipping it.
func TestFreeze_BacklogBatchScoredAgainstLevelBeforeIt(t *testing.T) {
	for _, unpriceable := range []bool{false, true} {
		for _, window := range DefaultWindows {
			name := fmt.Sprintf("%s/unpriceableNewest=%v", window, unpriceable)
			t.Run("phase1/"+name, func(t *testing.T) {
				checker, err := anomaly.NewChecker(anomaly.DefaultThresholds(), anomaly.NewClassifier(map[string]anomaly.AssetClass{
					xlmUSDPair(t).Base.String(): anomaly.ClassCrypto,
				}))
				if err != nil {
					t.Fatalf("NewChecker: %v", err)
				}
				published, frozen := backlogCase(t, window, Config{Anomaly: checker}, unpriceable)
				if published || !frozen {
					t.Errorf("a +100%% backlog batch published=%v frozen=%v, want held and frozen", published, frozen)
				}
			})
			t.Run("phase2/"+name, func(t *testing.T) {
				bsrc := stubBaselineSource{
					multi:      baseline.MultiBaseline{Day30: &baseline.Baseline{Median: 0, MAD: 0.01, N: 1000}},
					computedAt: time.Date(2026, 7, 25, 11, 0, 0, 0, time.UTC),
				}
				published, frozen := backlogCase(t, window, Config{Baselines: bsrc}, unpriceable)
				if published || !frozen {
					t.Errorf("a +100%% backlog batch published=%v frozen=%v, want held and frozen", published, frozen)
				}
			})
		}
	}
}
