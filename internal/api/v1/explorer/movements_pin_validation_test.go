package explorer

import (
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// A continuation cursor's pinned watermark is client-supplied: it must be
// re-validated against the live archive boundary before it partitions the
// ClickHouse/Postgres merge.

// callMovementsStatus runs AccountMovements with rawCursor and returns the
// HTTP status plus the captured view (zero when a problem was written).
func callMovementsStatus(t *testing.T, reader ExplorerReader, tail SEP41MovementsReader, rawCursor string) (int, AccountMovementsView) {
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
	r := httptest.NewRequest(http.MethodGet, "/v1/accounts/"+validTestAccount+"/movements?cursor="+rawCursor, nil)
	r.SetPathValue("g_strkey", validTestAccount)
	rec := httptest.NewRecorder()
	h.AccountMovements(rec, r)
	return rec.Code, captured
}

func pinTestCHRow(ledger uint32) clickhouse.AccountMovementRow {
	return clickhouse.AccountMovementRow{
		Address:         validTestAccount,
		Ledger:          ledger,
		LedgerCloseTime: time.Unix(1_700_000_000, 0).UTC(),
		TxHash:          validTestTxHash,
		Direction:       clickhouse.AccountMovementReceived,
		MovementKind:    "transfer",
		Provenance:      "cap67_derived",
		Asset:           "native",
		Amount:          big.NewInt(1000),
	}
}
