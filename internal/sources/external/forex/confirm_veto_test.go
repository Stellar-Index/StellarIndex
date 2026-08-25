package forex

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// uzsBars is the trailing-7d series from the 2026-08-24 Massive UZS
// incident: seven mutually-agreeing bars around the true ~11,800 level
// (median 11817.69).
func uzsBars(today time.Time) []HistoryPoint {
	return []HistoryPoint{
		{Date: today.Add(-7 * 24 * time.Hour), RateUSD: 11860.49},
		{Date: today.Add(-6 * 24 * time.Hour), RateUSD: 11847},
		{Date: today.Add(-5 * 24 * time.Hour), RateUSD: 11791.69},
		{Date: today.Add(-4 * 24 * time.Hour), RateUSD: 11785},
		{Date: today.Add(-3 * 24 * time.Hour), RateUSD: 11817.69},
		{Date: today.Add(-2 * 24 * time.Hour), RateUSD: 11817},
		{Date: today.Add(-1 * 24 * time.Hour), RateUSD: 11838.98},
	}
}

// currentDayRate returns the persisted RateUSD for ticker's CURRENT-day
// row in the batch (bucket == day), or -1 when absent. Distinct from
// rateFor, which matches any row for the ticker including history rows.
func currentDayRate(batch []FXQuote, ticker string, day time.Time) float64 {
	want := day.UTC().Truncate(24 * time.Hour)
	for _, q := range batch {
		if q.Ticker == ticker && q.Bucket.Equal(want) {
			return q.RateUSD
		}
	}
	return -1
}

// TestPersistSnapshot_BrokenCurrentFeedNeverRepoisonsPostHeal is the
// 2026-08-24 Massive UZS incident's SECOND act, end to end. The
// restart-heal fixed the poisoned bootstrap baseline, but the current
// feed KEPT serving the broken ~1820 bar (true level ≈ 11,800): the
// deviation arm rejected it into pending, and the next fetch of the
// same broken value pending-CONFIRMED it — two repeats of one broken
// endpoint counted as two independent witnesses — re-poisoning the
// baseline the heal cannot re-fix (a confirm clears
// bootstrapUnconfirmed, deliberately). Net: one wrong UZS row per day
// while the upstream stayed broken.
//
// With the confirm veto: the ticker's heal-grade history majority
// refutes the candidate every refresh, so the confirm is refused
// (reason "deviation_history_conflict") across arbitrarily many cycles
// — the broken value NEVER reaches fx_quotes and the baseline never
// moves. Red-proof: reverting the vetoConfirmByHistory call in
// acceptRate turns the cycle-3 assertions red (the confirm arm accepts
// 1817 and writes it).
func TestPersistSnapshot_BrokenCurrentFeedNeverRepoisonsPostHeal(t *testing.T) {
	w, cw := bandTestWorker(io.Discard)
	ctx := context.Background()

	today := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	bars := uzsBars(today)
	const healedMedian = 11817.69

	// Cycle 1 — restart: broken bootstrap, then the history-majority heal.
	snap := snapshotWithHistory(
		map[string]float64{"UZS": 1820},
		map[string][]HistoryPoint{"UZS": bars},
	)
	snap.PublishedAt = today
	w.persistSnapshot(ctx, snap)
	if got := w.guards["UZS"].lastAccepted; got != healedMedian {
		t.Fatalf("heal precondition: lastAccepted = %v, want %v", got, healedMedian)
	}

	// Cycles 2..4 — the upstream keeps serving the broken level with its
	// live jitter. Cycle 2 re-arms pending; cycles 3 and 4 hit the
	// two-fetch confirm arm, which the history majority must veto.
	for i, rate := range []float64{1820, 1817, 1822} {
		cycle := snapshotWithHistory(
			map[string]float64{"UZS": rate},
			map[string][]HistoryPoint{"UZS": bars},
		)
		cycle.PublishedAt = today
		w.persistSnapshot(ctx, cycle)

		batch := cw.batches[len(cw.batches)-1]
		if got := currentDayRate(batch, "UZS", today); got != -1 {
			t.Errorf("cycle %d: broken current bar %v reached fx_quotes as today's row (%v) — "+
				"the pending-confirm arm re-poisoned the healed baseline", i+2, rate, got)
		}
		if got := w.guards["UZS"].lastAccepted; got != healedMedian {
			t.Errorf("cycle %d: baseline moved to %v, want %v held — history must keep "+
				"refuting the repeating broken value", i+2, got, healedMedian)
		}
	}

	// The heal must not have re-fired (MR-1: one heal per poisoning, and
	// nothing here re-poisoned).
	if w.guards["UZS"].bootstrapUnconfirmed {
		t.Error("bootstrapUnconfirmed re-set — the veto must refuse the confirm, not re-open the bootstrap")
	}
	// A recovered upstream at the true level is accepted first try.
	if !w.acceptRate("UZS", 11795) {
		t.Errorf("recovered current rate at the true level rejected; baseline=%v",
			w.guards["UZS"].lastAccepted)
	}
}

// TestPersistSnapshot_GenuineDevaluationConfirms_HistoryFollowed pins
// the veto's release path: a REAL devaluation the upstream keeps
// reporting is still pending-confirmed once the trailing-7d majority
// follows the move (the EGP ~38%-in-a-day shape). History that AGREES
// with the candidate has no power to refuse it — the veto only fires on
// a majority that REFUTES. Red-proof: replacing the veto's
// withinBand(rate, med) release with an unconditional refusal (or
// vetoing on any median != candidate) turns the confirm assertion red.
func TestPersistSnapshot_GenuineDevaluationConfirms_HistoryFollowed(t *testing.T) {
	w, cw := bandTestWorker(io.Discard)
	ctx := context.Background()

	today := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)

	// Confirmed baseline at the old level.
	for _, r := range []float64{30.9, 30.95} {
		s := snapshotOf(map[string]float64{"EGP": r})
		s.PublishedAt = today
		w.persistSnapshot(ctx, s)
	}

	// Devaluation day: first sighting of the ~60% move is held (the
	// two-strike shape, unchanged).
	first := snapshotOf(map[string]float64{"EGP": 49.5})
	first.PublishedAt = today
	w.persistSnapshot(ctx, first)
	if got := currentDayRate(cw.batches[len(cw.batches)-1], "EGP", today); got != -1 {
		t.Fatalf("first sighting of the move persisted at %v, want held for confirmation", got)
	}

	// Next refresh: the upstream keeps reporting it AND the trailing-7d
	// history has moved to the new level — the majority now agrees with
	// the candidate, so the confirm must go through.
	moved := []HistoryPoint{
		{Date: today.Add(-4 * 24 * time.Hour), RateUSD: 49.9},
		{Date: today.Add(-3 * 24 * time.Hour), RateUSD: 50.0},
		{Date: today.Add(-2 * 24 * time.Hour), RateUSD: 50.05},
		{Date: today.Add(-1 * 24 * time.Hour), RateUSD: 49.95},
	}
	second := snapshotWithHistory(
		map[string]float64{"EGP": 50.1},
		map[string][]HistoryPoint{"EGP": moved},
	)
	second.PublishedAt = today
	w.persistSnapshot(ctx, second)

	if got := currentDayRate(cw.batches[len(cw.batches)-1], "EGP", today); got != 50.1 {
		t.Errorf("confirmed devaluation persisted at %v, want 50.1 — an AGREEING history "+
			"majority must not veto; only a refuting one may", got)
	}
	if got := w.guards["EGP"].lastAccepted; got != 50.1 {
		t.Errorf("baseline = %v after a confirmed genuine devaluation, want 50.1", got)
	}
}

// TestPersistSnapshot_GenuineDevaluationConfirms_SplitWindowNoVeto pins
// the veto's OTHER release path — the mid-transition days. While the
// trailing-7d window still spans both levels (old-level bars plus
// new-level bars), there is no heal-grade mutually-agreeing majority at
// all ([historyMajority] fails the agreement test by construction), so
// the veto stands down and the two-fetch confirmation behaves exactly
// as before the fix: a real devaluation costs one refresh interval of
// lag, never a wedge. Red-proof: a naive veto that compares against the
// plain median of the full series (skipping the heal-grade agreement
// test) computes median ≈ 49.9 here — but a naive veto against, say,
// the majority-side median of a 4-of-7 split (30.8..30.9 majority in a
// [30.8 30.85 30.9 30.88 49.9 50.0 49.95] window) refuses this confirm
// and turns the assertion red.
func TestPersistSnapshot_GenuineDevaluationConfirms_SplitWindowNoVeto(t *testing.T) {
	w, cw := bandTestWorker(io.Discard)
	ctx := context.Background()

	today := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)

	for _, r := range []float64{30.9, 30.95} {
		s := snapshotOf(map[string]float64{"NGN": r})
		s.PublishedAt = today
		w.persistSnapshot(ctx, s)
	}

	hold := snapshotOf(map[string]float64{"NGN": 49.5})
	hold.PublishedAt = today
	w.persistSnapshot(ctx, hold)

	// Mid-transition window: four old-level bars, three new-level bars.
	// >10% apart, so there is no mutually-agreeing majority to consult.
	split := []HistoryPoint{
		{Date: today.Add(-7 * 24 * time.Hour), RateUSD: 30.8},
		{Date: today.Add(-6 * 24 * time.Hour), RateUSD: 30.85},
		{Date: today.Add(-5 * 24 * time.Hour), RateUSD: 30.9},
		{Date: today.Add(-4 * 24 * time.Hour), RateUSD: 30.88},
		{Date: today.Add(-3 * 24 * time.Hour), RateUSD: 49.9},
		{Date: today.Add(-2 * 24 * time.Hour), RateUSD: 50.0},
		{Date: today.Add(-1 * 24 * time.Hour), RateUSD: 49.95},
	}
	confirm := snapshotWithHistory(
		map[string]float64{"NGN": 50.1},
		map[string][]HistoryPoint{"NGN": split},
	)
	confirm.PublishedAt = today
	w.persistSnapshot(ctx, confirm)

	if got := currentDayRate(cw.batches[len(cw.batches)-1], "NGN", today); got != 50.1 {
		t.Errorf("mid-transition confirm persisted at %v, want 50.1 — a split window has "+
			"no heal-grade majority, so the veto must stand down", got)
	}
	if got := w.guards["NGN"].lastAccepted; got != 50.1 {
		t.Errorf("baseline = %v, want 50.1 — the veto must not wedge a genuine move "+
			"behind a non-majority window", got)
	}
}

// TestAcceptRate_HistoryConflictVetoReclassifiesWhenStuck mirrors the
// history band's stuck reclassification for the confirm veto: a
// provider persistently serving the same (within 1% jitter) broken
// current bar keeps hitting the veto, and past
// [stuckRejectionThreshold] consecutive refusals the repeats count
// under "deviation_history_conflict_stuck" — excluded from the
// rejection alert — while the veto keeps refusing either way. Any
// accepted current rate resets the streak.
func TestAcceptRate_HistoryConflictVetoReclassifiesWhenStuck(t *testing.T) {
	w, _ := bandTestWorker(io.Discard)
	w.guards["UZS"] = &rateGuard{lastAccepted: 11817.69}
	w.historyVeto = map[string]float64{"UZS": 11817.69}

	freshBefore := testutil.ToFloat64(
		obs.ExternalFXRateRejectedTotal.WithLabelValues(fxSource, "deviation_history_conflict"))
	stuckBefore := testutil.ToFloat64(
		obs.ExternalFXRateRejectedTotal.WithLabelValues(fxSource, "deviation_history_conflict_stuck"))

	// First sighting arms pending (plain deviation — the veto only
	// guards the confirm arm).
	if w.acceptRate("UZS", 1820) {
		t.Fatal("first sighting of the broken bar must be rejected")
	}
	// Every further sighting reaches the confirm arm and must be vetoed;
	// jitter within the 1% tolerance extends one streak.
	jitter := []float64{1817, 1822, 1820, 1815, 1824, 1819, 1821, 1816, 1823, 1818, 1820, 1822, 1819}
	for i, rate := range jitter {
		if w.acceptRate("UZS", rate) {
			t.Fatalf("veto %d: repeating broken bar %v confirmed", i+1, rate)
		}
	}
	g := w.guards["UZS"]
	if g.conflictStuckCount != len(jitter) {
		t.Fatalf("conflictStuckCount = %d, want %d", g.conflictStuckCount, len(jitter))
	}
	if g.lastAccepted != 11817.69 {
		t.Fatalf("baseline mutated to %v — the veto must never move it", g.lastAccepted)
	}

	// 12 fresh vetoes, then reclassified repeats (13 vetoes total).
	freshDelta := testutil.ToFloat64(
		obs.ExternalFXRateRejectedTotal.WithLabelValues(fxSource, "deviation_history_conflict")) - freshBefore
	stuckDelta := testutil.ToFloat64(
		obs.ExternalFXRateRejectedTotal.WithLabelValues(fxSource, "deviation_history_conflict_stuck")) - stuckBefore
	if freshDelta != float64(stuckRejectionThreshold) {
		t.Errorf("fresh deviation_history_conflict delta = %v, want %d", freshDelta, stuckRejectionThreshold)
	}
	if stuckDelta != float64(len(jitter)-stuckRejectionThreshold) {
		t.Errorf("deviation_history_conflict_stuck delta = %v, want %d",
			stuckDelta, len(jitter)-stuckRejectionThreshold)
	}

	// An accepted current rate resets the streak — recovery is fresh news.
	if !w.acceptRate("UZS", 11800) {
		t.Fatal("in-band current rate must be accepted")
	}
	if g.conflictStuckCount != 0 || g.conflictStuckRate != 0 {
		t.Errorf("streak after acceptance = (%d, %v), want (0, 0)",
			g.conflictStuckCount, g.conflictStuckRate)
	}
}
