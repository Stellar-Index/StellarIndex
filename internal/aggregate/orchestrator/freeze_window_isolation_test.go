package orchestrator

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/baseline"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/freeze"
	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// ─── E1: per-window freeze ladders ────────────────────────────────
//
// The freeze marker (and the migration-0119 durable ladder behind it)
// is keyed by (asset, quote) because its PRESENCE is the pair-wide
// `flags.frozen` the API serves — but the ADR-0019 lifecycle inside it
// advances per (pair, WINDOW), and the windows of a pair freeze and
// release independently.
//
// Two failures follow from getting that wrong, in opposite directions,
// and both are pinned below:
//
//   - BLEED. A window that never reached the freeze step —
//     refreshPairWindow drops a window under [Config.MinUSDVolume]
//     BEFORE computeNormalizedVWAP and the confidence/freeze steps, so
//     its stateKey never enters o.freezeStates — is a COLD key the
//     first time it does qualify. With one pair-level ladder in the
//     marker it adopted a SIBLING window's FiredAt / HoldUntil /
//     ExtensionsUsed / Escalated: a last-known-good price served for a
//     window nothing is wrong with, a severity:page freeze it never
//     earned, and (once the inherited ladder escalated) no exit but a
//     manual `stellarindex-ops freeze-unfreeze`.
//   - STRANDING. The opposite over-correction — tagging the marker
//     with ONE owning window — silently drops every other window's
//     freeze across a restart, where all windows are cold at once and
//     only one can match the tag. A restarted process has no prev-VWAP
//     comparator either, so the stranded window cannot re-fire on its
//     own signal: it publishes the manipulated bucket the freeze
//     existed to withhold.
//
// Every test drives the PRODUCTION [freeze.Writer] over miniredis
// rather than the package's pair-level recording fake, so what they
// pin is the marker shape the aggregator actually writes.

const (
	// isolationMinUSDVolume is the floor that makes a window "thin":
	// the full-volume prints below clear it by ~3 orders of magnitude,
	// the thin ones miss it by ~2.
	isolationMinUSDVolume = 500.0

	// thinBaseAmount / thinQuoteAmount: a print at exactly the
	// last-known-good price (so the window is HEALTHY, never anomalous)
	// carrying 1/1000th of the volume — under the floor.
	thinBaseAmount  = lkgBaseAmount / 1000
	thinQuoteAmount = lkgQuoteAmount / 1000

	// nudgedQuoteAmount is the last-known-good price plus 0.1%: a
	// HEALTHY bucket (z = 1, well under the freeze's z > 5) that is
	// nevertheless distinguishable from the pinned LKG, so "did this
	// window publish, or was it refused by a freeze?" is answerable
	// from the served value alone.
	nudgedQuoteAmount = lkgQuoteAmount * 1001 / 1000
	nudgedFormatted   = "0.124324200000"
)

// windowIsolationFixture drives one pair over two windows (5m + 1h)
// with a per-window trade fixture and the real freeze.Writer.
type windowIsolationFixture struct {
	t     *testing.T
	orch  *Orchestrator
	store *windowRoutedStore
	rdb   *redis.Client
	mr    *miniredis.Miniredis
	pair  canonical.Pair
	now   time.Time

	short time.Duration
	long  time.Duration
}

func newWindowIsolationFixture(t *testing.T) *windowIsolationFixture {
	t.Helper()
	rdb, mr := newTestRedis(t)
	f := &windowIsolationFixture{
		t:     t,
		store: &windowRoutedStore{byWindow: map[time.Duration][]canonical.Trade{}},
		rdb:   rdb,
		mr:    mr,
		pair:  xlmUSDPair(t),
		now:   time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC),
		short: 5 * time.Minute,
		long:  time.Hour,
	}
	f.orch = f.newOrchestrator()

	// The LONG window has published before: it is the window that
	// freezes in the bleed tests. The short window is left unseeded by
	// default — thin from the start, so it has neither a cached VWAP nor
	// a prev-VWAP comparator, exactly the state a never-published window
	// is in. [windowIsolationFixture.seedLKG] opts it in.
	f.seedLKG(f.long)
	return f
}

// seedLKG puts a window in the state right after a healthy publish at
// the last-known-good price: the comparator slot filled and the value
// in cache.
func (f *windowIsolationFixture) seedLKG(w time.Duration) {
	f.t.Helper()
	f.orch.prevVWAPs[f.key(w)] = big.NewRat(lkgQuoteAmount, lkgBaseAmount)
	if err := f.rdb.Set(context.Background(), f.vwapKey(w), lkgFormatted, time.Hour).Err(); err != nil {
		f.t.Fatalf("seed LKG for %v: %v", w, err)
	}
}

// newOrchestrator builds an orchestrator over the fixture's store and
// Redis. Called again by [windowIsolationFixture.restart] to model a
// deploy: fresh in-memory ladder, same Redis marker.
func (f *windowIsolationFixture) newOrchestrator() *Orchestrator {
	f.t.Helper()
	writer, err := freeze.NewWriter(f.rdb, 0, freeze.WithClock(func() time.Time { return f.now }))
	if err != nil {
		f.t.Fatalf("freeze.NewWriter: %v", err)
	}
	o := New(f.store, f.rdb, Config{
		Pairs:        []canonical.Pair{f.pair},
		Windows:      []time.Duration{f.short, f.long},
		Interval:     time.Hour,
		MinUSDVolume: isolationMinUSDVolume,
		FreezeWriter: writer,
		Baselines: stubBaselineSource{
			multi: baseline.MultiBaseline{
				Day30: &baseline.Baseline{Median: 0, MAD: 0.001, N: maxDay30Returns},
			},
		},
	})
	o.clock = func() time.Time { return f.now }
	return o
}

// restart replaces the orchestrator with a fresh one over the same
// Redis — every (pair, window) key is cold again, as after a deploy,
// and no prev-VWAP comparator survives.
func (f *windowIsolationFixture) restart() {
	f.t.Helper()
	f.orch = f.newOrchestrator()
}

func (f *windowIsolationFixture) key(w time.Duration) string {
	return f.pair.String() + ":" + w.String()
}

func (f *windowIsolationFixture) vwapKey(w time.Duration) string {
	return cachekeys.VWAP(f.pair.Base, f.pair.Quote, w).String()
}

// feed sets both windows' prints: `shortBase/shortQuote` and
// `longBase/longQuote` in 1e7-scaled amounts, one source each (so the
// 3-signal AND's source_count leg is satisfiable).
func (f *windowIsolationFixture) feed(shortBase, shortQuote, longBase, longQuote int64) {
	f.t.Helper()
	ts := f.now.Add(-10 * time.Second)
	f.store.byWindow[f.short] = []canonical.Trade{
		makeXLMUSDTrade(f.t, "soroswap", shortBase, shortQuote, ts),
	}
	f.store.byWindow[f.long] = []canonical.Trade{
		makeXLMUSDTrade(f.t, "soroswap", longBase, longQuote, ts),
	}
}

func (f *windowIsolationFixture) tick(d time.Duration) {
	f.t.Helper()
	f.now = f.now.Add(d)
	if err := f.orch.Tick(context.Background()); err != nil {
		f.t.Fatalf("Tick: %v", err)
	}
}

// markerPresent reports whether the shared (asset, quote) freeze
// marker — what the API serves as flags.frozen — exists.
func (f *windowIsolationFixture) markerPresent() bool {
	f.t.Helper()
	return f.mr.Exists(cachekeys.Freeze(f.pair.Base, f.pair.Quote).String())
}

// served is the VWAP the API would read for `w` right now.
func (f *windowIsolationFixture) served(w time.Duration) string {
	f.t.Helper()
	got, err := f.mr.Get(f.vwapKey(w))
	if err != nil {
		f.t.Fatalf("no cached VWAP for %v: %v", w, err)
	}
	return got
}

// freezeTheLongWindow drives the 1h window into a freeze and one rung
// up the extension ladder, while the 5m window stays thin (dropped by
// the USD-volume floor, so its stateKey never enters o.freezeStates).
func (f *windowIsolationFixture) freezeTheLongWindow() {
	f.t.Helper()

	// Bucket 1: single-source manipulated print on the long window.
	f.feed(thinBaseAmount, thinQuoteAmount, lkgBaseAmount, manipQuoteAmount)
	f.tick(closedBucket)
	if !f.orch.freezeStates[f.key(f.long)].Active() {
		f.t.Fatal("setup: the manipulated long-window bucket did not freeze")
	}
	if _, cached := f.orch.freezeStates[f.key(f.short)]; cached {
		f.t.Fatal("setup: the thin short window reached the freeze step — it must be " +
			"dropped by the MinUSDVolume floor, which is what makes its key cold")
	}

	// Bucket 2, past the (uncorroborated) initial hold with the
	// manipulation still swinging: the ladder grants an extension, so
	// the marker now carries a ladder no other window could have earned.
	f.feed(thinBaseAmount, thinQuoteAmount, lkgBaseAmount, manipQuoteAmount*3/2)
	f.tick(freeze.DefaultUncorroboratedInitialHold + time.Minute)
	if got := f.orch.freezeStates[f.key(f.long)].ExtensionsUsed; got != 1 {
		f.t.Fatalf("setup: long-window ExtensionsUsed = %d, want 1", got)
	}
	if !f.markerPresent() {
		f.t.Fatal("setup: the shared freeze marker should be present")
	}
}

// assertShortWindowStartsClean is the bleed verdict: the short window
// must own NO ladder and must publish its own healthy bucket.
func (f *windowIsolationFixture) assertShortWindowStartsClean(wantValue string) {
	f.t.Helper()
	st := f.orch.freezeStates[f.key(f.short)]
	if st.Active() {
		f.t.Errorf("E1: the short window inherited the long window's ladder "+
			"(fired_at=%s hold_until=%s extensions_used=%d escalated=%v) — it was frozen "+
			"on a sibling window's anomaly, serves a last-known-good price for a window "+
			"nothing is wrong with, and once the inherited ladder escalates only a manual "+
			"freeze-unfreeze ends it",
			st.FiredAt, st.HoldUntil, st.ExtensionsUsed, st.Escalated)
	}
	if st.ExtensionsUsed != 0 {
		f.t.Errorf("E1: short-window ExtensionsUsed = %d, want 0 — the ladder is the "+
			"long window's", st.ExtensionsUsed)
	}
	got, err := f.mr.Get(f.vwapKey(f.short))
	if err != nil {
		f.t.Fatalf("E1: the short window published nothing (%v) — its bucket was refused "+
			"by a freeze it never earned; want the fresh VWAP %s", err, wantValue)
	}
	if got != wantValue {
		f.t.Errorf("E1: short window served %q, want its own fresh VWAP %q", got, wantValue)
	}
}

// TestFreezeWindowIsolation_ColdThinWindowDoesNotAdoptASiblingsLadder
// is E1's no-restart path: while the 1h window is frozen IN THIS
// PROCESS, the 5m window clears the USD-volume floor for the first
// time. Its first qualifying bucket is healthy and unscorable (no
// prior VWAP to derive a return from), so the only way it can end up
// frozen is by adopting the marker the 1h window wrote.
func TestFreezeWindowIsolation_ColdThinWindowDoesNotAdoptASiblingsLadder(t *testing.T) {
	f := newWindowIsolationFixture(t)
	f.freezeTheLongWindow()

	// The 5m window's book thickens: same (healthy, last-known-good)
	// price, now with volume above the floor. First time this stateKey
	// reaches the freeze step.
	f.feed(lkgBaseAmount, lkgQuoteAmount, lkgBaseAmount, manipQuoteAmount*2)
	f.tick(closedBucket)

	f.assertShortWindowStartsClean(lkgFormatted)

	// The sibling's own freeze must be untouched by the isolation fix.
	if !f.orch.freezeStates[f.key(f.long)].Active() {
		t.Error("E1: the long window's freeze was released — window isolation must not " +
			"weaken the freeze that actually fired")
	}
	if !f.markerPresent() {
		t.Error("E1: the shared freeze marker was cleared while the long window is still " +
			"frozen — the API would serve flags.frozen=false for it")
	}
}

// TestFreezeWindowIsolation_RestartDoesNotSpreadALadderAcrossWindows
// is the same bleed with no in-process sibling to compare against:
// after a restart EVERY window is cold, so the thin window's first
// qualifying bucket reads the marker with nothing in memory to say
// which window's ladder it carries. The marker itself must carry that.
func TestFreezeWindowIsolation_RestartDoesNotSpreadALadderAcrossWindows(t *testing.T) {
	f := newWindowIsolationFixture(t)
	f.freezeTheLongWindow()

	// Deploy: fresh process, same Redis. The long window stays thin from
	// here (nothing re-evaluates it this tick), so the ONLY window
	// reaching the freeze step is the short one, and there is no
	// in-memory sibling state at all.
	f.restart()
	f.feed(lkgBaseAmount, lkgQuoteAmount, thinBaseAmount, thinQuoteAmount)
	f.tick(closedBucket)

	f.assertShortWindowStartsClean(lkgFormatted)
}

// TestFreezeWindowIsolation_RestartRehydratesEveryFrozenWindow is the
// opposite failure, and the reason the marker carries a ladder PER
// window instead of the single owning-window tag that would also stop
// the bleed above.
//
// Both windows freeze on the same manipulated print, so both have a
// ladder of their own. A deploy makes every (pair, window) key cold at
// once; the manipulation is still swinging. Every window must rehydrate
// ITS OWN ladder and keep serving the last-known-good 0.1242. A marker
// that can only name one owner strands the other window with no ladder
// — and a restarted process has no prev-VWAP comparator, so that window
// cannot re-fire on its own signal either: it publishes the manipulated
// 0.2000 the freeze exists to withhold.
func TestFreezeWindowIsolation_RestartRehydratesEveryFrozenWindow(t *testing.T) {
	f := newWindowIsolationFixture(t)
	f.seedLKG(f.short) // both windows have published before

	// One manipulated print, both windows, both above the volume floor.
	f.feed(lkgBaseAmount, manipQuoteAmount, lkgBaseAmount, manipQuoteAmount)
	f.tick(closedBucket)
	for _, w := range []time.Duration{f.short, f.long} {
		if !f.orch.freezeStates[f.key(w)].Active() {
			t.Fatalf("setup: the %v window did not freeze on the manipulated bucket", w)
		}
	}

	// Deploy, with the manipulation still live on both windows.
	f.restart()
	f.feed(lkgBaseAmount, manipQuoteAmount, lkgBaseAmount, manipQuoteAmount)
	f.tick(closedBucket)

	for _, w := range []time.Duration{f.short, f.long} {
		st := f.orch.freezeStates[f.key(w)]
		if !st.Active() {
			t.Errorf("E1: the %v window lost its freeze across the restart — its ladder "+
				"was in the marker, and a restarted process has no prev-VWAP comparator "+
				"to re-fire on, so the manipulated bucket publishes", w)
		}
		if got := f.served(w); got != lkgFormatted {
			t.Errorf("E1: the %v window served %q after the restart, want the "+
				"last-known-good %q — the manipulated print must stay withheld",
				w, got, lkgFormatted)
		} else if got == manipFormatted {
			t.Errorf("E1: the %v window published the manipulated %q", w, manipFormatted)
		}
	}
	if !f.markerPresent() {
		t.Error("E1: the shared freeze marker is gone while both windows are frozen")
	}
}

// TestFreezeWindowIsolation_ReleasedWindowLadderLeavesTheMarker pins
// the other half of a per-window ladder map: when one window releases
// while a sibling is still frozen, the marker STAYS (it is the pair's
// flags.frozen) but the released window's ladder must not — otherwise a
// restart would rehydrate a freeze that had already ended and re-pin a
// healthy window to a last-known-good price for the rest of a stale
// hold.
func TestFreezeWindowIsolation_ReleasedWindowLadderLeavesTheMarker(t *testing.T) {
	f := newWindowIsolationFixture(t)
	f.seedLKG(f.short)

	// Both windows freeze on the same manipulated print.
	f.feed(lkgBaseAmount, manipQuoteAmount, lkgBaseAmount, manipQuoteAmount)
	f.tick(closedBucket)
	for _, w := range []time.Duration{f.short, f.long} {
		if !f.orch.freezeStates[f.key(w)].Active() {
			t.Fatalf("setup: the %v window did not freeze on the manipulated bucket", w)
		}
	}

	// The short window releases while the long one stays frozen. Driven
	// through releaseFreeze directly rather than through the ADR-0019
	// auto-unfreeze: the streak needs a RELEASE-CORROBORATING lens
	// (freeze.Signal.ReleaseCorroborated) and this single-source fixture
	// has none, so the transition is unreachable from trade fixtures
	// alone. releaseFreeze is the production release path for every mode
	// — auto, operator, ladder — and the marker bookkeeping under test
	// is all downstream of it.
	shortKey := f.key(f.short)
	f.orch.releaseFreeze(context.Background(), f.pair, f.short, shortKey,
		f.orch.freezeStates[shortKey], freeze.TransitionReleased)

	if !f.markerPresent() {
		t.Fatal("W3-freeze-1: the marker must stay while the long window is still frozen")
	}

	// A deploy must not bring the released freeze back. The short window
	// gets a healthy bucket at a slightly different price, so the value
	// it serves says unambiguously whether it published (fresh price) or
	// was refused by a rehydrated ladder (the pinned last-known-good).
	f.restart()
	f.feed(lkgBaseAmount, nudgedQuoteAmount, lkgBaseAmount, manipQuoteAmount*3)
	f.tick(closedBucket)

	if st := f.orch.freezeStates[shortKey]; st.Active() {
		t.Errorf("E1: the short window rehydrated a ladder it had already released "+
			"(fired_at=%s hold_until=%s) — a released ladder must be retired from the "+
			"marker, not left for the next restart", st.FiredAt, st.HoldUntil)
	}
	if got := f.served(f.short); got != nudgedFormatted {
		t.Errorf("E1: the short window served %q after releasing, want its own fresh "+
			"VWAP %q", got, nudgedFormatted)
	}
}
