package orchestrator

import (
	"context"
	"encoding/json"
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

// ─── ADR-0019 freeze-lifecycle fixtures ───────────────────────────
//
// One pair (native/fiat:USD, so USD volume is measurable), one 1m
// window, one source, and a baseline with MAD = 0.001 so a return of
// r maps to z = r / 0.001. Two bucket shapes:
//
//	lkgPrice      0.1242 → return 0, z 0,   confidence 0.4972
//	manipPrice    0.2000 → return 0.610, z 610, confidence 0.0000
//
// The healthy bucket clears BOTH legs of ADR-0019's auto-unfreeze
// condition (confidence > 0.30 AND z < 3.0) and the manipulated one
// clears all three legs of the freeze condition (confidence < 0.45
// AND z > 5.0 AND sources <= 1). Values measured against the real
// confidence combiner, not asserted from a table.
const (
	lkgBaseAmount    = 1_000_000_000_000 // 100,000 XLM at 1e7
	lkgQuoteAmount   = 124_200_000_000   // $12,420 at 1e7 → price 0.1242
	manipQuoteAmount = 200_000_000_000   // $20,000 at 1e7 → price 0.2000

	lkgFormatted   = "0.124200000000"
	manipFormatted = "0.200000000000"

	freezeTestWindow = time.Minute
)

// freezeFixture is the shared harness for the lifecycle tests: an
// orchestrator with a controllable clock, a swappable trade fixture,
// and direct access to the Redis + marker state the freeze path
// writes.
type freezeFixture struct {
	orch   *Orchestrator
	store  *mockStore
	marker *recordingFreezeMarker
	rdb    *redis.Client
	mr     *miniredis.Miniredis
	pair   canonical.Pair
	now    time.Time
}

// newFreezeFixture wires the harness with the LKG already in cache
// and in the prev-VWAP comparator slot, i.e. the state right after a
// healthy publish. Tests then feed it buckets.
func newFreezeFixture(t *testing.T) *freezeFixture {
	t.Helper()
	pair := xlmUSDPair(t)
	rdb, mr := newTestRedis(t)
	marker := &recordingFreezeMarker{}
	store := &mockStore{}

	orch := New(store, rdb, Config{
		Pairs:        []canonical.Pair{pair},
		Windows:      []time.Duration{freezeTestWindow},
		Interval:     time.Hour,
		FreezeWriter: marker,
		Baselines: stubBaselineSource{
			multi: baseline.MultiBaseline{
				// N large enough that BaselineQualityFactor is 1.0
				// (60000 / 1440 ≈ 41.7 "days" of bucket density), so
				// the bootstrap cap doesn't muddy the arithmetic.
				Day30: &baseline.Baseline{Median: 0, MAD: 0.001, N: 60_000},
			},
		},
	})

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

	// Seed the LKG: prev-VWAP comparator slot + the cached value the
	// API would be serving.
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

// feed sets the window's trades: one trade per source, all at
// quote/base = the requested price.
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

// TestFreezeLifecycle_SingleCleanBucketDoesNotRelease is the
// regression that motivated the whole lifecycle (N-F6).
//
// The attack: freeze the pair on a manipulated single-source bucket,
// then produce ONE bucket carrying a second source at the SAME
// manipulated price. Pre-lifecycle, the freeze's release condition
// was the negation of its fire condition evaluated on that single
// bucket — `source_count <= 1` stops holding the moment any second
// venue prints, whatever the price is doing — so the marker stopped
// being refreshed and the orchestrator published the manipulated VWAP
// it had refused one bucket earlier. An attacker needed one trade on
// one other venue (or a lull that let z drift from 5.1 to 4.9) to
// clear a freeze while the manipulation was still in force.
//
// ADR-0019 §"Freeze duration" says a freeze holds for a MINIMUM of
// its initial hold and is released only by the auto-unfreeze
// condition — confidence > 0.30 AND z < 3.0 for two CONSECUTIVE
// buckets, once that minimum has been served. A price still 61% away
// from the last-known-good satisfies neither leg.
func TestFreezeLifecycle_SingleCleanBucketDoesNotRelease(t *testing.T) {
	f := newFreezeFixture(t)

	// Bucket 1: single-source manipulated print → freeze fires.
	f.feed(t, manipQuoteAmount, "soroswap")
	f.tick(t, 30*time.Second)
	if !f.state().Active() {
		t.Fatal("setup: manipulated single-source bucket did not freeze")
	}
	if got := f.served(t); got != lkgFormatted {
		t.Fatalf("setup: freeze published %q, want the LKG %q", got, lkgFormatted)
	}

	// Bucket 2: same manipulated price, now printed on TWO venues.
	// source_count = 2 breaks the 3-signal AND, so the pre-lifecycle
	// code published this bucket.
	f.feed(t, manipQuoteAmount, "soroswap", "phoenix")
	f.tick(t, 30*time.Second)

	if got := f.served(t); got != lkgFormatted {
		t.Errorf("a single second-source bucket released the freeze and published %q; "+
			"want the last-known-good %q held — ADR-0019 releases only on "+
			"confidence > 0.30 AND z < 3.0 for two consecutive buckets past its "+
			"minimum hold, "+
			"and this bucket is still 61%% away from the LKG", got, lkgFormatted)
	}
	if !f.state().Active() {
		t.Error("freeze state cleared on a bucket that met no auto-unfreeze condition")
	}
	if f.orch.prevVWAPs[f.stateKey()].Cmp(big.NewRat(lkgQuoteAmount, lkgBaseAmount)) != 0 {
		t.Error("prev-VWAP comparator moved to the manipulated price — the next " +
			"bucket would score the manipulation as the new normal")
	}
}

// TestFreezeLifecycle_HoldSurvivesAHealthyBucket — the same shape as
// the test above but with the price fully back at the last-known-good
// value, which is the case pre-lifecycle code released on FASTEST
// (all three legs of the AND clear at once).
//
// ADR-0019 still holds: the initial hold is a minimum, and one
// healthy bucket is half of the two the auto-unfreeze condition
// requires. Releasing here is how a held manipulation walks free —
// the attacker parks the price, returns are flat, and every
// per-bucket signal reads healthy.
func TestFreezeLifecycle_HoldSurvivesAHealthyBucket(t *testing.T) {
	f := newFreezeFixture(t)

	f.feed(t, manipQuoteAmount, "soroswap")
	f.tick(t, 30*time.Second)
	if !f.state().Active() {
		t.Fatal("setup: freeze did not fire")
	}

	// Perfectly healthy bucket: z = 0, confidence 0.4972.
	f.feed(t, lkgQuoteAmount, "soroswap")
	f.tick(t, 30*time.Second)

	if got := f.served(t); got != lkgFormatted {
		t.Errorf("served %q after one healthy bucket", got)
	}
	if !f.state().Active() {
		t.Error("freeze released after ONE healthy bucket; ADR-0019 requires two " +
			"consecutive AND the initial hold to have elapsed")
	}
	if got := f.state().UnfreezeStreak; got != 1 {
		t.Errorf("UnfreezeStreak = %d after one healthy bucket, want 1", got)
	}
}

// TestFreezeLifecycle_AutoUnfreezeAfterTwoHealthyBucketsPastTheHold —
// the release path. Past the initial hold, with the auto-unfreeze
// condition met on the two most recent buckets, the freeze ends: the
// fresh value publishes, the marker is DELETED (not left to lapse on
// its remaining-hold TTL, which would keep flags.frozen true long
// after the price was republished), and the release is counted.
func TestFreezeLifecycle_AutoUnfreezeAfterTwoHealthyBucketsPastTheHold(t *testing.T) {
	f := newFreezeFixture(t)
	before := testutil.ToFloat64(obs.AnomalyFreezeReleasedTotal.WithLabelValues("auto"))

	f.feed(t, manipQuoteAmount, "soroswap")
	f.tick(t, 30*time.Second)
	if !f.state().Active() {
		t.Fatal("setup: freeze did not fire")
	}

	// Two healthy buckets, the second landing after the initial hold
	// (uncorroborated → 10 minutes) has expired.
	f.feed(t, lkgQuoteAmount, "soroswap")
	f.tick(t, freeze.DefaultUncorroboratedInitialHold+time.Minute)
	if !f.state().Active() {
		t.Fatal("released at expiry on a streak of one — the ADR wants two consecutive")
	}
	f.tick(t, 30*time.Second)

	if f.state().Active() {
		t.Fatalf("still frozen after two consecutive healthy buckets past the hold: %+v", f.state())
	}
	if got := f.served(t); got != lkgFormatted {
		t.Errorf("served %q after release; want the freshly published %q", got, lkgFormatted)
	}
	if f.marker.present {
		t.Error("freeze marker still present after release — flags.frozen would stay " +
			"true for the marker's remaining-hold TTL")
	}
	if f.marker.clears == 0 {
		t.Error("release did not Clear the marker")
	}
	after := testutil.ToFloat64(obs.AnomalyFreezeReleasedTotal.WithLabelValues("auto"))
	if after-before != 1 {
		t.Errorf("AnomalyFreezeReleasedTotal{auto} delta = %v, want 1", after-before)
	}
}

// TestFreezeLifecycle_ExtendsThenEscalates — the ladder. A
// manipulation the attacker simply HOLDS produces a pair that never
// earns its release, so each hold expiry grants one 30-minute
// extension until the four are spent, at which point the freeze
// escalates to operator review and stops auto-unfreezing.
//
// This is the case the pre-lifecycle code had no answer for at all:
// it neither bounded the freeze nor ever told an operator that one
// had been running for two hours.
func TestFreezeLifecycle_ExtendsThenEscalates(t *testing.T) {
	f := newFreezeFixture(t)
	beforeExt := testutil.ToFloat64(obs.AnomalyFreezeExtensionsTotal)
	beforeEsc := testutil.ToFloat64(obs.AnomalyFreezeEscalatedTotal)

	f.feed(t, manipQuoteAmount, "soroswap")
	f.tick(t, 30*time.Second)

	// Walk the ladder: each step lands just past the current hold.
	f.tick(t, freeze.DefaultUncorroboratedInitialHold+time.Minute)
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

	// An escalated freeze does NOT auto-unfreeze: ADR-0019 holds it
	// "until manual unfreeze", however healthy the pair now looks.
	f.feed(t, lkgQuoteAmount, "soroswap")
	f.tick(t, 30*time.Second)
	f.tick(t, 30*time.Second)
	f.tick(t, freeze.DefaultExtension+time.Minute)
	if !f.state().Active() {
		t.Error("escalated freeze auto-unfroze; ADR-0019 requires operator action")
	}
	if got := f.served(t); got != lkgFormatted {
		t.Errorf("escalated freeze published %q", got)
	}
}

// TestFreezeLifecycle_CorroborationScalesInitialHold pins the
// deliberate deviation from ADR-0019's flat 30-minute initial hold.
//
// A freeze taken with a corroborating lens available (here: a cached
// cross-oracle result above the trust floor) serves the ADR's full 30
// minutes. A freeze on a pair with no lens at all — one venue, its
// own history, nothing else — serves 10, because that population is
// where false freezes concentrate and a false freeze bills the
// customer 100% of its duration in stale last-known-good price.
func TestFreezeLifecycle_CorroborationScalesInitialHold(t *testing.T) {
	holdFor := func(t *testing.T, seedCrossOracle bool) (freeze.State, time.Duration) {
		t.Helper()
		f := newFreezeFixture(t)
		if seedCrossOracle {
			body, err := json.Marshal(divergence.CachedResult{
				PairID:         f.pair.String(),
				DivergencePct:  0.3, // inside tolerance
				SuccessCount:   3,   // above divergenceMinSources
				AgreementCount: 2,
			})
			if err != nil {
				t.Fatalf("seed marshal: %v", err)
			}
			if err := f.rdb.Set(context.Background(),
				cachekeys.Divergence(f.pair).String(), body, time.Hour).Err(); err != nil {
				t.Fatalf("seed divergence: %v", err)
			}
		}
		f.feed(t, manipQuoteAmount, "soroswap")
		f.tick(t, 30*time.Second)
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
		t.Errorf("uncorroborated hold %v is not shorter than corroborated %v — the "+
			"thin-pair false-freeze cost is not being scaled at all", uncorrHold, corrHold)
	}
}

// TestFreezeLifecycle_MarkerTTLCoversTheHold — the marker's expiry
// must cover the remaining hold plus the silence grace, and the
// last-known-good value must survive at least as long. A 5-minute
// marker on a 30-minute hold is how a freeze silently ends early
// under a stalled aggregator; a 5-minute LKG under a 30-minute freeze
// is F-1345 all over again (frozen=true with nothing to serve).
func TestFreezeLifecycle_MarkerTTLCoversTheHold(t *testing.T) {
	f := newFreezeFixture(t)
	f.feed(t, manipQuoteAmount, "soroswap")
	f.tick(t, 30*time.Second)

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
		t.Errorf("marker carries no lifecycle state: %+v — an operator dumping "+
			"freeze:* keys cannot see where on the ladder the pair is", m.state)
	}
	if ttl := f.mr.TTL(f.vwapKey()); ttl != want {
		t.Errorf("LKG VWAP TTL = %v, want %v (it must outlive the marker)", ttl, want)
	}
}

// TestFreezeLifecycle_OperatorOverrideReleases — ADR-0019: "Operator
// override always available: force unfreeze". Deleting the marker is
// that override. Without the orchestrator noticing, its in-memory
// ladder would simply re-write the marker on the next tick and the
// operator's action would silently not stick.
func TestFreezeLifecycle_OperatorOverrideReleases(t *testing.T) {
	f := newFreezeFixture(t)
	before := testutil.ToFloat64(obs.AnomalyFreezeReleasedTotal.WithLabelValues("operator"))

	f.feed(t, manipQuoteAmount, "soroswap")
	f.tick(t, 30*time.Second)
	if !f.state().Active() {
		t.Fatal("setup: freeze did not fire")
	}

	// Operator clears the marker out of band, mid-hold.
	f.marker.present = false

	f.tick(t, 30*time.Second)
	if f.state().Active() {
		t.Fatalf("in-memory ladder survived the operator override: %+v", f.state())
	}
	if got := f.served(t); got != manipFormatted {
		t.Errorf("served %q after the override; the operator asked for the fresh "+
			"bucket %q to publish", got, manipFormatted)
	}
	after := testutil.ToFloat64(obs.AnomalyFreezeReleasedTotal.WithLabelValues("operator"))
	if after-before != 1 {
		t.Errorf("AnomalyFreezeReleasedTotal{operator} delta = %v, want 1", after-before)
	}
}

// TestFreezeLifecycle_RehydratesLadderAcrossRestart — a deploy or
// crash mid-freeze must not restart the 2-hour escalation clock, and
// must not publish the bucket the previous process was refusing.
//
// The restarted process has no prev-VWAP comparator, so its first
// bucket cannot be scored at all: it must hold on the marker's state
// alone. An unscored bucket is the absence of evidence, not evidence
// of recovery.
func TestFreezeLifecycle_RehydratesLadderAcrossRestart(t *testing.T) {
	f := newFreezeFixture(t)
	f.feed(t, manipQuoteAmount, "soroswap")
	f.tick(t, 30*time.Second)
	f.tick(t, freeze.DefaultUncorroboratedInitialHold+time.Minute) // one extension
	if got := f.state().ExtensionsUsed; got != 1 {
		t.Fatalf("setup: ExtensionsUsed = %d, want 1", got)
	}

	// Restart: brand-new Orchestrator, same Redis + same marker.
	restarted := New(f.store, f.rdb, Config{
		Pairs:        []canonical.Pair{f.pair},
		Windows:      []time.Duration{freezeTestWindow},
		Interval:     time.Hour,
		FreezeWriter: f.marker,
		Baselines: stubBaselineSource{
			multi: baseline.MultiBaseline{
				Day30: &baseline.Baseline{Median: 0, MAD: 0.001, N: 60_000},
			},
		},
	})
	restarted.clock = func() time.Time { return f.now }
	f.orch = restarted

	f.tick(t, 30*time.Second)

	st := restarted.freezeStates[f.stateKey()]
	if !st.Active() {
		t.Fatal("restarted aggregator lost the freeze and would publish the manipulated bucket")
	}
	if st.ExtensionsUsed != 1 {
		t.Errorf("ExtensionsUsed = %d after restart, want 1 — the escalation clock "+
			"restarted, so a restart cadence under 2h would never page anyone",
			st.ExtensionsUsed)
	}
	if got := f.served(t); got != lkgFormatted {
		t.Errorf("restarted aggregator published %q, want the held LKG %q", got, lkgFormatted)
	}
}

// TestFreezeLifecycle_Phase1SharesTheLadder — Phase 1 (the per-class
// deviation stop-gap) and Phase 2 (the per-asset baseline) share ONE
// lifecycle per `freeze:<asset>:<quote>` key.
//
// Two owners of one marker would be a correctness bug, not untidiness:
// a Phase 1 fire writing the flat-TTL marker would truncate a Phase 2
// hold to the silence grace, and a Phase 1 freeze that never advanced
// the ladder would hold a pair indefinitely without ever escalating it
// to an operator. This test pins the visible half: a Phase 1 freeze
// keeps holding after Phase 1 itself stops flagging the pair.
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
		t.Errorf("Phase 1 hold = %v, want the uncorroborated %v (Phase 1 runs before "+
			"the confidence step, so it has no corroboration signal)",
			got, freeze.DefaultUncorroboratedInitialHold)
	}

	// Next bucket is back at the LKG: Phase 1 now says Allow, and
	// pre-lifecycle that was the end of the freeze.
	store.trades = []canonical.Trade{
		buildTrade(t, big.NewInt(100_000_000), big.NewInt(100_000_000), now),
	}
	now = now.Add(30 * time.Second)
	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if !o.freezeStates[stateKey].Active() {
		t.Error("Phase 1 freeze ended the moment Phase 1 stopped flagging the pair — " +
			"the ADR-0019 hold must outlive the per-bucket decision")
	}
	if got, err := mr.Get(cacheKey); err != nil || got != "1.000000000000" {
		t.Errorf("cached VWAP = %q (err %v); the held LKG must not be overwritten", got, err)
	}
}

// TestFreezeLifecycle_ActiveGaugeTracksHeldFreezes — the gauge the
// freeze-active rule reads. It must fall back to zero on release,
// not stay latched: an operator reading a stuck "1 frozen" would
// treat every later freeze as pre-existing.
func TestFreezeLifecycle_ActiveGaugeTracksHeldFreezes(t *testing.T) {
	f := newFreezeFixture(t)

	f.feed(t, manipQuoteAmount, "soroswap")
	f.tick(t, 30*time.Second)
	if got := testutil.ToFloat64(obs.AnomalyFreezeActive); got != 1 {
		t.Errorf("AnomalyFreezeActive = %v while one pair is frozen, want 1", got)
	}

	f.feed(t, lkgQuoteAmount, "soroswap")
	f.tick(t, freeze.DefaultUncorroboratedInitialHold+time.Minute)
	f.tick(t, 30*time.Second)
	if f.state().Active() {
		t.Fatal("setup: expected release")
	}
	if got := testutil.ToFloat64(obs.AnomalyFreezeActive); got != 0 {
		t.Errorf("AnomalyFreezeActive = %v after release, want 0", got)
	}
}
