package explorer

import (
	"context"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// The getAccountMovements description must state the merge seam: the archive has a second writer (ch-cap67-movements,
// cap67_derived rows past P23). The CH/PG boundary is its watermark; a single-writer P23 claim would make integrators
// attribute post-P23 rows to the Postgres tail.
func TestMovementsSpecDescribesWatermarkSeam(t *testing.T) {
	raw, err := os.ReadFile("../../../../openapi/stellar-index.v1.yaml")
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	var doc struct {
		Paths map[string]map[string]struct {
			OperationID string `yaml:"operationId"`
			Description string `yaml:"description"`
		} `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse spec: %v", err)
	}
	op := doc.Paths["/accounts/{g_strkey}/movements"]["get"]
	if op.OperationID != "getAccountMovements" {
		t.Fatalf("GET /accounts/{g_strkey}/movements operationId = %q, want getAccountMovements", op.OperationID)
	}
	desc := strings.Join(strings.Fields(op.Description), " ")

	for _, want := range []string{
		"ch-cap67-movements",
		clickhouse.ProvenanceCAP67Derived,
		"classic-movements-backfill",
		"watermark",
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("getAccountMovements description does not mention %q — the handler's seam is the cap67 derive's watermark", want)
		}
	}
	for _, stale := range []string{"only writer", "hard-clamps below the P23 boundary"} {
		if strings.Contains(desc, stale) {
			t.Errorf("getAccountMovements description still claims %q — the archive has two writers and a watermark boundary", stale)
		}
	}
}

func intPtr(v int) *int { return &v }

// movementsEnvelope runs AccountMovements with the lake tip at lakeTip and
// the cap67 archive watermark at wm, returning the coverage note and the
// envelope's stale flag.
func movementsEnvelope(t *testing.T, wm, lakeTip uint32) (string, bool) {
	t.Helper()
	reader := &movementsArmReader{capReader: &capReader{probe: &deadlineProbe{}}, wm: wm}
	h := newProbeHandler(reader, nil)
	h.SEP41Movements = &stubSEP41Tail{}
	h.LakeWatermark = func(context.Context) (uint32, bool, bool) { return lakeTip, false, true }
	var note string
	var stale, wrote bool
	h.WriteJSON = func(w http.ResponseWriter, v any, s bool) {
		note, stale, wrote = v.(AccountMovementsView).CoverageNote, s, true
		w.WriteHeader(http.StatusOK)
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/accounts/"+validTestAccount+"/movements", nil)
	r.SetPathValue("g_strkey", validTestAccount)
	h.AccountMovements(httptest.NewRecorder(), r)
	if !wrote {
		t.Fatal("AccountMovements wrote no envelope")
	}
	return note, stale
}

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
