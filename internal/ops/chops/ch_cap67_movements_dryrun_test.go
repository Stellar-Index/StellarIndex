package chops

import (
	"context"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// A dry run sizes a window in the rows a write run inserts: a two-sided
// transfer fans out to a sent row and a received row, so one event is two
// rows, not one.
func TestDeriveCap67MovementsWindow_DryRunCountsFannedOutRows(t *testing.T) {
	orig := streamCap67TransferEvents
	t.Cleanup(func() { streamCap67TransferEvents = orig })
	streamCap67TransferEvents = func(_ context.Context, _ string, _, _ uint32, _, _, _ []string, _, _, _ bool, fn func(events.Event) error) error {
		for _, sep0011 := range []string{"native", ""} {
			if err := fn(cap67TransferEvent(t, sep0011)); err != nil {
				return err
			}
		}
		return nil
	}

	rows, skipped, err := deriveCap67MovementsWindow(context.Background(), "ch", 63_000_000, 63_000_000, true)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if skipped != 0 {
		t.Fatalf("skipped = %d, want 0", skipped)
	}
	if rows != 4 {
		t.Fatalf("dry-run rows = %d, want 4 (two transfers × sent+received)", rows)
	}
}
