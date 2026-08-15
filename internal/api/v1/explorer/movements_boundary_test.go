package explorer

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// These tests pin the two cap67 merge-boundary defects on
// GET /v1/accounts/{g}/movements (audit-2026-08-14):
//
//   - W1-chrollup-1 (HIGH): a cap67 watermark READ ERROR must not disable
//     the ClickHouse ceiling. The old code set wm=0 on error and guarded
//     the CH trim with `if wm > 0`, so a populated cap67 archive was served
//     untrimmed across the whole post-P23 range AND the Postgres tail
//     served the same watched-token transfers — every post-P23 movement
//     double-listed. The fix fails closed to the static P23 boundary.
//
//   - W1-chrollup-2 (MED): the CH/PG split must not move under a paginated
//     scroll. The watermark that produced page 1 is pinned into the cursor
//     and reused on continuation pages; otherwise a mid-session derive
//     advance re-reads a higher live watermark, moving the boundary and
//     making the coverage note falsely claim completeness through the new
//     (higher) watermark.

// movementsArmReader drives the CH arm and the cap67 watermark directly so
// the handler's boundary logic can be exercised without a live ClickHouse.
type movementsArmReader struct {
	*capReader
	wm     uint32
	wmErr  error
	chRows []clickhouse.AccountMovementRow
}

func (r *movementsArmReader) Cap67MovementsWatermark(context.Context) (uint32, error) {
	return r.wm, r.wmErr
}

func (r *movementsArmReader) AccountMovements(ctx context.Context, _ string, _ int, _ clickhouse.AccountMovementCursor, _ clickhouse.AccountMovementFilter) ([]clickhouse.AccountMovementRow, error) {
	r.probe.record(ctx)
	// Return a fresh copy: the handler trims the CH slice in place
	// (chRows[:0]), which would otherwise corrupt the fixture.
	return append([]clickhouse.AccountMovementRow(nil), r.chRows...), nil
}

// stubSEP41Tail is the Postgres recent-tail seam: it honours the floor
// (mirroring ListSEP41TransfersByAddress's ledger >= floorLedger clamp).
type stubSEP41Tail struct {
	rows []timescale.SEP41TransferRow
}

func (s *stubSEP41Tail) ListSEP41TransfersByAddress(_ context.Context, _ string, _ int, _ timescale.SEP41TransferCursor, _ string, floor uint32) ([]timescale.SEP41TransferRow, error) {
	var out []timescale.SEP41TransferRow
	for _, r := range s.rows {
		if r.Ledger >= floor {
			out = append(out, r)
		}
	}
	return out, nil
}

// callMovements runs AccountMovements and captures the wire view.
func callMovements(t *testing.T, reader ExplorerReader, tail SEP41MovementsReader, rawCursor string) AccountMovementsView {
	t.Helper()
	var captured AccountMovementsView
	h := newProbeHandler(reader, nil)
	h.SEP41Movements = tail
	h.WriteJSON = func(w http.ResponseWriter, v any, _ bool) {
		view, ok := v.(AccountMovementsView)
		if !ok {
			t.Fatalf("WriteJSON received %T, want AccountMovementsView", v)
		}
		captured = view
		w.WriteHeader(http.StatusOK)
	}

	target := "/v1/accounts/" + validTestAccount + "/movements"
	if rawCursor != "" {
		target += "?cursor=" + rawCursor
	}
	r := httptest.NewRequest(http.MethodGet, target, nil)
	r.SetPathValue("g_strkey", validTestAccount)
	h.AccountMovements(httptest.NewRecorder(), r)
	return captured
}

// TestAccountMovements_WatermarkReadError_DoesNotDoubleServe pins
// W1-chrollup-1: on a cap67 watermark read error with a POPULATED archive,
// the CH arm must be clamped to the static P23 boundary so its post-P23
// cap67_derived rows are NOT served alongside the Postgres tail's identical
// watched-token rows. Against the un-fixed code (wm=0 disables the CH trim)
// the same transfer is emitted twice.
func TestAccountMovements_WatermarkReadError_DoesNotDoubleServe(t *testing.T) {
	floor := timescale.SEP41MovementsFloorLedger
	postP23 := floor + 100 // a real cap67_derived movement above the P23 boundary
	when := time.Unix(1_700_000_000, 0).UTC()

	reader := &movementsArmReader{
		capReader: &capReader{probe: &deadlineProbe{}},
		wm:        0,
		wmErr:     errors.New("clickhouse: cap67 watermark: connection reset"),
		chRows: []clickhouse.AccountMovementRow{{
			Address:         validTestAccount,
			Ledger:          postP23,
			LedgerCloseTime: when,
			TxHash:          validTestTxHash,
			OpIndex:         0,
			LegIndex:        0,
			Direction:       clickhouse.AccountMovementReceived,
			MovementKind:    "transfer",
			Provenance:      "cap67_derived", // the ClickHouse arm's provenance
			Asset:           "USDC-" + validTestAccount,
			Amount:          big.NewInt(1000),
		}},
	}
	tail := &stubSEP41Tail{rows: []timescale.SEP41TransferRow{{
		ContractID: validTestContract,
		Ledger:     postP23, // the SAME transfer, served by the watched-token tail
		TxHash:     validTestTxHash,
		OpIndex:    0,
		EventIndex: 0,
		ObservedAt: when,
		ToAddr:     validTestAccount,
		Amount:     big.NewInt(1000),
	}}}

	view := callMovements(t, reader, tail, "")

	for _, m := range view.Movements {
		if m.Provenance == "cap67_derived" && m.Ledger >= floor {
			t.Fatalf("post-P23 cap67_derived (ClickHouse) row at ledger %d served during a watermark-read error — "+
				"double-listed with the Postgres watched-token tail (W1-chrollup-1)", m.Ledger)
		}
	}
	if len(view.Movements) != 1 {
		t.Fatalf("expected exactly 1 movement (the Postgres watched-token row); got %d — "+
			"the CH arm was not clamped to the static P23 boundary on a watermark read error (W1-chrollup-1)",
			len(view.Movements))
	}
}

// TestAccountMovements_PaginationPinsWatermark pins W1-chrollup-2: a
// continuation page must reuse the watermark pinned into the cursor by
// page 1, NOT the live (advanced) watermark. Against the un-fixed code the
// handler re-reads the live watermark, moving the CH ceiling up and making
// the coverage note claim completeness through the higher boundary.
func TestAccountMovements_PaginationPinsWatermark(t *testing.T) {
	base := timescale.SEP41MovementsFloorLedger
	pinnedWM := base + 1000     // the watermark page 1 committed to
	liveWM := base + 1300       // the derive has since advanced
	nativeLedger := base + 1250 // a native row in the sliver (pinned < it < live)
	cursorLedger := base + 2000 // keyset above nativeLedger, so only the ceiling can exclude it
	when := time.Unix(1_700_000_000, 0).UTC()

	reader := &movementsArmReader{
		capReader: &capReader{probe: &deadlineProbe{}},
		wm:        liveWM, // what the live read would return (used by un-fixed code)
		chRows: []clickhouse.AccountMovementRow{{
			Address:         validTestAccount,
			Ledger:          nativeLedger,
			LedgerCloseTime: when,
			TxHash:          validTestTxHash,
			OpIndex:         0,
			LegIndex:        0,
			Direction:       clickhouse.AccountMovementReceived,
			MovementKind:    "transfer",
			Provenance:      "cap67_derived",
			Asset:           "native",
			Amount:          big.NewInt(1000),
		}},
	}

	cursor := fmt.Sprintf("%d.%s.0.0.%d", cursorLedger, validTestTxHash, pinnedWM)
	view := callMovements(t, reader, &stubSEP41Tail{}, cursor)

	for _, m := range view.Movements {
		if m.Ledger == nativeLedger {
			t.Fatalf("row at ledger %d (above the PINNED page-1 watermark %d) served on a continuation page — "+
				"the CH ceiling followed the LIVE watermark %d instead of the pinned one (W1-chrollup-2)",
				nativeLedger, pinnedWM, liveWM)
		}
	}
	wantNote := fmt.Sprintf("ledger %d", pinnedWM)
	if !strings.Contains(view.CoverageNote, wantNote) {
		t.Fatalf("coverage note %q does not report the pinned watermark %d — a mid-scroll derive advance "+
			"moved the disclosed completeness boundary (W1-chrollup-2)", view.CoverageNote, pinnedWM)
	}
}
