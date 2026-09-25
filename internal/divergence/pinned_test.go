package divergence_test

import (
	"context"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/divergence"
)

// TestRefreshPinnedPair_ReachesNoVerdict: a pinned refresh polls the
// references (fresh Median) but neither sets nor clears the warning, fires
// no hook, and marks the entry Pinned. Its observations are still recorded,
// against the pinned price.
func TestRefreshPinnedPair_ReachesNoVerdict(t *testing.T) {
	refs := []divergence.Reference{
		&stubReference{name: "coingecko", price: 1.00},
		&stubReference{name: "chainlink", price: 1.00},
	}
	var fired int
	sink := &recordingObservationSink{}
	svc, rdb, _ := newTestService(t, refs, divergence.ServiceOptions{
		Threshold:            5.0,
		MinSourcesForWarning: 2,
		ObservationSink:      sink,
		OnWarningFired: func(context.Context, canonical.Pair, divergence.CachedResult) {
			fired++
		},
	})
	pair := xlmUSD(t)
	ctx := context.Background()
	t0 := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	step := divergence.DefaultWarningPersistence + time.Minute

	// A pinned value 10% off, held past the persistence window, never fires.
	for i := range 3 {
		if err := svc.RefreshPinnedPair(ctx, pair, 1.10, t0.Add(time.Duration(i)*step)); err != nil {
			t.Fatalf("RefreshPinnedPair: %v", err)
		}
	}
	got := readDivergence(t, rdb, pair)
	if got.WarningFired || fired != 0 {
		t.Fatalf("pinned refreshes fired the warning (cached %v, hooks %d)", got.WarningFired, fired)
	}
	if !got.Pinned || got.Median != 1.00 || got.SuccessCount != 2 {
		t.Errorf("entry = {pinned %v median %v success %d}, want {true 1 2}", got.Pinned, got.Median, got.SuccessCount)
	}
	if len(sink.records) != 6 || sink.records[0].OurPrice != "1.1" {
		t.Errorf("pinned observations = %d (first our_price %q), want 6 against the pinned 1.1",
			len(sink.records), firstOurPrice(sink.records))
	}

	// A warning raised by fresh prices is carried through a pinned refresh
	// whose value would, if judged, read as an all-clear.
	t1 := t0.Add(10 * step)
	_ = svc.RefreshPair(ctx, pair, 1.10, t1)
	_ = svc.RefreshPair(ctx, pair, 1.10, t1.Add(step))
	if fired != 1 {
		t.Fatalf("fresh divergence: hooks = %d, want 1", fired)
	}
	_ = svc.RefreshPinnedPair(ctx, pair, 1.00, t1.Add(2*step))
	got = readDivergence(t, rdb, pair)
	if !got.WarningFired || got.FiringSince.IsZero() {
		t.Errorf("pinned refresh cleared the carried warning: fired %v since %v", got.WarningFired, got.FiringSince)
	}
	if fired != 1 {
		t.Errorf("hooks = %d after the pinned refresh, want 1", fired)
	}
	if !got.Pinned {
		t.Error("entry written by a pinned refresh is not marked pinned")
	}
}

func firstOurPrice(recs []divergence.ObservationRecord) string {
	if len(recs) == 0 {
		return ""
	}
	return recs[0].OurPrice
}
