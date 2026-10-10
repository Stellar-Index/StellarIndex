package orchestrator

import (
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/anomaly"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/baseline"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/freeze"
	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
)

// A comparator older than the silence grace has no cached last-known-good
// beside it, so a later single-source print must publish instead of freezing
// with nothing held to serve.
func TestComparatorAging_StaleComparatorDoesNotFreezeAFreshPrint(t *testing.T) {
	f := newFreezeFixture(t)
	f.orch.cfg.Interval = time.Minute
	f.orch.prevVWAPAt[f.stateKey()] = f.now
	f.mr.Del(f.vwapKey())

	f.feed(t, manipQuoteAmount, "soroswap")
	f.tick(t, f.orch.vwapMaxAge()+time.Minute)

	if f.state().Active() {
		t.Fatalf("froze against a comparator older than %s", f.orch.vwapMaxAge())
	}
	if got := f.served(t); got != manipFormatted {
		t.Errorf("served = %q, want the fresh print %q", got, manipFormatted)
	}
}

func TestComparatorAging_FreshComparatorStillFreezes(t *testing.T) {
	f := newFreezeFixture(t)
	f.orch.prevVWAPAt[f.stateKey()] = f.now

	f.feed(t, manipQuoteAmount, "soroswap")
	f.tick(t, time.Minute)

	if !f.state().Active() {
		t.Fatal("a fresh comparator must still freeze a manipulated single-source print")
	}
	if got := f.served(t); got != lkgFormatted {
		t.Errorf("served = %q, want the held %q", got, lkgFormatted)
	}
}

func TestAgeComparator_KeepsLiveFreezeComparator(t *testing.T) {
	f := newFreezeFixture(t)
	key := f.stateKey()
	f.orch.prevVWAPs[key] = big.NewRat(1, 1)
	f.orch.prevVWAPAt[key] = f.now.Add(-time.Hour)
	f.orch.freezeStates[key] = f.orch.cfg.Phase2Thresholds.Lifecycle.WithDefaults().Evaluate(
		f.state(), freeze.Signal{Now: f.now, Fires: true}).State

	if !f.state().Active() {
		t.Fatal("fixture: freeze did not engage")
	}
	f.orch.ageComparator(key, f.now)

	if f.orch.prevVWAPs[key] == nil {
		t.Fatal("aged out the held value of a live freeze")
	}
}

// Mid-freeze the scoring comparator is the previous REFUSED bucket; a cache
// flush must still re-serve the last published value, never the refused one.
func TestFreeze_ReseedAfterFlushMidFreezeServesPublishedValue(t *testing.T) {
	f := newFreezeFixture(t)
	f.orch.prevVWAPBucketEnd[f.stateKey()] = f.now.Add(-time.Minute).Truncate(time.Minute)
	f.feed(t, manipQuoteAmount, "soroswap")
	f.tick(t, time.Minute)
	if !f.state().Active() {
		t.Fatal("fixture: freeze did not engage")
	}

	f.mr.FlushAll()
	f.tick(t, time.Minute)

	if !f.state().Active() {
		t.Fatal("freeze released on the scored follow-up bucket")
	}
	if got := f.served(t); got != lkgFormatted {
		t.Errorf("served after mid-freeze flush = %q, want the published %q", got, lkgFormatted)
	}
}

// A cache flush mid-hold leaves flags.frozen with no value to serve; the next
// frozen tick must put the held comparator back, and never overwrite a
// surviving value.
func TestFreeze_ReseedsHeldVWAPAfterCacheLoss(t *testing.T) {
	f := newFreezeFixture(t)
	heldEnd := f.now.Add(-time.Minute).Truncate(time.Minute)
	f.orch.prevVWAPBucketEnd[f.stateKey()] = heldEnd
	f.feed(t, manipQuoteAmount, "soroswap")
	f.mr.FlushAll()

	f.tick(t, time.Minute)

	if !f.state().Active() {
		t.Fatal("fixture: freeze did not engage")
	}
	if got := f.served(t); got != lkgFormatted {
		t.Errorf("served after cache loss = %q, want the held %q", got, lkgFormatted)
	}
	atKey := cachekeys.VWAPObservedAt(f.pair.Base, f.pair.Quote, freezeTestWindow).String()
	raw, err := f.mr.Get(atKey)
	if err != nil {
		t.Fatalf("observed-at stamp not reseeded: %v", err)
	}
	at, err := cachekeys.ParseVWAPObservedAt(raw)
	if err != nil || !at.Equal(heldEnd) {
		t.Errorf("stamp = %q (%v), want held bucket end %s", raw, err, heldEnd)
	}

	if err := f.mr.Set(f.vwapKey(), "0.5"); err != nil {
		t.Fatal(err)
	}
	f.tick(t, time.Minute)
	if got := f.served(t); got != "0.5" {
		t.Errorf("surviving value overwritten: %q", got)
	}
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
