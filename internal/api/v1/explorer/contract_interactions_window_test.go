package explorer

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// ixWindowGenesis and ixWindowCadence model a synthetic, deliberately
// NON-theoretical ledger close cadence (6s, i.e. 14,400/day — distinct from
// the old code's hardcoded 17,280/day) so windowFloorLedger's close_time
// boundary produces a DIFFERENT, independently-computable answer than the
// old ledger-count arithmetic (CA2-A03-correct-0). ixWindowCloseTime is the
// single source of truth both the fake reader and the test's expectation
// are built from.
var (
	ixWindowGenesis = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	ixWindowCadence = 6 * time.Second
)

func ixWindowCloseTime(seq uint32) time.Time {
	return ixWindowGenesis.Add(ixWindowCadence * time.Duration(seq))
}

// ixWindowReader embeds capReader (a full ExplorerReader) and overrides the
// seams ContractInteractions exercises: it hands back a fixed tip and a
// close_time for every sequence (so windowFloorLedger's binary search is
// deterministic), and it counts every ContractInteractions compute so the
// test can prove distinct raw ?days= values on ONE ladder rung collapse to
// a single shared cache key rather than N cold scans.
type ixWindowReader struct {
	*capReader
	tipSeq    uint32
	calls     int
	lastSince uint32
}

func (r *ixWindowReader) RecentLedgers(_ context.Context, _ int, _ uint32) ([]clickhouse.LedgerHeader, error) {
	return []clickhouse.LedgerHeader{{Seq: r.tipSeq, CloseTime: ixWindowCloseTime(r.tipSeq)}}, nil
}

func (r *ixWindowReader) LedgerBySeq(_ context.Context, seq uint32) (clickhouse.LedgerHeader, bool, error) {
	if seq == 0 || seq > r.tipSeq {
		return clickhouse.LedgerHeader{}, false, nil
	}
	return clickhouse.LedgerHeader{Seq: seq, CloseTime: ixWindowCloseTime(seq)}, true, nil
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
	// Independently derived from the model, not from windowFloorLedger's own
	// arithmetic: 90 days at the fake reader's 6s cadence is exactly
	// 90*86400/6 = 1,296,000 ledgers — the boundary is exact (no rounding),
	// so the smallest sequence at/after it is precisely tip-1,296,000.
	wantSince := tip - 1_296_000
	if got.SinceLedger != wantSince {
		t.Fatalf("since_ledger = %d, want %d (floor must be computed from close_time, not a theoretical ledgers/day constant)", got.SinceLedger, wantSince)
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

// failingTipReader embeds capReader but fails the tip read RecentLedgers
// relies on, and counts ContractInteractions calls so the test can prove the
// handler never reaches the reader with a genesis-wide floor.
type failingTipReader struct {
	*capReader
	ixCalls int
}

func (r *failingTipReader) RecentLedgers(context.Context, int, uint32) ([]clickhouse.LedgerHeader, error) {
	return nil, errors.New("clickhouse: connection reset")
}

func (r *failingTipReader) ContractInteractions(_ context.Context, _ string, _ int, since uint32) ([]clickhouse.ContractEdgeRow, uint32, error) {
	r.ixCalls++
	return nil, since, nil
}

// TestContractInteractions_TipReadFailureRefusesWindow is the RLT-099 / #581b
// regression guard. windowFloorLedger must not fold a FAILED tip read into
// the same 0 it returns for "genuinely no ledgers captured yet": the un-fixed
// handler serves 200 OK with since_ledger=0 (an unbounded, genesis-wide scan)
// on every ClickHouse tip-read error; the fixed handler refuses the request
// instead of ever asking the reader to scan from ledger 0.
func TestContractInteractions_TipReadFailureRefusesWindow(t *testing.T) {
	reader := &failingTipReader{capReader: &capReader{probe: &deadlineProbe{}}}

	var (
		captured   ContractInteractionsView
		gotProblem bool
	)
	h := &Handler{
		Reader: reader,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		ParseLimit: func(_ http.ResponseWriter, _ *http.Request, def, _ int) (int, bool) {
			return def, true
		},
		ClientAborted: func(*http.Request, error) bool { return false },
		WriteProblem: func(w http.ResponseWriter, _ *http.Request, _, _ string, status int, _ string) {
			gotProblem = true
			w.WriteHeader(status)
		},
		WriteJSON: func(w http.ResponseWriter, data any, _ bool) {
			if v, ok := data.(ContractInteractionsView); ok {
				captured = v
			}
			w.WriteHeader(http.StatusOK)
		},
	}

	r := httptest.NewRequest(http.MethodGet, "/v1/contracts/"+validTestContract+"/interactions?days=30", nil)
	r.SetPathValue("contract_id", validTestContract)
	w := httptest.NewRecorder()
	h.ContractInteractions(w, r)

	if !gotProblem {
		t.Fatalf("tip-read failure must be surfaced as a problem response, got 200 OK with body %+v", captured)
	}
	if w.Code == http.StatusOK {
		t.Fatalf("status = %d, want a non-200 refusal on a failed tip read", w.Code)
	}
	if reader.ixCalls != 0 {
		t.Fatalf("ContractInteractions called %d times, want 0 "+
			"(a failed tip read must not fall through to a since=0 genesis-wide scan)", reader.ixCalls)
	}
}
