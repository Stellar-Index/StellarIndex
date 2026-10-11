package explorer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// These tests pin the cap67 merge boundary on GET /v1/accounts/{g}/movements:
//
//   - A cap67 watermark READ ERROR must not disable the ClickHouse ceiling. An untrimmed archive
//     plus the Postgres tail would double-list every post-P23 movement, so it fails closed to
//     the static P23 boundary.
//
//   - The CH/PG split must not move under a paginated scroll. Page 1's watermark is pinned into
//     the cursor, else a mid-session derive advance moves the boundary and the coverage note
//     falsely claims completeness through the newer watermark.

// movementsArmReader drives the CH arm and the cap67 watermark directly so
// the handler's boundary logic can be exercised without a live ClickHouse.
type movementsArmReader struct {
	*capReader
	wm    uint32
	wmErr error
	// supplyFrom..supplyThru is the supply-kind range (supplyFrom 0: none).
	supplyFrom, supplyThru uint32
	chRows                 []clickhouse.AccountMovementRow

	// gotFilter is the filter the handler actually passed to the CH arm
	// — the seam the ceiling travels through.
	gotFilter clickhouse.AccountMovementFilter
}

func (r *movementsArmReader) Cap67MovementsWatermark(context.Context) (uint32, error) {
	return r.wm, r.wmErr
}

func (r *movementsArmReader) Cap67SupplyCoverage(context.Context) (uint32, uint32, bool, error) {
	return r.supplyFrom, r.supplyThru, r.supplyFrom != 0, r.wmErr
}

func (r *movementsArmReader) AccountMovements(ctx context.Context, _ string, limit int, _ clickhouse.AccountMovementCursor, f clickhouse.AccountMovementFilter) ([]clickhouse.AccountMovementRow, error) {
	r.probe.record(ctx)
	r.gotFilter = f
	// Mirror the real SQL: the ledger ceiling is a WHERE predicate applied BEFORE the LIMIT, so a
	// row above filter.MaxLedger is never returned. A fake ignoring the filter could not tell a
	// post-read trim in the handler from a bounded query.
	out := make([]clickhouse.AccountMovementRow, 0, len(r.chRows))
	for _, row := range r.chRows {
		if f.HasMaxLedger && row.Ledger > f.MaxLedger {
			continue
		}
		out = append(out, row)
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out, nil
}

// stubSEP41Tail is the Postgres recent-tail seam: it honours the floor
// (mirroring ListSEP41TransfersByAddress's ledger >= floorLedger clamp).
type stubSEP41Tail struct {
	rows []timescale.SEP41TransferRow
}

func (s *stubSEP41Tail) ListSEP41TransfersByAddress(_ context.Context, _ string, _ int, _ timescale.SEP41TransferCursor, _, contractID string, floor uint32) ([]timescale.SEP41TransferRow, error) {
	var out []timescale.SEP41TransferRow
	for _, r := range s.rows {
		if r.Ledger >= floor && (contractID == "" || r.ContractID == contractID) {
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
