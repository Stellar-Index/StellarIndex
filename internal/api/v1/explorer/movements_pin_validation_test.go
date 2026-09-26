package explorer

import (
	"fmt"
	"math"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// A continuation cursor's pinned watermark is client-supplied: it must be
// re-validated against the live archive boundary before it partitions the
// ClickHouse/Postgres merge (GH-622).

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

// TestAccountMovements_PinAboveLiveWatermarkRejected: a pin above the live
// boundary would ceiling CH at the pin (rows in (live, pin] not derived yet)
// and floor PG above it, dropping that range from BOTH arms.
func TestAccountMovements_PinAboveLiveWatermarkRejected(t *testing.T) {
	base := timescale.SEP41MovementsFloorLedger
	liveWM := base + 1000
	reader := &movementsArmReader{capReader: &capReader{probe: &deadlineProbe{}}, wm: liveWM}
	cursor := fmt.Sprintf("%d.%s.0.0.%d", base+5000, validTestTxHash, base+4000)

	code, _ := callMovementsStatus(t, reader, &stubSEP41Tail{}, cursor)
	if code != http.StatusBadRequest {
		t.Fatalf("pinned watermark %d above live %d: status %d, want 400 invalid-cursor", base+4000, liveWM, code)
	}
}

// TestAccountMovements_MaxUint32PinDoesNotDoubleList: wm=MaxUint32 wrapped
// wm+1 to 0 so the PG floor fell back to P23 while CH was ceilinged at
// MaxUint32 — every post-P23 transfer served by both arms.
func TestAccountMovements_MaxUint32PinDoesNotDoubleList(t *testing.T) {
	base := timescale.SEP41MovementsFloorLedger
	postP23 := base + 100
	when := time.Unix(1_700_000_000, 0).UTC()
	reader := &movementsArmReader{
		capReader: &capReader{probe: &deadlineProbe{}},
		wm:        base + 50, // live archive boundary is below the row
		chRows:    []clickhouse.AccountMovementRow{pinTestCHRow(postP23)},
	}
	tail := &stubSEP41Tail{rows: []timescale.SEP41TransferRow{{
		ContractID: validTestContract, Ledger: postP23, TxHash: validTestTxHash,
		ObservedAt: when, ToAddr: validTestAccount, Amount: big.NewInt(1000),
	}}}
	cursor := fmt.Sprintf("%d.%s.0.0.%d", base+9000, validTestTxHash, uint32(math.MaxUint32))

	code, view := callMovementsStatus(t, reader, tail, cursor)
	if code == http.StatusOK && len(view.Movements) > 1 {
		t.Fatalf("pinned wm=MaxUint32 served ledger %d from both arms (%d rows) — wm+1 overflowed the PG floor to 0",
			postP23, len(view.Movements))
	}
	if code != http.StatusBadRequest {
		t.Fatalf("pinned wm=MaxUint32 above live %d: status %d, want 400 invalid-cursor", base+50, code)
	}
}

// TestAccountMovements_PinBelowFloorDoesNotHidePreP23Archive: a pin below
// the P23 boundary must not lower the CH ceiling beneath the classic
// archive's range — PG is floored at P23 regardless, so every ledger in
// (pin, P23) would be served by neither arm.
func TestAccountMovements_PinBelowFloorDoesNotHidePreP23Archive(t *testing.T) {
	base := timescale.SEP41MovementsFloorLedger
	preP23 := base - 10
	reader := &movementsArmReader{
		capReader: &capReader{probe: &deadlineProbe{}},
		wm:        base + 1000,
		chRows:    []clickhouse.AccountMovementRow{pinTestCHRow(preP23)},
	}
	cursor := fmt.Sprintf("%d.%s.0.0.%d", base, validTestTxHash, 1)

	code, view := callMovementsStatus(t, reader, &stubSEP41Tail{}, cursor)
	if code != http.StatusOK {
		t.Fatalf("status %d, want 200", code)
	}
	if len(view.Movements) != 1 || view.Movements[0].Ledger != preP23 {
		t.Fatalf("pre-P23 archive row at ledger %d hidden by a pin of 1 (got %d rows) — CH ceiling dropped below the P23 floor",
			preP23, len(view.Movements))
	}
	if strings.Contains(view.CoverageNote, "through ledger 1 ") {
		t.Fatalf("coverage note %q claims all-assets coverage through a pre-P23 pin", view.CoverageNote)
	}
}

// TestAccountMovements_ValidPinStillHonoured: a pin at or below the live
// boundary is the legitimate case and keeps working (W1-chrollup-2).
func TestAccountMovements_ValidPinStillHonoured(t *testing.T) {
	base := timescale.SEP41MovementsFloorLedger
	reader := &movementsArmReader{capReader: &capReader{probe: &deadlineProbe{}}, wm: base + 1300}
	cursor := fmt.Sprintf("%d.%s.0.0.%d", base+2000, validTestTxHash, base+1000)

	code, _ := callMovementsStatus(t, reader, &stubSEP41Tail{}, cursor)
	if code != http.StatusOK {
		t.Fatalf("valid pin below live watermark: status %d, want 200", code)
	}
	if !reader.gotFilter.HasMaxLedger || reader.gotFilter.MaxLedger != base+1000 {
		t.Fatalf("CH ceiling = %d (set=%v), want the pinned %d", reader.gotFilter.MaxLedger, reader.gotFilter.HasMaxLedger, base+1000)
	}
}
