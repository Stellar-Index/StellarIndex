package projector

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sorobanevents"
)

// TestCycle_LogsLastSeenLedger pins T119: the cycle-summary "projector
// cycle" log line must actually carry last_seen_ledger, matching the
// highest ledger this cycle's stream callback observed — the comment above
// the commit-watermark logic claims "lastSeenLedger is only logged", but
// until this the variable was tracked and never referenced by any log call.
func TestCycle_LogsLastSeenLedger(t *testing.T) {
	const source = "t119-last-seen-ledger"
	rows := []sorobanevents.Row{lakeRow(101, 1), lakeRow(102, 2)}

	h := newWedgeHarness(t, source, rows, 105, func(_ consumer.Event) error { return nil })
	var logs bytes.Buffer
	h.proj.logger = slog.New(slog.NewTextHandler(&logs, nil))

	h.cycle()

	if got := h.store.cursor(); got != 105 {
		t.Fatalf("cursor = %d, want 105 (full window committed)", got)
	}
	if !strings.Contains(logs.String(), "last_seen_ledger=102") {
		t.Errorf("projector cycle log missing last_seen_ledger=102 (highest ledger scanned); logs:\n%s", logs.String())
	}
}
