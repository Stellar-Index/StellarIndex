package orchestrator

import (
	"context"
	"encoding/json"
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
// seedDivergence installs a cross-oracle divergence result in the
// fixture's redis at the key lookupCrossOracle reads, mirroring what the
// divergence worker publishes in production.
func seedDivergence(t *testing.T, f *freezeFixture, res divergence.CachedResult) {
	t.Helper()
	seedDivergenceCache(t, f.rdb, f.pair, res)
}

// seedDivergenceCache is seedDivergence for harnesses that don't use
// freezeFixture (the two-window and Phase-1 tests build their own).
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

// agreeingLens is the divergence cache entry production writes when the
// corroborating references sit AT the given level and we are serving a
// price at servedPrice: OurPrice is the SERVED price (the divergence
// worker reads what the API serves — mid-freeze that is the pinned
// LKG, never the refused candidate), Median is where the references
// are, and DivergencePct relates the two. The release gate
// (releaseCorroborated) reads only Median and compares it against the
// FRESH candidate itself — that asymmetry is the 2026-08-24
// corroborated-release repair.
func agreeingLens(pair canonical.Pair, servedPrice, level float64) divergence.CachedResult {
	pct := 0.0
	if level > 0 {
		pct = math.Abs(servedPrice-level) / level * 100
	}
	return divergence.CachedResult{
		PairID:         pair.String(),
		OurPrice:       servedPrice,
		Median:         level,
		DivergencePct:  pct,
		SuccessCount:   3, // ≥ divergenceMinSources: trusted multi-reference signal
		AgreementCount: 2,
	}
}

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
	// References agree with the LKG throughout, so the return TO the LKG
	// is corroborated and may earn the streak (2026-08-24: an
	// uncorroboratable calm bucket no longer counts — see
	// Signal.ReleaseCorroborated).
	seedDivergence(t, f, agreeingLens(f.pair, 0.1242, 0.1242))

	f.feed(t, manipQuoteAmount, "soroswap")
	f.tick(t, 30*time.Second)
	if !f.state().Active() {
		t.Fatal("setup: freeze did not fire")
	}

	// The first bucket back at LKG is itself a JUMP tick-over-tick
	// (manip -> LKG against the shadow comparator), so it cannot start
	// the calm streak; the second settled bucket does. Both must stay
	// refused (the hold is active either way).
	f.feed(t, lkgQuoteAmount, "soroswap")
	f.tick(t, 30*time.Second)
	if got := f.state().UnfreezeStreak; got != 0 {
		t.Errorf("UnfreezeStreak = %d on the transition bucket, want 0 (it is a jump)", got)
	}
	f.feed(t, lkgQuoteAmount, "soroswap")
	f.tick(t, 30*time.Second)

	if got := f.served(t); got != lkgFormatted {
		t.Errorf("served %q during the hold", got)
	}
	if !f.state().Active() {
		t.Error("freeze released inside the initial hold; ADR-0019 requires the hold " +
			"to elapse regardless of the streak")
	}
	if got := f.state().UnfreezeStreak; got != 1 {
		t.Errorf("UnfreezeStreak = %d after one settled bucket, want 1", got)
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

	// References agree with the LKG: the release candidate (back at the
	// LKG) is corroborated, which the streak requires since 2026-08-24.
	// The lens also makes the freeze itself corroborated → the full
	// 30-minute ADR hold.
	seedDivergence(t, f, agreeingLens(f.pair, 0.1242, 0.1242))

	f.feed(t, manipQuoteAmount, "soroswap")
	f.tick(t, 30*time.Second)
	if !f.state().Active() {
		t.Fatal("setup: freeze did not fire")
	}

	// Settling bucket first: the return TO the healthy level is itself a
	// jump tick-over-tick and must not count toward the streak.
	f.feed(t, lkgQuoteAmount, "soroswap")
	f.tick(t, 30*time.Second)
	// Two settled healthy buckets, the second landing after the initial
	// hold (corroborated → 30 minutes) has expired.
	f.tick(t, freeze.DefaultInitialHold+time.Minute)
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

// TestFreezeLifecycle_NoLensMeansNoAutoRelease — the uncorroboratable
// half of the 2026-08-24 corroborated-release repair. A pair with NO
// corroborating lens (no cross-oracle divergence entry, no
// triangulation chain) cannot prove that a calm, healthy-looking level
// is the market rather than a parked manipulation — the shadow
// comparator makes ANY held level read calm. So the calm streak must
// not accumulate and the freeze must walk the ladder to an operator
// instead of auto-releasing, matching the ADR's escalation posture for
// pairs the automatic layer cannot judge.
func TestFreezeLifecycle_NoLensMeansNoAutoRelease(t *testing.T) {
	f := newFreezeFixture(t)

	f.feed(t, manipQuoteAmount, "soroswap")
	f.tick(t, 30*time.Second)
	if !f.state().Active() {
		t.Fatal("setup: freeze did not fire")
	}

	// The same shape that releases in the corroborated sibling test:
	// settle at the LKG, then two calm buckets past the (uncorroborated →
	// 10-minute) hold. Without a lens the streak must stay at zero and
	// the freeze must hold.
	f.feed(t, lkgQuoteAmount, "soroswap")
	f.tick(t, 30*time.Second)
	f.tick(t, freeze.DefaultUncorroboratedInitialHold+time.Minute)
	f.tick(t, 30*time.Second)

	if !f.state().Active() {
		t.Fatal("a pair with no corroborating lens auto-released — an uncorroboratable " +
			"calm bucket is not evidence of recovery (the shadow comparator reads any " +
			"held level as calm), so release requires a lens that agrees with the candidate")
	}
	if got := f.state().UnfreezeStreak; got != 0 {
		t.Errorf("UnfreezeStreak = %d with no lens, want 0", got)
	}
	if got := f.served(t); got != lkgFormatted {
		t.Errorf("served %q; want the held LKG %q", got, lkgFormatted)
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
//
// 2026-08-24 (frozenPrevVWAPs + corroborated release): a HELD level is
// per-tick CALM (z=0), so the z leg alone no longer distinguishes
// "attacker holds the manip" from "market settled at a new level". The
// discrimination is the release gate: the streak counts only buckets
// whose FRESH candidate a corroborating lens agrees with
// (Signal.ReleaseCorroborated). This test seeds the divergence state
// production actually reaches mid-freeze — the worker compares the
// references against the SERVED price, which is the pinned LKG, so
// OurPrice = Median = LKG and DivergencePct ≈ 0 even while the attacker
// holds a 61%-off level. Under that state both legacy release legs pass
// (calm z, healthy confidence): pre-repair the held manipulation walked
// free at hold expiry; post-repair the candidate-vs-median check (0.2000
// vs 0.1242 → 61% ≫ 5%) refuses the streak and the ladder escalates to
// an operator. (A held manip that ALSO drags every reference with it is
// out of scope for the automatic layer, per the ADR's escalation
// rationale.)
func TestFreezeLifecycle_ExtendsThenEscalates(t *testing.T) {
	f := newFreezeFixture(t)
	beforeExt := testutil.ToFloat64(obs.AnomalyFreezeExtensionsTotal)
	beforeEsc := testutil.ToFloat64(obs.AnomalyFreezeEscalatedTotal)

	// The REACHABLE mid-freeze lens state: references at the LKG,
	// compared against the served LKG → agreement. (Seeding the
	// candidate-vs-references divergence here would be the inverted-
	// evidence mistake the 2026-08-24 panel rejected: mid-freeze the
	// worker never sees the refused candidate.)
	seedDivergence(t, f, agreeingLens(f.pair, 0.1242, 0.1242))

	f.feed(t, manipQuoteAmount, "soroswap")
	f.tick(t, 30*time.Second)

	// Walk the ladder: each step lands just past the current hold. The
	// seeded divergence result means a lens WAS consulted, so the freeze
	// is CORROBORATED and serves the full 30-minute initial hold.
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

// windowRoutedStore returns a different trade fixture per WINDOW,
// keyed by the query range width (to - from, which refreshPairWindow
// sets to exactly the window). The package's mockStore is per-PAIR
// only, so it can't drive two windows of the same pair down divergent
// freeze paths in one tick — which is precisely the W3-freeze-1
// scenario.
type windowRoutedStore struct {
	byWindow map[time.Duration][]canonical.Trade
}

func (s *windowRoutedStore) TradesInRange(
	_ context.Context, _ canonical.Pair, from, to time.Time, _ int,
) ([]canonical.Trade, error) {
	return s.byWindow[to.Sub(from)], nil
}

// newTwoWindowFreeze wires an orchestrator with two windows (5m, 1h)
// for one pair, both seeded at the last-known-good, plus a per-window
// store. Returned closures drive the shared clock and read state.
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
				Day30: &baseline.Baseline{Median: 0, MAD: 0.001, N: 60_000},
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

// TestFreezeLifecycle_SiblingWindowReleaseKeepsLongWindowFrozen is the
// W3-freeze-1 regression: the freeze marker + durable ladder + serving
// Looker are keyed by (asset, quote) but the freeze lifecycle runs per
// WINDOW. When a short window auto-releases, it must NOT clear the
// shared marker out from under a longer window that is still frozen —
// otherwise the next tick reads the absent marker as an operator
// override and republishes the still-manipulated long-window VWAP with
// flags.frozen=false, defeating the freeze in exactly the
// thin/single-source case it exists for.
//
// The attack shape: manipulate a pair so BOTH the 5m and 1h windows
// freeze; recover the 5m print (short window earns its auto-unfreeze)
// while parking the 1h price high. Pre-fix, the 5m release's Clear
// deletes the pair-global marker and the still-frozen 1h window then
// publishes the manipulated price unflagged.
func TestFreezeLifecycle_SiblingWindowReleaseKeepsLongWindowFrozen(t *testing.T) {
	orch, marker, feed, tick, served, shortWindow, longWindow, pair := newTwoWindowFreeze(t)
	shortKey := pair.String() + ":" + shortWindow.String()
	longKey := pair.String() + ":" + longWindow.String()
	lkg := big.NewRat(lkgQuoteAmount, lkgBaseAmount)

	// References agree with the LKG: the short window's return there is
	// release-corroborated; the long window's swinging manipulation never
	// is (candidate ≫ 5% from the median).
	seedDivergenceCache(t, orch.cache, pair, agreeingLens(pair, 0.1242, 0.1242))

	// Tick 1: manipulated print on BOTH windows → both freeze.
	feed(manipQuoteAmount, manipQuoteAmount)
	tick(30 * time.Second)
	if !orch.freezeStates[shortKey].Active() || !orch.freezeStates[longKey].Active() {
		t.Fatalf("setup: both windows should freeze (short=%v long=%v)",
			orch.freezeStates[shortKey].Active(), orch.freezeStates[longKey].Active())
	}
	if !marker.present {
		t.Fatal("setup: freeze marker should be present after both windows froze")
	}

	// The short window recovers; the long window stays ACTIVELY
	// manipulated — its price keeps swinging (per-tick semantics: a
	// HELD level reads calm, so a long-window freeze that must persist
	// here needs the manipulation to stay visible tick-over-tick; the
	// held-level case is the escalation test's job, discriminated by
	// the confidence leg). The short window's FIRST healthy bucket is
	// a jump vs the shadow comparator and settles the level.
	feed(lkgQuoteAmount, manipQuoteAmount)
	tick(30 * time.Second)
	// Tick past the 30-minute corroborated hold: short earns
	// streak=1 (held), long keeps firing on a fresh swing — both
	// still frozen.
	feed(lkgQuoteAmount, manipQuoteAmount*3/2)
	tick(freeze.DefaultInitialHold + time.Minute)
	if !orch.freezeStates[shortKey].Active() {
		t.Fatal("short window released on a single healthy bucket; ADR-0019 needs two consecutive")
	}
	if !orch.freezeStates[longKey].Active() {
		t.Fatal("long window released while still manipulated")
	}

	// Tick 3: the short window's SECOND consecutive healthy bucket →
	// it auto-releases. The long window swings again and stays frozen.
	feed(lkgQuoteAmount, manipQuoteAmount*2)
	tick(30 * time.Second)

	// The short window releases in both the buggy and fixed code — that
	// is not the defect.
	if orch.freezeStates[shortKey].Active() {
		t.Fatal("short window did not auto-release after two healthy buckets past the hold")
	}

	// W3-freeze-1: the long window is still frozen, so the shared marker
	// the API reads for flags.frozen MUST still be present, the long
	// window's lifecycle MUST still be Active, and the long window MUST
	// NOT publish its manipulated VWAP.
	if !marker.present {
		t.Error("W3-freeze-1: short-window auto-release cleared the shared (asset,quote) freeze " +
			"marker while the long window is still frozen — the API's FrozenForPair now serves " +
			"flags.frozen=false for the still-frozen long window")
	}
	if !orch.freezeStates[longKey].Active() {
		t.Error("W3-freeze-1: long window read the cleared marker as an operator override and " +
			"dropped its still-live freeze")
	}
	if got := served(longWindow); got != lkgFormatted {
		t.Errorf("W3-freeze-1: long window published %q; want the held last-known-good %q — the "+
			"manipulated price was served with flags.frozen=false", got, lkgFormatted)
	}
	if orch.prevVWAPs[longKey].Cmp(lkg) != 0 {
		t.Error("W3-freeze-1: long window's prev-VWAP comparator advanced off the LKG — the " +
			"manipulation became the new baseline and the freeze cannot self-heal")
	}
}

// TestFreezeLifecycle_OperatorOverrideReleasesAllWindows guards the
// W3-freeze-1 fix's edge case: the sibling-active check on the shared
// marker Clear must NOT block a genuine operator force-unfreeze.
// Deleting the marker out of band is the ADR-0019 override, and it
// releases EVERY window for the pair — each observes the missing marker
// independently in loadFreezeState — not just whichever window's
// releaseFreeze happens to reach the Clear.
func TestFreezeLifecycle_OperatorOverrideReleasesAllWindows(t *testing.T) {
	orch, marker, feed, tick, _, shortWindow, longWindow, pair := newTwoWindowFreeze(t)
	shortKey := pair.String() + ":" + shortWindow.String()
	longKey := pair.String() + ":" + longWindow.String()

	feed(manipQuoteAmount, manipQuoteAmount)
	tick(30 * time.Second)
	if !orch.freezeStates[shortKey].Active() || !orch.freezeStates[longKey].Active() {
		t.Fatal("setup: both windows should be frozen")
	}

	// Operator force-unfreezes the pair mid-hold: the marker is deleted
	// out of band (recordingFreezeMarker models that as present=false).
	marker.present = false

	feed(manipQuoteAmount, manipQuoteAmount)
	tick(30 * time.Second)

	if orch.freezeStates[shortKey].Active() {
		t.Error("operator override left the short window frozen")
	}
	if orch.freezeStates[longKey].Active() {
		t.Error("operator override left the LONG window frozen — the sibling-active guard on the " +
			"marker Clear must not block a genuine force-unfreeze")
	}
}

// TestFreezeLifecycle_ActiveGaugeTracksHeldFreezes — the gauge the
// freeze-active rule reads. It must fall back to zero on release,
// not stay latched: an operator reading a stuck "1 frozen" would
// treat every later freeze as pre-existing.
func TestFreezeLifecycle_ActiveGaugeTracksHeldFreezes(t *testing.T) {
	f := newFreezeFixture(t)
	// Agreeing references at the LKG so the release path is reachable
	// (corroborated lens → 30-minute hold + streak eligibility).
	seedDivergence(t, f, agreeingLens(f.pair, 0.1242, 0.1242))

	f.feed(t, manipQuoteAmount, "soroswap")
	f.tick(t, 30*time.Second)
	if got := testutil.ToFloat64(obs.AnomalyFreezeActive); got != 1 {
		t.Errorf("AnomalyFreezeActive = %v while one pair is frozen, want 1", got)
	}

	// Settle the level (the first healthy bucket is a jump vs the shadow
	// comparator), then two calm buckets past the hold release.
	f.feed(t, lkgQuoteAmount, "soroswap")
	f.tick(t, 30*time.Second)
	f.tick(t, freeze.DefaultInitialHold+time.Minute)
	f.tick(t, 30*time.Second)
	if f.state().Active() {
		t.Fatal("setup: expected release")
	}
	if got := testutil.ToFloat64(obs.AnomalyFreezeActive); got != 0 {
		t.Errorf("AnomalyFreezeActive = %v after release, want 0", got)
	}
}

// bandQuoteAmount is a price 2.5% above the last-known-good (LKG):
// $12,730.50 over 100,000 XLM → 0.127305. Its role in the W3-freeze-3
// regression is to sit in the band where the two freeze phases DISAGREE:
//
//   - Phase 1 (per-class deviation, FreezePct 2% via newAnomalyChecker):
//     2.5% > 2% → FIRES on a single source.
//   - Phase 2 (per-asset baseline, MAD 0.02): z = 0.025 / 0.02 = 1.25 <
//     the auto-unfreeze bound 3.0, confidence ~0.49 > 0.30 → HEALTHY.
//
// i.e. a residual wobble that is statistically normal for a 2%-MAD asset
// but still trips Phase 1's cruder class threshold.
const bandQuoteAmount = 127_305_000_000 // 0.127305, +2.5% vs the 0.1242 LKG

// TestFreezeLifecycle_Phase1FreezeReleasesWhenAnomalyClears is the
// W3-freeze-3 regression: a Phase 1 freeze must still RELEASE once the
// pair returns to statistical health, and must not stay frozen forever.
//
// The stuck condition (pre-fix): Phase 1's class-deviation is measured
// against prevVWAPs, the last-known-good comparator, which is held FIXED
// for the whole hold (frozen buckets skip the prevVWAPs update). So once
// the manipulation ends and the price settles at a residual level still
// past the class FreezePct — here 2.5% over the LKG, which is normal
// noise for a 2%-MAD asset — Phase 1 re-fires on EVERY bucket. A Phase 1
// fire short-circuits refreshPairWindow before the Phase 2 confidence
// step, and ADR-0019's auto-unfreeze streak is produced ONLY by that step
// (Scored=true, healthy). So the streak can never leave 0, the ladder
// extends to escalation, and the pair serves the LKG indefinitely — even
// though it meets the auto-unfreeze condition (z < 3 AND confidence >
// 0.30) on every one of those buckets.
//
// The fix lets Phase 2 drive the lifecycle once a freeze is active, so
// the streak accumulates and the freeze releases within a bounded number
// of ticks. Protection is preserved: a genuinely-anomalous bucket fires
// Phase 2's own 3-signal AND, which resets the streak.
func TestFreezeLifecycle_Phase1FreezeReleasesWhenAnomalyClears(t *testing.T) {
	pair := xlmUSDPair(t)
	rdb, mr := newTestRedis(t)
	marker := &recordingFreezeMarker{}
	store := &mockStore{}

	o := New(store, rdb, Config{
		Pairs:        []canonical.Pair{pair},
		Windows:      []time.Duration{freezeTestWindow},
		Interval:     time.Hour,
		Anomaly:      newAnomalyChecker(t, pair), // native forced to stablecoin: FreezePct 2%
		FreezeWriter: marker,
		Baselines: stubBaselineSource{
			multi: baseline.MultiBaseline{
				// MAD 0.02 (2%): a 2.5% return scores z = 1.25, comfortably
				// below the auto-unfreeze bound, so the band bucket is
				// statistically healthy while Phase 1 still flags it.
				Day30: &baseline.Baseline{Median: 0, MAD: 0.02, N: 60_000},
			},
		},
	})

	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	o.clock = func() time.Time { return now }
	stateKey := pair.String() + ":" + freezeTestWindow.String()
	vwapKey := cachekeys.VWAP(pair.Base, pair.Quote, freezeTestWindow).String()

	// Seed the LKG comparator + the cached value the API serves.
	o.prevVWAPs[stateKey] = big.NewRat(lkgQuoteAmount, lkgBaseAmount)
	if err := rdb.Set(context.Background(), vwapKey, lkgFormatted, time.Minute).Err(); err != nil {
		t.Fatalf("seed LKG: %v", err)
	}
	// References sit at the residual band level the market settles at, so
	// the release candidate is corroborated (required for the streak
	// since 2026-08-24) — a 2.5% residual is well inside every real
	// reference's agreement band.
	seedDivergenceCache(t, rdb, pair, agreeingLens(pair, 0.1242, 0.127305))

	feed := func(quote int64) {
		store.trades = []canonical.Trade{
			makeXLMUSDTrade(t, "soroswap", lkgBaseAmount, quote, now.Add(-10*time.Second)),
		}
	}
	tick := func(d time.Duration) {
		now = now.Add(d)
		if err := o.Tick(context.Background()); err != nil {
			t.Fatalf("Tick: %v", err)
		}
	}
	served := func() string {
		got, err := mr.Get(vwapKey)
		if err != nil {
			t.Fatalf("cached VWAP missing: %v", err)
		}
		return got
	}

	// Bucket 1: a manipulated single-source spike → Phase 1 freezes.
	feed(manipQuoteAmount)
	tick(30 * time.Second)
	if !o.freezeStates[stateKey].Active() {
		t.Fatal("setup: manipulated single-source bucket did not enter a Phase 1 freeze")
	}
	if got := served(); got != lkgFormatted {
		t.Fatalf("setup: freeze published %q, want the LKG %q held", got, lkgFormatted)
	}

	// The anomaly clears to a residual 2.5% above the LKG — healthy by the
	// per-asset baseline (z = 1.25 < 3, confidence > 0.30) but still past
	// Phase 1's 2% class threshold, so Phase 1 keeps firing every bucket.
	// Feed healthy-band buckets and require a RELEASE within a bounded
	// number of ticks. Each tick advances well into the hold/extension
	// ladder so two consecutive healthy buckets straddle the initial hold.
	const maxTicks = 8
	released := false
	for i := 0; i < maxTicks; i++ {
		feed(bandQuoteAmount)
		tick(6 * time.Minute)
		if !o.freezeStates[stateKey].Active() {
			released = true
			break
		}
	}
	if !released {
		t.Fatalf("W3-freeze-3: Phase 1 freeze never released after the anomaly cleared to a "+
			"statistically-healthy residual — stuck frozen after %d ticks (state=%+v). Pre-fix, "+
			"Phase 1's stale-comparator fire short-circuits the confidence step every bucket, so "+
			"the ADR-0019 auto-unfreeze streak can never accumulate and the freeze extends to "+
			"escalation instead of releasing.", maxTicks, o.freezeStates[stateKey])
	}

	// Released: the live band price is served again (NOT the frozen LKG),
	// the marker is cleared, and the comparator advances off the LKG so the
	// freeze self-heals (Phase 1 no longer fires against a stale baseline).
	wantBand := formatRatFixed(big.NewRat(bandQuoteAmount, lkgBaseAmount), 12)
	if got := served(); got != wantBand {
		t.Errorf("after release served %q, want the freshly published live price %q "+
			"(still serving %q would mean the freeze released but kept pinning the LKG)",
			got, wantBand, lkgFormatted)
	}
	if got := served(); got == lkgFormatted {
		t.Error("after release the pair is still serving the frozen last-known-good price")
	}
	if marker.present {
		t.Error("freeze marker still present after release — flags.frozen would stay true")
	}
	if o.prevVWAPs[stateKey].Cmp(big.NewRat(lkgQuoteAmount, lkgBaseAmount)) == 0 {
		t.Error("prev-VWAP comparator still pinned to the LKG after release — Phase 1 would " +
			"immediately re-fire against the stale baseline and the freeze could not self-heal")
	}
}

// TestFreezeLifecycle_AutoUnfreezeAtANewStablePriceLevel is the 2026-08-24
// XLM/GBP ratchet regression. The sibling auto-unfreeze test releases by
// feeding the price BACK to its pre-freeze value — which is why the ratchet
// survived it: mid-freeze buckets used to score against the PINNED
// pre-freeze prevVWAP, so z measured total-drift-since-freeze and the
// ADR's "two calm buckets" release was only reachable if the market
// round-tripped. Here the market settles at a NEW level (~6% above LKG,
// far outside 3×MAD of the freeze-time price) and simply stays there:
// per ADR-0019's intent ("is the market calm NOW") that MUST release.
// Pre-fix this test spins through the extension ladder instead
// (frozenPrevVWAPs is the fix: refused buckets score per-tick returns
// against the previous refused bucket's fresh VWAP).
func TestFreezeLifecycle_AutoUnfreezeAtANewStablePriceLevel(t *testing.T) {
	// $13,170 at 1e7 → price 0.1317: +6.04% from LKG's 0.1242 — z ≈ 60 on
	// the fixture's MAD if compared against the pinned pre-freeze prev,
	// z = 0 tick-over-tick once the level holds.
	const driftedQuoteAmount = 131_700_000_000
	const driftedFormatted = "0.131700000000"

	f := newFreezeFixture(t)
	before := testutil.ToFloat64(obs.AnomalyFreezeReleasedTotal.WithLabelValues("auto"))
	// A GENUINE repricing carries the corroborating references with it:
	// the lens median sits at the new level while OurPrice (what the
	// worker compares — the SERVED price) is still the pinned LKG. This
	// is exactly what production sees mid-freeze after a real market
	// move, and it is what separates this release from the held-
	// manipulation case in ExtendsThenEscalates (whose references stay
	// at the LKG).
	seedDivergence(t, f, agreeingLens(f.pair, 0.1242, 0.1317))

	f.feed(t, manipQuoteAmount, "soroswap")
	f.tick(t, 30*time.Second)
	if !f.state().Active() {
		t.Fatal("setup: freeze did not fire")
	}

	// The market moves to the new level. The FIRST bucket there is a real
	// jump tick-over-tick (manip → drifted), so it may not start the calm
	// streak — that is correct. It must still be refused (hold active).
	f.feed(t, driftedQuoteAmount, "soroswap")
	f.tick(t, 30*time.Second)
	if !f.state().Active() {
		t.Fatal("released inside the initial hold — the hold must be served first")
	}

	// Two consecutive buckets AT the new level, the second landing after
	// the initial (corroborated → 30 minute) hold has expired.
	f.feed(t, driftedQuoteAmount, "soroswap")
	f.tick(t, freeze.DefaultInitialHold+time.Minute)
	f.feed(t, driftedQuoteAmount, "soroswap")
	f.tick(t, 30*time.Second)

	if f.state().Active() {
		t.Fatalf("still frozen after the market held a NEW stable level for two "+
			"consecutive buckets past the hold — the drift-since-freeze ratchet is back: %+v",
			f.state())
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
