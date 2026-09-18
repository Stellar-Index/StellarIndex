package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/anomaly"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/freeze"
)

// ─── a release must not clear a sibling it has never seen ─────────
//
// The marker is pair-scoped and carries one ladder per window, so the
// last window to release deletes it. "Last" was decided from
// o.freezeStates alone — and a window only enters that map when it reaches
// the freeze step IN THIS PROCESS. A window sitting under the USD-volume
// floor never does, and after a restart no window has yet. Such a window's
// freeze exists only in the marker, so the in-memory check could not see
// it: a sibling's release deleted the marker AND retired the durable
// ladder, and an ESCALATED freeze — which ADR-0019 holds "until manual
// unfreeze" — ended because a different window recovered. That is the
// under-freeze direction: the window's next qualifying bucket finds no
// marker, no ladder, and (cold, so no prev-VWAP comparator) nothing to
// re-fire on, and publishes.

func coldSiblingDecision() anomaly.Decision {
	return anomaly.Decision{
		Action: anomaly.ActionFreeze,
		Class:  anomaly.ClassCrypto,
		Reason: "phase2:3_signal_AND",
	}
}

func TestFreezeWindowIsolation_ReleaseDoesNotClearAColdSiblingsFreeze(t *testing.T) {
	f := newWindowIsolationFixture(t)
	ctx := context.Background()
	writer, ok := f.orch.cfg.FreezeWriter.(*freeze.Writer)
	if !ok {
		t.Fatalf("fixture FreezeWriter is %T, want the production *freeze.Writer", f.orch.cfg.FreezeWriter)
	}

	// Wall-clock states: the Writer judges a ladder's liveness against
	// time.Now, as it does in production.
	now := time.Now().UTC()
	escalated := freeze.State{
		FiredAt:        now.Add(-2*time.Hour - 5*time.Minute),
		HoldUntil:      now.Add(25 * time.Minute),
		ExtensionsUsed: freeze.DefaultMaxExtensions,
		Escalated:      true,
	}
	recovering := freeze.State{
		FiredAt:   now.Add(-15 * time.Minute),
		HoldUntil: now.Add(5 * time.Minute),
	}
	if err := writer.MarkHoldForWindow(ctx, f.pair.Base, f.pair.Quote, f.long, lkgFormatted,
		coldSiblingDecision(), escalated, 30*time.Minute); err != nil {
		t.Fatalf("MarkHoldForWindow(1h): %v", err)
	}
	if err := writer.MarkHoldForWindow(ctx, f.pair.Base, f.pair.Quote, f.short, lkgFormatted,
		coldSiblingDecision(), recovering, 10*time.Minute); err != nil {
		t.Fatalf("MarkHoldForWindow(5m): %v", err)
	}

	// A deploy. Every key is cold. The 5m window rehydrates and goes on to
	// earn its release; the 1h window is under the volume floor and has
	// not reached the freeze step in this process at all.
	f.restart()
	f.orch.freezeStates[f.key(f.short)] = recovering
	if _, cached := f.orch.freezeStates[f.key(f.long)]; cached {
		t.Fatal("setup: the 1h window must be cold in the restarted process")
	}

	f.orch.releaseFreeze(ctx, f.pair, f.short, f.key(f.short), recovering, freeze.TransitionReleased)

	if !f.markerPresent() {
		t.Fatal("the 5m window's release deleted the pair's freeze marker while the 1h window's " +
			"ESCALATED ladder was still in it — flags.frozen cleared and the escalated " +
			"freeze ended because a sibling window recovered")
	}
	gotLong, present, err := writer.LoadStateForWindow(ctx, f.pair.Base, f.pair.Quote, f.long)
	if err != nil || !present {
		t.Fatalf("LoadStateForWindow(1h) = (present=%v, err=%v), want present", present, err)
	}
	if !gotLong.Escalated || gotLong.ExtensionsUsed != freeze.DefaultMaxExtensions {
		t.Errorf("1h ladder after its sibling's release = %+v, want the escalated ladder intact", gotLong)
	}
	gotShort, _, err := writer.LoadStateForWindow(ctx, f.pair.Base, f.pair.Quote, f.short)
	if err != nil {
		t.Fatalf("LoadStateForWindow(5m): %v", err)
	}
	if gotShort.Active() {
		t.Errorf("the released 5m window still owns a ladder in the marker: %+v", gotShort)
	}
	if f.orch.freezeStates[f.key(f.short)].Active() {
		t.Error("the 5m window's in-memory freeze survived its own release")
	}
}

// TestFreezeWindowIsolation_LastReleaseStillClearsTheMarker is the other
// half: consulting the marker must not turn into never clearing it. With
// no other window's ladder in the record, the release deletes the marker
// so flags.frozen clears at once rather than after the remaining hold.
func TestFreezeWindowIsolation_LastReleaseStillClearsTheMarker(t *testing.T) {
	f := newWindowIsolationFixture(t)
	ctx := context.Background()
	writer, ok := f.orch.cfg.FreezeWriter.(*freeze.Writer)
	if !ok {
		t.Fatalf("fixture FreezeWriter is %T, want the production *freeze.Writer", f.orch.cfg.FreezeWriter)
	}
	now := time.Now().UTC()
	recovering := freeze.State{FiredAt: now.Add(-15 * time.Minute), HoldUntil: now.Add(5 * time.Minute)}
	if err := writer.MarkHoldForWindow(ctx, f.pair.Base, f.pair.Quote, f.short, lkgFormatted,
		coldSiblingDecision(), recovering, 10*time.Minute); err != nil {
		t.Fatalf("MarkHoldForWindow(5m): %v", err)
	}
	f.restart()
	f.orch.freezeStates[f.key(f.short)] = recovering

	f.orch.releaseFreeze(ctx, f.pair, f.short, f.key(f.short), recovering, freeze.TransitionReleased)

	if f.markerPresent() {
		t.Error("the pair's only frozen window released and the marker is still there — " +
			"flags.frozen stays set for the rest of a hold that has ended")
	}
}
