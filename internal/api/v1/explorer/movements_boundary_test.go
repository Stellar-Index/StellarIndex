package explorer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// These tests pin the two cap67 merge-boundary defects on
// GET /v1/accounts/{g}/movements:
//
//   - a cap67 watermark READ ERROR must not disable
//     the ClickHouse ceiling. Setting wm=0 on error and guarding
//     the CH trim with `if wm > 0` would serve a populated cap67 archive
//     untrimmed across the whole post-P23 range AND the Postgres tail
//     would serve the same watched-token transfers — every post-P23 movement
//     double-listed. It fails closed to the static P23 boundary.
//
//   - the CH/PG split must not move under a paginated
//     scroll. The watermark that produced page 1 is pinned into the cursor
//     and reused on continuation pages; otherwise a mid-session derive
//     advance re-reads a higher live watermark, moving the boundary and
//     making the coverage note falsely claim completeness through the new
//     (higher) watermark.

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
	// Model the real reader's SQL semantics: the ledger ceiling
	// is a WHERE predicate applied BEFORE the LIMIT, so a fixture row
	// above filter.MaxLedger is never returned at all. A fake that
	// ignored the filter would let a post-read trim in the handler look
	// indistinguishable from a bounded query.
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
