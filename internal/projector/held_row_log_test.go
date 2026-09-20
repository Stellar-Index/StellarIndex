package projector

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sorobanevents"
)

// heldRowWarning is the per-row line under test; the per-cycle
// "no fully-committed progress" warning also says "holding cursor" and is
// deliberately NOT throttled, so the count keys on the row-level text.
const heldRowWarning = "sink failure — holding cursor for retry (NOT advancing past this ledger)"

// TestCycle_HeldRowWarningIsThrottled pins the log budget for a row the
// projector keeps retrying: an infra fault holds the cursor every cycle for
// as long as it lasts, and the per-row warning must fire on the first
// failing cycle and every heldRowLogEvery-th after — not once per cycle,
// which turned a sustained outage into one identical line per row per
// Interval and buried the ERROR lines an operator needs (RLT-142).
func TestCycle_HeldRowWarningIsThrottled(t *testing.T) {
	const source = "rlt142-held-row-log"
	rows := []sorobanevents.Row{lakeRow(101, 1), lakeRow(102, 2)}

	h := newWedgeHarness(t, source, rows, 105, func(ev consumer.Event) error {
		if ev.(ledgerEvent).ledger == 101 {
			return errors.New("dial tcp 127.0.0.1:5432: connect: connection refused")
		}
		return nil
	})
	var logs bytes.Buffer
	h.proj.logger = slog.New(slog.NewTextHandler(&logs, nil))

	const cycles = 2*heldRowLogEvery + 1
	for i := 0; i < cycles; i++ {
		h.cycle()
	}
	if got := h.store.cursor(); got != 100 {
		t.Fatalf("cursor = %d, want 100 (an infra fault is held for retry, never shed)", got)
	}
	// Cycles 1, heldRowLogEvery and 2*heldRowLogEvery.
	const want = 3
	if got := strings.Count(logs.String(), heldRowWarning); got != want {
		t.Errorf("held-row warnings over %d cycles = %d, want %d (first cycle, then every %d)", cycles, got, want, heldRowLogEvery)
	}
	if !strings.Contains(logs.String(), "consecutive_cycles=40") {
		t.Errorf("the throttled line must still carry the running consecutive_cycles count; logs:\n%s", logs.String())
	}
}
