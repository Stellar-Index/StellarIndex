//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/sources/classicmovements"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestAccountMovements_MergesCHArchiveAndPGTail is the ADR-0048 D5
// end-to-end proof: real ClickHouse rows in stellar.account_movements
// (pre-P23 archive) + real Postgres rows in sep41_transfers (post-P23
// tail), read through the actual production stack
// (clickhouse.ExplorerReader + timescale.Store, wired into v1.New
// exactly like cmd/stellarindex-api/main.go does), come back as ONE
// merged, correctly-ordered, correctly-paginated feed over real HTTP —
// catching any regression a stubbed unit test (explorer_movements_test.go)
// can't: real SQL WHERE-clause correctness on both sides, real
// ClickHouse tuple-comparison pagination, real Postgres index usage.
func TestAccountMovements_MergesCHArchiveAndPGTail(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// ─── Postgres side (sep41_transfers "recent tail") ──────────────
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	g := gAccountFromSeed(t, 0x21)
	counterparty := gAccountFromSeed(t, 0x22)
	postP23Ledger := classicmovements.P23StartLedger + 1000

	if err := store.InsertSEP41TransferBatch(ctx, []timescale.SEP41TransferRow{
		{
			ObservedAt: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
			Ledger:     postP23Ledger,
			TxHash:     "pgtxaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			OpIndex:    0,
			EventIndex: 0,
			ContractID: "CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCTAIL",
			Kind:       timescale.SEP41Transfer,
			FromAddr:   g,
			ToAddr:     counterparty,
			Amount:     big.NewInt(9_000_000),
		},
	}); err != nil {
		t.Fatalf("InsertSEP41TransferBatch: %v", err)
	}

	// ─── ClickHouse side (pre-P23 account_movements archive) ────────
	chAddr := clickhouseAddr(t)
	if err := clickhouse.EnsureAccountMovementsTable(ctx, chAddr); err != nil {
		t.Fatalf("EnsureAccountMovementsTable: %v", err)
	}
	prePayment := clickhouse.AccountMovement{
		MovementKind:    "payment",
		Provenance:      "classic_derived",
		Ledger:          classicmovements.P23StartLedger - 1000,
		LedgerCloseTime: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
		TxHash:          "chtxbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		OpIndex:         0,
		LegIndex:        0,
		Asset:           "native",
		Amount:          big.NewInt(500_000),
		FromAddress:     g,
		ToAddress:       counterparty,
	}
	if _, err := clickhouse.InsertAccountMovements(ctx, chAddr, []clickhouse.AccountMovement{prePayment}); err != nil {
		t.Fatalf("InsertAccountMovements: %v", err)
	}

	// ─── Wire the real production stack (mirrors cmd/stellarindex-api) ──
	er, err := clickhouse.NewExplorerReader(ctx, chAddr)
	if err != nil {
		t.Fatalf("NewExplorerReader: %v", err)
	}
	t.Cleanup(func() { _ = er.Close() })

	srv := v1.New(v1.Options{Explorer: er, SEP41Movements: store})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/v1/accounts/" + g + "/movements?limit=10")
	if err != nil {
		t.Fatalf("GET movements: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	var body struct {
		Data v1.AccountMovementsView `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if body.Data.CoverageNote != "" {
		t.Errorf("coverage_note = %q, want empty — both stores are wired and populated", body.Data.CoverageNote)
	}
	if len(body.Data.Movements) != 2 {
		t.Fatalf("movements = %d, want 2 (1 CH + 1 PG): %+v", len(body.Data.Movements), body.Data.Movements)
	}
	// Newest first: the post-P23 Postgres row (higher ledger) precedes
	// the pre-P23 ClickHouse row.
	if got := body.Data.Movements[0].TxHash; got != "pgtxaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Errorf("movements[0].tx_hash = %q, want the Postgres-tail row (newest)", got)
	}
	if got := body.Data.Movements[0].Provenance; got != "cap67_event" {
		t.Errorf("movements[0].provenance = %q, want cap67_event", got)
	}
	if got := body.Data.Movements[1].TxHash; got != "chtxbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Errorf("movements[1].tx_hash = %q, want the ClickHouse archive row (oldest)", got)
	}
	if got := body.Data.Movements[1].Provenance; got != "classic_derived" {
		t.Errorf("movements[1].provenance = %q, want classic_derived", got)
	}
	if body.Data.NextCursor != "" {
		t.Errorf("next_cursor = %q, want empty — both rows fit in one page (limit=10, 2 rows)", body.Data.NextCursor)
	}
}
