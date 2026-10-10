package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/baseline"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/freeze"
	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/divergence"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// ADR-0019 freeze-lifecycle fixtures: one pair (native/fiat:USD), one 1m
// window, baseline MAD 0.001 so a return r scores z = r / 0.001.
//
//	lkgPrice      0.1242 -> return 0, z 0,   confidence 0.4972 (clears auto-unfreeze: > 0.30, z < 3.0)
//	manipPrice    0.2000 -> return 0.610, z 610, confidence 0.0000 (clears freeze: < 0.45, z > 5.0, sources <= 1)
//
// Measured against the real confidence combiner.
const (
	lkgBaseAmount    = 1_000_000_000_000 // 100,000 XLM at 1e7
	lkgQuoteAmount   = 124_200_000_000   // $12,420 at 1e7 → price 0.1242
	manipQuoteAmount = 200_000_000_000   // $20,000 at 1e7 → price 0.2000

	lkgFormatted   = "0.124200000000"
	manipFormatted = "0.200000000000"

	freezeTestWindow = time.Minute
)

// freezeFixture: an orchestrator with a controllable clock, swappable trades
// and direct access to the Redis and marker state the freeze path writes.
type freezeFixture struct {
	orch   *Orchestrator
	store  *mockStore
	marker *recordingFreezeMarker
	rdb    *redis.Client
	mr     *miniredis.Miniredis
	pair   canonical.Pair
	now    time.Time
}

// newFreezeFixture seeds the LKG in cache and in the prev-VWAP comparator
// slot: the state right after a healthy publish.
func newFreezeFixture(t *testing.T) *freezeFixture {
	t.Helper()
	return newFreezeFixtureWith(t, 0.001, nil)
}

// newFreezeFixtureWith sets the baseline MAD and lets mod adjust the Config.
func newFreezeFixtureWith(t *testing.T, mad float64, mod func(*Config)) *freezeFixture {
	t.Helper()
	pair := xlmUSDPair(t)
	rdb, mr := newTestRedis(t)
	marker := &recordingFreezeMarker{}
	store := &mockStore{}

	cfg := Config{
		Pairs:        []canonical.Pair{pair},
		Windows:      []time.Duration{freezeTestWindow},
		Interval:     time.Hour,
		FreezeWriter: marker,
		Baselines: stubBaselineSource{
			multi: baseline.MultiBaseline{
				// Fully observed 30d window: quality factor 1.0, no bootstrap cap.
				Day30: &baseline.Baseline{Median: 0, MAD: mad, N: maxDay30Returns},
			},
		},
	}
	if mod != nil {
		mod(&cfg)
	}
	orch := New(store, rdb, cfg)

	f := &freezeFixture{
		orch:   orch,
		store:  store,
		marker: marker,
		rdb:    rdb,
		mr:     mr,
		pair:   pair,
		now:    time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC),
	}
	orch.clock = func() time.Time { return f.now }

	orch.prevVWAPs[f.stateKey()] = big.NewRat(lkgQuoteAmount, lkgBaseAmount)
	if err := rdb.Set(context.Background(), f.vwapKey(), lkgFormatted, time.Minute).Err(); err != nil {
		t.Fatalf("seed LKG: %v", err)
	}
	return f
}

func (f *freezeFixture) stateKey() string {
	return f.pair.String() + ":" + freezeTestWindow.String()
}

func (f *freezeFixture) vwapKey() string {
	return cachekeys.VWAP(f.pair.Base, f.pair.Quote, freezeTestWindow).String()
}

// feed sets the window's trades: one per source, all at quote/base = the price.
func (f *freezeFixture) feed(t *testing.T, quoteAmount int64, sources ...string) {
	t.Helper()
	trades := make([]canonical.Trade, 0, len(sources))
	for _, src := range sources {
		trades = append(trades, makeXLMUSDTrade(t, src,
			lkgBaseAmount/int64(len(sources)),
			quoteAmount/int64(len(sources)),
			f.now.Add(-10*time.Second)))
	}
	f.store.trades = trades
}

// tick advances the fake clock by d, then runs one Tick.
func (f *freezeFixture) tick(t *testing.T, d time.Duration) {
	t.Helper()
	f.now = f.now.Add(d)
	if err := f.orch.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
}

// served returns the value the API would serve for the pair.
func (f *freezeFixture) served(t *testing.T) string {
	t.Helper()
	got, err := f.mr.Get(f.vwapKey())
	if err != nil {
		t.Fatalf("cached VWAP missing: %v", err)
	}
	return got
}

// state returns the orchestrator's live lifecycle state for the pair.
func (f *freezeFixture) state() freeze.State {
	return f.orch.freezeStates[f.stateKey()]
}

// restart swaps in a brand-new Orchestrator on the same Redis, as a deploy would.
func (f *freezeFixture) restart(fm FreezeMarker) *Orchestrator {
	o := New(f.store, f.rdb, Config{
		Pairs:        []canonical.Pair{f.pair},
		Windows:      []time.Duration{freezeTestWindow},
		Interval:     time.Hour,
		FreezeWriter: fm,
		Baselines: stubBaselineSource{
			multi: baseline.MultiBaseline{
				Day30: &baseline.Baseline{Median: 0, MAD: 0.001, N: maxDay30Returns},
			},
		},
	})
	o.clock = func() time.Time { return f.now }
	f.orch = o
	return o
}

// seedDivergence installs a cross-oracle divergence result at the key
// lookupCrossOracle reads, as the divergence worker does in production.
func seedDivergence(t *testing.T, f *freezeFixture, res divergence.CachedResult) {
	t.Helper()
	seedDivergenceCache(t, f.rdb, f.pair, res)
}

// seedDivergenceCache is seedDivergence for harnesses without a freezeFixture.
func seedDivergenceCache(t *testing.T, cache Cache, pair canonical.Pair, res divergence.CachedResult) {
	t.Helper()
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal divergence result: %v", err)
	}
	if err := cache.Set(context.Background(), cachekeys.Divergence(pair).String(), raw, time.Hour).Err(); err != nil {
		t.Fatalf("seed divergence: %v", err)
	}
}

// agreeingLens is the divergence entry production writes when the references
// sit AT level while we serve servedPrice. OurPrice is the SERVED price
// (mid-freeze that is the pinned LKG, never the refused candidate); the
// release gate reads only Median and compares it to the FRESH candidate.
func agreeingLens(pair canonical.Pair, servedPrice, level float64) divergence.CachedResult {
	pct := 0.0
	if level > 0 {
		pct = math.Abs(servedPrice-level) / level * 100
	}
	// AgreementCount counts references within tolerance of OurPrice
	// (compare.go CountAgreeing); seed only producible values.
	agreeing := 3
	if pct > 5.0 {
		agreeing = 0
	}
	return divergence.CachedResult{
		PairID:         pair.String(),
		OurPrice:       servedPrice,
		Median:         level,
		DivergencePct:  pct,
		SuccessCount:   3, // ≥ divergenceMinSources: trusted multi-reference signal
		AgreementCount: agreeing,
	}
}

// Regression N-F6: freeze on a manipulated single-source bucket, then one
// bucket with a second source at the SAME manipulated price. The old release
// (negated fire condition) let `source_count <= 1` failing clear the freeze.
// ADR-0019 releases only on confidence > 0.30 AND z < 3.0 for two consecutive
// buckets past the minimum hold; a price still 61% off the LKG meets neither.
func TestFreezeLifecycle_SingleCleanBucketDoesNotRelease(t *testing.T) {
	f := newFreezeFixture(t)

	// Bucket 1: single-source manipulated print → freeze fires.
	f.feed(t, manipQuoteAmount, "soroswap")
	f.tick(t, closedBucket)
	if !f.state().Active() {
		t.Fatal("setup: manipulated single-source bucket did not freeze")
	}
	if got := f.served(t); got != lkgFormatted {
		t.Fatalf("setup: freeze published %q, want the LKG %q", got, lkgFormatted)
	}

	// Bucket 2: same price on TWO venues; source_count = 2 breaks the 3-signal AND.
	f.feed(t, manipQuoteAmount, "soroswap", "phoenix")
	f.tick(t, closedBucket)

	if got := f.served(t); got != lkgFormatted {
		t.Errorf("a second-source bucket released the freeze and published %q; want the held LKG %q (still 61%% off)",
			got, lkgFormatted)
	}
	if !f.state().Active() {
		t.Error("freeze state cleared on a bucket that met no auto-unfreeze condition")
	}
	if f.orch.prevVWAPs[f.stateKey()].Cmp(big.NewRat(lkgQuoteAmount, lkgBaseAmount)) != 0 {
		t.Error("prev-VWAP comparator moved to the manipulated price")
	}
}

// One healthy bucket back at the LKG is half of the two auto-unfreeze needs,
// and the initial hold is a minimum; releasing here lets a parked
// manipulation walk free.
func TestFreezeLifecycle_HoldSurvivesAHealthyBucket(t *testing.T) {
	f := newFreezeFixture(t)
	// References agree with the LKG, so the return TO it is corroborated and may earn the streak.
	seedDivergence(t, f, agreeingLens(f.pair, 0.1242, 0.1242))

	f.feed(t, manipQuoteAmount, "soroswap")
	f.tick(t, closedBucket)
	if !f.state().Active() {
		t.Fatal("setup: freeze did not fire")
	}

	// The first bucket back at LKG is a jump against the shadow comparator
	// and cannot start the streak; the second settled bucket does.
	f.feed(t, lkgQuoteAmount, "soroswap")
	f.tick(t, closedBucket)
	if got := f.state().UnfreezeStreak; got != 0 {
		t.Errorf("UnfreezeStreak = %d on the transition bucket, want 0 (it is a jump)", got)
	}
	f.feed(t, lkgQuoteAmount, "soroswap")
	f.tick(t, closedBucket)

	if got := f.served(t); got != lkgFormatted {
		t.Errorf("served %q during the hold", got)
	}
	if !f.state().Active() {
		t.Error("freeze released inside the initial hold")
	}
	if got := f.state().UnfreezeStreak; got != 1 {
		t.Errorf("UnfreezeStreak = %d after one settled bucket, want 1", got)
	}
}

// Release path: past the initial hold with two healthy buckets the fresh value
// publishes, the marker is DELETED (not left to lapse, which would keep
// flags.frozen true) and the release is counted.
func TestFreezeLifecycle_AutoUnfreezeAfterTwoHealthyBucketsPastTheHold(t *testing.T) {
	f := newFreezeFixture(t)
	before := testutil.ToFloat64(obs.AnomalyFreezeReleasedTotal.WithLabelValues("auto"))

	// Corroborated release candidate, and the full 30-minute hold.
	seedDivergence(t, f, agreeingLens(f.pair, 0.1242, 0.1242))

	f.feed(t, manipQuoteAmount, "soroswap")
	f.tick(t, closedBucket)
	if !f.state().Active() {
		t.Fatal("setup: freeze did not fire")
	}
	if got := testutil.ToFloat64(obs.AnomalyFreezeActive); got != 1 {
		t.Errorf("AnomalyFreezeActive = %v while one pair is frozen, want 1", got)
	}

	// The settling bucket is a jump and must not count toward the streak.
	f.feed(t, lkgQuoteAmount, "soroswap")
	f.tick(t, closedBucket)
	// Two settled healthy buckets, the second after the 30-minute hold.
	f.tick(t, freeze.DefaultInitialHold+time.Minute)
	if !f.state().Active() {
		t.Fatal("released at expiry on a streak of one — the ADR wants two consecutive")
	}
	f.tick(t, closedBucket)

	if f.state().Active() {
		t.Fatalf("still frozen after two consecutive healthy buckets past the hold: %+v", f.state())
	}
	if got := f.served(t); got != lkgFormatted {
		t.Errorf("served %q after release; want the freshly published %q", got, lkgFormatted)
	}
	if f.marker.present {
		t.Error("freeze marker still present after release")
	}
	if f.marker.clears == 0 {
		t.Error("release did not Clear the marker")
	}
	after := testutil.ToFloat64(obs.AnomalyFreezeReleasedTotal.WithLabelValues("auto"))
	if after-before != 1 {
		t.Errorf("AnomalyFreezeReleasedTotal{auto} delta = %v, want 1", after-before)
	}
	// The gauge must fall back to zero, not stay latched.
	if got := testutil.ToFloat64(obs.AnomalyFreezeActive); got != 0 {
		t.Errorf("AnomalyFreezeActive = %v after release, want 0", got)
	}
}

// A pair with no corroborating lens (no divergence entry, no triangulation)
// cannot prove a calm level is the market rather than a parked manipulation
// (the shadow comparator reads any held level as calm), so the streak must not
// accumulate and the ladder walks to an operator.
func TestFreezeLifecycle_NoLensMeansNoAutoRelease(t *testing.T) {
	f := newFreezeFixture(t)

	f.feed(t, manipQuoteAmount, "soroswap")
	f.tick(t, closedBucket)
	if !f.state().Active() {
		t.Fatal("setup: freeze did not fire")
	}

	// As the release test, but past the uncorroborated 10-minute hold.
	f.feed(t, lkgQuoteAmount, "soroswap")
	f.tick(t, closedBucket)
	f.tick(t, freeze.DefaultUncorroboratedInitialHold+time.Minute)
	f.tick(t, closedBucket)

	if !f.state().Active() {
		t.Fatal("a pair with no corroborating lens auto-released")
	}
	if got := f.state().UnfreezeStreak; got != 0 {
		t.Errorf("UnfreezeStreak = %d with no lens, want 0", got)
	}
	if got := f.served(t); got != lkgFormatted {
		t.Errorf("served %q; want the held LKG %q", got, lkgFormatted)
	}
}

// The ladder: a manipulation the attacker HOLDS never earns release, so each
// hold expiry grants one 30-minute extension until the four are spent, then it
// escalates to operator review and stops auto-unfreezing.
//
// A held level is per-tick calm (z=0), so the release gate is what separates
// it from a real settle: the streak counts only buckets whose FRESH candidate a
// lens agrees with. Mid-freeze the worker compares references to the SERVED
// (pinned LKG) price, so OurPrice = Median = LKG; the candidate-vs-median check
// (0.2000 vs 0.1242, 61% >> 5%) is what refuses the streak.
func TestFreezeLifecycle_ExtendsThenEscalates(t *testing.T) {
	f := newFreezeFixture(t)
	beforeExt := testutil.ToFloat64(obs.AnomalyFreezeExtensionsTotal)
	beforeEsc := testutil.ToFloat64(obs.AnomalyFreezeEscalatedTotal)

	// The reachable mid-freeze lens state: references at the served LKG.
	seedDivergence(t, f, agreeingLens(f.pair, 0.1242, 0.1242))

	f.feed(t, manipQuoteAmount, "soroswap")
	f.tick(t, closedBucket)

	// Each step lands just past the current hold; a lens was consulted, so the hold is 30 minutes.
	f.tick(t, freeze.DefaultInitialHold+time.Minute)
	for i := 2; i <= freeze.DefaultMaxExtensions; i++ {
		f.tick(t, freeze.DefaultExtension+time.Minute)
		used := f.state().ExtensionsUsed
		if used != i {
			t.Fatalf("after expiry %d: ExtensionsUsed = %d, want %d", i, used, i)
		}
		if f.state().Escalated {
			t.Fatalf("escalated after only %d extensions, want %d",
				used, freeze.DefaultMaxExtensions)
		}
	}
	if got := testutil.ToFloat64(obs.AnomalyFreezeExtensionsTotal) - beforeExt; got != float64(freeze.DefaultMaxExtensions) {
		t.Errorf("extensions counter delta = %v, want %d", got, freeze.DefaultMaxExtensions)
	}

	// One more expiry: the ladder is spent → escalate.
	f.tick(t, freeze.DefaultExtension+time.Minute)
	if !f.state().Escalated {
		t.Fatalf("did not escalate after %d extensions: %+v", freeze.DefaultMaxExtensions, f.state())
	}
	if got := testutil.ToFloat64(obs.AnomalyFreezeEscalatedTotal) - beforeEsc; got != 1 {
		t.Errorf("escalation counter delta = %v, want 1 (the P1 alert's only producer)", got)
	}
	if got := f.served(t); got != lkgFormatted {
		t.Errorf("served %q while escalated; want the LKG %q", got, lkgFormatted)
	}

	// An escalated freeze holds "until manual unfreeze" (ADR-0019), however healthy the pair looks.
	f.feed(t, lkgQuoteAmount, "soroswap")
	f.tick(t, closedBucket)
	f.tick(t, closedBucket)
	f.tick(t, freeze.DefaultExtension+time.Minute)
	if !f.state().Active() {
		t.Error("escalated freeze auto-unfroze")
	}
	if got := f.served(t); got != lkgFormatted {
		t.Errorf("escalated freeze published %q", got)
	}
}

// A freeze with a corroborating lens (trusted cross-oracle result) serves the
// ADR's 30 minutes; with no lens at all it serves 10, since false freezes
// concentrate there and bill the customer in stale LKG price.
func TestFreezeLifecycle_CorroborationScalesInitialHold(t *testing.T) {
	holdFor := func(t *testing.T, seedCrossOracle bool) (freeze.State, time.Duration) {
		t.Helper()
		f := newFreezeFixture(t)
		if seedCrossOracle {
			seedDivergence(t, f, divergence.CachedResult{
				PairID:         f.pair.String(),
				DivergencePct:  0.3, // inside tolerance
				SuccessCount:   3,   // above divergenceMinSources
				AgreementCount: 2,
			})
		}
		f.feed(t, manipQuoteAmount, "soroswap")
		f.tick(t, closedBucket)
		if !f.state().Active() {
			t.Fatal("setup: freeze did not fire")
		}
		return f.state(), f.state().HoldUntil.Sub(f.now)
	}

	uncorrState, uncorrHold := holdFor(t, false)
	corrState, corrHold := holdFor(t, true)

	if uncorrState.Corroborated {
		t.Error("pair with no cross-oracle and no triangulation read as corroborated")
	}
	if !corrState.Corroborated {
		t.Fatal("pair with a trusted cross-oracle result read as uncorroborated")
	}
	if uncorrHold != freeze.DefaultUncorroboratedInitialHold {
		t.Errorf("uncorroborated initial hold = %v, want %v",
			uncorrHold, freeze.DefaultUncorroboratedInitialHold)
	}
	if corrHold != freeze.DefaultInitialHold {
		t.Errorf("corroborated initial hold = %v, want ADR-0019's %v",
			corrHold, freeze.DefaultInitialHold)
	}
	if uncorrHold >= corrHold {
		t.Errorf("uncorroborated hold %v is not shorter than corroborated %v", uncorrHold, corrHold)
	}
}

// The marker's expiry must cover the remaining hold plus the silence grace,
// and the LKG value must outlive it: a short marker ends a freeze early under
// a stalled aggregator.
func TestFreezeLifecycle_MarkerTTLCoversTheHold(t *testing.T) {
	f := newFreezeFixture(t)
	f.feed(t, manipQuoteAmount, "soroswap")
	f.tick(t, closedBucket)

	if len(f.marker.marks) != 1 {
		t.Fatalf("marker written %d times, want 1", len(f.marker.marks))
	}
	m := f.marker.marks[0]
	want := freeze.DefaultUncorroboratedInitialHold + freeze.DefaultMarkerGrace
	if m.ttl != want {
		t.Errorf("marker TTL = %v, want hold+grace %v", m.ttl, want)
	}
	if m.frozenValue != lkgFormatted {
		t.Errorf("frozen value = %q, want the LKG %q", m.frozenValue, lkgFormatted)
	}
	if m.state.HoldUntil.IsZero() || m.state.FiredAt.IsZero() {
		t.Errorf("marker carries no lifecycle state: %+v", m.state)
	}
	if ttl := f.mr.TTL(f.vwapKey()); ttl != want {
		t.Errorf("LKG VWAP TTL = %v, want %v (it must outlive the marker)", ttl, want)
	}
}

// ADR-0019 operator override: deleting the marker force-unfreezes. The
// orchestrator must notice, or its in-memory ladder re-writes the marker.
func TestFreezeLifecycle_OperatorOverrideReleases(t *testing.T) {
	f := newFreezeFixture(t)
	before := testutil.ToFloat64(obs.AnomalyFreezeReleasedTotal.WithLabelValues("operator"))

	f.feed(t, manipQuoteAmount, "soroswap")
	f.tick(t, closedBucket)
	if !f.state().Active() {
		t.Fatal("setup: freeze did not fire")
	}

	// Operator clears the marker out of band, mid-hold.
	f.marker.present = false

	f.tick(t, closedBucket)
	if f.state().Active() {
		t.Fatalf("in-memory ladder survived the operator override: %+v", f.state())
	}
	if got := f.served(t); got != manipFormatted {
		t.Errorf("served %q after the override; want the fresh bucket %q", got, manipFormatted)
	}
	after := testutil.ToFloat64(obs.AnomalyFreezeReleasedTotal.WithLabelValues("operator"))
	if after-before != 1 {
		t.Errorf("AnomalyFreezeReleasedTotal{operator} delta = %v, want 1", after-before)
	}
}

// A restart mid-freeze must not restart the 2-hour escalation clock nor
// publish the refused bucket. The new process has no prev-VWAP comparator, so
// it holds on the marker state alone: an unscored bucket is no evidence of
// recovery.
func TestFreezeLifecycle_RehydratesLadderAcrossRestart(t *testing.T) {
	f := newFreezeFixture(t)
	f.feed(t, manipQuoteAmount, "soroswap")
	f.tick(t, closedBucket)
	f.tick(t, freeze.DefaultUncorroboratedInitialHold+time.Minute) // one extension
	if got := f.state().ExtensionsUsed; got != 1 {
		t.Fatalf("setup: ExtensionsUsed = %d, want 1", got)
	}

	restarted := f.restart(f.marker)
	f.tick(t, closedBucket)

	st := restarted.freezeStates[f.stateKey()]
	if !st.Active() {
		t.Fatal("restarted aggregator lost the freeze and would publish the manipulated bucket")
	}
	if st.ExtensionsUsed != 1 {
		t.Errorf("ExtensionsUsed = %d after restart, want 1 (the escalation clock restarted)", st.ExtensionsUsed)
	}
	if got := f.served(t); got != lkgFormatted {
		t.Errorf("restarted aggregator published %q, want the held LKG %q", got, lkgFormatted)
	}
}

// flakyLoadFreezeMarker fails the next loadErrs marker reads, like a Redis
// still loading its dataset on the first tick after a restart.
type flakyLoadFreezeMarker struct {
	*recordingFreezeMarker
	loadErrs int
}

func (m *flakyLoadFreezeMarker) LoadState(ctx context.Context, asset, quote canonical.Asset) (freeze.State, bool, error) {
	if m.loadErrs > 0 {
		m.loadErrs--
		return freeze.State{}, false, errors.New("LOADING Redis is loading the dataset in memory")
	}
	return m.recordingFreezeMarker.LoadState(ctx, asset, quote)
}

// A marker read that ERRORS on a restarted process's first evaluation is not
// evidence the pair is unfrozen. That bucket is unscored, so it cannot re-fire
// the freeze itself; caching "never frozen" published the withheld bucket and
// never re-read the marker.
func TestFreezeLifecycle_ColdMarkerReadErrorWithholdsAndRetries(t *testing.T) {
	f := newFreezeFixture(t)
	f.feed(t, manipQuoteAmount, "soroswap")
	f.tick(t, closedBucket)
	if !f.state().Active() {
		t.Fatal("setup: the manipulated bucket did not freeze the pair")
	}

	flaky := &flakyLoadFreezeMarker{recordingFreezeMarker: f.marker, loadErrs: 1}
	f.restart(flaky)

	f.tick(t, closedBucket)
	if got := f.served(t); got != lkgFormatted {
		t.Fatalf("marker read failed on a cold key and the aggregator published %q; want the held LKG %q", got, lkgFormatted)
	}
	if !f.marker.present {
		t.Error("a failed marker read cleared the freeze marker")
	}

	f.tick(t, closedBucket)
	if !f.state().Active() {
		t.Error("the marker was never re-read after the failed read: the freeze was not rehydrated")
	}
	if got := f.served(t); got != lkgFormatted {
		t.Errorf("after the rehydrate the aggregator published %q, want the held LKG %q", got, lkgFormatted)
	}
}

// Phase 1 (per-class deviation) and Phase 2 (per-asset baseline) share ONE
// lifecycle per freeze:<asset>:<quote> key. Two owners would let a Phase 1 fire
// truncate a Phase 2 hold to the silence grace, or hold a pair forever without
// escalating. A Phase 1 freeze must keep holding after Phase 1 stops flagging.
func TestFreezeLifecycle_Phase1SharesTheLadder(t *testing.T) {
	pair := xlmUsdtPair(t)
	cache, mr := newTestRedis(t)
	marker := &recordingFreezeMarker{}
	store := &mockStore{}
	o := New(store, cache, Config{
		Pairs:        []canonical.Pair{pair},
		Windows:      []time.Duration{5 * time.Minute},
		Interval:     time.Hour,
		Anomaly:      newAnomalyChecker(t, pair), // stablecoin: freeze at 2% deviation
		FreezeWriter: marker,
	})
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	o.clock = func() time.Time { return now }

	stateKey := pair.String() + ":" + (5 * time.Minute).String()
	o.prevVWAPs[stateKey] = big.NewRat(1, 1)
	cacheKey := cachekeys.VWAP(pair.Base, pair.Quote, 5*time.Minute).String()
	if err := cache.Set(context.Background(), cacheKey, "1.000000000000", time.Minute).Err(); err != nil {
		t.Fatalf("seed LKG: %v", err)
	}

	// 110% deviation on a single source → Phase 1 freezes.
	store.trades = []canonical.Trade{
		buildTrade(t, big.NewInt(100_000_000), big.NewInt(210_000_000), now),
	}
	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	st := o.freezeStates[stateKey]
	if !st.Active() {
		t.Fatal("Phase 1 freeze did not enter the lifecycle")
	}
	if got := st.HoldUntil.Sub(now); got != freeze.DefaultUncorroboratedInitialHold {
		t.Errorf("Phase 1 hold = %v, want the uncorroborated %v (no corroboration signal before the confidence step)",
			got, freeze.DefaultUncorroboratedInitialHold)
	}

	// Back at the LKG: Phase 1 now says Allow.
	store.trades = []canonical.Trade{
		buildTrade(t, big.NewInt(100_000_000), big.NewInt(100_000_000), now),
	}
	now = now.Add(30 * time.Second)
	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if !o.freezeStates[stateKey].Active() {
		t.Error("Phase 1 freeze ended the moment Phase 1 stopped flagging the pair")
	}
	if got, err := mr.Get(cacheKey); err != nil || got != "1.000000000000" {
		t.Errorf("cached VWAP = %q (err %v); the held LKG must not be overwritten", got, err)
	}
}

// windowRoutedStore returns a different trade fixture per WINDOW, keyed by the
// query range width, so one tick can drive two windows of a pair down divergent
// freeze paths (mockStore is per-pair only).
type windowRoutedStore struct {
	byWindow map[time.Duration][]canonical.Trade
}

func (s *windowRoutedStore) TradesInRange(
	_ context.Context, _ canonical.Pair, from, to time.Time, _ int,
) ([]canonical.Trade, error) {
	return s.byWindow[to.Sub(from)], nil
}

// newTwoWindowFreeze wires two windows (5m, 1h) for one pair, both seeded at
// the LKG, with a per-window store; the closures drive the shared clock.
func newTwoWindowFreeze(t *testing.T) (
	orch *Orchestrator,
	marker *recordingFreezeMarker,
	feed func(shortQuote, longQuote int64),
	tick func(d time.Duration),
	served func(w time.Duration) string,
	shortWindow, longWindow time.Duration,
	pair canonical.Pair,
) {
	t.Helper()
	shortWindow, longWindow = 5*time.Minute, time.Hour
	pair = xlmUSDPair(t)
	rdb, mr := newTestRedis(t)
	marker = &recordingFreezeMarker{}
	store := &windowRoutedStore{byWindow: map[time.Duration][]canonical.Trade{}}

	orch = New(store, rdb, Config{
		Pairs:        []canonical.Pair{pair},
		Windows:      []time.Duration{shortWindow, longWindow},
		Interval:     time.Hour,
		FreezeWriter: marker,
		Baselines: stubBaselineSource{
			multi: baseline.MultiBaseline{
				Day30: &baseline.Baseline{Median: 0, MAD: 0.001, N: maxDay30Returns},
			},
		},
	})
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	orch.clock = func() time.Time { return now }

	lkg := big.NewRat(lkgQuoteAmount, lkgBaseAmount)
	for _, w := range []time.Duration{shortWindow, longWindow} {
		orch.prevVWAPs[pair.String()+":"+w.String()] = new(big.Rat).Set(lkg)
		k := cachekeys.VWAP(pair.Base, pair.Quote, w).String()
		if err := rdb.Set(context.Background(), k, lkgFormatted, time.Hour).Err(); err != nil {
			t.Fatalf("seed LKG for %v: %v", w, err)
		}
	}

	feed = func(shortQuote, longQuote int64) {
		ts := now.Add(-10 * time.Second)
		store.byWindow[shortWindow] = []canonical.Trade{
			makeXLMUSDTrade(t, "soroswap", lkgBaseAmount, shortQuote, ts),
		}
		store.byWindow[longWindow] = []canonical.Trade{
			makeXLMUSDTrade(t, "soroswap", lkgBaseAmount, longQuote, ts),
		}
	}
	tick = func(d time.Duration) {
		now = now.Add(d)
		if err := orch.Tick(context.Background()); err != nil {
			t.Fatalf("Tick: %v", err)
		}
	}
	served = func(w time.Duration) string {
		got, err := mr.Get(cachekeys.VWAP(pair.Base, pair.Quote, w).String())
		if err != nil {
			t.Fatalf("cached VWAP missing for %v: %v", w, err)
		}
		return got
	}
	return orch, marker, feed, tick, served, shortWindow, longWindow, pair
}

// Regression: the marker, ladder and serving lookup are keyed by (asset,
// quote) but the lifecycle runs per WINDOW. A short-window auto-release must
// not clear the shared marker under a still-frozen longer window, or the next
// tick reads the absent marker as an operator override and publishes the
// manipulated long-window VWAP unflagged.
func TestFreezeLifecycle_SiblingWindowReleaseKeepsLongWindowFrozen(t *testing.T) {
	orch, marker, feed, tick, served, shortWindow, longWindow, pair := newTwoWindowFreeze(t)
	shortKey := pair.String() + ":" + shortWindow.String()
	longKey := pair.String() + ":" + longWindow.String()
	lkg := big.NewRat(lkgQuoteAmount, lkgBaseAmount)

	// References agree with the LKG: the short window's return is release-corroborated;
	// the long window's swinging manipulation never is.
	seedDivergenceCache(t, orch.cache, pair, agreeingLens(pair, 0.1242, 0.1242))

	// Tick 1: manipulated print on BOTH windows → both freeze.
	feed(manipQuoteAmount, manipQuoteAmount)
	tick(closedBucket)
	if !orch.freezeStates[shortKey].Active() || !orch.freezeStates[longKey].Active() {
		t.Fatalf("setup: both windows should freeze (short=%v long=%v)",
			orch.freezeStates[shortKey].Active(), orch.freezeStates[longKey].Active())
	}
	if !marker.present {
		t.Fatal("setup: freeze marker should be present after both windows froze")
	}

	// Short recovers (its first healthy bucket settles the level); the long
	// window keeps swinging, since a HELD level reads calm per tick.
	feed(lkgQuoteAmount, manipQuoteAmount)
	tick(closedBucket)
	// Past the 30-minute hold: short has streak=1, long fires on a fresh swing.
	feed(lkgQuoteAmount, manipQuoteAmount*3/2)
	tick(freeze.DefaultInitialHold + time.Minute)
	if !orch.freezeStates[shortKey].Active() {
		t.Fatal("short window released on a single healthy bucket; ADR-0019 needs two consecutive")
	}
	if !orch.freezeStates[longKey].Active() {
		t.Fatal("long window released while still manipulated")
	}

	// Short window's SECOND consecutive healthy bucket releases; long swings again.
	feed(lkgQuoteAmount, manipQuoteAmount*2)
	tick(closedBucket)

	if orch.freezeStates[shortKey].Active() {
		t.Fatal("short window did not auto-release after two healthy buckets past the hold")
	}

	// The shared marker, the long window's lifecycle and its LKG must survive.
	if !marker.present {
		t.Error("W3-freeze-1: short-window release cleared the shared freeze marker while the long window is frozen")
	}
	if !orch.freezeStates[longKey].Active() {
		t.Error("W3-freeze-1: long window read the cleared marker as an operator override")
	}
	if got := served(longWindow); got != lkgFormatted {
		t.Errorf("W3-freeze-1: long window published %q; want the held LKG %q", got, lkgFormatted)
	}
	if orch.prevVWAPs[longKey].Cmp(lkg) != 0 {
		t.Error("W3-freeze-1: long window's prev-VWAP comparator advanced off the LKG")
	}
}

// The sibling-active check on the shared marker Clear must not block an
// operator force-unfreeze: deleting the marker releases EVERY window, each
// observing the missing marker in loadFreezeState.
func TestFreezeLifecycle_OperatorOverrideReleasesAllWindows(t *testing.T) {
	orch, marker, feed, tick, _, shortWindow, longWindow, pair := newTwoWindowFreeze(t)
	shortKey := pair.String() + ":" + shortWindow.String()
	longKey := pair.String() + ":" + longWindow.String()

	feed(manipQuoteAmount, manipQuoteAmount)
	tick(closedBucket)
	if !orch.freezeStates[shortKey].Active() || !orch.freezeStates[longKey].Active() {
		t.Fatal("setup: both windows should be frozen")
	}

	// Operator deletes the marker out of band.
	marker.present = false

	feed(manipQuoteAmount, manipQuoteAmount)
	tick(closedBucket)

	if orch.freezeStates[shortKey].Active() {
		t.Error("operator override left the short window frozen")
	}
	if orch.freezeStates[longKey].Active() {
		t.Error("operator override left the LONG window frozen")
	}
}

// bandQuoteAmount is +2.5% over the LKG (0.127305), where the phases DISAGREE:
// Phase 1 (FreezePct 2%) fires on it, but Phase 2 (MAD 0.02) scores z = 1.25 < 3
// with confidence ~0.49 > 0.30, i.e. healthy.
const bandQuoteAmount = 127_305_000_000 // 0.127305, +2.5% vs the 0.1242 LKG

// Regression: a Phase 1 freeze must release once the pair is statistically
// healthy. Phase 1 measures against the pinned LKG comparator, so a residual
// level past FreezePct re-fires every bucket, short-circuiting the Phase 2
// confidence step that alone produces the auto-unfreeze streak; the pair would
// serve the LKG until escalation. Phase 2 now drives the lifecycle once a
// freeze is active.
func TestFreezeLifecycle_Phase1FreezeReleasesWhenAnomalyClears(t *testing.T) {
	pair := xlmUSDPair(t)
	// MAD 0.02: the 2.5% band bucket scores z = 1.25, below the auto-unfreeze bound.
	f := newFreezeFixtureWith(t, 0.02, func(c *Config) {
		c.Anomaly = newAnomalyChecker(t, pair) // native forced to stablecoin: FreezePct 2%
	})
	// References sit at the residual band level, so the release candidate is corroborated.
	seedDivergence(t, f, agreeingLens(pair, 0.1242, 0.127305))

	// A manipulated single-source spike: Phase 1 freezes.
	f.feed(t, manipQuoteAmount, "soroswap")
	f.tick(t, closedBucket)
	if !f.state().Active() {
		t.Fatal("setup: manipulated single-source bucket did not enter a Phase 1 freeze")
	}
	if got := f.served(t); got != lkgFormatted {
		t.Fatalf("setup: freeze published %q, want the LKG %q held", got, lkgFormatted)
	}

	// The anomaly clears to the band: healthy by Phase 2 but past Phase 1's 2%
	// threshold. Require a release within a bounded number of ticks; each tick
	// advances far enough that two healthy buckets straddle the initial hold.
	const maxTicks = 8
	released := false
	for i := 0; i < maxTicks; i++ {
		f.feed(t, bandQuoteAmount, "soroswap")
		f.tick(t, 6*time.Minute)
		if !f.state().Active() {
			released = true
			break
		}
	}
	if !released {
		t.Fatalf("W3-freeze-3: Phase 1 freeze never released after the anomaly cleared; stuck after %d ticks (state=%+v)",
			maxTicks, f.state())
	}

	// Released: the live band price is served (not the LKG), the marker is
	// cleared, and the comparator advances off the LKG so the freeze self-heals.
	wantBand := formatRatFixed(big.NewRat(bandQuoteAmount, lkgBaseAmount), 12)
	if got := f.served(t); got != wantBand {
		t.Errorf("after release served %q, want the freshly published live price %q (LKG was %q)",
			got, wantBand, lkgFormatted)
	}
	if f.marker.present {
		t.Error("freeze marker still present after release")
	}
	if f.orch.prevVWAPs[f.stateKey()].Cmp(big.NewRat(lkgQuoteAmount, lkgBaseAmount)) == 0 {
		t.Error("prev-VWAP comparator still pinned to the LKG after release; Phase 1 would re-fire")
	}
}

// XLM/GBP ratchet regression: scoring mid-freeze buckets against the PINNED
// pre-freeze prev makes z measure drift-since-freeze, so release is reachable
// only if the market round-trips. When it settles at a NEW level (~6% above LKG)
// and stays, ADR-0019's "is the market calm NOW" must release; frozenPrevVWAPs
// scores refused buckets per tick against the previous refused bucket.
func TestFreezeLifecycle_AutoUnfreezeAtANewStablePriceLevel(t *testing.T) {
	// 0.1317: +6.04% from the LKG. z ~ 60 against the pinned prev, 0 tick-over-tick.
	const driftedQuoteAmount = 131_700_000_000
	const driftedFormatted = "0.131700000000"

	f := newFreezeFixture(t)
	before := testutil.ToFloat64(obs.AnomalyFreezeReleasedTotal.WithLabelValues("auto"))
	// A genuine repricing carries the references with it: Median at the new level
	// while OurPrice is still the pinned LKG. That separates this release from the
	// held manipulation in ExtendsThenEscalates, whose references stay at the LKG.
	seedDivergence(t, f, agreeingLens(f.pair, 0.1242, 0.1317))

	f.feed(t, manipQuoteAmount, "soroswap")
	f.tick(t, closedBucket)
	if !f.state().Active() {
		t.Fatal("setup: freeze did not fire")
	}

	// The first bucket at the new level is a real jump: no streak, still refused.
	f.feed(t, driftedQuoteAmount, "soroswap")
	f.tick(t, closedBucket)
	if !f.state().Active() {
		t.Fatal("released inside the initial hold")
	}

	// Two buckets AT the new level, the second after the 30-minute hold expires.
	f.feed(t, driftedQuoteAmount, "soroswap")
	f.tick(t, freeze.DefaultInitialHold+time.Minute)
	f.feed(t, driftedQuoteAmount, "soroswap")
	f.tick(t, closedBucket)

	if f.state().Active() {
		t.Fatalf("still frozen after a NEW stable level held two buckets past the hold (drift ratchet): %+v", f.state())
	}
	if got := f.served(t); got != driftedFormatted {
		t.Errorf("served %q after release; want the freshly published new-level %q", got, driftedFormatted)
	}
	if f.marker.present {
		t.Error("freeze marker still present after release")
	}
	after := testutil.ToFloat64(obs.AnomalyFreezeReleasedTotal.WithLabelValues("auto"))
	if after-before != 1 {
		t.Errorf("AnomalyFreezeReleasedTotal{auto} delta = %v, want 1", after-before)
	}
}

// TestFreezeLifecycle_RefireAfterOverrideReturnsEscalated — the operator
// force-unfreezes an escalated pair and the anomaly is still live. The
// override must not zero the state: the re-fire would be a fresh 10-minute hold
// with extensions_used=0 that would not page again for two hours.
func TestFreezeLifecycle_RefireAfterOverrideReturnsEscalated(t *testing.T) {
	f := newFreezeFixture(t)
	escalated := freeze.State{
		FiredAt:        f.now.Add(-3 * time.Hour),
		HoldUntil:      f.now.Add(20 * time.Minute),
		ExtensionsUsed: freeze.DefaultMaxExtensions,
		Escalated:      true,
	}
	f.orch.freezeStates[f.stateKey()] = escalated
	f.marker.present, f.marker.state = true, escalated
	beforeEsc := testutil.ToFloat64(obs.AnomalyFreezeEscalatedTotal)
	beforeRefire := testutil.ToFloat64(obs.AnomalyFreezeRefiredAfterOverrideTotal)

	// Override: the marker goes; the next bucket publishes, as asked.
	f.marker.present = false
	f.feed(t, manipQuoteAmount, "soroswap")
	f.tick(t, closedBucket)
	if f.state().Active() {
		t.Fatalf("setup: the override did not release: %+v", f.state())
	}

	// Still anomalous one bucket later.
	f.feed(t, manipQuoteAmount*3/2, "soroswap")
	f.tick(t, closedBucket)
	st := f.state()
	if !st.Active() || !st.Escalated || st.ExtensionsUsed != freeze.DefaultMaxExtensions {
		t.Fatalf("re-fire after override = %+v, want the escalated ladder resumed", st)
	}
	if got := testutil.ToFloat64(obs.AnomalyFreezeEscalatedTotal) - beforeEsc; got != 1 {
		t.Errorf("escalation counter delta = %v, want 1: the P1 must page again", got)
	}
	if got := testutil.ToFloat64(obs.AnomalyFreezeRefiredAfterOverrideTotal) - beforeRefire; got != 1 {
		t.Errorf("refired-after-override counter delta = %v, want 1", got)
	}
}

// TestFreezeLifecycle_ReleaseModeTellsOperatorFromLapse — a live freeze
// whose marker and ladder are gone was always counted as mode="operator",
// so a lapse nobody performed polluted the series the on-call reads as the
// manual-unfreeze rate. freeze-unfreeze's tombstone is what separates them.
func TestFreezeLifecycle_ReleaseModeTellsOperatorFromLapse(t *testing.T) {
	for _, tc := range []struct {
		name      string
		tombstone bool
		want      string
	}{
		{name: "freeze_unfreeze", tombstone: true, want: "operator"},
		{name: "lapse", tombstone: false, want: "lapsed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newWiredFreezeFixture(t)
			w, ok := f.orch.cfg.FreezeWriter.(*freeze.Writer)
			if !ok {
				t.Fatal("setup: the wired fixture must use freeze.Writer")
			}
			f.feed(t, manipQuoteAmount, "soroswap")
			f.advance(t, closedBucket)
			if !f.state().Active() {
				t.Fatal("setup: freeze did not fire")
			}

			ctx := context.Background()
			if tc.tombstone {
				if err := w.RecordOverride(ctx, f.pair.Base, f.pair.Quote, "oncall", "verified by hand"); err != nil {
					t.Fatalf("RecordOverride: %v", err)
				}
			}
			if err := w.Clear(ctx, f.pair.Base, f.pair.Quote); err != nil {
				t.Fatalf("Clear: %v", err)
			}
			before := map[string]float64{}
			for _, m := range []string{"operator", "lapsed"} {
				before[m] = testutil.ToFloat64(obs.AnomalyFreezeReleasedTotal.WithLabelValues(m))
			}

			f.advance(t, closedBucket)
			if f.state().Active() {
				t.Fatalf("setup: the missing marker did not release: %+v", f.state())
			}
			for _, m := range []string{"operator", "lapsed"} {
				want := 0.0
				if m == tc.want {
					want = 1
				}
				if got := testutil.ToFloat64(obs.AnomalyFreezeReleasedTotal.WithLabelValues(m)) - before[m]; got != want {
					t.Errorf("AnomalyFreezeReleasedTotal{%s} delta = %v, want %v", m, got, want)
				}
			}
		})
	}
}

// TestFreezeLifecycle_ReleaseDuringRedisLossKeepsEscalatedSibling drives
// the real entry point. After a deploy the 5m window is live in memory and
// the 1h window — under the volume floor — has not reached the freeze step.
// Redis loses the marker, and the 5m window earns its release.
func TestFreezeLifecycle_ReleaseDuringRedisLossKeepsEscalatedSibling(t *testing.T) {
	f := newProductionWiredFixture(t)
	ctx := context.Background()
	_, recovering := f.freezeBoth(f.newOrch())

	o := f.newOrch() // the deploy: every key cold
	o.freezeStates[f.key(f.short)] = recovering
	f.flushRedis()

	refused := o.stepFreezeLifecycle(ctx, f.pair, f.short, f.key(f.short), healthySignal(),
		coldSiblingDecision())
	if refused {
		t.Fatal("setup: the 5m window was expected to earn its release on this tick")
	}
	if f.store.retired {
		t.Error("the 5m window's release retired the WHOLE durable record during a Redis loss")
	}
	if got := f.store.windows[f.long]; !got.Escalated || got.ExtensionsUsed != freeze.DefaultMaxExtensions {
		t.Errorf("1h durable ladder after the 5m release = %+v, want the escalated ladder intact", got)
	}
	if f.store.windows[f.short].Active() {
		t.Error("the released 5m window's durable ladder survived its release")
	}

	// The 1h window's next qualifying bucket: its first evaluation in this
	// process, against the only record left.
	st, overridden, err := o.loadFreezeState(ctx, f.pair, f.long, f.key(f.long))
	if err != nil {
		t.Fatalf("loadFreezeState: %v", err)
	}
	if overridden {
		t.Error("a Redis loss read as the operator override for the 1h window")
	}
	out := o.cfg.Phase2Thresholds.Lifecycle.Evaluate(st, healthySignal())
	if !st.Escalated || !out.Frozen {
		t.Errorf("1h window after its sibling's release: state=%+v Frozen=%v, want it escalated and "+
			"FROZEN — ADR-0019 holds an escalated freeze until manual unfreeze, and it published",
			st, out.Frozen)
	}
}

// TestFreezeLifecycle_OperatorOverrideStillSticksWithADurableRecord: asking
// the durable record before a clear must not make the override fail to
// take. `freeze-unfreeze` clears through the writer, which retires the
// durable record; every window then observes the absent marker, releases,
// and nothing re-freezes the pair.
func TestFreezeLifecycle_OperatorOverrideStillSticksWithADurableRecord(t *testing.T) {
	f := newProductionWiredFixture(t)
	ctx := context.Background()
	o := f.newOrch()
	escalated, recovering := f.freezeBoth(o)
	o.freezeStates[f.key(f.long)] = escalated
	o.freezeStates[f.key(f.short)] = recovering

	if err := o.cfg.FreezeWriter.Clear(ctx, f.pair.Base, f.pair.Quote); err != nil {
		t.Fatalf("operator Clear: %v", err)
	}

	for _, w := range []time.Duration{f.short, f.long} {
		if _, overridden, err := o.loadFreezeState(ctx, f.pair, w, f.key(w)); err != nil || !overridden {
			t.Fatalf("window %s did not observe the operator override", w)
		}
		o.releaseFreeze(ctx, f.pair, w, f.key(w), o.freezeStates[f.key(w)], freeze.TransitionOverridden)
		if o.freezeStates[f.key(w)].Active() {
			t.Errorf("window %s is still frozen in memory after the override", w)
		}
	}
	if len(f.store.windows) != 0 {
		t.Errorf("durable ladders after the override = %+v, want none", f.store.windows)
	}
	if st, present, _ := o.cfg.FreezeWriter.LoadState(ctx, f.pair.Base, f.pair.Quote); present {
		t.Errorf("the pair still reads as frozen after the override: %+v", st)
	}
}

// TestFreezeLifecycle_UnpricedBucketsKeepTheFreezeAlive — a frozen window
// whose buckets go empty or fall under MinUSDVolume must keep advancing its
// lifecycle. If those buckets returned before the lifecycle step,
// nothing would refresh the marker or the durable ladder; once hold + grace
// lapsed, the next priced bucket would read the absence as the operator
// override, release with mode="operator" and publish the manipulated
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
