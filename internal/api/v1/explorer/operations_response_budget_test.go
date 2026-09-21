package explorer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// ledgerOpsReader is a capReader whose OperationsByLedger returns a
// caller-supplied fixed page — used to feed GET /v1/operations?ledger=N
// rows with controlled BodyXDR sizes.
type ledgerOpsReader struct {
	*capReader
	rows []clickhouse.OpRow
}

func (r *ledgerOpsReader) OperationsByLedger(ctx context.Context, _ uint32, _ int) ([]clickhouse.OpRow, error) {
	r.probe.record(ctx)
	return r.rows, nil
}

// garbageXDR is deliberately undecodable so opView always takes the decode-
// failure path, which echoes the row's own BodyXDR back onto RawXDR — a
// clean, deterministic signal that a row was FULLY processed. opViewLight
// never sets RawXDR at all, so RawXDR == "" cleanly signals the row was
// served undecoded.
func garbageXDR(n int) string {
	return strings.Repeat("Z", n)
}

// Q207: ParseLimit(500, 2000) bounds ROW count but not the response's BYTE
// size — an op's decoded body is attacker-influenced in size. Past
// operationsResponseByteBudget, further rows must be served UNDECODED
// (RawXDR empty) rather than fully decoded, bounding the response to
// roughly the budget regardless of how many giant-bodied ops a ledger has.
func TestOperations_LedgerPath_BoundsDecodedResponseBytes(t *testing.T) {
	half := operationsResponseByteBudget / 2
	rows := []clickhouse.OpRow{
		{Seq: 42, CloseTime: time.Unix(1700000000, 0).UTC(), TxHash: "h0", BodyXDR: garbageXDR(half)},        // fits: decoded
		{Seq: 42, CloseTime: time.Unix(1700000000, 0).UTC(), TxHash: "h1", BodyXDR: garbageXDR(half + 1024)}, // would exceed budget: light
		{Seq: 42, CloseTime: time.Unix(1700000000, 0).UTC(), TxHash: "h2", BodyXDR: garbageXDR(10)},          // still fits under budget: decoded
	}
	reader := &ledgerOpsReader{capReader: &capReader{probe: &deadlineProbe{}}, rows: rows}
	h := newProbeHandler(reader, nil)
	var captured OperationsView
	h.WriteJSON = func(w http.ResponseWriter, data any, _ bool) {
		if v, ok := data.(OperationsView); ok {
			captured = v
		}
		w.WriteHeader(http.StatusOK)
	}

	rec := httptest.NewRecorder()
	h.Operations(rec, httptest.NewRequest(http.MethodGet, "/v1/operations?ledger=42", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if len(captured.Operations) != 3 {
		t.Fatalf("got %d operations, want 3 (rows are degraded, never dropped)", len(captured.Operations))
	}
	if captured.Operations[0].RawXDR != rows[0].BodyXDR {
		t.Errorf("row 0 (fits the budget) was not fully decoded: RawXDR = %d bytes, want %d",
			len(captured.Operations[0].RawXDR), len(rows[0].BodyXDR))
	}
	if captured.Operations[1].RawXDR != "" {
		t.Errorf("row 1 (would exceed the budget) was fully decoded anyway: RawXDR = %d bytes, want 0 (light)",
			len(captured.Operations[1].RawXDR))
	}
	if captured.Operations[2].RawXDR != rows[2].BodyXDR {
		t.Errorf("row 2 (still under budget after row 1 was skipped) was not decoded: RawXDR = %d bytes, want %d",
			len(captured.Operations[2].RawXDR), len(rows[2].BodyXDR))
	}
}
