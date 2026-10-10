package forex

import (
	"io"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

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

// persistedDayRate returns the RateUSD fx_quotes ends up holding for
// (ticker, day) after the batch lands: InsertFXQuoteBatch upserts row by
// row on (ticker, bucket), so the LAST matching row wins. -1 when absent.
func persistedDayRate(batch []FXQuote, ticker string, day time.Time) float64 {
	want := day.UTC().Truncate(24 * time.Hour)
	got := -1.0
	for _, q := range batch {
		if q.Ticker == ticker && q.Bucket.Equal(want) {
			got = q.RateUSD
		}
	}
	return got
}
