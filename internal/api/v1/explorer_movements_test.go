package v1_test

import (
	"context"
	"math/big"
	"net/http"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/sources/classicmovements"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestP23BoundaryConstantsAgree is ADR-0048 D5's "assert [the CH/PG
// non-overlap] in code" requirement, the compile+run-time half: the
// ClickHouse archive's hard clamp (classicmovements.P23StartLedger) and
// the Postgres tail's hard floor (timescale.SEP41MovementsFloorLedger)
// are two SEPARATE constants (import-direction rules forbid
// internal/storage from importing internal/sources — see either
// constant's doc comment) that MUST hold the same value for
// explorer.Handler.assertP23NonOverlap's invariant to mean anything. A
// package that can import both (this one) is the only place able to
// pin them together.
func TestP23BoundaryConstantsAgree(t *testing.T) {
	if classicmovements.P23StartLedger != timescale.SEP41MovementsFloorLedger {
		t.Fatalf("P23 boundary constants drifted: classicmovements.P23StartLedger=%d != timescale.SEP41MovementsFloorLedger=%d",
			classicmovements.P23StartLedger, timescale.SEP41MovementsFloorLedger)
	}
}

// stubSEP41MovementsReader is a canned explorerpkg.SEP41MovementsReader
// (structural — this file doesn't import that package's interface
// declaration, only satisfies its method set).
type stubSEP41MovementsReader struct {
	rows []timescale.SEP41TransferRow
	err  error
}

func (s *stubSEP41MovementsReader) ListSEP41TransfersByAddress(_ context.Context, _ string, limit int, _ timescale.SEP41TransferCursor, _ string) ([]timescale.SEP41TransferRow, error) {
	if s.err != nil {
		return nil, s.err
	}
	rows := s.rows
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}

const otherG = "GBRPYHIL2CI3FNQ4BXLFMNDLFJUNPU2HY3ZMFSHONUCEOASW7QC7OX2H"

// TestExplorer_AccountMovements_Merge pins the ADR-0048 D5 merge: a
// ClickHouse pre-P23 page + a Postgres post-P23 tail page combine into
// one newest-first feed, truncated to `limit`, with a next_cursor
// pointing at the last row served.
func TestExplorer_AccountMovements_Merge(t *testing.T) {
	now := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	chReader := &stubExplorerReader{movements: []clickhouse.AccountMovementRow{
		{
			Address: testG, Ledger: 100, LedgerCloseTime: now, TxHash: "chtx2", OpIndex: 0, LegIndex: 0,
			Direction: clickhouse.AccountMovementSent, MovementKind: "payment", Provenance: "classic_derived",
			Asset: "native", Counterparty: otherG, Amount: bigAmt(500),
		},
		{
			Address: testG, Ledger: 90, LedgerCloseTime: now, TxHash: "chtx1", OpIndex: 0, LegIndex: 0,
			Direction: clickhouse.AccountMovementReceived, MovementKind: "payment", Provenance: "classic_derived",
			Asset: "native", Counterparty: otherG, Amount: bigAmt(100),
		},
	}}
	pgReader := &stubSEP41MovementsReader{rows: []timescale.SEP41TransferRow{
		{
			Ledger: 60_000_005, TxHash: "pgtx2", OpIndex: 0, EventIndex: 0, ObservedAt: now,
			ContractID: "CCONTRACT1", Kind: timescale.SEP41Transfer, FromAddr: testG, ToAddr: otherG, Amount: bigAmt(2000),
		},
		{
			Ledger: 60_000_003, TxHash: "pgtx1", OpIndex: 0, EventIndex: 0, ObservedAt: now,
			ContractID: "CCONTRACT1", Kind: timescale.SEP41Transfer, FromAddr: otherG, ToAddr: testG, Amount: bigAmt(3000),
		},
	}}

	srv := v1.New(v1.Options{Explorer: chReader, SEP41Movements: pgReader})
	base := httpTestServer(t, srv).URL

	// limit=3: PG's 2 rows (newest) + the top CH row, next_cursor set.
	resp := mustGet(t, base+"/v1/accounts/"+testG+"/movements?limit=3")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body struct {
		Data v1.AccountMovementsView `json:"data"`
	}
	mustDecode(t, resp, &body)

	if body.Data.CoverageNote != "" {
		t.Errorf("coverage_note = %q, want empty (both readers wired)", body.Data.CoverageNote)
	}
	if len(body.Data.Movements) != 3 {
		t.Fatalf("movements = %d, want 3: %+v", len(body.Data.Movements), body.Data.Movements)
	}
	// Newest first: pgtx2 (60000005) > pgtx1 (60000003) > chtx2 (100).
	gotOrder := []string{body.Data.Movements[0].TxHash, body.Data.Movements[1].TxHash, body.Data.Movements[2].TxHash}
	wantOrder := []string{"pgtx2", "pgtx1", "chtx2"}
	for i := range wantOrder {
		if gotOrder[i] != wantOrder[i] {
			t.Errorf("movements[%d].tx_hash = %q, want %q (full order %v)", i, gotOrder[i], wantOrder[i], gotOrder)
		}
	}
	if body.Data.NextCursor == "" {
		t.Error("next_cursor should be set — a full page was served (limit=3, 3 rows)")
	}

	// PG-tail row mapping: sent/received computed against testG, asset
	// falls back to the raw contract_id (stubExplorerReader's SAC
	// resolvers always report not-found), amount round-trips as a string.
	pg2 := body.Data.Movements[0]
	if pg2.Direction != "sent" || pg2.Counterparty != otherG {
		t.Errorf("pgtx2 direction/counterparty = %q/%q, want sent/%s", pg2.Direction, pg2.Counterparty, otherG)
	}
	if pg2.Asset != "CCONTRACT1" || pg2.Provenance != "cap67_event" || pg2.MovementKind != "transfer" {
		t.Errorf("pgtx2 asset/provenance/kind = %q/%q/%q", pg2.Asset, pg2.Provenance, pg2.MovementKind)
	}
	if pg2.Amount != "2000" {
		t.Errorf("pgtx2 amount = %q, want 2000", pg2.Amount)
	}
	pg1 := body.Data.Movements[1]
	if pg1.Direction != "received" || pg1.Counterparty != otherG {
		t.Errorf("pgtx1 direction/counterparty = %q/%q, want received/%s", pg1.Direction, pg1.Counterparty, otherG)
	}

	// limit=1: only the newest row, still a next_cursor (more remain).
	resp = mustGet(t, base+"/v1/accounts/"+testG+"/movements?limit=1")
	mustDecode(t, resp, &body)
	if len(body.Data.Movements) != 1 || body.Data.Movements[0].TxHash != "pgtx2" {
		t.Fatalf("limit=1 movements = %+v", body.Data.Movements)
	}
	if body.Data.NextCursor == "" {
		t.Error("limit=1: next_cursor should be set")
	}
}

// TestExplorer_AccountMovements_HonestDegrade pins the coverage_note
// honest-empty-state contract: no SEP41Movements reader wired -> the
// endpoint still 200s with the CH archive alone, and says so.
func TestExplorer_AccountMovements_HonestDegrade(t *testing.T) {
	chReader := &stubExplorerReader{movements: []clickhouse.AccountMovementRow{
		{Address: testG, Ledger: 50, TxHash: "chtx", MovementKind: "payment", Provenance: "classic_derived", Asset: "native", Amount: bigAmt(1)},
	}}
	srv := v1.New(v1.Options{Explorer: chReader})
	base := httpTestServer(t, srv).URL

	resp := mustGet(t, base+"/v1/accounts/"+testG+"/movements")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body struct {
		Data v1.AccountMovementsView `json:"data"`
	}
	mustDecode(t, resp, &body)
	if body.Data.CoverageNote == "" {
		t.Error("coverage_note should be set when no Postgres tail reader is wired")
	}
	if len(body.Data.Movements) != 1 {
		t.Fatalf("movements = %+v, want the 1 CH row", body.Data.Movements)
	}
}

// TestExplorer_AccountMovements_ValidationAndUnavailable covers the
// error paths: invalid strkey, invalid direction, invalid cursor, and
// the 503 when no explorer reader is wired at all.
func TestExplorer_AccountMovements_ValidationAndUnavailable(t *testing.T) {
	srv := v1.New(v1.Options{Explorer: &stubExplorerReader{}})
	base := httpTestServer(t, srv).URL

	if r := mustGet(t, base+"/v1/accounts/notanaccount/movements"); r.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid account: status = %d, want 400", r.StatusCode)
	}
	if r := mustGet(t, base+"/v1/accounts/"+testG+"/movements?direction=sideways"); r.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid direction: status = %d, want 400", r.StatusCode)
	}
	if r := mustGet(t, base+"/v1/accounts/"+testG+"/movements?cursor=not-a-cursor"); r.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid cursor: status = %d, want 400", r.StatusCode)
	}

	unavailable := v1.New(v1.Options{})
	ubase := httpTestServer(t, unavailable).URL
	if r := mustGet(t, ubase+"/v1/accounts/"+testG+"/movements"); r.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("no explorer reader: status = %d, want 503", r.StatusCode)
	}
}

func bigAmt(v int64) *big.Int { return big.NewInt(v) }
