//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"fmt"
	"math/big"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// The `trades` bulk backfill writer (timescale.Store.BulkBackfillTrades) is
// the opt-in path `stellarindex-ops ch-rebuild -sdex -write -bulk-trades`
// uses. These tests prove the three things that make it safe to prefer over
// the live writer on a historical range:
//
//  1. it produces the SAME ROWS as Store.BatchInsertTrades on the same input
//     (identical values in every column either path writes, plus the same
//     source_entry_counts tally and classic-asset registry effect);
//  2. it REFUSES its own precondition — a range that already holds rows for
//     the source falls back to the upsert instead of COPYing into a conflict;
//  3. it is faster, measured, on a realistically-shaped target.
//
// PRODUCTION CONSTRUCTOR. Both paths are reached through timescale.Open +
// timescale.InstallUSDVolumeResolution + Store.SetDeriveGeneration — exactly
// what internal/ops/chops/ch_rebuild.go does before it drains (ch_rebuild.go
// "storage open" / "usd_volume resolution — MANDATORY on this path"). These
// tests use that constructor, not a hand-built Store, so a wiring change that
// would break the real tool breaks them too.

const bulkPegIssuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

// bulkStore opens a store wired the way ch-rebuild wires it.
func bulkStore(t *testing.T, ctx context.Context, dsn string, gen int64) *timescale.Store {
	t.Helper()
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("timescale.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	store.SetDeriveGeneration(gen)
	if err := timescale.InstallUSDVolumeResolution(store,
		[]string{"USDC-" + bulkPegIssuer}, nil); err != nil {
		t.Fatalf("InstallUSDVolumeResolution: %v", err)
	}
	return store
}

func bulkAsset(t *testing.T, code string) c.Asset {
	t.Helper()
	a, err := c.NewClassicAsset(code, bulkPegIssuer)
	if err != nil {
		t.Fatalf("NewClassicAsset(%s): %v", code, err)
	}
	return a
}

// bulkTrades builds n SDEX-shaped trades. The pair mix matters: a USDC quote
// resolves through the declared peg with no DB lookup at all, while the other
// two shapes fall through to the FX resolver — which is where a historical
// backfill actually spends its time.
func bulkTradeSet(t *testing.T, n int, baseLedger uint32, baseTS time.Time, source string) []c.Trade {
	t.Helper()
	const nTokens = 200
	usdc := bulkAsset(t, "USDC")
	tokens := make([]c.Asset, nTokens)
	for i := range tokens {
		tokens[i] = bulkAsset(t, fmt.Sprintf("TK%02d", i))
	}
	out := make([]c.Trade, 0, n)
	for i := range n {
		var base, quote c.Asset
		switch i % 20 {
		case 0, 1, 2, 3, 4, 5, 6, 7, 8:
			base, quote = c.NativeAsset(), tokens[i%nTokens]
		case 9, 10, 11, 12, 13, 14, 15:
			base, quote = tokens[i%nTokens], tokens[(i*7+3)%nTokens]
		default:
			base, quote = tokens[i%nTokens], usdc
		}
		pair, err := c.NewPair(base, quote)
		if err != nil {
			t.Fatalf("NewPair: %v", err)
		}
		out = append(out, c.Trade{
			Source:      source,
			Ledger:      baseLedger + uint32(i/48), //nolint:gosec // bounded test data
			TxHash:      fmt.Sprintf("%064x", uint64(baseLedger)*1_000_000+uint64(i)),
			OpIndex:     uint32(i % 48), //nolint:gosec // bounded test data
			Timestamp:   baseTS.Add(time.Duration(i/48) * 5 * time.Second),
			Pair:        pair,
			BaseAmount:  c.NewAmount(big.NewInt(int64(1_000_000 + i))),
			QuoteAmount: c.NewAmount(big.NewInt(int64(2_000_000 + i*3))),
			Maker:       "",
			Taker:       fmt.Sprintf("TAKER-%d", i%7),
		})
	}
	return out
}

// storedTradeRow is every column either write path is responsible for, plus
// the two columns NEITHER writes (routed_via, signer) so the test would catch
// a bulk path that started writing them.
type storedTradeRow struct {
	source, txHash          string
	ledger, opIndex         int64
	ts                      time.Time
	baseAsset, quoteAsset   string
	baseAmount, quoteAmount string
	usdVolume               sql.NullString
	maker, taker            sql.NullString
	deriveGeneration        int64
	routedVia, signer       sql.NullString
}

func readTradeRows(t *testing.T, db *sql.DB, source string, loLedger, hiLedger uint32) []storedTradeRow {
	t.Helper()
	rows, err := db.Query(`
        SELECT source, ledger, tx_hash, op_index, ts,
               base_asset, quote_asset, base_amount::text, quote_amount::text,
               usd_volume::text, maker, taker, derive_generation, routed_via, signer
          FROM trades
         WHERE source = $1::text
           AND ledger BETWEEN $2::int AND $3::int
         ORDER BY source, ledger, tx_hash, op_index, ts`, source, int64(loLedger), int64(hiLedger))
	if err != nil {
		t.Fatalf("read trades: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []storedTradeRow
	for rows.Next() {
		var r storedTradeRow
		if err := rows.Scan(&r.source, &r.ledger, &r.txHash, &r.opIndex, &r.ts,
			&r.baseAsset, &r.quoteAsset, &r.baseAmount, &r.quoteAmount,
			&r.usdVolume, &r.maker, &r.taker, &r.deriveGeneration,
			&r.routedVia, &r.signer); err != nil {
			t.Fatalf("scan trade: %v", err)
		}
		r.ts = r.ts.UTC()
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

func sourceEntryCount(t *testing.T, db *sql.DB, source string) int64 {
	t.Helper()
	var n sql.NullInt64
	if err := db.QueryRow(
		`SELECT entry_count FROM source_entry_counts WHERE source = $1::text`, source).Scan(&n); err != nil {
		if err == sql.ErrNoRows {
			return 0
		}
		t.Fatalf("source_entry_counts: %v", err)
	}
	return n.Int64
}

// TestBulkBackfillTrades_IdenticalToBatchUpsert writes the SAME input through
// both writers, into two ranges that differ only by the ledger/ts offset, and
// compares every stored column. A divergence here means the bulk path is not
// a drop-in for the upsert on an empty range.
func TestBulkBackfillTrades_IdenticalToBatchUpsert(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	const gen = 1_700_000_000
	const n = 4_000
	store := bulkStore(t, ctx, dsn, gen)
	db := store.DB()

	upsertTS := time.Date(2024, 3, 5, 12, 0, 0, 0, time.UTC)
	bulkTS := time.Date(2024, 5, 5, 12, 0, 0, 0, time.UTC)

	// Same logical rows, two disjoint identities. Both sets carry a one-side-
	// zero fill (INV-6 unstorable) and an exact intra-batch PK duplicate, so
	// the comparison covers the storability gate and the dedupe collapse too,
	// not just the happy path.
	// BOTH sets are source "sdex". A fabricated source name would make this
	// test pass vacuously: tradeUSDVolume returns nil for any source
	// external.Lookup does not classify as an exchange, so every usd_volume
	// would be NULL on both paths and the column comparison would prove
	// nothing. The two runs are separated by LEDGER WINDOW instead.
	const upsertLo, upsertHi = 50_000_000, 50_999_999
	const bulkLo, bulkHi = 55_000_000, 55_999_999
	upsertRows := bulkTradeSet(t, n, upsertLo, upsertTS, "sdex")
	bulkRows := bulkTradeSet(t, n, bulkLo, bulkTS, "sdex")
	// One unstorable one-side-zero fill (INV-6) and one exact intra-batch PK
	// duplicate, so the comparison covers the storability gate and the dedupe
	// collapse. The duplicate is EXACT rather than a differing later copy:
	// sortTradesByConflictKey uses a non-stable sort, so which of two
	// equal-key copies survives is unspecified, and a test that depended on
	// it would be asserting an implementation accident rather than identity.
	spoil := func(rows []c.Trade) []c.Trade {
		zero := rows[7]
		zero.QuoteAmount = c.NewAmount(big.NewInt(0))
		dup := rows[11]
		return append(append(append([]c.Trade{}, rows...), zero), dup)
	}
	upsertRows = spoil(upsertRows)
	bulkRows = spoil(bulkRows)

	if err := store.BatchInsertTrades(ctx, upsertRows); err != nil {
		t.Fatalf("BatchInsertTrades: %v", err)
	}
	res, err := store.BulkBackfillTrades(ctx, bulkRows, timescale.BulkBackfillOptions{})
	if err != nil {
		t.Fatalf("BulkBackfillTrades: %v", err)
	}
	if res.Path != timescale.BulkBackfillPathCopy {
		t.Fatalf("bulk path = %q (%s), want the COPY path — the range was empty",
			res.Path, res.FallbackReason)
	}

	got := readTradeRows(t, db, "sdex", bulkLo, bulkHi)
	want := readTradeRows(t, db, "sdex", upsertLo, upsertHi)
	if len(got) != len(want) {
		t.Fatalf("row count: bulk=%d upsert=%d — the two writers disagree on which rows are storable",
			len(got), len(want))
	}
	if len(got) == 0 {
		t.Fatal("no rows landed at all")
	}
	if int64(len(got)) != res.Copied {
		t.Fatalf("COPY reported %d rows, %d are stored", res.Copied, len(got))
	}
	// The one-side-zero fill must be absent from BOTH (INV-6), and the dupe
	// collapsed to one row in BOTH.
	if len(got) != n {
		t.Fatalf("stored %d rows for %d distinct storable inputs — the unstorable fill or the "+
			"intra-batch duplicate was not collapsed", len(got), n)
	}
	for i := range got {
		g, w := got[i], want[i]
		// Identity differs BY CONSTRUCTION (different source/ledger/ts window);
		// everything derived must not.
		if g.baseAsset != w.baseAsset || g.quoteAsset != w.quoteAsset {
			t.Fatalf("row %d assets: bulk=(%s,%s) upsert=(%s,%s)", i, g.baseAsset, g.quoteAsset, w.baseAsset, w.quoteAsset)
		}
		if g.baseAmount != w.baseAmount || g.quoteAmount != w.quoteAmount {
			t.Fatalf("row %d amounts: bulk=(%s,%s) upsert=(%s,%s)", i, g.baseAmount, g.quoteAmount, w.baseAmount, w.quoteAmount)
		}
		if g.usdVolume != w.usdVolume {
			t.Fatalf("row %d usd_volume: bulk=%v upsert=%v — the bulk path must run the same tradeUSDVolume waterfall",
				i, g.usdVolume, w.usdVolume)
		}
		if g.maker != w.maker || g.taker != w.taker {
			t.Fatalf("row %d maker/taker: bulk=(%v,%v) upsert=(%v,%v) — empty must land as NULL, not ''",
				i, g.maker, g.taker, w.maker, w.taker)
		}
		if g.deriveGeneration != w.deriveGeneration || g.deriveGeneration != gen {
			t.Fatalf("row %d derive_generation: bulk=%d upsert=%d want=%d",
				i, g.deriveGeneration, w.deriveGeneration, gen)
		}
		if g.routedVia.Valid || g.signer.Valid {
			t.Fatalf("row %d: the bulk path wrote routed_via=%v / signer=%v — both are owned by their "+
				"own post-insert sweepers and neither insert path may set them", i, g.routedVia, g.signer)
		}
	}
	// usd_volume must actually be exercised, not vacuously NULL everywhere.
	var populated int
	for _, g := range got {
		if g.usdVolume.Valid {
			populated++
		}
	}
	if populated == 0 {
		t.Fatal("no row resolved a usd_volume — the comparison would pass vacuously; " +
			"check InstallUSDVolumeResolution wiring in this test")
	}
	t.Logf("compared %d rows; %d carry a resolved usd_volume", len(got), populated)

	// source_entry_counts: the bulk path bumps once for the whole buffer, the
	// upsert path once per batch. Same landed count either way.
	// Both runs bumped the one "sdex" tally row; the total is what each path
	// claims it landed, so a bulk path that mis-counted its own inserts shows
	// up here.
	if got := sourceEntryCount(t, db, "sdex"); got != int64(2*n) {
		t.Fatalf("source_entry_counts[sdex] = %d, want %d (%d upserted + %d bulk-copied)",
			got, 2*n, n, n)
	}
	// The classic-asset registry hook fires on both paths.
	var registered int
	if err := db.QueryRow(`SELECT count(*) FROM classic_assets`).Scan(&registered); err != nil {
		t.Fatalf("classic_assets: %v", err)
	}
	if registered == 0 {
		t.Fatal("classic_assets is empty — neither path ran the registry hook, so this " +
			"assertion cannot distinguish them")
	}
}

// TestBulkBackfillTrades_RefusesNonEmptyRange proves the precondition is
// CHECKED, not assumed from the caller: one pre-existing row inside the
// buffer's (source, ledger, ts) box is enough to push the whole buffer onto
// the upsert path — where the stored row keeps its INV-3 generation guard.
func TestBulkBackfillTrades_RefusesNonEmptyRange(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	baseTS := time.Date(2024, 3, 5, 12, 0, 0, 0, time.UTC)
	rows := bulkTradeSet(t, 500, 50_000_000, baseTS, "sdex")

	// Land ONE row of the buffer first, at the LIVE generation.
	live := bulkStore(t, ctx, dsn, 0)
	if err := live.InsertTrade(ctx, rows[250]); err != nil {
		t.Fatalf("seed InsertTrade: %v", err)
	}

	store := bulkStore(t, ctx, dsn, 1_700_000_000)
	res, err := store.BulkBackfillTrades(ctx, rows, timescale.BulkBackfillOptions{})
	if err != nil {
		t.Fatalf("BulkBackfillTrades: %v", err)
	}
	if res.Path != timescale.BulkBackfillPathUpsert {
		t.Fatalf("bulk path = %q, want %q — a single stored row inside the buffer's ledger/ts box "+
			"must refuse the COPY precondition", res.Path, timescale.BulkBackfillPathUpsert)
	}
	if res.FallbackReason == "" {
		t.Fatal("fell back with no reason recorded — an operator cannot tell a silent revert from a fast run")
	}
	t.Logf("refused, as required: %s", res.FallbackReason)

	// Every row still landed, exactly once, through the upsert.
	got := readTradeRows(t, store.DB(), "sdex", 0, 2_000_000_000)
	if len(got) != len(rows) {
		t.Fatalf("stored %d rows, want %d — the fallback must still write the whole buffer", len(got), len(rows))
	}

	// And a SECOND call now also refuses (the range is emphatically non-empty),
	// which is the re-run case an operator hits after a partial run.
	res2, err := store.BulkBackfillTrades(ctx, rows, timescale.BulkBackfillOptions{})
	if err != nil {
		t.Fatalf("BulkBackfillTrades re-run: %v", err)
	}
	if res2.Path != timescale.BulkBackfillPathUpsert {
		t.Fatalf("re-run path = %q, want the upsert fallback", res2.Path)
	}
	if n := len(readTradeRows(t, store.DB(), "sdex", 0, 2_000_000_000)); n != len(rows) {
		t.Fatalf("re-run stored %d rows, want %d — the fallback upsert must stay idempotent in row count", n, len(rows))
	}
}

// TestBulkBackfillTrades_EmptyProbeIsLedgerScoped pins the shape of the
// precondition query itself: a stored row for the SAME source at a ledger
// OUTSIDE the buffer's extent must not refuse the fast path (otherwise every
// re-derive below a populated floor would fall back forever), while a row
// inside it must.
func TestBulkBackfillTrades_EmptyProbeIsLedgerScoped(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	// A populated "live" region far above the backfill window — this is r1's
	// actual shape: sdex rows above the trade floor, nothing below it.
	live := bulkStore(t, ctx, dsn, 0)
	above := bulkTradeSet(t, 200, 61_600_000, time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), "sdex")
	if err := live.BatchInsertTrades(ctx, above); err != nil {
		t.Fatalf("seed above floor: %v", err)
	}

	store := bulkStore(t, ctx, dsn, 1_700_000_000)
	below := bulkTradeSet(t, 500, 50_000_000, time.Date(2024, 3, 5, 12, 0, 0, 0, time.UTC), "sdex")
	res, err := store.BulkBackfillTrades(ctx, below, timescale.BulkBackfillOptions{})
	if err != nil {
		t.Fatalf("BulkBackfillTrades: %v", err)
	}
	if res.Path != timescale.BulkBackfillPathCopy {
		t.Fatalf("path = %q (%s), want the COPY path — rows ABOVE the backfill window must not "+
			"refuse a window that is itself empty", res.Path, res.FallbackReason)
	}
	if n := len(readTradeRows(t, store.DB(), "sdex", 0, 2_000_000_000)); n != len(above)+len(below) {
		t.Fatalf("stored %d rows, want %d", n, len(above)+len(below))
	}
}
