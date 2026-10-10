package forex

import (
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

// A break spanning several trailing bars is re-scored in full on every
// refresh; the stuck streak must advance once per refresh, not per bar.
func TestGuardSnapshot_StuckStreakCountsRefreshesNotBars(t *testing.T) {
	w, _ := bandTestWorker(io.Discard)
	today := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
	w.guards["ETB"] = &rateGuard{lastAccepted: 160}

	const brokenBars = 7
	points := make([]HistoryPoint, brokenBars)
	for i := range points {
		points[i] = HistoryPoint{Date: today.Add(-time.Duration(i+1) * 24 * time.Hour), RateUSD: 44}
	}
	for i := 1; i <= stuckRejectionThreshold; i++ {
		snap := snapshotWithHistory(map[string]float64{"ETB": 160}, map[string][]HistoryPoint{"ETB": points})
		snap.PublishedAt = today.Add(18 * time.Hour)
		w.guardSnapshot(snap)
		if got := w.guards["ETB"].stuckCount; got != i {
			t.Fatalf("after %d refreshes stuckCount = %d, want %d", i, got, i)
		}
	}
}

// TestAcceptHistoryRate_StuckUpstreamReclassifies is the Massive ETB=44
// incident: a provider serving the SAME broken history bar
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
// threshold. Guards against each sweep's good bars resetting the streak the
// broken bar just incremented, which oscillates 0↔1 so the alert
// never de-noises and pages for days.
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

// TestGuardSnapshot_CachedTodayHistoryBarNeverOverwritesCurrentRow pins
// CA2-A18-correct-1. The history window includes the publication date and
// that bar is cached from the day's first fetch until the date rolls.
// Appended after the current row, the per-row upsert let the frozen bar
// replace every later refresh's current rate, so fx_quotes(EUR, today)
// disagreed with the served cache all day. The current row must be the
// only writer of today's bucket; today's bar stays in the served history.
func TestGuardSnapshot_CachedTodayHistoryBarNeverOverwritesCurrentRow(t *testing.T) {
	w, _ := bandTestWorker(io.Discard)
	today := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)

	snap := snapshotWithHistory(
		map[string]float64{"EUR": 0.9290}, // 18:00 current close
		map[string][]HistoryPoint{"EUR": {
			{Date: today.Add(-2 * 24 * time.Hour), RateUSD: 0.9210},
			{Date: today.Add(-1 * 24 * time.Hour), RateUSD: 0.9230},
			{Date: today, RateUSD: 0.9200}, // cached at the day's first fetch
		}},
	)
	snap.PublishedAt = today.Add(18 * time.Hour)

	res := w.guardSnapshot(snap)
	if got := persistedDayRate(res.batch, "EUR", today); got != 0.9290 {
		t.Fatalf("fx_quotes(EUR, today) after upsert = %v, want the current rate 0.9290", got)
	}
	if got := persistedDayRate(res.batch, "EUR", today.Add(-24*time.Hour)); got != 0.9230 {
		t.Fatalf("fx_quotes(EUR, today-1) = %v, want history bar 0.9230", got)
	}
	var sawToday bool
	for _, p := range res.history["EUR"] {
		if p.Date.Equal(today) && p.RateUSD == 0.9200 {
			sawToday = true
		}
	}
	if !sawToday {
		t.Fatalf("served history lost today's bar: %+v", res.history["EUR"])
	}
}
