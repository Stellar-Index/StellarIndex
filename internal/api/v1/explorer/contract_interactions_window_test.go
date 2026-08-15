package explorer

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// ixWindowReader embeds capReader (a full ExplorerReader) and overrides the
// two seams ContractInteractions exercises: it hands back a fixed tip so
// windowFloorLedger is deterministic, and it counts every ContractInteractions
// compute so the test can prove distinct raw ?days= values on ONE ladder rung
// collapse to a single shared cache key rather than N cold scans.
type ixWindowReader struct {
	*capReader
	tipSeq    uint32
	calls     int
	lastSince uint32
}

func (r *ixWindowReader) RecentLedgers(_ context.Context, _ int, _ uint32) ([]clickhouse.LedgerHeader, error) {
	return []clickhouse.LedgerHeader{{Seq: r.tipSeq}}, nil
}

func (r *ixWindowReader) ContractInteractions(_ context.Context, _ string, _ int, since uint32) ([]clickhouse.ContractEdgeRow, uint32, error) {
	r.calls++
	r.lastSince = since
	return nil, since, nil
}

// TestContractInteractions_QuantizesWindow is the W1-explorer-perf-1 regression
// guard. The cache key, the window floor, and the echoed window_days must all
// be built from the LADDER-QUANTIZED window, not the raw ?days=. Against the
// un-fixed handler (raw days) each distinct ?days= is its own cold cache key
// and window_days echoes the raw value, so both assertions below fail.
func TestContractInteractions_QuantizesWindow(t *testing.T) {
	const tip = uint32(60_000_000)
	reader := &ixWindowReader{capReader: &capReader{probe: &deadlineProbe{}}, tipSeq: tip}

	var captured ContractInteractionsView
	h := &Handler{
		Reader: reader,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		ParseLimit: func(_ http.ResponseWriter, _ *http.Request, def, _ int) (int, bool) {
			return def, true
		},
		ClientAborted: func(*http.Request, error) bool { return false },
		WriteProblem: func(w http.ResponseWriter, _ *http.Request, _, _ string, status int, _ string) {
			w.WriteHeader(status)
		},
		WriteJSON: func(w http.ResponseWriter, data any, _ bool) {
			if v, ok := data.(ContractInteractionsView); ok {
				captured = v
			}
			w.WriteHeader(http.StatusOK)
		},
	}

	do := func(days string) ContractInteractionsView {
		captured = ContractInteractionsView{}
		r := httptest.NewRequest(http.MethodGet, "/v1/contracts/"+validTestContract+"/interactions?days="+days, nil)
		r.SetPathValue("contract_id", validTestContract)
		h.ContractInteractions(httptest.NewRecorder(), r)
		return captured
	}

	// 45 rounds UP to the 90-day rung on contractsWindowLadder{1,7,30,90,365}.
	got := do("45")
	if got.WindowDays != 90 {
		t.Fatalf("window_days echo = %d, want 90 (45 must quantise up to the 90 rung)", got.WindowDays)
	}
	wantSince := tip - uint32(90*ledgersPerDay)
	if got.SinceLedger != wantSince {
		t.Fatalf("since_ledger = %d, want %d (floor must be computed from the quantised window)", got.SinceLedger, wantSince)
	}
	if reader.calls != 1 {
		t.Fatalf("reader calls after first request = %d, want 1", reader.calls)
	}

	// A DIFFERENT raw days on the SAME rung (80 -> 90) must hit the shared
	// cache key "ix:<cid>:90", not mint a second cold scan.
	got2 := do("80")
	if got2.WindowDays != 90 {
		t.Fatalf("second request window_days = %d, want 90", got2.WindowDays)
	}
	if reader.calls != 1 {
		t.Fatalf("reader calls after two same-rung requests = %d, want 1 "+
			"(raw ?days= is not quantised, so the cache key is not shared)", reader.calls)
	}
}
