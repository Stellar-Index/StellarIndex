//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/classicmovements"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestAccountMovements_AssetFilterScopesPostgresTail executes the
// contract-scoped sep41_transfers query against real Postgres and the
// full /movements stack over HTTP. The account's newest transfers are all
// another token; its USDC transfers sit below them. ?asset=USDC must fill
// the page from USDC rows and page through all of them — the filter
// applied after the LIMIT served an empty page with no cursor.
func TestAccountMovements_AssetFilterScopesPostgresTail(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	g := gAccountFromSeed(t, 0x51)
	issuer := gAccountFromSeed(t, 0x52)
	counterparty := gAccountFromSeed(t, 0x53)
	usdc, err := canonical.NewClassicAsset("USDC", issuer)
	if err != nil {
		t.Fatal(err)
	}
	eurc, err := canonical.NewClassicAsset("EURC", issuer)
	if err != nil {
		t.Fatal(err)
	}
	usdcSAC, err := usdc.SacContractID()
	if err != nil {
		t.Fatal(err)
	}
	otherContract, err := eurc.SacContractID()
	if err != nil {
		t.Fatal(err)
	}

	base := classicmovements.P23StartLedger + 1000
	var rows []timescale.SEP41TransferRow
	add := func(ledger uint32, contract, from, to string) {
		rows = append(rows, timescale.SEP41TransferRow{
			ObservedAt: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC).Add(time.Duration(ledger-base) * time.Second),
			Ledger:     ledger,
			TxHash:     fmt.Sprintf("%064x", ledger),
			ContractID: contract,
			Kind:       timescale.SEP41Transfer,
			FromAddr:   from,
			ToAddr:     to,
			Amount:     big.NewInt(int64(ledger - base + 1)),
		})
	}
	for l := base + 100; l < base+105; l++ { // newest: another token
		add(l, otherContract, g, counterparty)
	}
	for l := base + 10; l < base+14; l++ { // older: USDC, both directions
		if (l-base)%2 == 0 {
			add(l, usdcSAC, g, counterparty)
		} else {
			add(l, usdcSAC, counterparty, g)
		}
	}
	if err := store.InsertSEP41TransferBatch(ctx, rows); err != nil {
		t.Fatalf("InsertSEP41TransferBatch: %v", err)
	}

	t.Run("store", func(t *testing.T) {
		page, err := store.ListSEP41TransfersByAddress(ctx, g, 3, timescale.SEP41TransferCursor{}, "", usdcSAC, 0)
		if err != nil {
			t.Fatalf("page 1: %v", err)
		}
		assertTransferLedgers(t, "page 1", page, base+13, base+12, base+11)
		last := page[len(page)-1]
		cur := timescale.SEP41TransferCursor{Ledger: last.Ledger, TxHash: last.TxHash, OpIndex: last.OpIndex, EventIndex: last.EventIndex}
		page, err = store.ListSEP41TransfersByAddress(ctx, g, 3, cur, "", usdcSAC, 0)
		if err != nil {
			t.Fatalf("page 2: %v", err)
		}
		assertTransferLedgers(t, "page 2", page, base+10)
		sent, err := store.ListSEP41TransfersByAddress(ctx, g, 3, timescale.SEP41TransferCursor{}, "sent", usdcSAC, 0)
		if err != nil {
			t.Fatalf("sent: %v", err)
		}
		assertTransferLedgers(t, "sent", sent, base+12, base+10)
	})

	t.Run("http", func(t *testing.T) {
		chAddr := clickhouseAddr(t)
		if err := clickhouse.EnsureAccountMovementsTable(ctx, chAddr); err != nil {
			t.Fatalf("EnsureAccountMovementsTable: %v", err)
		}
		er, err := clickhouse.NewExplorerReader(ctx, chAddr)
		if err != nil {
			t.Fatalf("NewExplorerReader: %v", err)
		}
		t.Cleanup(func() { _ = er.Close() })
		ts := httptest.NewServer(v1.New(v1.Options{Explorer: er, SEP41Movements: store}).Handler())
		t.Cleanup(ts.Close)

		q := url.Values{"asset": {usdc.String()}, "limit": {"3"}}
		page1 := getMovements(t, ts.URL, g, q)
		assertMovementLedgers(t, "page 1", page1, usdc.String(), base+13, base+12, base+11)
		if page1.NextCursor == "" {
			t.Fatal("page 1 is full but next_cursor is empty — the older USDC transfer is unreachable")
		}
		q.Set("cursor", page1.NextCursor)
		page2 := getMovements(t, ts.URL, g, q)
		assertMovementLedgers(t, "page 2", page2, usdc.String(), base+10)
	})
}

func getMovements(t *testing.T, base, g string, q url.Values) v1.AccountMovementsView {
	t.Helper()
	resp, err := http.Get(base + "/v1/accounts/" + g + "/movements?" + q.Encode())
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
	return body.Data
}

func assertTransferLedgers(t *testing.T, label string, got []timescale.SEP41TransferRow, want ...uint32) {
	t.Helper()
	ledgers := make([]uint32, len(got))
	for i, r := range got {
		ledgers[i] = r.Ledger
	}
	if fmt.Sprint(ledgers) != fmt.Sprint(want) {
		t.Fatalf("%s ledgers = %v, want %v", label, ledgers, want)
	}
}

func assertMovementLedgers(t *testing.T, label string, v v1.AccountMovementsView, asset string, want ...uint32) {
	t.Helper()
	ledgers := make([]uint32, len(v.Movements))
	for i, m := range v.Movements {
		ledgers[i] = m.Ledger
		if m.Asset != asset {
			t.Errorf("%s ledger %d asset = %q, want %q", label, m.Ledger, m.Asset, asset)
		}
	}
	if fmt.Sprint(ledgers) != fmt.Sprint(want) {
		t.Fatalf("%s ledgers = %v, want %v", label, ledgers, want)
	}
}

// TestAccountMovements_LedgerCeilingBoundsTheQuery executes the fixed
// SQL against a real ClickHouse: the /movements merge ceiling travels in
// AccountMovementFilter.MaxLedger/HasMaxLedger and is applied as a WHERE
// predicate, so a bounded read returns a FULL page of servable rows.
//
// The un-fixed reader emitted no ledger bound at all and the handler
// dropped the over-ceiling rows afterwards: with every one of the `limit`
// newest rows above the ceiling the page collapsed to zero, next_cursor
// was suppressed, and the account's pre-watermark history was unreachable.
// Here the same shape must yield `limit` rows, the newest of them exactly
// at the ceiling.
func TestAccountMovements_LedgerCeilingBoundsTheQuery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	chAddr := clickhouseAddr(t)
	if err := clickhouse.EnsureAccountMovementsTable(ctx, chAddr); err != nil {
		t.Fatalf("EnsureAccountMovementsTable: %v", err)
	}

	g := gAccountFromSeed(t, 0x41)
	counterparty := gAccountFromSeed(t, 0x42)
	const (
		firstLedger = 40_000_000
		rowCount    = 40
		ceiling     = firstLedger + 19 // 20 servable ledgers, 20 above the ceiling
		limit       = 10
	)

	movements := make([]clickhouse.AccountMovement, 0, rowCount)
	for i := 0; i < rowCount; i++ {
		movements = append(movements, clickhouse.AccountMovement{
			MovementKind:    "payment",
			Provenance:      "classic_derived",
			Ledger:          uint32(firstLedger + i),
			LedgerCloseTime: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Minute),
			TxHash:          fmt.Sprintf("%064x", i),
			OpIndex:         0,
			LegIndex:        0,
			Asset:           "native",
			Amount:          big.NewInt(int64(1_000_000 + i)),
			FromAddress:     g,
			ToAddress:       counterparty,
		})
	}
	if _, err := clickhouse.InsertAccountMovements(ctx, chAddr, movements); err != nil {
		t.Fatalf("InsertAccountMovements: %v", err)
	}

	er, err := clickhouse.NewExplorerReader(ctx, chAddr)
	if err != nil {
		t.Fatalf("NewExplorerReader: %v", err)
	}
	t.Cleanup(func() { _ = er.Close() })

	rows, err := er.AccountMovements(ctx, g, limit, clickhouse.AccountMovementCursor{},
		clickhouse.AccountMovementFilter{HasMaxLedger: true, MaxLedger: ceiling})
	if err != nil {
		t.Fatalf("AccountMovements (bounded): %v", err)
	}
	if len(rows) != limit {
		t.Fatalf("bounded read returned %d rows, want a full page of %d — the ceiling did not bound the "+
			"query, so the newest (unservable) rows ate the LIMIT (F055)", len(rows), limit)
	}
	if rows[0].Ledger != ceiling {
		t.Errorf("newest bounded row is ledger %d, want %d (the ceiling itself is inclusive)", rows[0].Ledger, ceiling)
	}
	for _, r := range rows {
		if r.Ledger > ceiling {
			t.Fatalf("row at ledger %d exceeds the ceiling %d", r.Ledger, ceiling)
		}
	}

	// A ceiling of 0 is a real ceiling (an installed genesis movements
	// floor with no cap67 watermark), not "unset": it must serve nothing.
	zeroRows, err := er.AccountMovements(ctx, g, limit, clickhouse.AccountMovementCursor{},
		clickhouse.AccountMovementFilter{HasMaxLedger: true, MaxLedger: 0})
	if err != nil {
		t.Fatalf("AccountMovements (ceiling 0): %v", err)
	}
	if len(zeroRows) != 0 {
		t.Fatalf("ceiling 0 served %d rows, want 0 — the arm must fail closed at an installed genesis floor", len(zeroRows))
	}

	// No ceiling = the whole archive.
	allRows, err := er.AccountMovements(ctx, g, rowCount, clickhouse.AccountMovementCursor{},
		clickhouse.AccountMovementFilter{})
	if err != nil {
		t.Fatalf("AccountMovements (unbounded): %v", err)
	}
	if len(allRows) != rowCount {
		t.Fatalf("unbounded read returned %d rows, want %d", len(allRows), rowCount)
	}
}

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

	// The healthy-path note is ALWAYS
	// present, stating the feed's post-P23 scope: without the cap67
	// archive provisioned (this harness has no watermark table), the
	// watched-token disclosure; with it, the through-ledger statement.
	if !strings.Contains(body.Data.CoverageNote, "watched") {
		t.Errorf("coverage_note = %q, want the post-P23 scope statement", body.Data.CoverageNote)
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

// TestAccountMovements_DedupServesNewestVersion executes the account
// movements read against two un-merged versions of one key that differ
// only in counterparty — the shape an in-place re-derive leaves behind.
// The newer ingested_at must be served whichever part was written first;
// a LIMIT 1 BY without the version in its ORDER BY keeps an arbitrary one.
func TestAccountMovements_DedupServesNewestVersion(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	chAddr := clickhouseAddr(t)
	if err := clickhouse.EnsureAccountMovementsTable(ctx, chAddr); err != nil {
		t.Fatalf("EnsureAccountMovementsTable: %v", err)
	}
	raw := dialClickHouse(t, ctx, "stellar")
	if err := raw.Exec(ctx, "SYSTEM STOP MERGES stellar.account_movements"); err != nil {
		t.Fatalf("SYSTEM STOP MERGES: %v", err)
	}
	t.Cleanup(func() { _ = raw.Exec(context.Background(), "SYSTEM START MERGES stellar.account_movements") })

	er, err := clickhouse.NewExplorerReader(ctx, chAddr)
	if err != nil {
		t.Fatalf("NewExplorerReader: %v", err)
	}
	t.Cleanup(func() { _ = er.Close() })

	stale := gAccountFromSeed(t, 0x61) // the original derive's counterparty
	fresh := gAccountFromSeed(t, 0x62) // the re-derive's counterparty
	insert := func(address, counterparty, ingestedAt string) {
		t.Helper()
		q := fmt.Sprintf(`INSERT INTO stellar.account_movements
			(address, ledger, ledger_close_time, tx_hash, op_index, leg_index, direction,
			 movement_kind, provenance, asset, counterparty, amount, ingested_at)
			VALUES ('%s', 45000000, toDateTime64('2024-01-01 00:00:00', 0, 'UTC'), '%064x', 0, 0, 'sent',
			 'payment', 'classic_derived', 'native', '%s', 1000, toDateTime('%s', 'UTC'))`,
			address, 7, counterparty, ingestedAt)
		if err := raw.Exec(ctx, q); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	for i, newerFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("newer_written_first=%v", newerFirst), func(t *testing.T) {
			g := gAccountFromSeed(t, byte(0x63+i))
			if newerFirst {
				insert(g, fresh, "2026-09-02 00:00:00")
				insert(g, stale, "2026-09-01 00:00:00")
			} else {
				insert(g, stale, "2026-09-01 00:00:00")
				insert(g, fresh, "2026-09-02 00:00:00")
			}
			rows, err := er.AccountMovements(ctx, g, 10, clickhouse.AccountMovementCursor{}, clickhouse.AccountMovementFilter{})
			if err != nil {
				t.Fatalf("AccountMovements: %v", err)
			}
			if len(rows) != 1 {
				t.Fatalf("got %d rows, want 1 (LIMIT 1 BY dedup of the two versions)", len(rows))
			}
			if rows[0].Counterparty != fresh {
				t.Errorf("counterparty = %s, want the newer version's %s (stale %s was served)", rows[0].Counterparty, fresh, stale)
			}
		})
	}
}
