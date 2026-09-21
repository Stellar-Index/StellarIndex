package forex

import (
	"context"
	"io"
	"testing"
	"time"
)

// TestPersistSnapshot_HealedBaselineIsHealedAgainWhenHistoryCorrects
// is the mirror image of the UZS incident: the CURRENT feed is right
// and the HISTORY endpoint is the broken side. The bootstrap sample is
// refuted by seven mutually-agreeing wrong bars, so the heal re-points
// the baseline at the wrong level (that inversion is indistinguishable
// from inside the worker and is not what this test objects to). What
// must NOT happen is the wedge: once the history endpoint is fixed and
// its majority refutes the healed baseline, the baseline must be healed
// again. A heal that cleared the ticker to "confirmed" made the first
// heal final — the correct current rate stayed rejected and the confirm
// veto refused it against the same broken bars, permanently.
func TestPersistSnapshot_HealedBaselineIsHealedAgainWhenHistoryCorrects(t *testing.T) {
	w, _ := bandTestWorker(io.Discard)
	ctx := context.Background()
	today := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)

	bars := func(level float64) []HistoryPoint {
		out := make([]HistoryPoint, 0, 7)
		for i := 7; i >= 1; i-- {
			out = append(out, HistoryPoint{Date: today.Add(-time.Duration(i) * 24 * time.Hour), RateUSD: level})
		}
		return out
	}
	const trueLevel, brokenLevel = 11800.0, 1820.0

	// Day 1: correct bootstrap, broken history → inverted heal.
	snap := snapshotWithHistory(map[string]float64{"UZS": trueLevel}, map[string][]HistoryPoint{"UZS": bars(brokenLevel)})
	snap.PublishedAt = today
	w.persistSnapshot(ctx, snap)
	if g := w.guards["UZS"]; g == nil || !withinBand(g.lastAccepted, brokenLevel) {
		t.Fatalf("precondition: the history majority did not heal the bootstrap baseline; guard=%+v", w.guards["UZS"])
	}

	// Day 2: history endpoint fixed; the current feed was right all along.
	snap = snapshotWithHistory(map[string]float64{"UZS": trueLevel}, map[string][]HistoryPoint{"UZS": bars(trueLevel)})
	snap.PublishedAt = today.Add(24 * time.Hour)
	w.persistSnapshot(ctx, snap)

	g := w.guards["UZS"]
	if !withinBand(g.lastAccepted, trueLevel) {
		t.Fatalf("baseline still %v after the corrected history majority (%v) refuted the healed level: "+
			"the heal is once-per-ticker, so an inverted heal wedges the ticker for good", g.lastAccepted, trueLevel)
	}
	if !w.acceptRate("UZS", 11795) {
		t.Errorf("current rate at the true level rejected after the second heal; baseline=%v", g.lastAccepted)
	}
	if g.healedUncorroborated {
		t.Error("a current fetch agreeing with the healed baseline must clear healedUncorroborated")
	}
}
