package forex

import (
	"context"
	"io"
	"testing"
	"time"
)

// snapshotWithHistory builds a Snapshot whose Currencies establish today's
// baseline AND whose History7d carries the given trailing-day points per
// ticker. The current-rate loop runs first in persistSnapshot, so it seeds
// the per-ticker band baseline the history points are measured against.
func snapshotWithHistory(current map[string]float64, history map[string][]HistoryPoint) *Snapshot {
	s := snapshotOf(current)
	s.History7d = history
	return s
}

// TestPersistSnapshot_HistoryRowRejectsBadBar is the MR-1 proven-red guard
// (audit-2026-08-14). Regression (1): the trailing-7d history rows were
// written with ONLY a >0/finite filter — no [maxRateDeviation] band — so a
// transiently-wrong-but-positive upstream bar for a PAST date (provider
// glitch: EUR 0.85 instead of 0.92 two days ago, but here a decimal shift
// to 9.2 to sit unambiguously outside the 50% band) overwrote the correct
// stored rate in place. fx_quotes.rate_usd is the denominator of every
// fiat-quoted usd_volume, so that one bad history bar re-scales every
// EUR-quoted trade valued off that date, with nothing to revert it.
//
// After the fix persistSnapshot bands each history point via
// acceptHistoryRate against the ticker's current accepted rate: a bar >50%
// off is dropped, an in-band bar is written.
//
// Reverting the persistSnapshot history loop to the old
// `<=0 || IsNaN || IsInf` filter turns the "bad bar dropped" assertion red.
func TestPersistSnapshot_HistoryRowRejectsBadBar(t *testing.T) {
	w, cw := bandTestWorker(io.Discard)
	ctx := context.Background()

	today := time.Date(2026, 7, 26, 0, 0, 0, 0, time.UTC)
	twoDaysAgo := today.Add(-2 * 24 * time.Hour)
	threeDaysAgo := today.Add(-3 * 24 * time.Hour)

	snap := snapshotWithHistory(
		map[string]float64{"EUR": 0.92}, // today's baseline
		map[string][]HistoryPoint{
			"EUR": {
				{Date: threeDaysAgo, RateUSD: 0.93}, // in band → written
				{Date: twoDaysAgo, RateUSD: 9.2},    // decimal-shift glitch → must be dropped
			},
		},
	)
	w.persistSnapshot(ctx, snap)

	if len(cw.batches) != 1 {
		t.Fatalf("expected 1 persisted batch, got %d", len(cw.batches))
	}
	batch := cw.batches[0]

	// The good in-band history bar is written.
	if !hasHistoryRow(batch, "EUR", threeDaysAgo, 0.93) {
		t.Errorf("in-band history bar EUR@%s=0.93 was not persisted; batch=%v",
			threeDaysAgo.Format("2006-01-02"), batch)
	}
	// The decimal-shift history bar (9.2, ~10x the 0.92 baseline, far outside
	// the 50%% band) must be REJECTED — otherwise it overwrites the correct
	// stored 0.92 for that date and mis-scales every EUR-quoted usd_volume.
	if hasHistoryRow(batch, "EUR", twoDaysAgo, 9.2) {
		t.Errorf("bad history bar EUR@%s=9.2 was persisted, want it REJECTED "+
			"(baseline 0.92, band %v) — an unbanded past bar re-scales every "+
			"EUR-quoted usd_volume for that date (MR-1)",
			twoDaysAgo.Format("2006-01-02"), maxRateDeviation)
	}
}

// TestPersistSnapshot_HistoryRowNoBaselineBootstraps — with no current-rate
// baseline for the ticker (e.g. the ticker appears only in History7d), the
// history band must not refuse to bootstrap, mirroring acceptRate: dropping
// legitimate history would starve the feed.
func TestPersistSnapshot_HistoryRowNoBaselineBootstraps(t *testing.T) {
	w, cw := bandTestWorker(io.Discard)
	ctx := context.Background()

	today := time.Date(2026, 7, 26, 0, 0, 0, 0, time.UTC)
	yesterday := today.Add(-24 * time.Hour)

	// No current rate for MXN — only a history point.
	snap := snapshotWithHistory(
		map[string]float64{},
		map[string][]HistoryPoint{
			"MXN": {{Date: yesterday, RateUSD: 18.5}},
		},
	)
	w.persistSnapshot(ctx, snap)

	if len(cw.batches) != 1 {
		t.Fatalf("expected 1 persisted batch, got %d", len(cw.batches))
	}
	if !hasHistoryRow(cw.batches[0], "MXN", yesterday, 18.5) {
		t.Errorf("first-sighting history bar MXN@%s=18.5 was dropped, want it "+
			"bootstrapped — no baseline must accept, not block",
			yesterday.Format("2006-01-02"))
	}
}

// hasHistoryRow reports whether batch carries a row for ticker at the given
// date (truncated to the day, as persistSnapshot stores it) with rate.
func hasHistoryRow(batch []FXQuote, ticker string, date time.Time, rate float64) bool {
	want := date.UTC().Truncate(24 * time.Hour)
	for _, q := range batch {
		if q.Ticker == ticker && q.Bucket.Equal(want) && q.RateUSD == rate {
			return true
		}
	}
	return false
}

// TestAcceptHistoryRate_StuckUpstreamReclassifies is the Massive ETB=44
// incident (2026-08-24): a provider serving the SAME broken history bar
// refresh after refresh must keep being REFUSED, but stop counting as
// fresh `history_deviation` once the streak passes the threshold — so
// the rejection alert only carries new information. A different rejected
// value, or an in-band acceptance, resets the streak.
func TestAcceptHistoryRate_StuckUpstreamReclassifies(t *testing.T) {
	w, _ := bandTestWorker(io.Discard)
	w.guards["ETB"] = &rateGuard{lastAccepted: 161.75}

	// Threshold refusals: all classified as fresh deviation, all refused.
	for i := 0; i < stuckRejectionThreshold; i++ {
		if w.acceptHistoryRate("ETB", 44) {
			t.Fatalf("refusal %d: broken bar must be rejected", i+1)
		}
	}
	g := w.guards["ETB"]
	if g.stuckCount != stuckRejectionThreshold {
		t.Fatalf("stuckCount = %d, want %d", g.stuckCount, stuckRejectionThreshold)
	}

	// Past the threshold: still refused, now classified stuck.
	if w.acceptHistoryRate("ETB", 44) {
		t.Fatal("stuck bar must STILL be rejected — reclassification never accepts")
	}
	if g.stuckCount != stuckRejectionThreshold+1 {
		t.Fatalf("stuckCount = %d, want %d", g.stuckCount, stuckRejectionThreshold+1)
	}

	// A DIFFERENT out-of-band value is fresh news: streak restarts at 1.
	if w.acceptHistoryRate("ETB", 55) {
		t.Fatal("different out-of-band value must be rejected")
	}
	if g.stuckCount != 1 || g.stuckRejectedRate != 55 {
		t.Fatalf("streak after new value = (%d, %v), want (1, 55)", g.stuckCount, g.stuckRejectedRate)
	}

	// An in-band bar is accepted but must NOT reset the streak (the ETB
	// fix). A good sibling bar in the same sweep clearing a different
	// broken bar's streak is exactly what kept `_stuck` from ever
	// engaging. The 55-streak persists across the in-band accept.
	if !w.acceptHistoryRate("ETB", 160.0) {
		t.Fatal("in-band history bar must be accepted")
	}
	if g.stuckCount != 1 || g.stuckRejectedRate != 55 {
		t.Fatalf("streak after in-band accept = (%d, %v), want (1, 55) — acceptance must NOT clear the streak", g.stuckCount, g.stuckRejectedRate)
	}
}

// TestAcceptHistoryRate_BrokenBarWithGoodSiblingsReachesStuck is the ETB
// incident in miniature: a persistently-broken dated bar (44), refused
// sweep after sweep, INTERLEAVED with the accepted good sibling bars (160)
// of the same trailing window. It must still accumulate to the _stuck
// threshold. Before the fix, each sweep's good bars reset the streak the
// broken bar had just incremented, so it oscillated 0↔1 and the alert
// never de-noised — the bar paged for days.
func TestAcceptHistoryRate_BrokenBarWithGoodSiblingsReachesStuck(t *testing.T) {
	w, _ := bandTestWorker(io.Discard)
	w.guards["ETB"] = &rateGuard{lastAccepted: 160}

	// Each sweep: some in-band sibling bars (accepted) + the one broken
	// dated bar (refused). Repeat past the threshold.
	for i := 0; i <= stuckRejectionThreshold; i++ {
		w.acceptHistoryRate("ETB", 160) // good sibling, in-band
		w.acceptHistoryRate("ETB", 161) // another good sibling
		if w.acceptHistoryRate("ETB", 44) {
			t.Fatalf("sweep %d: broken bar must be rejected", i+1)
		}
	}
	g := w.guards["ETB"]
	if g.stuckCount <= stuckRejectionThreshold {
		t.Fatalf("broken bar interleaved with good siblings never reached _stuck: stuckCount=%d, want > %d", g.stuckCount, stuckRejectionThreshold)
	}
	// And the guard's current-rate baseline must be untouched throughout
	// (the method's read-only contract on lastAccepted/pending).
	if g.lastAccepted != 160 || g.pending != 0 {
		t.Fatalf("baseline mutated: lastAccepted=%v pending=%v", g.lastAccepted, g.pending)
	}
}

// TestPersistSnapshot_BootstrapPoisonHealedByHistoryMajority is the
// 2026-08-24 Massive UZS incident, end to end. At process restart the
// current feed served a broken 1820 (true level ≈ 11,800); the bootstrap
// arm accepted it sight-unseen, and the guard then rejected the ticker's
// entire CORRECT trailing-7d series against the poisoned baseline —
// today's wrong row reached fx_quotes while seven right rows were
// refused. With the heal: the agreeing history majority refutes the
// unconfirmed bootstrap, the baseline re-points at the bars' median, the
// bars are written, and the poisoned current-day row never reaches the
// writer.
func TestPersistSnapshot_BootstrapPoisonHealedByHistoryMajority(t *testing.T) {
	w, cw := bandTestWorker(io.Discard)
	ctx := context.Background()

	today := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	bars := []HistoryPoint{
		{Date: today.Add(-7 * 24 * time.Hour), RateUSD: 11860.49},
		{Date: today.Add(-6 * 24 * time.Hour), RateUSD: 11847},
		{Date: today.Add(-5 * 24 * time.Hour), RateUSD: 11791.69},
		{Date: today.Add(-4 * 24 * time.Hour), RateUSD: 11785},
		{Date: today.Add(-3 * 24 * time.Hour), RateUSD: 11817.69},
		{Date: today.Add(-2 * 24 * time.Hour), RateUSD: 11817},
		{Date: today.Add(-1 * 24 * time.Hour), RateUSD: 11838.98},
	}
	snap := snapshotWithHistory(
		map[string]float64{"UZS": 1820}, // the broken bootstrap sample
		map[string][]HistoryPoint{"UZS": bars},
	)
	snap.PublishedAt = today
	w.persistSnapshot(ctx, snap)

	if len(cw.batches) != 1 {
		t.Fatalf("expected 1 persisted batch, got %d", len(cw.batches))
	}
	batch := cw.batches[0]

	// The poisoned current-day row must be scrubbed.
	if hasHistoryRow(batch, "UZS", today, 1820) {
		t.Errorf("poisoned bootstrap row UZS@today=1820 reached the writer; " +
			"the ticker's own agreeing 7-day history refutes it")
	}
	// Every correct history bar must be written.
	for _, p := range bars {
		if !hasHistoryRow(batch, "UZS", p.Date, p.RateUSD) {
			t.Errorf("correct history bar UZS@%s=%v was rejected against the "+
				"poisoned baseline — evidence pointing the wrong way",
				p.Date.Format("2006-01-02"), p.RateUSD)
		}
	}
	// The baseline is healed: a follow-up current rate at the true level
	// must be accepted first try.
	if !w.acceptRate("UZS", 11795) {
		t.Errorf("post-heal current rate at the true level was rejected; "+
			"baseline=%v", w.guards["UZS"].lastAccepted)
	}
}

// TestPersistSnapshot_ConfirmedBaselineIsNeverHealed pins the heal's
// deliberate limit: a baseline corroborated by two agreeing current
// fetches is NOT flipped by an agreeing history majority — a systemically
// broken history endpoint against a healthy current endpoint is exactly
// the MR-1 poisoning, and from inside the worker the two cases are
// indistinguishable, so the confirmed baseline wins and the bars stay
// rejected.
func TestPersistSnapshot_ConfirmedBaselineIsNeverHealed(t *testing.T) {
	w, cw := bandTestWorker(io.Discard)
	ctx := context.Background()

	today := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)

	// Two agreeing snapshots confirm the (correct) baseline.
	first := snapshotOf(map[string]float64{"EUR": 0.92})
	first.PublishedAt = today
	w.persistSnapshot(ctx, first)

	// Second refresh: same current rate (confirms), but the history
	// endpoint now serves a decimal-shifted series (systemic glitch).
	bars := []HistoryPoint{
		{Date: today.Add(-4 * 24 * time.Hour), RateUSD: 9.3},
		{Date: today.Add(-3 * 24 * time.Hour), RateUSD: 9.2},
		{Date: today.Add(-2 * 24 * time.Hour), RateUSD: 9.25},
		{Date: today.Add(-1 * 24 * time.Hour), RateUSD: 9.18},
	}
	second := snapshotWithHistory(
		map[string]float64{"EUR": 0.92},
		map[string][]HistoryPoint{"EUR": bars},
	)
	second.PublishedAt = today
	w.persistSnapshot(ctx, second)

	batch := cw.batches[len(cw.batches)-1]
	for _, p := range bars {
		if hasHistoryRow(batch, "EUR", p.Date, p.RateUSD) {
			t.Errorf("decimal-shifted history bar EUR@%s=%v was written — the "+
				"heal flipped a CONFIRMED baseline (MR-1 in a new coat)",
				p.Date.Format("2006-01-02"), p.RateUSD)
		}
	}
	if got := w.guards["EUR"].lastAccepted; got != 0.92 {
		t.Errorf("confirmed baseline moved to %v, want 0.92 held", got)
	}
}

// TestPersistSnapshot_RedenominationSplitSeriesDoesNotHeal — a genuine
// mid-week redenomination splits the trailing series across two levels;
// the mutual-agreement test must refuse to call either side a "majority",
// leaving genuine moves to the two-fetch pending confirmation.
func TestPersistSnapshot_RedenominationSplitSeriesDoesNotHeal(t *testing.T) {
	w, cw := bandTestWorker(io.Discard)
	ctx := context.Background()

	today := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	bars := []HistoryPoint{
		{Date: today.Add(-4 * 24 * time.Hour), RateUSD: 1800}, // old level
		{Date: today.Add(-3 * 24 * time.Hour), RateUSD: 1810},
		{Date: today.Add(-2 * 24 * time.Hour), RateUSD: 11800}, // redenominated
		{Date: today.Add(-1 * 24 * time.Hour), RateUSD: 11810},
	}
	snap := snapshotWithHistory(
		map[string]float64{"XXX": 1805},
		map[string][]HistoryPoint{"XXX": bars},
	)
	snap.PublishedAt = today
	w.persistSnapshot(ctx, snap)

	// Split series: no heal. Baseline stays at the bootstrap sample and
	// the current-day row is written.
	if got := w.guards["XXX"].lastAccepted; got != 1805 {
		t.Errorf("split (non-agreeing) series healed the baseline to %v, want 1805", got)
	}
	if !hasHistoryRow(cw.batches[0], "XXX", today, 1805) {
		t.Errorf("current-day row was scrubbed without a heal")
	}
}

// TestAcceptHistoryRate_StuckStreakToleratesJitter — the second half of
// the UZS incident: the stuck reclassification keyed on EXACT float
// equality of consecutive rejected values, and a live broken upstream
// jitters (11791.69 → 11785 → 11817.69 …), so the streak reset every
// refresh and the alert never quieted. Rejections within
// [stuckSameRateTolerance] of the tracked value must extend the streak.
func TestAcceptHistoryRate_StuckStreakToleratesJitter(t *testing.T) {
	w, _ := bandTestWorker(io.Discard)
	w.guards["UZS"] = &rateGuard{lastAccepted: 1820}

	// Jittering rejections around one level: ±0.5% steps, all within the
	// 1% tolerance of the tracked stuck value.
	base := 11800.0
	jitter := []float64{0, 12, -20, 35, -8, 22, -30, 15, -12, 28, -18, 9, 3}
	for i, j := range jitter {
		if w.acceptHistoryRate("UZS", base+j) {
			t.Fatalf("bar %d unexpectedly accepted", i)
		}
	}
	g := w.guards["UZS"]
	if g.stuckCount <= stuckRejectionThreshold {
		t.Fatalf("stuckCount = %d after %d jittering refusals, want > threshold %d — "+
			"exact-equality streak tracking resets on every live-jitter refresh",
			g.stuckCount, len(jitter), stuckRejectionThreshold)
	}
}

// TestPersistSnapshot_SplitRejectedSeriesFailsAgreement pins the
// mutual-agreement band itself (verifier 2026-08-24: the redenomination
// test above actually exercises the min-bars floor — its in-band old-level
// bars are accepted, so only 2 bars reach the rejected set). Here the
// bootstrap sample is far from BOTH levels, every bar lands in the
// rejected set, and that set spans two levels >10% apart: heal-grade
// count (7 ≥ 4) but no mutual agreement → no heal, baseline held.
// Red-proof: widening historyHealAgreement (or removing the agreement
// loop in historyMajority) turns this test's baseline assertion red.
func TestPersistSnapshot_SplitRejectedSeriesFailsAgreement(t *testing.T) {
	w, cw := bandTestWorker(io.Discard)
	ctx := context.Background()

	today := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	bars := []HistoryPoint{
		{Date: today.Add(-7 * 24 * time.Hour), RateUSD: 1800}, // old level
		{Date: today.Add(-6 * 24 * time.Hour), RateUSD: 1810},
		{Date: today.Add(-5 * 24 * time.Hour), RateUSD: 1805},
		{Date: today.Add(-4 * 24 * time.Hour), RateUSD: 11800}, // new level
		{Date: today.Add(-3 * 24 * time.Hour), RateUSD: 11810},
		{Date: today.Add(-2 * 24 * time.Hour), RateUSD: 11790},
		{Date: today.Add(-1 * 24 * time.Hour), RateUSD: 11805},
	}
	snap := snapshotWithHistory(
		map[string]float64{"YYY": 500}, // bootstrap far from both levels
		map[string][]HistoryPoint{"YYY": bars},
	)
	snap.PublishedAt = today
	w.persistSnapshot(ctx, snap)

	// No heal: a two-level rejected series is not a majority, whatever
	// its size — the baseline (however dubious) holds for the pending
	// confirmation to sort out.
	if got := w.guards["YYY"].lastAccepted; got != 500 {
		t.Errorf("split rejected series healed the baseline to %v, want 500 held — "+
			"the mutual-agreement band is the only guard in this corner", got)
	}
	for _, p := range bars {
		if hasHistoryRow(cw.batches[0], "YYY", p.Date, p.RateUSD) {
			t.Errorf("bar %s=%v written without a heal", p.Date.Format("2006-01-02"), p.RateUSD)
		}
	}
}
