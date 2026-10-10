package chops

import (
	"context"
	"slices"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
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

// The window derive must ask the lake for mint, burn and clawback as well as
// transfer, and write every one of them. The stub honours the topic[0]
// prefilter the way the SQL does.
func TestDeriveCap67MovementsWindow_IncludesSupplyKinds(t *testing.T) {
	canonical.InstallNetworkPassphrase("")
	orig := streamCap67TransferEvents
	t.Cleanup(func() { streamCap67TransferEvents = orig })
	lake := []struct {
		sym string
		ev  events.Event
	}{
		{"transfer", cap67TransferEvent(t, "native")},
		{"mint", cap67SupplyEvent(t, "mint", cap67USDCSAC, cap67USDCName, scI128(1))},
		{"burn", cap67SupplyEvent(t, "burn", cap67USDCSAC, cap67USDCName, scI128(1))},
		{"clawback", cap67SupplyEvent(t, "clawback", cap67USDCSAC, cap67USDCName, scI128(1))},
	}
	streamCap67TransferEvents = func(_ context.Context, _ string, _, _ uint32, _, topic0Syms, _ []string, _, _, _ bool, fn func(events.Event) error) error {
		for _, l := range lake {
			if !slices.Contains(topic0Syms, l.sym) {
				continue
			}
			if err := fn(l.ev); err != nil {
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
	if rows != 8 {
		t.Fatalf("dry-run rows = %d, want 8 (transfer, mint, burn, clawback × two sides)", rows)
	}
}
