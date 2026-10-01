package projector

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sorobanevents"
)

const malformedRowWarning = "malformed landing-zone row — skipped"

// TestCycle_MalformedRowIsLoggedWithIdentity pins that a landing-zone row
// Reconstruct rejects is counted as reconstruct_error (not decode_error), marks
// the cycle decode_degraded, and is logged with its full identity and error,
// throttled to the first and every reconstructErrLogEvery-th failure.
func TestCycle_MalformedRowIsLoggedWithIdentity(t *testing.T) {
	const source = "malformed-row-log"
	const bad = 2*reconstructErrLogEvery + 1
	rows := make([]sorobanevents.Row, 0, bad+1)
	for i := 0; i < bad; i++ {
		r := lakeRow(uint32(101+i), byte(i+1))
		r.Topic0XDR = nil // Reconstruct: missing topic_0_xdr
		rows = append(rows, r)
	}
	rows = append(rows, lakeRow(uint32(101+bad), 0xff))
	beforeReconstruct := decodedCount(t, source, "reconstruct_error")
	beforeDecode := decodedCount(t, source, "decode_error")
	beforeDegraded := runsCount(t, source, "decode_degraded")

	h := newWedgeHarness(t, source, rows, uint32(101+bad+5), func(consumer.Event) error { return nil })
	var logs bytes.Buffer
	h.proj.logger = slog.New(slog.NewTextHandler(&logs, nil))
	h.cycle()

	if got := decodedCount(t, source, "reconstruct_error") - beforeReconstruct; got != bad {
		t.Errorf("reconstruct_error delta = %v, want %d", got, bad)
	}
	if got := decodedCount(t, source, "decode_error") - beforeDecode; got != 0 {
		t.Errorf("decode_error delta = %v, want 0 (reconstruct failures are counted separately)", got)
	}
	if got := runsCount(t, source, "decode_degraded") - beforeDegraded; got != 1 {
		t.Errorf("runs_total{outcome=decode_degraded} delta = %v, want 1", got)
	}
	out := logs.String()
	// Failures 1, reconstructErrLogEvery and 2*reconstructErrLogEvery.
	if got, want := strings.Count(out, malformedRowWarning), 3; got != want {
		t.Errorf("malformed-row warnings = %d, want %d; logs:\n%s", got, want, out)
	}
	first := rows[0]
	for _, want := range []string{
		"source=" + source,
		"ledger=101",
		"tx=0165",
		"op_index=0",
		"event_index=1",
		"contract=" + first.ContractID,
		"missing topic_0_xdr",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("malformed-row warning lacks %q; logs:\n%s", want, out)
		}
	}
}
