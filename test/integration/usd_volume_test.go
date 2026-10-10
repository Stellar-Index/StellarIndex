//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/jackc/pgx/v5/stdlib"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/ops/chops"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestUsdVolumePricingStats seeds priced, unpriced and same-issuer classic
// trades inside the window, plus rows outside it and from a source not asked
// for, and pins the exact counts. A requested source with no rows (kraken) still gets a zero row.
func TestUsdVolumePricingStats(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	issuerA := "G" + strings.Repeat("A", 55)
	issuerB := "G" + strings.Repeat("B", 55)
	from := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)
	in := from.Add(time.Hour)

	n := 0
	seed := func(source, base, quote string, ts time.Time, usd any) {
		t.Helper()
		n++
		_, err := store.DB().ExecContext(ctx, `
			INSERT INTO trades (source, ledger, tx_hash, op_index, ts, base_asset, quote_asset, base_amount, quote_amount, usd_volume)
			VALUES ($1, $2, $3, 0, $4::timestamptz, $5, $6, 1, 1, $7)`,
			source, 1000+n, strings.Repeat("0", 63)+string(rune('a'+n%26)), ts, base, quote, usd)
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	// external: 2 priced, 1 unpriced.
	seed("binance", "crypto:BTC", "fiat:USD", in, "10")
	seed("binance", "crypto:ETH", "fiat:USD", in.Add(time.Minute), "20")
	seed("binance", "crypto:XYZ", "fiat:ZZZ", in.Add(2*time.Minute), nil)
	// on-chain: 1 priced, 1 unpriced routable, 2 unroutable (one issuer), 1 mixed-issuer unpriced.
	seed("sdex", "native", "USDC-"+issuerA, in, "5")
	seed("sdex", "native", "FOO-"+issuerA, in.Add(time.Minute), nil)
	seed("sdex", "FOO-"+issuerA, "BAR-"+issuerA, in.Add(2*time.Minute), nil)
	seed("sdex", "BAR-"+issuerA, "FOO-"+issuerA, in.Add(3*time.Minute), nil)
	seed("sdex", "FOO-"+issuerA, "BAR-"+issuerB, in.Add(4*time.Minute), nil)
	// outside the window (both edges: end is exclusive) and an unrequested source.
	seed("binance", "crypto:BTC", "fiat:USD", from.Add(-time.Second), "1")
	seed("binance", "crypto:BTC", "fiat:USD", to, nil)
	seed("soroswap_router", "native", "FOO-"+issuerA, in, nil)

	got, err := store.UsdVolumePricingStats(ctx, from, to, []string{"binance", "sdex", "kraken"})
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	want := []timescale.UsdVolumePricingRow{
		{Source: "binance", Trades: 3, Priced: 2, Unpriced: 1, Unroutable: 0},
		{Source: "kraken"},
		{Source: "sdex", Trades: 5, Priced: 1, Unpriced: 2, Unroutable: 2},
	}
	if len(got) != len(want) {
		t.Fatalf("rows = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestUSDVolumeValueReconcile_ExactTierIdentity is the DB-backed half of
// the value-reconcile identity, run against real TimescaleDB.
//
// The standing usd-volume alerts only measure COVERAGE (the share of trades
// with a non-NULL `usd_volume`). A trade priced with the WRONG number is
// 100% covered and completely wrong, and every volume surface this system
// publishes is a sum of that column.
//
// This exercises the value check end to end, on rows written by the REAL
// insert path with the REAL peg configuration installed:
//
//  1. the day-scoped SQL aggregation actually groups and sums correctly;
//  2. the exact-tier identity (usd_volume == pegged_leg / 10^decimals)
//     holds to the last unit on rows the writer produced;
//  3. corrupting ONE row by ONE unit at the render scale (1e-8 USD) is
//     caught — the redness proof for the whole check. The fixture is sized
//     to a REALISTIC daily volume ($500M) precisely so that error sits
//     BELOW a float64 ulp at that magnitude (~5.96e-8): a naive float
//     subtraction reports the day clean, and the test asserts that
//     directly. That is why the comparison is exact rational arithmetic.
func TestUSDVolumeValueReconcile_ExactTierIdentity(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// The operator's declared USD peg — the same input the indexer installs
	// (cmd/stellarindex-indexer/main.go), so these rows are valued by the
	// production waterfall rather than a test-only shortcut.
	const usdcAssetID = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	if err := timescale.InstallUSDVolumeResolution(store, []string{usdcAssetID}, nil); err != nil {
		t.Fatalf("InstallUSDVolumeResolution: %v", err)
	}
	spec, err := timescale.NewUSDVolumeQuoteSpec([]string{usdcAssetID}, nil)
	if err != nil {
		t.Fatalf("NewUSDVolumeQuoteSpec: %v", err)
	}

	usdc, err := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	// On-chain, USDC-quoted: tier 2, decimals 7. usd_volume must come out as
	// quote_amount / 1e7 for every row.
	pair, err := c.NewPair(c.NativeAsset(), usdc)
	if err != nil {
		t.Fatal(err)
	}

	// Sized to a realistic day: Σquote = 5e15 stroops → $500,000,000. The
	// magnitude matters — see the float64-blindness assertion at the end.
	day := time.Date(2026, 5, 14, 0, 0, 0, 0, time.UTC)
	quotes := []int64{2_000_000_000_000_000, 2_000_000_000_000_000, 1_000_000_000_000_000}
	for i, q := range quotes {
		tr := mkIntegrationTrade("sdex", i, day.Add(time.Duration(i)*time.Hour), pair, 10_000_000_000, q)
		tr.Ledger = uint32(41_000_000 + i)
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade %d: %v", i, err)
		}
	}

	readGroup := func(t *testing.T) timescale.TradeValuationGroup {
		t.Helper()
		groups, gerr := store.TradeValuationByDay(ctx, day)
		if gerr != nil {
			t.Fatalf("TradeValuationByDay: %v", gerr)
		}
		if len(groups) != 1 {
			t.Fatalf("groups = %d, want exactly 1 (one source/base/quote in the fixture): %+v", len(groups), groups)
		}
		return groups[0]
	}

	g := readGroup(t)
	if g.Source != "sdex" || g.BaseAsset != "native" || g.QuoteAsset != usdcAssetID {
		t.Fatalf("group identity = %s %s/%s, want sdex native/%s", g.Source, g.BaseAsset, g.QuoteAsset, usdcAssetID)
	}
	if g.PricedRows != int64(len(quotes)) {
		t.Fatalf("PricedRows = %d, want %d — the insert path did not value every row", g.PricedRows, len(quotes))
	}
	if g.UnpricedRows != 0 {
		t.Errorf("UnpricedRows = %d, want 0", g.UnpricedRows)
	}

	tier, decimals, cerr := timescale.ClassifyUSDVolumeTier(g.Source, g.BaseAsset, g.QuoteAsset, spec)
	if cerr != nil {
		t.Fatalf("ClassifyUSDVolumeTier: %v", cerr)
	}
	if tier != timescale.TierQuotePegged || decimals != 7 {
		t.Fatalf("tier = %q/%d, want %q/7", tier, decimals, timescale.TierQuotePegged)
	}

	// The slack at 7 decimals is exactly zero — FloatString(8) renders a
	// 7-decimal quotient losslessly, so the identity is checkable with NO
	// tolerance whatsoever.
	slack := timescale.USDVolumeRoundingSlack(decimals, g.PricedRows)
	if slack.Sign() != 0 {
		t.Fatalf("slack = %s, want exactly 0 at 7 decimals", slack.FloatString(12))
	}

	delta, ok := timescale.ExactTierDelta(g, tier, decimals)
	if !ok {
		t.Fatal("ExactTierDelta: sums failed to parse")
	}
	if delta.Sign() != 0 {
		t.Fatalf("rows written by the real insert path violate the exact identity by %s USD "+
			"(stored Σ=%s, quote Σ=%s) — usd_volume is not quote_amount/1e7",
			delta.FloatString(12), g.SumUSDVolume, g.SumQuoteAmount)
	}

	// ── the redness fixture: one row, off by one unit ───────────────
	// A wrong-scale / wrong-leg / stale-peg / superseded-backfill defect
	// shows up here as a nonzero delta. One unit at the render scale is the
	// smallest such defect that can exist, so catching it proves the check
	// has no blind band at the bottom.
	const bump = `
		UPDATE trades
		   SET usd_volume = usd_volume + 0.00000001
		 WHERE source = 'sdex' AND ledger = 41000000`
	if _, err := store.DB().ExecContext(ctx, bump); err != nil {
		t.Fatalf("corrupt one row: %v", err)
	}

	g = readGroup(t)
	delta, ok = timescale.ExactTierDelta(g, tier, decimals)
	if !ok {
		t.Fatal("ExactTierDelta after corruption: sums failed to parse")
	}
	want := new(big.Rat).SetFrac(big.NewInt(1), big.NewInt(100_000_000))
	if delta.Cmp(want) != 0 {
		t.Errorf("delta after a one-unit corruption = %s, want %s", delta.FloatString(12), want.FloatString(12))
	}
	if new(big.Rat).Abs(delta).Cmp(slack) <= 0 {
		t.Error("a one-unit usd_volume corruption was absorbed by the rounding slack — " +
			"verify-usd-volume would report the day as clean")
	}

	// ── why this is exact rational arithmetic ───────────────────────
	// The same subtraction in float64, at this (entirely ordinary) daily
	// volume, loses the corruption completely: 1e-8 is below the float64
	// ulp near 5e8. Asserting the naive implementation's blindness pins the
	// design decision — a future "simplify this to float64" would keep every
	// other assertion above green while silently reopening the blind band.
	storedF, _ := new(big.Rat).SetString(g.SumUSDVolume)
	quoteF, _ := new(big.Rat).SetString(g.SumQuoteAmount)
	sf, _ := storedF.Float64()
	qf, _ := quoteF.Float64()
	if naive := sf - qf/1e7; naive != 0 {
		t.Logf("float64 delta = %g (this run's float happened to retain it; the exact delta is %s)",
			naive, delta.FloatString(12))
	} else {
		t.Logf("confirmed: float64 delta = 0 at $%s — the corruption is invisible to a naive "+
			"float check and only the exact comparison catches it", storedF.FloatString(2))
	}
}

// TestUSDVolumeRestamp_ExactTierRepair is the DB-backed proof for the
// `usd-volume-restamp` write path, on real TimescaleDB, against rows the
// REAL insert path wrote with the REAL peg configuration:
//
//  1. the SQL identity the tool evaluates (`round(leg / 10^d, 8)`) lands
//     the SAME value the Go formula [timescale.ExactTierUSDVolume] and
//     therefore [tradeUSDVolume] produce — no SQL reimplementation drift;
//  2. DIFFERENTIAL: a correctly-stamped row is untouched — value AND
//     derive_generation — so a re-run is a no-op (idempotent);
//  3. the dry-run count is the write's exact preview;
//  4. a row at a HIGHER generation is never clawed back, and every
//     rewritten row carries the run's generation;
//  5. NULL rows are left alone unless FillNull;
//  6. after the repair the verifier's own acceptance (ExactTierDelta == 0)
//     holds for the day.
func TestUSDVolumeRestamp_ExactTierRepair(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const usdcAssetID = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	if err := timescale.InstallUSDVolumeResolution(store, []string{usdcAssetID}, nil); err != nil {
		t.Fatalf("InstallUSDVolumeResolution: %v", err)
	}
	spec, err := timescale.NewUSDVolumeQuoteSpec([]string{usdcAssetID}, nil)
	if err != nil {
		t.Fatalf("NewUSDVolumeQuoteSpec: %v", err)
	}
	usdc, err := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	// USDC/XLM — the dollar leg is the BASE: tier 2b, the exact class a sweep
	// found dirty on every day it covered.
	pair, err := c.NewPair(usdc, c.NativeAsset())
	if err != nil {
		t.Fatal(err)
	}

	day := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	// base amounts in stroops: $125, $0.0000003 (dust), $9,876,543.21.
	bases := []int64{1_250_000_000, 3, 98_765_432_100_000}
	ledgers := make([]uint32, len(bases))
	for i, b := range bases {
		tr := mkIntegrationTrade("sdex", i, day.Add(time.Duration(i)*7*time.Hour), pair, b, 10_000_000_000)
		ledgers[i] = tr.Ledger
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade %d: %v", i, err)
		}
	}
	type row struct {
		usd *string
		gen int64
	}
	readRow := func(t *testing.T, ledger uint32) row {
		t.Helper()
		var (
			usd sql.NullString
			gen int64
		)
		err := store.DB().QueryRowContext(ctx,
			`SELECT usd_volume::text, derive_generation FROM trades WHERE source = 'sdex' AND ledger = $1`, ledger,
		).Scan(&usd, &gen)
		if err != nil {
			t.Fatalf("read ledger %d: %v", ledger, err)
		}
		if !usd.Valid {
			return row{nil, gen}
		}
		return row{&usd.String, gen}
	}
	mustRat := func(t *testing.T, s string) *big.Rat {
		t.Helper()
		r, ok := new(big.Rat).SetString(s)
		if !ok {
			t.Fatalf("unparseable NUMERIC %q", s)
		}
		return r
	}

	// ── poison the era: row 0 resolver-priced (+0.7%), row 1 NULL, row 2 correct ──
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE trades SET usd_volume = usd_volume * 1.007 WHERE source = 'sdex' AND ledger = $1`, ledgers[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE trades SET usd_volume = NULL WHERE source = 'sdex' AND ledger = $1`, ledgers[1]); err != nil {
		t.Fatal(err)
	}
	correctBefore := readRow(t, ledgers[2])
	if correctBefore.usd == nil || correctBefore.gen != 0 {
		t.Fatalf("fixture: correct row = %+v, want a gen-0 priced row", correctBefore)
	}

	const gen = int64(1_756_400_000)
	group := timescale.USDVolumeRestampGroup{Source: "sdex", BaseAsset: usdcAssetID, QuoteAsset: "native", Tier: timescale.TierBasePegged, Decimals: 7}
	params := func(fillNull bool) timescale.USDVolumeRestampParams {
		return timescale.USDVolumeRestampParams{
			Groups: []timescale.USDVolumeRestampGroup{group},
			From:   day, To: day.AddDate(0, 0, 1),
			FillNull: fillNull, Generation: gen,
		}
	}

	// ── 3. dry run previews exactly the write ──
	if n, err := store.CountUSDVolumeRestampCandidates(ctx, params(false)); err != nil || n != 1 {
		t.Fatalf("dry-run candidates = %d, %v; want 1 (the resolver-priced row only)", n, err)
	}
	if n, err := store.CountUSDVolumeRestampCandidates(ctx, params(true)); err != nil || n != 2 {
		t.Fatalf("dry-run candidates with FillNull = %d, %v; want 2", n, err)
	}

	// ── GUC hygiene: pin the pool to ONE connection, so the conn
	// the restamp borrows IS the conn every later statement lands on.
	// `Conn.Close`/`Tx.Commit` return it to the pool and pgx v5 stdlib
	// resets nothing on reuse, so a SESSION-level cap lift would still be
	// in force below.
	store.DB().SetMaxOpenConns(1)
	capSetting := func(t *testing.T) string {
		t.Helper()
		var v string
		if err := store.DB().QueryRowContext(ctx,
			`SELECT current_setting('timescaledb.max_tuples_decompressed_per_dml_transaction')`).Scan(&v); err != nil {
			t.Fatalf("read decompression cap: %v", err)
		}
		return v
	}
	capBaseline := capSetting(t)
	if capBaseline == "0" {
		t.Fatalf("fixture: decompression cap already lifted (%q) before the restamp ran", capBaseline)
	}

	poisoned := readRow(t, ledgers[0])

	// ── apply ──
	n, err := store.RestampExactTierUSDVolume(ctx, params(false))
	if err != nil {
		t.Fatalf("RestampExactTierUSDVolume: %v", err)
	}
	if n != 1 {
		t.Fatalf("restamped %d row(s), want 1", n)
	}

	// 0. the lifted cap did NOT ride the connection back into the pool.
	if got := capSetting(t); got != capBaseline {
		t.Errorf("after the restamp the pooled connection carries max_tuples_decompressed_per_dml_transaction = %q, want the untouched default %q — a later DML on this conn would run uncapped (#312)", got, capBaseline)
	}
	// …and this TimescaleDB really does honour the transaction-scoped form
	// the restamp relies on: visible for the rest of the transaction,
	// unwound by COMMIT.
	func() {
		tx, err := store.DB().BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.ExecContext(ctx, "SET LOCAL timescaledb.max_tuples_decompressed_per_dml_transaction = 0"); err != nil {
			t.Fatalf("SET LOCAL decompression cap: %v", err)
		}
		var inTx string
		if err := tx.QueryRowContext(ctx,
			`SELECT current_setting('timescaledb.max_tuples_decompressed_per_dml_transaction')`).Scan(&inTx); err != nil {
			t.Fatalf("read cap in tx: %v", err)
		}
		if inTx != "0" {
			t.Errorf("SET LOCAL cap inside the transaction = %q, want \"0\" — the restamp's DML would abort on compressed chunks", inTx)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}()
	if got := capSetting(t); got != capBaseline {
		t.Errorf("cap after COMMIT = %q, want %q", got, capBaseline)
	}
	store.DB().SetMaxOpenConns(0)

	// 7. the before-image: the row's prior value and generation, and what
	// the run wrote, logged in the same transaction — and undoing the run
	// from it restores the row exactly.
	logged := readRestampLog(t, ctx, store.DB(), "sdex", ledgers[0])
	restamped := readRow(t, ledgers[0])
	if len(logged) != 1 || !sameNumeric(logged[0].prior, poisoned.usd) || logged[0].priorGen != poisoned.gen ||
		!sameNumeric(&logged[0].written, restamped.usd) || logged[0].gen != gen {
		t.Fatalf("before-image of the restamped row = %+v, want prior %v@%d and written %v@%d",
			logged, poisoned.usd, poisoned.gen, restamped.usd, gen)
	}
	if got := restampLogCount(t, ctx, store.DB(), gen); got != n {
		t.Errorf("run logged %d before-image(s) but rewrote %d row(s)", got, n)
	}
	if undone := undoRestampRun(t, ctx, store.DB(), gen); undone != 1 {
		t.Fatalf("undo restored %d row(s), want 1", undone)
	}
	if r := readRow(t, ledgers[0]); !sameNumeric(r.usd, poisoned.usd) || r.gen != poisoned.gen {
		t.Fatalf("row after undo = %+v, want the before-image %+v", r, poisoned)
	}
	if n, err := store.RestampExactTierUSDVolume(ctx, params(false)); err != nil || n != 1 {
		t.Fatalf("re-apply after undo = %d, %v; want 1", n, err)
	}

	// 1. SQL identity == Go formula == what the insert path writes.
	want, ok := timescale.ExactTierUSDVolume(group.Tier, group.Decimals, "1250000000", "10000000000")
	if !ok || want != "125.00000000" {
		t.Fatalf("ExactTierUSDVolume = %q, %v", want, ok)
	}
	got := readRow(t, ledgers[0])
	if got.usd == nil || mustRat(t, *got.usd).Cmp(mustRat(t, want)) != 0 {
		t.Errorf("repaired row usd_volume = %v, want %s", got.usd, want)
	}
	// 4. stamped with the run's generation.
	if got.gen != gen {
		t.Errorf("repaired row derive_generation = %d, want %d (INV-3)", got.gen, gen)
	}
	// 5. NULL row left alone without FillNull.
	if r := readRow(t, ledgers[1]); r.usd != nil || r.gen != 0 {
		t.Errorf("NULL row was touched without FillNull: %+v", r)
	}
	// 2. DIFFERENTIAL: the correct row is byte-identical, generation included.
	if after := readRow(t, ledgers[2]); after.gen != 0 || *after.usd != *correctBefore.usd {
		t.Errorf("correctly-stamped row was rewritten: before %+v after %+v", correctBefore, after)
	}
	// idempotent
	if n, err := store.RestampExactTierUSDVolume(ctx, params(false)); err != nil || n != 0 {
		t.Errorf("second run restamped %d row(s), %v; want 0", n, err)
	}

	// 4b. generation guard: a newer generation is never clawed back.
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE trades SET usd_volume = 999, derive_generation = $2 WHERE source = 'sdex' AND ledger = $1`, ledgers[0], gen+10); err != nil {
		t.Fatal(err)
	}
	if n, err := store.RestampExactTierUSDVolume(ctx, params(false)); err != nil || n != 0 {
		t.Errorf("older-generation run rewrote %d newer row(s), %v; want 0 (derive_generation guard)", n, err)
	}
	if r := readRow(t, ledgers[0]); r.gen != gen+10 || mustRat(t, *r.usd).Cmp(big.NewRat(999, 1)) != 0 {
		t.Errorf("newer-generation row clawed back: %+v", r)
	}
	// Restore it at the run's own generation so the acceptance below is honest.
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE trades SET derive_generation = 0 WHERE source = 'sdex' AND ledger = $1`, ledgers[0]); err != nil {
		t.Fatal(err)
	}

	// 5b. FillNull stamps the NULL row (and re-repairs row 0).
	if n, err := store.RestampExactTierUSDVolume(ctx, params(true)); err != nil || n != 2 {
		t.Fatalf("FillNull run restamped %d row(s), %v; want 2", n, err)
	}
	if r := readRow(t, ledgers[1]); r.usd == nil || mustRat(t, *r.usd).Cmp(big.NewRat(3, 10_000_000)) != 0 || r.gen != gen {
		t.Errorf("NULL row after FillNull = %+v, want 0.00000030 at gen %d", r, gen)
	}

	// 6. the verifier's own acceptance: the day's exact-tier delta is zero.
	groups, err := store.TradeValuationByDay(ctx, day)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].PricedRows != 3 || groups[0].UnpricedRows != 0 {
		t.Fatalf("groups after repair = %+v", groups)
	}
	tier, decimals, cerr := timescale.ClassifyUSDVolumeTier(groups[0].Source, groups[0].BaseAsset, groups[0].QuoteAsset, spec)
	if cerr != nil || tier != timescale.TierBasePegged || decimals != 7 {
		t.Fatalf("classify = %q/%d, %v", tier, decimals, cerr)
	}
	delta, ok := timescale.ExactTierDelta(groups[0], tier, decimals)
	if !ok || delta.Sign() != 0 {
		t.Errorf("post-repair exact-tier delta = %s (ok=%v), want 0 — verify-usd-volume would still flag the day", delta, ok)
	}
}

// restampLogRow is one usd_volume_restamp_log row, NUMERICs as text.
type restampLogRow struct {
	prior    *string
	priorGen int64
	written  string
	gen      int64
}

// readRestampLog returns every before-image logged for one trade, oldest
// first.
func readRestampLog(t *testing.T, ctx context.Context, db *sql.DB, source string, ledger uint32) []restampLogRow {
	t.Helper()
	rows, err := db.QueryContext(ctx, `
		SELECT prior_usd_volume::text, prior_derive_generation, usd_volume::text, derive_generation
		  FROM usd_volume_restamp_log
		 WHERE source = $1 AND ledger = $2
		 ORDER BY id`, source, int64(ledger))
	if err != nil {
		t.Fatalf("read usd_volume_restamp_log: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []restampLogRow
	for rows.Next() {
		var (
			r     restampLogRow
			prior sql.NullString
		)
		if err := rows.Scan(&prior, &r.priorGen, &r.written, &r.gen); err != nil {
			t.Fatalf("scan usd_volume_restamp_log: %v", err)
		}
		if prior.Valid {
			r.prior = &prior.String
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate usd_volume_restamp_log: %v", err)
	}
	return out
}

// restampLogCount is how many before-images a run's generation logged.
func restampLogCount(t *testing.T, ctx context.Context, db *sql.DB, gen int64) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM usd_volume_restamp_log WHERE derive_generation = $1`, gen).Scan(&n); err != nil {
		t.Fatalf("count usd_volume_restamp_log: %v", err)
	}
	return n
}

// restampLogMigration documents the undo statement an operator runs.
const restampLogMigration = "0175_usd_volume_restamp_log.up.sql"

// undoRecipeFromMigration lifts the undo statement out of 0175's header
// comment, `$gen` bound as `$1`, so the test executes exactly what the
// operator is told to run.
func undoRecipeFromMigration(t *testing.T) string {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	src, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations", restampLogMigration))
	if err != nil {
		t.Fatalf("read %s: %v", restampLogMigration, err)
	}
	var stmt []string
	for _, line := range strings.Split(string(src), "\n") {
		body, ok := strings.CutPrefix(line, "--")
		if !ok {
			continue
		}
		body = strings.TrimSpace(body)
		if len(stmt) == 0 && body != "UPDATE trades t" {
			continue
		}
		stmt = append(stmt, body)
		if strings.HasSuffix(body, ";") {
			break
		}
	}
	recipe := strings.ReplaceAll(strings.TrimSuffix(strings.Join(stmt, "\n"), ";"), "$gen", "$1")
	for _, want := range []string{
		"SET usd_volume = l.prior_usd_volume,",
		"FROM (SELECT DISTINCT ON (source, ledger, tx_hash, op_index, ts) *",
		"ORDER BY source, ledger, tx_hash, op_index, ts, id) l",
		"AND t.derive_generation = $1",
	} {
		if !strings.Contains(recipe, want) {
			t.Fatalf("%s header undo statement lost %q:\n%s", restampLogMigration, want, recipe)
		}
	}
	return recipe
}

// undoRestampRun runs 0175's documented undo for one run's generation.
func undoRestampRun(t *testing.T, ctx context.Context, db *sql.DB, gen int64) int64 {
	t.Helper()
	res, err := db.ExecContext(ctx, undoRecipeFromMigration(t), gen)
	if err != nil {
		t.Fatalf("undo restamp run %d: %v", gen, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func sameNumeric(a *string, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// TestUSDVolumeRestampLog_Migration0175DownRefusesWhileHoldingBeforeImages:
// the log is the only record of what a restamp overwrote, so 0175's down
// must refuse while it holds a row and drop the table once it is empty.
func TestUSDVolumeRestampLog_Migration0175DownRefusesWhileHoldingBeforeImages(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.ExecContext(ctx, `
		INSERT INTO usd_volume_restamp_log
		       (source, ledger, tx_hash, op_index, ts, prior_usd_volume, prior_derive_generation, usd_volume, derive_generation)
		VALUES ('sdex', 1, $1, 0, now(), 1.25, 0, 1.00000000, 1756400000)`, strings.Repeat("ab", 32)); err != nil {
		t.Fatalf("seed log row: %v", err)
	}

	_, thisFile, _, _ := runtime.Caller(0)
	migrationsDir := "file://" + filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")

	// The RAISE EXCEPTION aborts the down's transaction mid-statement and
	// golang-migrate's postgres driver never issues a ROLLBACK on that
	// connection, so a later command on the same *Migrate instance (Force,
	// or another Migrate) fails to take its advisory lock ("database
	// locked") rather than surfacing the refusal. assertDownRefused in
	// migrations_test.go hits the same hazard: close the instance that ran
	// the failing down immediately, then use a fresh one per step.
	m, err := migrate.New(migrationsDir, dsn)
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	err = m.Migrate(172)
	_, _ = m.Close()
	if err == nil || !strings.Contains(err.Error(), "still holds before-images") {
		t.Fatalf("0175 down with a logged before-image: err = %v, want the RAISE refusal", err)
	}
	assertTableExists(t, db, ctx, "usd_volume_restamp_log")

	// The failed down rolled back inside its own transaction; clear
	// golang-migrate's dirty flag at the version the schema is really at.
	f, err := migrate.New(migrationsDir, dsn)
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	err = f.Force(175)
	_, _ = f.Close()
	if err != nil {
		t.Fatalf("force 175: %v", err)
	}

	if _, err := db.ExecContext(ctx, `DELETE FROM usd_volume_restamp_log`); err != nil {
		t.Fatal(err)
	}

	d, err := migrate.New(migrationsDir, dsn)
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	defer func() { _, _ = d.Close() }()
	if err := d.Migrate(172); err != nil {
		t.Fatalf("0175 down on an empty log: %v", err)
	}
	assertTableAbsent(t, db, ctx, "usd_volume_restamp_log")
}

// TestXLMBaseRestampChunks_RestampsInsideACompressedChunk is the DB-backed
// proof for `usd-volume-restamp -tier xlm-base -chunks`, driven through
// the real subcommand (flags, config, live-tail guard, generation) on real
// TimescaleDB, against a `trades` chunk that is COMPRESSED the way every
// chunk older than the 7-day policy is on production:
//
//  1. the DRY RUN (the default) prints the chunk plan and leaves the chunk
//     compressed and every row byte-identical;
//  2. -write restamps the rows through the same anchor the day walk uses
//     (10 XLM x $0.50 = $5.00), stamps them with the run's generation, and
//     leaves the chunk COMPRESSED again afterwards — with the trades
//     compression policy job (migration 0001) observed PAUSED while the
//     run is in flight and scheduled again after it;
//  3. a second -write run changes nothing: the chunk is probed read-only,
//     reported as skipped, never decompressed, and every row keeps its
//     value and generation;
//  4. with the run lock held from another session — the way a live run
//     holds it — -write refuses to start and touches nothing;
//  5. with the policy already unscheduled, -write refuses without
//     -resume-paused-policy and leaves the policy as it found it; with
//     the flag it restamps and re-enables the policy at exit;
//  6. with the policy removed, -write refuses to start and touches
//     nothing; a -generation in the future is refused the same way.
//
// The harness runs timescaledb.max_background_workers = 0, so the policy
// can never fire here; what this pins is that the real alter_job
// round-trips on the job the tool resolves, in both directions.
func TestXLMBaseRestampChunks_RestampsInsideACompressedChunk(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const usdcID = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	usdc, err := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	token, err := c.NewSorobanAsset("CAFJZQWSED6YAWZU3GWRTOCNPPCGBN32L7QV43XX5LZLFTK6JLN34DLN")
	if err != nil {
		t.Fatal(err)
	}
	xlm := c.NativeAsset()
	xlmUSDC, err := c.NewPair(xlm, usdc)
	if err != nil {
		t.Fatal(err)
	}
	xlmToken, err := c.NewPair(xlm, token)
	if err != nil {
		t.Fatal(err)
	}
	// The insert path's wiring, so the seeded rows are what production
	// rows are.
	if err := timescale.InstallUSDVolumeResolution(store, []string{usdcID}, nil); err != nil {
		t.Fatalf("InstallUSDVolumeResolution: %v", err)
	}

	day := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	anchorTS := day.Add(10 * time.Hour)

	// XLM/USD anchor: 100 XLM for 50 USDC -> $0.50.
	anchor := mkIntegrationTrade("sdex", 1, anchorTS, xlmUSDC, 1_000_000_000, 500_000_000)
	if err := store.InsertTrade(ctx, anchor); err != nil {
		t.Fatalf("InsertTrade anchor: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}

	// The XLM-base population: 10 XLM against a token with no USD market,
	// so the anchor's answer is exactly $5.00.
	const wantAnchored = "5.00000000"
	fixtures := []struct {
		name  string
		nonce int
		ts    time.Time
	}{
		{"quote-side wrong", 10, anchorTS.Add(5 * time.Minute)},
		{"stored NULL", 11, anchorTS.Add(6 * time.Minute)},
		{"already correct", 12, anchorTS.Add(7 * time.Minute)},
	}
	ledger := map[string]uint32{}
	var topLedger uint32
	for _, f := range fixtures {
		tr := mkIntegrationTrade("sdex", f.nonce, f.ts, xlmToken, 100_000_000, 300)
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade %s: %v", f.name, err)
		}
		ledger[f.name] = tr.Ledger
		if tr.Ledger > topLedger {
			topLedger = tr.Ledger
		}
	}
	exec := func(t *testing.T, q string, args ...any) {
		t.Helper()
		if _, err := store.DB().ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%q: %v", q, err)
		}
	}
	// The pre-fd1860bd state, imposed by hand.
	exec(t, `UPDATE trades SET usd_volume = 0.00372265 WHERE source='sdex' AND ledger=$1`, ledger["quote-side wrong"])
	exec(t, `UPDATE trades SET usd_volume = NULL       WHERE source='sdex' AND ledger=$1`, ledger["stored NULL"])

	type row struct {
		usd *string
		gen int64
	}
	readRow := func(t *testing.T, l uint32) row {
		t.Helper()
		var (
			usd sql.NullString
			gen int64
		)
		if err := store.DB().QueryRowContext(ctx,
			`SELECT usd_volume::text, derive_generation FROM trades WHERE source = 'sdex' AND ledger = $1`, l,
		).Scan(&usd, &gen); err != nil {
			t.Fatalf("read ledger %d: %v", l, err)
		}
		if !usd.Valid {
			return row{nil, gen}
		}
		return row{&usd.String, gen}
	}
	snapshot := func(t *testing.T) map[uint32]row {
		t.Helper()
		out := map[uint32]row{}
		for _, l := range ledger {
			out[l] = readRow(t, l)
		}
		out[anchor.Ledger] = readRow(t, anchor.Ledger)
		return out
	}
	sameSnapshot := func(t *testing.T, before, after map[uint32]row, what string) {
		t.Helper()
		for l, b := range before {
			a := after[l]
			switch {
			case (b.usd == nil) != (a.usd == nil):
				t.Errorf("%s: ledger %d usd_volume nullness changed (%v -> %v)", what, l, b.usd, a.usd)
			case b.usd != nil && *b.usd != *a.usd:
				t.Errorf("%s: ledger %d usd_volume %s -> %s", what, l, *b.usd, *a.usd)
			case b.gen != a.gen:
				t.Errorf("%s: ledger %d derive_generation %d -> %d", what, l, b.gen, a.gen)
			}
		}
	}
	chunkCompressed := func(t *testing.T) bool {
		t.Helper()
		var compressed bool
		if err := store.DB().QueryRowContext(ctx, `
			SELECT is_compressed FROM timescaledb_information.chunks
			 WHERE hypertable_name = 'trades' AND range_start <= $1 AND range_end > $1`, anchorTS,
		).Scan(&compressed); err != nil {
			t.Fatalf("read chunk state: %v", err)
		}
		return compressed
	}

	// ── compress the chunk, as the 7-day policy would have ────────────
	exec(t, `SELECT compress_chunk(c, true) FROM show_chunks('trades') c`)
	if !chunkCompressed(t) {
		t.Fatal("fixture: the day's chunk did not compress")
	}
	const policySQL = `SELECT scheduled FROM timescaledb_information.jobs WHERE proc_name = 'trades_compression_policy'`
	policyScheduled := func(t *testing.T) bool {
		t.Helper()
		var scheduled bool
		if err := store.DB().QueryRowContext(ctx, policySQL).Scan(&scheduled); err != nil {
			t.Fatalf("read the trades compression policy: %v", err)
		}
		return scheduled
	}
	if !policyScheduled(t) {
		t.Fatal("fixture: the trades compression policy (migration 0001) is not scheduled before the run")
	}
	// The live tail is past the window, so the one-writer guard admits it.
	if err := store.UpsertCursor(ctx, "ledgerstream", "", topLedger+1_000); err != nil {
		t.Fatalf("UpsertCursor: %v", err)
	}

	cfgPath := filepath.Join(t.TempDir(), "stellarindex.toml")
	cfg := fmt.Sprintf("[storage]\npostgres_dsn = %q\n\n[trades]\nusd_pegged_classic_assets = [%q]\n", dsn, usdcID)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	const gen = "1756800000"
	args := []string{
		"usd-volume-restamp", "-config", cfgPath, "-tier", "xlm-base", "-chunks",
		"-from", "2026-06-10", "-to", "2026-06-10", "-fill-null",
		"-generation", gen,
		// The database's data directory is inside the container, so the
		// host cannot statfs it: the operator-override path.
		"-min-free-bytes", fmt.Sprint(int64(1) << 40),
	}

	// ── 1. dry run: plan printed, nothing decompressed, nothing written ─
	before := snapshot(t)
	out, err := captureStdout(t, func() error { return chops.Run(args) })
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	for _, want := range []string{
		"chunk plan: 1 trades chunk(s) intersect [2026-06-10, 2026-06-10] — 1 compressed, 0 not",
		"WARNING: trusting -min-free-bytes",
		"DRY RUN: would take session advisory lock hashtext('usd-volume-restamp:trades')",
		"then pause compression policy job ",
		"DRY RUN: nothing is decompressed",
		"would change 2 row(s)",
		"would restamp 2 row(s)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output lacks %q:\n%s", want, out)
		}
	}
	sameSnapshot(t, before, snapshot(t), "dry run")
	if !chunkCompressed(t) {
		t.Fatal("the dry run decompressed the chunk")
	}
	if !policyScheduled(t) {
		t.Fatal("the dry run paused the compression policy")
	}

	// ── 2. -write: restamped through the anchor, chunk compressed again ─
	// A second session watches the policy job for the whole run: it must
	// be seen unscheduled while the run is in flight.
	stop := make(chan struct{})
	sawPaused := make(chan bool, 1)
	go func() {
		paused := false
		for {
			select {
			case <-stop:
				sawPaused <- paused
				return
			default:
			}
			var scheduled bool
			if err := store.DB().QueryRowContext(ctx, policySQL).Scan(&scheduled); err == nil && !scheduled {
				paused = true
			}
			time.Sleep(time.Millisecond)
		}
	}()
	out, err = captureStdout(t, func() error { return chops.Run(append(args, "-write")) })
	close(stop)
	if !<-sawPaused {
		t.Error("the trades compression policy was never observed PAUSED while the write run was in flight")
	}
	if !policyScheduled(t) {
		t.Error("the trades compression policy was not re-enabled after the write run")
	}
	if err != nil {
		t.Fatalf("write run: %v\n%s", err, out)
	}
	for _, want := range []string{
		"changed 2 row(s) (planned 2)",
		"bytes ",
		"restamped 2 row(s) in [2026-06-10, 2026-06-10] (tier xlm-base)",
		"CALL refresh_continuous_aggregate('prices_1m'",
		"CALL refresh_continuous_aggregate('twap_1d'",
		"acceptance: stellarindex-ops verify-usd-volume -config " + cfgPath + " -day 2026-06-10 -days 1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("write-run output lacks %q:\n%s", want, out)
		}
	}
	if !chunkCompressed(t) {
		t.Fatal("the chunk was left DECOMPRESSED after a successful write run")
	}
	after := snapshot(t)
	for _, name := range []string{"quote-side wrong", "stored NULL"} {
		got := after[ledger[name]]
		if got.usd == nil || *got.usd != wantAnchored {
			t.Errorf("%s: usd_volume = %v, want %s", name, got.usd, wantAnchored)
		}
		if fmt.Sprint(got.gen) != gen {
			t.Errorf("%s: derive_generation = %d, want the run's %s", name, got.gen, gen)
		}
	}
	// The already-correct row and the exact-tier anchor row are untouched.
	for _, l := range []uint32{ledger["already correct"], anchor.Ledger} {
		if b, a := before[l], after[l]; b.gen != a.gen || *b.usd != *a.usd {
			t.Errorf("ledger %d moved: %+v -> %+v", l, b, a)
		}
	}

	// ── 3. the rerun: probed, skipped, nothing moves ──────────────────
	before = snapshot(t)
	out, err = captureStdout(t, func() error { return chops.Run(append(args, "-write")) })
	if err != nil {
		t.Fatalf("rerun: %v\n%s", err, out)
	}
	for _, want := range []string{
		"nothing to change — skipped, chunk left compressed",
		"restamped 0 row(s)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rerun output lacks %q:\n%s", want, out)
		}
	}
	sameSnapshot(t, before, snapshot(t), "rerun")
	if !chunkCompressed(t) {
		t.Fatal("the rerun left the chunk decompressed")
	}
	if !policyScheduled(t) {
		t.Fatal("the rerun left the compression policy paused")
	}

	// ── 4. a second attempt beside a live one: the run lock ───────────
	// run-heavy-job.sh's lock is per job NAME, so the wrapper lets a
	// second -write start; the session advisory lock does not. Held here
	// from a second connection, the way a live run holds it.
	exec(t, `UPDATE trades SET usd_volume = 0.00372265, derive_generation = 0 WHERE source='sdex' AND ledger=$1`, ledger["quote-side wrong"])
	exec(t, `SELECT compress_chunk(c, true) FROM show_chunks('trades') c`)
	holder, err := store.DB().Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var locked bool
	if err := holder.QueryRowContext(ctx, `SELECT pg_try_advisory_lock(hashtext($1::text))`, timescale.USDVolumeRestampLockName).Scan(&locked); err != nil || !locked {
		t.Fatalf("fixture: take the run lock from a second connection: locked=%v err=%v", locked, err)
	}
	before = snapshot(t)
	out, err = captureStdout(t, func() error { return chops.Run(append(args, "-write")) })
	if err == nil || !errors.Is(err, timescale.ErrUSDVolumeRestampLockHeld) || !strings.Contains(err.Error(), "refuses to start") {
		t.Fatalf("with the run lock held elsewhere: err = %v, want the held-lock refusal\n%s", err, out)
	}
	sameSnapshot(t, before, snapshot(t), "refused run (lock held)")
	if !chunkCompressed(t) {
		t.Fatal("the refused run decompressed the chunk")
	}
	if !policyScheduled(t) {
		t.Fatal("the refused run paused the compression policy")
	}
	var released bool
	if err := holder.QueryRowContext(ctx, `SELECT pg_advisory_unlock(hashtext($1::text))`, timescale.USDVolumeRestampLockName).Scan(&released); err != nil || !released {
		t.Fatalf("fixture: release the run lock: released=%v err=%v", released, err)
	}
	_ = holder.Close()

	// ── 5. a policy already unscheduled: refused without the flag ─────
	exec(t, `SELECT alter_job(job_id, scheduled => false) FROM timescaledb_information.jobs WHERE proc_name = 'trades_compression_policy'`)
	if policyScheduled(t) {
		t.Fatal("fixture: the policy did not unschedule")
	}
	out, err = captureStdout(t, func() error { return chops.Run(append(args, "-write")) })
	if err == nil || !strings.Contains(err.Error(), "-resume-paused-policy") || !strings.Contains(err.Error(), "ALREADY unscheduled") {
		t.Fatalf("against an unscheduled policy: err = %v, want the refusal naming -resume-paused-policy\n%s", err, out)
	}
	sameSnapshot(t, before, snapshot(t), "refused run (policy unscheduled)")
	if !chunkCompressed(t) {
		t.Fatal("the refused run decompressed the chunk")
	}
	if policyScheduled(t) {
		t.Fatal("the refused run re-enabled the policy; that is the operator's call, or -resume-paused-policy's")
	}
	// With the flag: the run takes the paused policy over, restamps, and
	// re-enables it at exit.
	out, err = captureStdout(t, func() error { return chops.Run(append(args, "-resume-paused-policy", "-write")) })
	if err != nil {
		t.Fatalf("with -resume-paused-policy: %v\n%s", err, out)
	}
	if !strings.Contains(out, "changed 1 row(s) (planned 1)") {
		t.Errorf("-resume-paused-policy run output lacks the restamp:\n%s", out)
	}
	if !policyScheduled(t) {
		t.Fatal("-resume-paused-policy did not re-enable the policy at exit")
	}
	if !chunkCompressed(t) {
		t.Fatal("the -resume-paused-policy run left the chunk decompressed")
	}
	if got := readRow(t, ledger["quote-side wrong"]); got.usd == nil || *got.usd != wantAnchored || fmt.Sprint(got.gen) != gen {
		t.Errorf("quote-side wrong after the -resume-paused-policy run: usd=%v gen=%d", got.usd, got.gen)
	}

	// ── 6. refusals: no policy to pause; a generation in the future ───
	exec(t, `UPDATE trades SET usd_volume = 0.00372265, derive_generation = 0 WHERE source='sdex' AND ledger=$1`, ledger["quote-side wrong"])
	exec(t, `SELECT compress_chunk(c, true) FROM show_chunks('trades') c`)
	before = snapshot(t)
	var jobSchedule, jobConfig string
	if err := store.DB().QueryRowContext(ctx, `SELECT schedule_interval::text, config::text FROM timescaledb_information.jobs WHERE proc_name = 'trades_compression_policy'`).Scan(&jobSchedule, &jobConfig); err != nil {
		t.Fatalf("read the trades compression job: %v", err)
	}
	exec(t, `SELECT delete_job(job_id) FROM timescaledb_information.jobs WHERE proc_name = 'trades_compression_policy'`)
	out, err = captureStdout(t, func() error { return chops.Run(append(args, "-write")) })
	if err == nil || !strings.Contains(err.Error(), "no compression policy job on trades") || !strings.Contains(err.Error(), "refuses to start") {
		t.Fatalf("without a compression policy: err = %v, want a refusal naming it\n%s", err, out)
	}
	sameSnapshot(t, before, snapshot(t), "refused run (no policy)")
	if !chunkCompressed(t) {
		t.Fatal("the refused run decompressed the chunk")
	}
	exec(t, `SELECT add_job('trades_compression_policy', $1::interval, config => $2::jsonb)`, jobSchedule, jobConfig)
	if !policyScheduled(t) {
		t.Fatal("fixture: the re-added compression policy is not scheduled")
	}

	future := fmt.Sprint(time.Now().Add(24 * time.Hour).Unix())
	futureArgs := append([]string{}, args[:len(args)-4]...) // drop -generation and -min-free-bytes
	futureArgs = append(futureArgs, "-generation", future, "-min-free-bytes", fmt.Sprint(int64(1)<<40), "-write")
	out, err = captureStdout(t, func() error { return chops.Run(futureArgs) })
	if err == nil || !strings.Contains(err.Error(), "in the future") {
		t.Fatalf("-generation %s: err = %v, want the future refusal\n%s", future, err, out)
	}
	sameSnapshot(t, before, snapshot(t), "refused run (future generation)")
}

// captureStdout runs fn with os.Stdout redirected into a buffer. The ops
// subcommands print their plans and reports with fmt.Print*, which reads
// os.Stdout at call time.
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	var buf bytes.Buffer
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(&buf, r)
		close(done)
	}()
	ferr := fn()
	os.Stdout = old
	_ = w.Close()
	<-done
	_ = r.Close()
	return buf.String(), ferr
}

// TestExactTierRestampChunks_RestampsInsideACompressedChunk is the
// DB-backed proof for `usd-volume-restamp -tier exact -chunks`, driven
// through the real subcommand (flags, config, live-tail guard,
// generation) on real TimescaleDB, against a `trades` chunk that is
// COMPRESSED the way every chunk older than the 7-day policy is on
// production. It is the exact tier's half of the test above, and it
// exists because the exact-tier repair population — ~10M rows is 100+ hours at the ~1,574
// rows/min an in-place UPDATE sustains against compressed chunks.
//
//  1. the DRY RUN (the default) prints the chunk plan, counts, and leaves
//     the chunk compressed and every row byte-identical;
//  2. -write applies the peg identity (`base_amount / 10^7`, the value
//     [timescale.ExactTierUSDVolume] renders), stamps the rows with the
//     run's generation, and leaves the chunk COMPRESSED again;
//  3. the already-correct row is untouched — value AND generation;
//  4. NULL rows are filled only because -fill-null was passed;
//  5. a second -write run changes nothing: the chunk is probed read-only,
//     reported as skipped, and never decompressed;
//  6. `-chunk-batch` — the xlm-base tier's row batch — is still refused
//     with -tier exact, because this walk's transaction is one -slice
//     window.
func TestExactTierRestampChunks_RestampsInsideACompressedChunk(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const usdcID = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	usdc, err := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	// USDC/XLM — the dollar leg is the BASE: tier 2b, the exact class a sweep
	// found dirty on every day it covered.
	pair, err := c.NewPair(usdc, c.NativeAsset())
	if err != nil {
		t.Fatal(err)
	}
	if err := timescale.InstallUSDVolumeResolution(store, []string{usdcID}, nil); err != nil {
		t.Fatalf("InstallUSDVolumeResolution: %v", err)
	}

	day := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	// base amounts in stroops: $125, $0.0000003 (dust), $9,876,543.21.
	fixtures := []struct {
		name string
		base int64
		ts   time.Time
	}{
		{"resolver-priced", 1_250_000_000, day.Add(3 * time.Hour)},
		{"stored NULL", 3, day.Add(9 * time.Hour)},
		{"already correct", 98_765_432_100_000, day.Add(15 * time.Hour)},
	}
	ledger := map[string]uint32{}
	var topLedger uint32
	for i, f := range fixtures {
		tr := mkIntegrationTrade("sdex", 20+i, f.ts, pair, f.base, 10_000_000_000)
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade %s: %v", f.name, err)
		}
		ledger[f.name] = tr.Ledger
		if tr.Ledger > topLedger {
			topLedger = tr.Ledger
		}
	}
	exec := func(t *testing.T, q string, args ...any) {
		t.Helper()
		if _, err := store.DB().ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%q: %v", q, err)
		}
	}
	type row struct {
		usd *string
		gen int64
	}
	readRow := func(t *testing.T, l uint32) row {
		t.Helper()
		var (
			usd sql.NullString
			gen int64
		)
		if err := store.DB().QueryRowContext(ctx,
			`SELECT usd_volume::text, derive_generation FROM trades WHERE source = 'sdex' AND ledger = $1`, l,
		).Scan(&usd, &gen); err != nil {
			t.Fatalf("read ledger %d: %v", l, err)
		}
		if !usd.Valid {
			return row{nil, gen}
		}
		return row{&usd.String, gen}
	}
	snapshot := func(t *testing.T) map[uint32]row {
		t.Helper()
		out := map[uint32]row{}
		for _, l := range ledger {
			out[l] = readRow(t, l)
		}
		return out
	}
	sameSnapshot := func(t *testing.T, before, after map[uint32]row, what string) {
		t.Helper()
		for l, b := range before {
			a := after[l]
			switch {
			case (b.usd == nil) != (a.usd == nil):
				t.Errorf("%s: ledger %d usd_volume nullness changed (%v -> %v)", what, l, b.usd, a.usd)
			case b.usd != nil && *b.usd != *a.usd:
				t.Errorf("%s: ledger %d usd_volume %s -> %s", what, l, *b.usd, *a.usd)
			case b.gen != a.gen:
				t.Errorf("%s: ledger %d derive_generation %d -> %d", what, l, b.gen, a.gen)
			}
		}
	}
	chunkCompressed := func(t *testing.T) bool {
		t.Helper()
		var compressed bool
		if err := store.DB().QueryRowContext(ctx, `
			SELECT is_compressed FROM timescaledb_information.chunks
			 WHERE hypertable_name = 'trades' AND range_start <= $1 AND range_end > $1`, day.Add(3*time.Hour),
		).Scan(&compressed); err != nil {
			t.Fatalf("read chunk state: %v", err)
		}
		return compressed
	}
	policyScheduled := func(t *testing.T) bool {
		t.Helper()
		var scheduled bool
		if err := store.DB().QueryRowContext(ctx,
			`SELECT scheduled FROM timescaledb_information.jobs WHERE proc_name = 'trades_compression_policy'`,
		).Scan(&scheduled); err != nil {
			t.Fatalf("read the trades compression policy: %v", err)
		}
		return scheduled
	}

	// ── the stale state, imposed by hand ─────────────────────
	exec(t, `UPDATE trades SET usd_volume = usd_volume * 1.007 WHERE source='sdex' AND ledger=$1`, ledger["resolver-priced"])
	exec(t, `UPDATE trades SET usd_volume = NULL WHERE source='sdex' AND ledger=$1`, ledger["stored NULL"])
	correctBefore := readRow(t, ledger["already correct"])
	if correctBefore.usd == nil || correctBefore.gen != 0 {
		t.Fatalf("fixture: the correct row = %+v, want a gen-0 priced row", correctBefore)
	}

	// ── compress the chunk, as the 7-day policy would have ────────────
	exec(t, `SELECT compress_chunk(c, true) FROM show_chunks('trades') c`)
	if !chunkCompressed(t) {
		t.Fatal("fixture: the day's chunk did not compress")
	}
	if !policyScheduled(t) {
		t.Fatal("fixture: the trades compression policy (migration 0001) is not scheduled before the run")
	}
	if err := store.UpsertCursor(ctx, "ledgerstream", "", topLedger+1_000); err != nil {
		t.Fatalf("UpsertCursor: %v", err)
	}

	cfgPath := filepath.Join(t.TempDir(), "stellarindex.toml")
	cfg := fmt.Sprintf("[storage]\npostgres_dsn = %q\n\n[trades]\nusd_pegged_classic_assets = [%q]\n", dsn, usdcID)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	const gen = "1756800000"
	args := []string{
		"usd-volume-restamp", "-config", cfgPath, "-tier", "exact", "-chunks",
		"-from", "2026-06-10", "-to", "2026-06-10", "-fill-null",
		"-generation", gen,
		// The database's data directory is inside the container, so the
		// host cannot statfs it: the operator-override path.
		"-min-free-bytes", fmt.Sprint(int64(1) << 40),
	}

	// ── 1. dry run: plan printed, nothing decompressed, nothing written ─
	before := snapshot(t)
	out, err := captureStdout(t, func() error { return chops.Run(args) })
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	for _, want := range []string{
		"chunk plan: 1 trades chunk(s) intersect [2026-06-10, 2026-06-10] — 1 compressed, 0 not",
		"WARNING: trusting -min-free-bytes",
		"DRY RUN: would take session advisory lock hashtext('usd-volume-restamp:trades')",
		"DRY RUN: nothing is decompressed",
		"would change 2 row(s)",
		"would restamp 2 row(s) across 1 exact-tier group-day(s)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output lacks %q:\n%s", want, out)
		}
	}
	sameSnapshot(t, before, snapshot(t), "dry run")
	if !chunkCompressed(t) {
		t.Fatal("the dry run decompressed the chunk")
	}
	if !policyScheduled(t) {
		t.Fatal("the dry run paused the compression policy")
	}

	// ── 2. -write: identity applied INSIDE the chunk, left compressed ──
	out, err = captureStdout(t, func() error { return chops.Run(append(args, "-write")) })
	if err != nil {
		t.Fatalf("write run: %v\n%s", err, out)
	}
	for _, want := range []string{
		"changed 2 row(s)",
		"bytes ",
		"restamped 2 row(s) across 1 exact-tier group-day(s) in [2026-06-10, 2026-06-10]",
		"CALL refresh_continuous_aggregate('prices_1m'",
		"acceptance: stellarindex-ops verify-usd-volume -config " + cfgPath + " -day 2026-06-10 -days 1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("write-run output lacks %q:\n%s", want, out)
		}
	}
	if !chunkCompressed(t) {
		t.Fatal("the chunk was left DECOMPRESSED after a successful write run")
	}
	if !policyScheduled(t) {
		t.Error("the trades compression policy was not re-enabled after the write run")
	}
	// The identity, from the SAME function the insert path renders with.
	after := snapshot(t)
	for _, f := range fixtures[:2] {
		want, ok := timescale.ExactTierUSDVolume(timescale.TierBasePegged, 7, fmt.Sprint(f.base), "10000000000")
		if !ok {
			t.Fatalf("%s: ExactTierUSDVolume declined", f.name)
		}
		got := after[ledger[f.name]]
		if got.usd == nil {
			t.Errorf("%s: usd_volume is NULL, want %s", f.name, want)
			continue
		}
		gotRat, ok1 := new(big.Rat).SetString(*got.usd)
		wantRat, ok2 := new(big.Rat).SetString(want)
		if !ok1 || !ok2 || gotRat.Cmp(wantRat) != 0 {
			t.Errorf("%s: usd_volume = %s, want %s (pegged_leg / 10^7)", f.name, *got.usd, want)
		}
		if fmt.Sprint(got.gen) != gen {
			t.Errorf("%s: derive_generation = %d, want the run's %s (INV-3)", f.name, got.gen, gen)
		}
	}
	// 3. the already-correct row is byte-identical, generation included.
	if a := after[ledger["already correct"]]; a.gen != 0 || *a.usd != *correctBefore.usd {
		t.Errorf("correctly-stamped row was rewritten: before %+v after %+v", correctBefore, a)
	}

	// ── 5. the rerun: probed, skipped, nothing moves ──────────────────
	before = snapshot(t)
	out, err = captureStdout(t, func() error { return chops.Run(append(args, "-write")) })
	if err != nil {
		t.Fatalf("rerun: %v\n%s", err, out)
	}
	for _, want := range []string{
		"nothing to change — skipped, chunk left compressed",
		"restamped 0 row(s)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rerun output lacks %q:\n%s", want, out)
		}
	}
	sameSnapshot(t, before, snapshot(t), "rerun")
	if !chunkCompressed(t) {
		t.Fatal("the rerun left the chunk decompressed")
	}
	if !policyScheduled(t) {
		t.Fatal("the rerun left the compression policy paused")
	}

	// ── 6. the estimated tiers' row batch is still refused here ───────
	out, err = captureStdout(t, func() error { return chops.Run(append(args, "-chunk-batch", "5000", "-write")) })
	if err == nil || !strings.Contains(err.Error(), "-chunk-batch") || !strings.Contains(err.Error(), "estimated tiers") {
		t.Fatalf("-chunk-batch with -tier exact: err = %v, want a refusal naming the flag\n%s", err, out)
	}
	sameSnapshot(t, before, snapshot(t), "refused run (-chunk-batch)")
}

// ─── the two mirror tiers, against a real COMPRESSED chunk ──────────────
//
// These are the DB-backed proofs for `usd-volume-restamp -tier xlm-quote`
// and `-tier cex-fx`, driven through the real subcommand (flags, config,
// live-tail guard, generation) on real TimescaleDB, against a `trades`
// chunk that is COMPRESSED the way every chunk older than the 7-day
// policy is on production. They are the mirrors of
// TestXLMBaseRestampChunks_RestampsInsideACompressedChunk, and they exist
// for the same reason: an in-place UPDATE into a compressed chunk
// measured ~1,574 rows/min, and these two populations are
// ~18.0M and ~12.6M rows.
//
// Each one pins the same four things:
//
//  1. the rows the tier owns are restamped to the value the LIVE insert
//     path computes, and stamped with the run's generation;
//  2. the chunk is COMPRESSED again afterwards;
//  3. a row the anchor / the FX feed cannot price is left EXACTLY as it
//     was — a stored NULL stays NULL — and is COUNTED in the report,
//     never guessed at;
//  4. a token/token spam row is untouched by both tiers, whatever the
//     window: the substance gate from the tier-3b valuation incident.

// restampRow is one row's money state, for the before/after comparisons.
type restampRow struct {
	usd *string
	gen int64
}

// mirrorRestampFixture is the shared harness: a store, a compressed
// `trades` chunk, a config file and the row readers the two tests below
// assert with.
type mirrorRestampFixture struct {
	store   *timescale.Store
	ctx     context.Context
	cfgPath string
	t       *testing.T
}

func newMirrorRestampFixture(t *testing.T, ctx context.Context, usdcID string) *mirrorRestampFixture {
	t.Helper()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	// The insert path's own wiring, so the seeded rows are what
	// production rows are.
	if err := timescale.InstallUSDVolumeResolution(store, []string{usdcID}, nil); err != nil {
		t.Fatalf("InstallUSDVolumeResolution: %v", err)
	}
	cfgPath := filepath.Join(t.TempDir(), "stellarindex.toml")
	cfg := fmt.Sprintf("[storage]\npostgres_dsn = %q\n\n[trades]\nusd_pegged_classic_assets = [%q]\n", dsn, usdcID)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return &mirrorRestampFixture{store: store, ctx: ctx, cfgPath: cfgPath, t: t}
}

func (f *mirrorRestampFixture) exec(q string, args ...any) {
	f.t.Helper()
	if _, err := f.store.DB().ExecContext(f.ctx, q, args...); err != nil {
		f.t.Fatalf("%q: %v", q, err)
	}
}

func (f *mirrorRestampFixture) read(source string, ledger uint32) restampRow {
	f.t.Helper()
	var (
		usd sql.NullString
		gen int64
	)
	if err := f.store.DB().QueryRowContext(f.ctx,
		`SELECT usd_volume::text, derive_generation FROM trades WHERE source = $1 AND ledger = $2`, source, ledger,
	).Scan(&usd, &gen); err != nil {
		f.t.Fatalf("read %s ledger %d: %v", source, ledger, err)
	}
	if !usd.Valid {
		return restampRow{nil, gen}
	}
	return restampRow{&usd.String, gen}
}

// compressTrades compresses every `trades` chunk, as the 7-day policy
// would have, and asserts the window's chunk is compressed.
func (f *mirrorRestampFixture) compressTrades(at time.Time) {
	f.t.Helper()
	f.exec(`SELECT compress_chunk(c, true) FROM show_chunks('trades') c`)
	if !f.chunkCompressed(at) {
		f.t.Fatal("fixture: the day's chunk did not compress")
	}
}

func (f *mirrorRestampFixture) chunkCompressed(at time.Time) bool {
	f.t.Helper()
	var compressed bool
	if err := f.store.DB().QueryRowContext(f.ctx, `
		SELECT is_compressed FROM timescaledb_information.chunks
		 WHERE hypertable_name = 'trades' AND range_start <= $1 AND range_end > $1`, at,
	).Scan(&compressed); err != nil {
		f.t.Fatalf("read chunk state: %v", err)
	}
	return compressed
}

// TestXLMQuoteRestampChunks_RestampsInsideACompressedChunk is the
// xlm-quote mirror's proof: DEX trades whose QUOTE leg is XLM are valued
// off that leg (`quote_amount/1e7 x XLM/USD at ts`) through the same
// anchor the xlm-base tier calls, inside a compressed chunk, which is
// left compressed.
func TestXLMQuoteRestampChunks_RestampsInsideACompressedChunk(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	const usdcID = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	f := newMirrorRestampFixture(t, ctx, usdcID)

	usdc, err := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	token, err := c.NewSorobanAsset("CAFJZQWSED6YAWZU3GWRTOCNPPCGBN32L7QV43XX5LZLFTK6JLN34DLN")
	if err != nil {
		t.Fatal(err)
	}
	other, err := c.NewSorobanAsset("CAQQR5SWBXKIGZKPBZDH3KM5GQ5GUTPKB7JAFCINLZBC5WXPJKRG3IM7")
	if err != nil {
		t.Fatal(err)
	}
	xlm := c.NativeAsset()
	xlmUSDC, err := c.NewPair(xlm, usdc)
	if err != nil {
		t.Fatal(err)
	}
	// The mirror population: XLM in the QUOTE leg.
	tokenXLM, err := c.NewPair(token, xlm)
	if err != nil {
		t.Fatal(err)
	}
	// The spam population: neither leg is XLM, USD-pegged or fiat.
	tokenToken, err := c.NewPair(token, other)
	if err != nil {
		t.Fatal(err)
	}

	day := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	anchorTS := day.Add(10 * time.Hour)

	// XLM/USD anchor: 100 XLM for 50 USDC -> $0.50.
	anchor := mkIntegrationTrade("sdex", 1, anchorTS, xlmUSDC, 1_000_000_000, 500_000_000)
	if err := f.store.InsertTrade(ctx, anchor); err != nil {
		t.Fatalf("InsertTrade anchor: %v", err)
	}
	f.exec(`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`)

	// 10 XLM in the QUOTE leg against a token with no USD market, so the
	// anchor's answer is exactly $5.00.
	const wantAnchored = "5.00000000"
	fixtures := []struct {
		name  string
		nonce int
		ts    time.Time
	}{
		{"quote-side wrong", 10, anchorTS.Add(5 * time.Minute)},
		{"stored NULL", 11, anchorTS.Add(6 * time.Minute)},
		{"already correct", 12, anchorTS.Add(7 * time.Minute)},
		// Three hours BEFORE the anchor trade: prices_1m holds no XLM/USD
		// bucket at or before this row, so the anchor declines it. It is
		// the "cannot price" case — reported, never guessed at.
		{"anchor declines", 13, anchorTS.Add(-3 * time.Hour)},
	}
	ledger := map[string]uint32{}
	var topLedger uint32
	for _, fx := range fixtures {
		tr := mkIntegrationTrade("sdex", fx.nonce, fx.ts, tokenXLM, 300, 100_000_000)
		if err := f.store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade %s: %v", fx.name, err)
		}
		ledger[fx.name] = tr.Ledger
		if tr.Ledger > topLedger {
			topLedger = tr.Ledger
		}
	}
	// The spam row: a token/token pair whose only possible rate is the
	// tier-3b bridge its own counterparties author.
	spam := mkIntegrationTrade("sdex", 20, anchorTS.Add(8*time.Minute), tokenToken, 10_000_000, 50_000_000_000_000)
	if err := f.store.InsertTrade(ctx, spam); err != nil {
		t.Fatalf("InsertTrade spam: %v", err)
	}
	if spam.Ledger > topLedger {
		topLedger = spam.Ledger
	}

	// The stale state, imposed by hand.
	f.exec(`UPDATE trades SET usd_volume = 0.00372265 WHERE source='sdex' AND ledger=$1`, ledger["quote-side wrong"])
	f.exec(`UPDATE trades SET usd_volume = NULL       WHERE source='sdex' AND ledger=$1`, ledger["stored NULL"])

	// The row the anchor cannot price must START as NULL for the claim
	// "a stored NULL stays NULL" to mean anything.
	if got := f.read("sdex", ledger["anchor declines"]); got.usd != nil {
		t.Fatalf("fixture: the pre-anchor row was priced at insert (%s); the anchor was expected to decline it", *got.usd)
	}
	if got := f.read("sdex", spam.Ledger); got.usd != nil {
		t.Fatalf("fixture: the token/token row was priced at insert (%s)", *got.usd)
	}

	f.compressTrades(anchorTS)
	if err := f.store.UpsertCursor(ctx, "ledgerstream", "", topLedger+1_000); err != nil {
		t.Fatalf("UpsertCursor: %v", err)
	}

	const gen = "1756800000"
	args := []string{
		"usd-volume-restamp", "-config", f.cfgPath, "-tier", "xlm-quote", "-chunks",
		"-from", "2026-06-10", "-to", "2026-06-10", "-fill-null",
		"-generation", gen,
		// The database's data directory is inside the container, so the
		// host cannot statfs it: the operator-override path.
		"-min-free-bytes", fmt.Sprint(int64(1) << 40),
	}

	// ── the dry run: the plan is printed, nothing is decompressed ──────
	out, err := captureStdout(t, func() error { return chops.Run(args) })
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	for _, want := range []string{
		"chunk plan: 1 trades chunk(s) intersect [2026-06-10, 2026-06-10] — 1 compressed, 0 not",
		"DRY RUN: nothing is decompressed",
		"would change 2 row(s)",
		"would restamp 2 row(s) in [2026-06-10, 2026-06-10] (tier xlm-quote)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output lacks %q:\n%s", want, out)
		}
	}
	if !f.chunkCompressed(anchorTS) {
		t.Fatal("the dry run decompressed the chunk")
	}

	// ── -write: restamped through the anchor, chunk compressed again ───
	out, err = captureStdout(t, func() error { return chops.Run(append(args, "-write")) })
	if err != nil {
		t.Fatalf("write run: %v\n%s", err, out)
	}
	for _, want := range []string{
		"changed 2 row(s) (planned 2)",
		"restamped 2 row(s) in [2026-06-10, 2026-06-10] (tier xlm-quote)",
		"scanned (source=DEX, quote=XLM form, derive_generation <= 1756800000)",
		// The unpriceable row is REPORTED, with what it holds.
		"anchor declined, stored NULL   (coverage NOT recoverable)    1",
		"CALL refresh_continuous_aggregate('prices_1m'",
		"CALL refresh_continuous_aggregate('twap_1d'",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("write-run output lacks %q:\n%s", want, out)
		}
	}
	if !f.chunkCompressed(anchorTS) {
		t.Fatal("the chunk was left DECOMPRESSED after a successful write run")
	}
	for _, name := range []string{"quote-side wrong", "stored NULL"} {
		got := f.read("sdex", ledger[name])
		if got.usd == nil || *got.usd != wantAnchored {
			t.Errorf("%s: usd_volume = %v, want %s", name, got.usd, wantAnchored)
		}
		if fmt.Sprint(got.gen) != gen {
			t.Errorf("%s: derive_generation = %d, want the run's %s", name, got.gen, gen)
		}
	}
	// The row the anchor declined keeps its NULL, at its own generation.
	if got := f.read("sdex", ledger["anchor declines"]); got.usd != nil || got.gen != 0 {
		t.Errorf("the unpriceable row was written: usd=%v gen=%d", got.usd, got.gen)
	}
	// The token/token row is untouched — the substance gate.
	if got := f.read("sdex", spam.Ledger); got.usd != nil || got.gen != 0 {
		t.Errorf("the token/token row was valued by the xlm-quote tier: usd=%v gen=%d", got.usd, got.gen)
	}
	// The already-correct row and the exact-tier anchor row are untouched.
	if got := f.read("sdex", ledger["already correct"]); got.usd == nil || *got.usd != wantAnchored || got.gen != 0 {
		t.Errorf("the already-correct row moved: usd=%v gen=%d", got.usd, got.gen)
	}
	if got := f.read("sdex", anchor.Ledger); got.usd == nil || got.gen != 0 {
		t.Errorf("the exact-tier anchor row moved: usd=%v gen=%d", got.usd, got.gen)
	}

	// ── the rerun: probed, skipped, nothing moves ─────────────────────
	out, err = captureStdout(t, func() error { return chops.Run(append(args, "-write")) })
	if err != nil {
		t.Fatalf("rerun: %v\n%s", err, out)
	}
	for _, want := range []string{"nothing to change — skipped, chunk left compressed", "restamped 0 row(s)"} {
		if !strings.Contains(out, want) {
			t.Errorf("rerun output lacks %q:\n%s", want, out)
		}
	}
	if !f.chunkCompressed(anchorTS) {
		t.Fatal("the rerun left the chunk decompressed")
	}
}

// TestCEXFiatRestampChunks_RestampsInsideACompressedChunk is the cex-fx
// tier's proof: off-chain CEX trades quoted in a non-USD fiat are valued
// from `fx_quotes` at or before the trade, inside a compressed chunk,
// which is left compressed — and a trade whose nearest quote is outside
// the as-of tolerance is REFUSED rather than extrapolated to.
func TestCEXFiatRestampChunks_RestampsInsideACompressedChunk(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	const usdcID = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	f := newMirrorRestampFixture(t, ctx, usdcID)

	btc, err := c.NewCryptoAsset("BTC")
	if err != nil {
		t.Fatal(err)
	}
	eur, err := c.NewFiatAsset("EUR")
	if err != nil {
		t.Fatal(err)
	}
	gbp, err := c.NewFiatAsset("GBP")
	if err != nil {
		t.Fatal(err)
	}
	usd, err := c.NewFiatAsset("USD")
	if err != nil {
		t.Fatal(err)
	}
	btcEUR, err := c.NewPair(btc, eur)
	if err != nil {
		t.Fatal(err)
	}
	btcGBP, err := c.NewPair(btc, gbp)
	if err != nil {
		t.Fatal(err)
	}
	btcUSD, err := c.NewPair(btc, usd)
	if err != nil {
		t.Fatal(err)
	}
	tokenA, err := c.NewSorobanAsset("CAFJZQWSED6YAWZU3GWRTOCNPPCGBN32L7QV43XX5LZLFTK6JLN34DLN")
	if err != nil {
		t.Fatal(err)
	}
	tokenB, err := c.NewSorobanAsset("CAQQR5SWBXKIGZKPBZDH3KM5GQ5GUTPKB7JAFCINLZBC5WXPJKRG3IM7")
	if err != nil {
		t.Fatal(err)
	}
	tokenToken, err := c.NewPair(tokenA, tokenB)
	if err != nil {
		t.Fatal(err)
	}

	day := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	ts := day.Add(10 * time.Hour)

	// The vendor feed, as the forex worker writes it: rate_usd is
	// UNITS-OF-TICKER PER 1 USD (migration 0028), so 0.8 EUR per USD is
	// $1.25 per EUR. Written as SQL rather than through the float64
	// batch API so the fixture rate is exact.
	//
	//   EUR — a bucket at 00:00 on the trades' own day (10 h before them)
	//   GBP — a bucket NINE DAYS earlier and nothing since: the nearest
	//         quote at or before the trade is outside the 7-day as-of
	//         tolerance, which is a refusal, not an extrapolation.
	f.exec(`INSERT INTO fx_quotes (bucket, ticker, rate_usd, inverse_usd, source) VALUES ($1, 'EUR', 0.8, 1.25, 'massive')`, day)
	f.exec(`INSERT INTO fx_quotes (bucket, ticker, rate_usd, inverse_usd, source) VALUES ($1, 'GBP', 0.5, 2.0,  'massive')`, day.AddDate(0, 0, -9))

	// 100.00000000 EUR (the CEX 1e8 amount scale) x $1.25 = $125.
	const wantEUR = "125.00000000"
	fixtures := []struct {
		name  string
		nonce int
		pair  c.Pair
	}{
		{"fx wrong", 30, btcEUR},
		{"stored NULL", 31, btcEUR},
		{"already correct", 32, btcEUR},
		{"outside tolerance", 33, btcGBP},
		{"USD quote (tier 1)", 34, btcUSD},
	}
	ledger := map[string]uint32{}
	var topLedger uint32
	for i, fx := range fixtures {
		tr := mkIntegrationTrade("binance", fx.nonce, ts.Add(time.Duration(i)*time.Minute), fx.pair, 100_000, 10_000_000_000)
		if err := f.store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade %s: %v", fx.name, err)
		}
		ledger[fx.name] = tr.Ledger
		if tr.Ledger > topLedger {
			topLedger = tr.Ledger
		}
	}
	spam := mkIntegrationTrade("sdex", 40, ts.Add(9*time.Minute), tokenToken, 10_000_000, 50_000_000_000_000)
	if err := f.store.InsertTrade(ctx, spam); err != nil {
		t.Fatalf("InsertTrade spam: %v", err)
	}
	if spam.Ledger > topLedger {
		topLedger = spam.Ledger
	}

	// The stale state: the population this tier exists for is stored
	// NULL (prices_1m has no fiat pair, so before the resolver read
	// fx_quotes these rows fell through every tier).
	f.exec(`UPDATE trades SET usd_volume = 0.50000000 WHERE source='binance' AND ledger=$1`, ledger["fx wrong"])
	f.exec(`UPDATE trades SET usd_volume = NULL       WHERE source='binance' AND ledger=$1`, ledger["stored NULL"])
	if got := f.read("binance", ledger["already correct"]); got.usd == nil || *got.usd != wantEUR {
		t.Fatalf("fixture: the insert path valued the EUR row as %v, want %s — the tier's arithmetic and the insert path's have drifted", got.usd, wantEUR)
	}
	if got := f.read("binance", ledger["outside tolerance"]); got.usd != nil {
		t.Fatalf("fixture: the GBP row was priced at insert (%s) off a nine-day-old quote", *got.usd)
	}
	usdBefore := f.read("binance", ledger["USD quote (tier 1)"])
	if usdBefore.usd == nil {
		t.Fatal("fixture: the USD-quoted row is not priced; tier 1 should have valued it exactly")
	}

	f.compressTrades(ts)
	if err := f.store.UpsertCursor(ctx, "ledgerstream", "", topLedger+1_000); err != nil {
		t.Fatalf("UpsertCursor: %v", err)
	}

	const gen = "1756800000"
	args := []string{
		"usd-volume-restamp", "-config", f.cfgPath, "-tier", "cex-fx", "-chunks",
		"-from", "2026-06-10", "-to", "2026-06-10", "-fill-null",
		"-generation", gen,
		"-min-free-bytes", fmt.Sprint(int64(1) << 40),
	}

	// ── the as-of tolerance is the TOOL's, not just the resolver's ─────
	// At -fx-max-staleness 1h the EUR bucket (10 h before the trades) is
	// outside the tolerance, so the run refuses every row rather than
	// pricing them off it.
	narrow := append(append([]string{}, args...), "-fx-max-staleness", "1h")
	out, err := captureStdout(t, func() error { return chops.Run(narrow) })
	if err != nil {
		t.Fatalf("narrowed-tolerance dry run: %v\n%s", err, out)
	}
	for _, want := range []string{
		"would change 0 row(s)",
		"refused for want of a quote within the as-of tolerance",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("narrowed-tolerance output lacks %q:\n%s", want, out)
		}
	}
	// Widening past the live insert path's own lookback is refused.
	wide := append(append([]string{}, args...), "-fx-max-staleness", "240h")
	if _, err := captureStdout(t, func() error { return chops.Run(wide) }); err == nil ||
		!strings.Contains(err.Error(), "exceeds the live insert path") {
		t.Fatalf("-fx-max-staleness 240h: err = %v, want the widened-tolerance refusal", err)
	}

	// ── the dry run at the default tolerance ──────────────────────────
	out, err = captureStdout(t, func() error { return chops.Run(args) })
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	for _, want := range []string{
		"DRY RUN: nothing is decompressed",
		"would change 2 row(s)",
		"would restamp 2 row(s) in [2026-06-10, 2026-06-10] (tier cex-fx)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output lacks %q:\n%s", want, out)
		}
	}
	if !f.chunkCompressed(ts) {
		t.Fatal("the dry run decompressed the chunk")
	}

	// ── -write ────────────────────────────────────────────────────────
	out, err = captureStdout(t, func() error { return chops.Run(append(args, "-write")) })
	if err != nil {
		t.Fatalf("write run: %v\n%s", err, out)
	}
	for _, want := range []string{
		"changed 2 row(s) (planned 2)",
		"restamped 2 row(s) in [2026-06-10, 2026-06-10] (tier cex-fx)",
		"scanned (source=CEX, quote=non-USD fiat, derive_generation <= 1756800000)",
		// The row whose nearest quote is nine days old: refused, counted,
		// and said out loud.
		"fx declined, stored NULL   (coverage NOT recoverable)    1",
		"refused for want of a quote within the as-of tolerance  1",
		"CALL refresh_continuous_aggregate('prices_1m'",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("write-run output lacks %q:\n%s", want, out)
		}
	}
	if !f.chunkCompressed(ts) {
		t.Fatal("the chunk was left DECOMPRESSED after a successful write run")
	}
	for _, name := range []string{"fx wrong", "stored NULL"} {
		got := f.read("binance", ledger[name])
		if got.usd == nil || *got.usd != wantEUR {
			t.Errorf("%s: usd_volume = %v, want %s", name, got.usd, wantEUR)
		}
		if fmt.Sprint(got.gen) != gen {
			t.Errorf("%s: derive_generation = %d, want the run's %s", name, got.gen, gen)
		}
	}
	// Outside the tolerance: still NULL, never extrapolated to the
	// nine-day-old rate.
	if got := f.read("binance", ledger["outside tolerance"]); got.usd != nil || got.gen != 0 {
		t.Errorf("the out-of-tolerance row was written: usd=%v gen=%d", got.usd, got.gen)
	}
	// The USD-quoted row is tier 1's, and the token/token row is nobody's.
	if got := f.read("binance", ledger["USD quote (tier 1)"]); got.usd == nil || *got.usd != *usdBefore.usd || got.gen != usdBefore.gen {
		t.Errorf("the exact-tier USD row moved: %v -> %v", usdBefore, got)
	}
	if got := f.read("sdex", spam.Ledger); got.usd != nil || got.gen != 0 {
		t.Errorf("the token/token row was valued by the cex-fx tier: usd=%v gen=%d", got.usd, got.gen)
	}
	if got := f.read("binance", ledger["already correct"]); got.usd == nil || *got.usd != wantEUR || got.gen != 0 {
		t.Errorf("the already-correct row moved: usd=%v gen=%d", got.usd, got.gen)
	}

	// ── the rerun: probed, skipped, nothing moves ─────────────────────
	out, err = captureStdout(t, func() error { return chops.Run(append(args, "-write")) })
	if err != nil {
		t.Fatalf("rerun: %v\n%s", err, out)
	}
	for _, want := range []string{"nothing to change — skipped, chunk left compressed", "restamped 0 row(s)"} {
		if !strings.Contains(out, want) {
			t.Errorf("rerun output lacks %q:\n%s", want, out)
		}
	}
	if !f.chunkCompressed(ts) {
		t.Fatal("the rerun left the chunk decompressed")
	}
}

// TestXLMBaseRestamp_RederivesThroughTheLiveAnchor is the DB-backed proof
// for `usd-volume-restamp -tier xlm-base` on real TimescaleDB, against rows the REAL insert path wrote with the REAL
// resolver wiring:
//
//  1. the re-derived value is the ANCHOR value — base_amount/1e7 x the
//     XLM/USD rate the resolver reads out of prices_1m at the row's ts —
//     and not the quote-side thin-book number the pre-fd1860bd waterfall
//     stored;
//  2. DRY RUN WRITES NOTHING: planning the window leaves every row's
//     value and derive_generation byte-identical;
//  3. -write writes EXACTLY the rows the dry run reported, and nothing
//     else in the window moves;
//  4. a row the anchor cannot price is left alone — a stored NULL stays
//     NULL rather than inheriting the quote-side estimate, and a stored
//     (wrong) value is not blanked;
//  5. the NULL population is opt-in (-fill-null) and counted either way;
//  6. a row at a HIGHER derive_generation is never clawed back,
//     and every rewritten row carries the run's generation;
//  7. idempotent: an immediate re-run plans zero rows;
//  8. the exact tiers are NOT touched by this tier (a USD-pegged quote
//     belongs to `-tier exact`), so the two tools cannot undo each other.
func TestXLMBaseRestamp_RederivesThroughTheLiveAnchor(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const usdcID = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	usdc, err := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	token, err := c.NewSorobanAsset("CAFJZQWSED6YAWZU3GWRTOCNPPCGBN32L7QV43XX5LZLFTK6JLN34DLN")
	if err != nil {
		t.Fatal(err)
	}
	xlm := c.NativeAsset()
	xlmUSDC, err := c.NewPair(xlm, usdc)
	if err != nil {
		t.Fatal(err)
	}
	xlmToken, err := c.NewPair(xlm, token)
	if err != nil {
		t.Fatal(err)
	}

	// The blessed wiring every trade writer gets — NOT a hand-built
	// resolver, so the re-derive runs against exactly the production
	// resolution the insert path uses.
	if err := timescale.InstallUSDVolumeResolution(store, []string{usdcID}, nil); err != nil {
		t.Fatalf("InstallUSDVolumeResolution: %v", err)
	}

	day := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	anchorTS := day.Add(10 * time.Hour)

	// ── the XLM/USD anchor: 100 XLM for 50 USDC -> vwap 0.5 ──────────
	// Clears the resolver's $0.01 direct-leg dust floor by a wide margin
	// (quote notional $50), so it is the rate every XLM-base row below
	// resolves through.
	anchor := mkIntegrationTrade("sdex", 1, anchorTS, xlmUSDC, 1_000_000_000, 500_000_000)
	if err := store.InsertTrade(ctx, anchor); err != nil {
		t.Fatalf("InsertTrade anchor: %v", err)
	}

	// ── the THIN TOKEN/USDC BOOK: the defect's other half ───────
	// A direct `<token>/USDC` market 4h later, when the XLM/USD anchor
	// above has aged past the resolver's 1h direct-leg freshness. This is
	// exactly the BUCK shape: the XLM leg cannot be priced,
	// but the counterparty-authored token book CAN — so the pre-fd1860bd
	// waterfall valued the trade through it. A restamp that fell through
	// to the quote side (rather than reporting "the anchor declined")
	// would re-commit that value at a winning derive_generation, which is
	// what fixtures 20/21 below prove it does not.
	tokenUSDC, err := c.NewPair(token, usdc)
	if err != nil {
		t.Fatal(err)
	}
	thinBook := mkIntegrationTrade("sdex", 2, anchorTS.Add(3*time.Hour+55*time.Minute), tokenUSDC, 1_000, 5_000_000)
	if err := store.InsertTrade(ctx, thinBook); err != nil {
		t.Fatalf("InsertTrade thin book: %v", err)
	}
	// $1000 at the same rate earlier that day, so the book clears the
	// valuation substance floor and the quote side really can price.
	depth := mkIntegrationTrade("sdex", 3, day.Add(6*time.Hour+30*time.Minute), tokenUSDC, 2_000_000, 10_000_000_000)
	if err := store.InsertTrade(ctx, depth); err != nil {
		t.Fatalf("InsertTrade depth: %v", err)
	}
	for _, view := range []string{"prices_1m", "prices_1h"} {
		if _, err := store.DB().ExecContext(ctx,
			`CALL refresh_continuous_aggregate('`+view+`', NULL, NULL)`); err != nil {
			t.Fatalf("refresh %s: %v", view, err)
		}
	}

	// ── the XLM-base population ─────────────────────────────────────
	// Every one is 10 XLM against a pure SEP-41 token with no USD market,
	// so the anchor's answer is 10 x $0.50 = $5.00 exactly.
	const wantAnchored = "5.00000000"
	type fixture struct {
		name  string
		nonce int
		ts    time.Time
	}
	fixtures := []fixture{
		{"quote-side wrong", 10, anchorTS.Add(5 * time.Minute)},
		{"stored NULL", 11, anchorTS.Add(6 * time.Minute)},
		{"already correct", 12, anchorTS.Add(7 * time.Minute)},
		{"higher generation", 13, anchorTS.Add(8 * time.Minute)},
		// 4h past the anchor bucket: beyond the resolver's 1h direct-leg
		// freshness, and XLM is the bridge's own base case, so the anchor
		// declines — while the thin TOKEN/USDC book above is fresh, so
		// the QUOTE side can still price them. These two rows are the
		// whole point: the restamp must report them, not value them.
		{"declined, stored NULL", 20, anchorTS.Add(4 * time.Hour)},
		{"declined, stored value", 21, anchorTS.Add(4*time.Hour + time.Minute)},
	}
	ledger := map[string]uint32{}
	for _, f := range fixtures {
		tr := mkIntegrationTrade("sdex", f.nonce, f.ts, xlmToken, 100_000_000, 300)
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade %s: %v", f.name, err)
		}
		ledger[f.name] = tr.Ledger
	}

	type row struct {
		usd *string
		gen int64
	}
	readRow := func(t *testing.T, l uint32) row {
		t.Helper()
		var (
			usd sql.NullString
			gen int64
		)
		if err := store.DB().QueryRowContext(ctx,
			`SELECT usd_volume::text, derive_generation FROM trades WHERE source = 'sdex' AND ledger = $1`, l,
		).Scan(&usd, &gen); err != nil {
			t.Fatalf("read ledger %d: %v", l, err)
		}
		if !usd.Valid {
			return row{nil, gen}
		}
		return row{&usd.String, gen}
	}
	snapshot := func(t *testing.T) map[uint32]row {
		t.Helper()
		out := map[uint32]row{}
		for _, l := range ledger {
			out[l] = readRow(t, l)
		}
		out[anchor.Ledger] = readRow(t, anchor.Ledger)
		out[thinBook.Ledger] = readRow(t, thinBook.Ledger)
		return out
	}
	sameSnapshot := func(t *testing.T, before, after map[uint32]row, what string) {
		t.Helper()
		for l, b := range before {
			a := after[l]
			switch {
			case (b.usd == nil) != (a.usd == nil):
				t.Errorf("%s: ledger %d usd_volume nullness changed (%v -> %v)", what, l, b.usd, a.usd)
			case b.usd != nil && *b.usd != *a.usd:
				t.Errorf("%s: ledger %d usd_volume %s -> %s", what, l, *b.usd, *a.usd)
			case b.gen != a.gen:
				t.Errorf("%s: ledger %d derive_generation %d -> %d", what, l, b.gen, a.gen)
			}
		}
	}

	// ── the stale state, imposed by hand ──────────────────────
	// HEAD's insert path already writes the anchor value, so the defect
	// has to be re-created: row 10 valued quote-side through the token's
	// own thin book (the BUCK shape), row 11 unpriced, row 21
	// carrying a wrong value the anchor cannot re-derive.
	exec := func(t *testing.T, q string, args ...any) {
		t.Helper()
		if _, err := store.DB().ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("fixture %q: %v", q, err)
		}
	}
	exec(t, `UPDATE trades SET usd_volume = 0.00372265 WHERE source='sdex' AND ledger=$1`, ledger["quote-side wrong"])
	exec(t, `UPDATE trades SET usd_volume = NULL       WHERE source='sdex' AND ledger=$1`, ledger["stored NULL"])
	const higherGen = int64(9_000_000_000)
	exec(t, `UPDATE trades SET usd_volume = 0.11111111, derive_generation = $2 WHERE source='sdex' AND ledger=$1`,
		ledger["higher generation"], higherGen)
	// NOT poisoned by hand: the insert path itself valued this one
	// quote-side through the thin book (0.15 = 300/1e7 x the 5000 raw
	// TOKEN/USDC vwap), which IS the pre-fd1860bd defect state.

	// Sanity: the fixture really did land the states the test asserts on.
	if got := readRow(t, ledger["already correct"]); got.usd == nil || *got.usd != wantAnchored {
		t.Fatalf("fixture: the 'already correct' row is %v, want %s written by the insert path", got.usd, wantAnchored)
	}
	// The quote side really can price the declined pair — otherwise the
	// "only the anchor" assertions below would pass vacuously.
	quoteSide := readRow(t, ledger["declined, stored value"])
	if quoteSide.usd == nil {
		t.Fatal("fixture: the thin TOKEN/USDC book did not price the 4h row — the 'only the anchor' assertions would be vacuous")
	}
	if *quoteSide.usd == wantAnchored {
		t.Fatalf("fixture: the quote-side value %s coincides with the anchor value; pick amounts that differ", *quoteSide.usd)
	}
	exec(t, `UPDATE trades SET usd_volume = NULL WHERE source='sdex' AND ledger=$1`, ledger["declined, stored NULL"])

	const gen = int64(1_756_400_000)
	params := func(fillNull bool) timescale.XLMBaseRestampParams {
		return timescale.XLMBaseRestampParams{
			From: day, To: day.AddDate(0, 0, 1),
			FillNull: fillNull, MaxGeneration: gen,
		}
	}

	// ── 1+2. plan (the DRY RUN) and prove it wrote nothing ───────────
	before := snapshot(t)
	plan, err := store.PlanXLMBaseUSDVolumeRestamp(ctx, params(false))
	if err != nil {
		t.Fatalf("PlanXLMBaseUSDVolumeRestamp: %v", err)
	}
	sameSnapshot(t, before, snapshot(t), "dry run")

	if len(plan.Rows) != 1 {
		t.Fatalf("dry run planned %d row(s), want exactly 1 (the quote-side row); stats %+v", len(plan.Rows), plan.Stats)
	}
	if plan.Rows[0].Ledger != ledger["quote-side wrong"] {
		t.Fatalf("dry run planned ledger %d, want %d", plan.Rows[0].Ledger, ledger["quote-side wrong"])
	}
	if plan.Rows[0].Want != wantAnchored {
		t.Fatalf("planned value = %q, want %q (10 XLM x $0.50 via the anchor)", plan.Rows[0].Want, wantAnchored)
	}
	// 4. the two declined rows are counted, not written.
	if plan.Stats.AnchorDeclinedNull != 1 || plan.Stats.AnchorDeclinedStored != 1 {
		t.Errorf("declined split = %d null / %d valued, want 1/1 (stats %+v)",
			plan.Stats.AnchorDeclinedNull, plan.Stats.AnchorDeclinedStored, plan.Stats)
	}
	// 5. the NULL row is counted but not admitted.
	if plan.Stats.NullCandidates != 1 || plan.Stats.NullFilled != 0 {
		t.Errorf("null accounting = %d candidates / %d filled, want 1/0", plan.Stats.NullCandidates, plan.Stats.NullFilled)
	}
	// 6. the higher-generation row is out of scope entirely.
	for _, r := range plan.Rows {
		if r.Ledger == ledger["higher generation"] {
			t.Error("the higher-generation row entered the plan — INV-3 says a newer re-derive is never clawed back")
		}
	}
	// 8. the exact-tier anchor trade (native/USDC) is classified out.
	if plan.Stats.QuotePegged != 1 {
		t.Errorf("QuotePegged = %d, want 1 (the native/USDC anchor row belongs to `-tier exact`)", plan.Stats.QuotePegged)
	}
	if plan.Stats.Unchanged != 1 {
		t.Errorf("Unchanged = %d, want 1 (the already-correct row)", plan.Stats.Unchanged)
	}
	if got := plan.Stats.Residual(); got != 0 {
		t.Errorf("Residual() = %d, want 0 — a scanned row was filed nowhere (stats %+v)", got, plan.Stats)
	}

	// ── 3. -write writes EXACTLY the planned rows ────────────────────
	before = snapshot(t)
	n, err := store.ApplyXLMBaseUSDVolumeRestamp(ctx, plan, gen, 0)
	if err != nil {
		t.Fatalf("ApplyXLMBaseUSDVolumeRestamp: %v", err)
	}
	if n != int64(len(plan.Rows)) {
		t.Fatalf("applied %d row(s), want %d (the plan)", n, len(plan.Rows))
	}
	after := snapshot(t)
	fixed := after[ledger["quote-side wrong"]]
	if fixed.usd == nil || *fixed.usd != wantAnchored {
		t.Errorf("restamped row = %v, want %s", fixed.usd, wantAnchored)
	}
	if fixed.gen != gen {
		t.Errorf("restamped row derive_generation = %d, want the run's %d", fixed.gen, gen)
	}
	// The plan path logs the same before-image as the exact tier.
	prior := before[ledger["quote-side wrong"]]
	logged := readRestampLog(t, ctx, store.DB(), "sdex", ledger["quote-side wrong"])
	if len(logged) != 1 || !sameNumeric(logged[0].prior, prior.usd) || logged[0].priorGen != prior.gen ||
		logged[0].written != wantAnchored || logged[0].gen != gen {
		t.Errorf("before-image of the restamped row = %+v, want prior %v@%d and written %s@%d",
			logged, prior.usd, prior.gen, wantAnchored, gen)
	}
	if got := restampLogCount(t, ctx, store.DB(), gen); got != n {
		t.Errorf("run logged %d before-image(s) but rewrote %d row(s)", got, n)
	}
	// Everything the plan did not name is byte-identical.
	delete(before, ledger["quote-side wrong"])
	delete(after, ledger["quote-side wrong"])
	sameSnapshot(t, before, after, "write run (unplanned rows)")

	// 4 (again), now against the written state: the declined rows kept
	// exactly what they held.
	if got := readRow(t, ledger["declined, stored NULL"]); got.usd != nil {
		t.Errorf("a row the anchor cannot price was given the value %s — it must stay NULL", *got.usd)
	}
	if got := readRow(t, ledger["declined, stored value"]); got.usd == nil {
		t.Error("a row the anchor cannot price was BLANKED — the restamp must never write NULL over a value")
	} else if *got.usd != *quoteSide.usd {
		t.Errorf("a row the anchor cannot price moved from %s to %s — it must be reported, not re-valued", *quoteSide.usd, *got.usd)
	}

	// ── 7. idempotent: an immediate re-run plans nothing ─────────────
	replan, err := store.PlanXLMBaseUSDVolumeRestamp(ctx, params(false))
	if err != nil {
		t.Fatalf("re-plan: %v", err)
	}
	if len(replan.Rows) != 0 {
		t.Errorf("re-run planned %d row(s), want 0 — the restamp is not idempotent", len(replan.Rows))
	}
	if replan.Stats.Unchanged != 2 {
		t.Errorf("re-run Unchanged = %d, want 2 (the original correct row plus the one just fixed)", replan.Stats.Unchanged)
	}

	// ── 5. -fill-null admits the NULL population ─────────────────────
	fillPlan, err := store.PlanXLMBaseUSDVolumeRestamp(ctx, params(true))
	if err != nil {
		t.Fatalf("plan with FillNull: %v", err)
	}
	if len(fillPlan.Rows) != 1 || fillPlan.Rows[0].Ledger != ledger["stored NULL"] || !fillPlan.Rows[0].NullFill {
		t.Fatalf("FillNull plan = %+v, want exactly the stored-NULL row", fillPlan.Rows)
	}
	if fillPlan.Rows[0].Want != wantAnchored {
		t.Errorf("NULL fill value = %q, want %q", fillPlan.Rows[0].Want, wantAnchored)
	}
	if _, err := store.ApplyXLMBaseUSDVolumeRestamp(ctx, fillPlan, gen, 0); err != nil {
		t.Fatalf("apply FillNull: %v", err)
	}
	if got := readRow(t, ledger["stored NULL"]); got.usd == nil || *got.usd != wantAnchored {
		t.Errorf("filled row = %v, want %s", got.usd, wantAnchored)
	}

	// ── 6. the higher-generation row never moved ─────────────────────
	if got := readRow(t, ledger["higher generation"]); got.gen != higherGen || got.usd == nil || *got.usd != "0.11111111" {
		t.Errorf("higher-generation row = %+v (usd %v), want it untouched at generation %d", got, got.usd, higherGen)
	}

	// ── the live-overlap guard's input: the window's top ledger ──────
	top, ok, err := store.MaxTradeLedgerInRange(ctx, day, day.AddDate(0, 0, 1))
	if err != nil || !ok {
		t.Fatalf("MaxTradeLedgerInRange = %d, %v, %v", top, ok, err)
	}
	if want := ledger["declined, stored value"]; top != want {
		t.Errorf("window top ledger = %d, want %d (the highest on-chain ledger in the window)", top, want)
	}
	if _, ok, err := store.MaxTradeLedgerInRange(ctx, day.AddDate(0, 0, 10), day.AddDate(0, 0, 11)); err != nil || ok {
		t.Errorf("an empty window reported a top ledger (ok=%v, err=%v)", ok, err)
	}
}

// TestXLMBaseRestamp_RefusesWithoutResolution is the configuration
// fail-closed: a re-derive whose resolver was never installed would
// report every row as "the anchor cannot price this", which is a wiring
// error wearing a finding's clothes — and, if it ever gained a write
// path, the A-CRIT-1 shape that overwrites correct values with NULL at a
// winning generation.
func TestXLMBaseRestamp_RefusesWithoutResolution(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	day := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	_, err = store.PlanXLMBaseUSDVolumeRestamp(ctx, timescale.XLMBaseRestampParams{
		From: day, To: day.AddDate(0, 0, 1), MaxGeneration: 1,
	})
	if err == nil {
		t.Fatal("planning without an installed FX resolver succeeded; want a refusal")
	}
	if want := "no USD-volume FX resolver installed"; !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %v, want containing %q", err, want)
	}
}

// TestXLMBaseRestamp_SkipsARowMovedAfterThePlan: the apply writes a planned
// row only while it still holds the amounts, usd_volume and generation the
// plan derived from. A row another writer moved in between keeps that
// writer's state, and the shortfall is visible as planned-vs-changed.
func TestXLMBaseRestamp_SkipsARowMovedAfterThePlan(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const usdcID = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	usdc, err := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	token, err := c.NewSorobanAsset("CAFJZQWSED6YAWZU3GWRTOCNPPCGBN32L7QV43XX5LZLFTK6JLN34DLN")
	if err != nil {
		t.Fatal(err)
	}
	xlmUSDC, err := c.NewPair(c.NativeAsset(), usdc)
	if err != nil {
		t.Fatal(err)
	}
	xlmToken, err := c.NewPair(c.NativeAsset(), token)
	if err != nil {
		t.Fatal(err)
	}
	if err := timescale.InstallUSDVolumeResolution(store, []string{usdcID}, nil); err != nil {
		t.Fatalf("InstallUSDVolumeResolution: %v", err)
	}

	day := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	anchorTS := day.Add(10 * time.Hour)
	if err := store.InsertTrade(ctx, mkIntegrationTrade("sdex", 1, anchorTS, xlmUSDC, 1_000_000_000, 500_000_000)); err != nil {
		t.Fatalf("InsertTrade anchor: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.DB().ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}
	readUSD := func(l uint32) string {
		t.Helper()
		var v sql.NullString
		if err := store.DB().QueryRowContext(ctx,
			`SELECT usd_volume::text FROM trades WHERE source = 'sdex' AND ledger = $1`, l).Scan(&v); err != nil {
			t.Fatalf("read ledger %d: %v", l, err)
		}
		return v.String
	}

	// Three rows the anchor values at $5.00, each stored with a wrong value.
	names := []string{"untouched", "amount moved", "value moved"}
	ledger := map[string]uint32{}
	for i, name := range names {
		tr := mkIntegrationTrade("sdex", 10+i, anchorTS.Add(time.Duration(5+i)*time.Minute), xlmToken, 100_000_000, 300)
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade %s: %v", name, err)
		}
		ledger[name] = tr.Ledger
		exec(`UPDATE trades SET usd_volume = 0.00372265 WHERE source = 'sdex' AND ledger = $1`, tr.Ledger)
	}

	const gen = int64(1_756_400_000)
	plan, err := store.PlanXLMBaseUSDVolumeRestamp(ctx, timescale.XLMBaseRestampParams{
		From: day, To: day.AddDate(0, 0, 1), MaxGeneration: gen,
	})
	if err != nil {
		t.Fatalf("PlanXLMBaseUSDVolumeRestamp: %v", err)
	}
	if len(plan.Rows) != 3 || plan.Stats.Changed != 3 {
		t.Fatalf("planned %d row(s) (Changed %d), want 3; stats %+v", len(plan.Rows), plan.Stats.Changed, plan.Stats)
	}

	// Another writer, between the plan and the apply, still below gen.
	exec(`UPDATE trades SET base_amount = 200000000 WHERE source = 'sdex' AND ledger = $1`, ledger["amount moved"])
	exec(`UPDATE trades SET usd_volume = 7.25 WHERE source = 'sdex' AND ledger = $1`, ledger["value moved"])

	n, err := store.ApplyXLMBaseUSDVolumeRestamp(ctx, plan, gen, 0)
	if err != nil {
		t.Fatalf("ApplyXLMBaseUSDVolumeRestamp: %v", err)
	}
	if n != 1 {
		t.Fatalf("applied %d row(s), want 1 — only the row still in its planned state (planned %d)", n, plan.Stats.Changed)
	}
	if got := readUSD(ledger["untouched"]); got != "5.00000000" {
		t.Errorf("untouched row = %s, want the anchor value 5.00000000", got)
	}
	if got := readUSD(ledger["amount moved"]); got != "0.00372265" {
		t.Errorf("row whose amount moved after the plan = %s, want it left at 0.00372265 — the planned value was derived from the old amount", got)
	}
	if got := readUSD(ledger["value moved"]); got != "7.25" {
		t.Errorf("row another writer revalued after the plan = %s, want that writer's 7.25 kept", got)
	}
	if got := restampLogCount(t, ctx, store.DB(), gen); got != n {
		t.Errorf("run logged %d before-image(s) but rewrote %d row(s)", got, n)
	}
}

// TestXLMLegVolume_TradeTimeNeverSpot pins the volume readers that must not
// value an unpriced XLM leg at today's XLM/USD: the per-source breakdowns
// exclude it and count it, and the MEV scan values it at the anchor of the
// trade's own minute, or not at all when that anchor is over an hour old.
// XLM trades at 0.2 (3h ago), 0.4 (30m ago) and 0.5 (5m ago); spot is 0.5.
func TestXLMLegVolume_TradeTimeNeverSpot(t *testing.T) {
	f := newAnchorFixture(t)
	const unit = 10_000_000
	usdc, err := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	native := c.NativeAsset()
	f.trade("sdex", 3*time.Hour, native, usdc, 10*unit, 2*unit, "2")
	f.trade("sdex", 30*time.Minute, native, usdc, 10*unit, 4*unit, "4")
	f.trade("sdex", 5*time.Minute, native, usdc, 10*unit, 5*unit, "5")
	f.trade("sdex", 10*time.Minute, native, usdc, 10*unit, 5*unit, "")
	X := f.contract("X")
	f.trade("sdex", 29*time.Minute, native, X, 10*unit, 1*unit, "")
	fresh := uint32(50_000_000 + f.nonce)
	f.trade("sdex", 110*time.Minute, X, native, 1*unit, 10*unit, "")
	stale := uint32(50_000_000 + f.nonce)

	db := f.store.DB()
	for ledger, usd := range f.usd {
		if _, err := db.ExecContext(f.ctx, `UPDATE trades SET usd_volume = $1::numeric WHERE ledger = $2`, usd, ledger); err != nil {
			t.Fatalf("stamp usd_volume: %v", err)
		}
	}
	for _, q := range []string{
		`UPDATE trades SET taker = 'GTAKER'`,
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`,
		`CALL refresh_continuous_aggregate('pools_per_source_1h', NULL, NULL)`,
	} {
		if _, err := db.ExecContext(f.ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	xlm := c.AssetAliasStrings(native)
	pair, err := f.store.PairSourceStats(f.ctx, xlm, []string{usdc.String()})
	if err != nil {
		t.Fatalf("PairSourceStats: %v", err)
	}
	if len(pair) != 1 || !ratEq(t, pair[0].VolumeUSD24h.String, "11") || pair[0].UnpricedTrades24h != 1 {
		t.Errorf("PairSourceStats = %+v, want sdex volume 11 (trade-time only) with 1 unpriced trade", pair)
	}
	asset, err := f.store.AssetSourceStats(f.ctx, xlm)
	if err != nil {
		t.Fatalf("AssetSourceStats: %v", err)
	}
	if len(asset) != 1 || !ratEq(t, asset[0].VolumeUSD24h.String, "11") || asset[0].UnpricedTrades24h != 3 {
		t.Errorf("AssetSourceStats = %+v, want sdex volume 11 (trade-time only) with 3 unpriced trades", asset)
	}

	// Spot would read 10 (20 XLM at 0.5); trade time is 10 XLM at 0.4, and the
	// stale leg is excluded.
	soroban, lowerBound, err := f.store.SorobanVolume24hUSDForAsset(f.ctx, X.String())
	if err != nil {
		t.Fatalf("SorobanVolume24hUSDForAsset: %v", err)
	}
	if !ratEq(t, soroban, "4") || !lowerBound {
		t.Errorf("SorobanVolume24hUSDForAsset(X) = %s lowerBound=%v, want 4 at trade time, flagged a lower bound", soroban, lowerBound)
	}

	pools, _, err := f.store.AllPools(f.ctx, timescale.PoolsFilter{Sources: []string{"sdex"}}, "", 100, timescale.MarketsOrderVolume24hDesc)
	if err != nil {
		t.Fatalf("AllPools: %v", err)
	}
	markets, _, err := f.store.SourceMarkets(f.ctx, "sdex", "", 100, timescale.MarketsOrderVolume24hDesc)
	if err != nil {
		t.Fatalf("SourceMarkets: %v", err)
	}
	for _, p := range pools {
		if p.Pair.Base.String() == "native" && p.Pair.Quote == usdc &&
			(p.Volume24hUSD == nil || !ratEq(t, *p.Volume24hUSD, "11") || !p.VolumeLowerBound) {
			t.Errorf("AllPools sdex XLM/USDC = %+v, want 11 (trade-time only, spot would add 5) flagged a lower bound", p)
		}
	}
	for _, m := range markets {
		if m.Pair.Base.String() == "native" && m.Pair.Quote == usdc &&
			(m.Volume24hUSD == nil || !ratEq(t, *m.Volume24hUSD, "11") || !m.VolumeLowerBound) {
			t.Errorf("SourceMarkets sdex XLM/USDC = %+v, want 11 flagged a lower bound", m)
		}
	}
	if len(pools) == 0 || len(markets) == 0 {
		t.Errorf("AllPools/SourceMarkets(sdex) returned %d/%d rows, want both non-empty", len(pools), len(markets))
	}

	trades, usd, err := f.store.TradesForArbScan(f.ctx, f.now.Add(-4*time.Hour), 0)
	if err != nil {
		t.Fatalf("TradesForArbScan: %v", err)
	}
	got := map[uint32]string{}
	for i, tr := range trades {
		got[tr.Ledger] = usd[i]
	}
	if v, ok := got[fresh]; !ok || !ratEq(t, v, "4") {
		t.Errorf("fresh XLM leg notional = %q (present=%v), want 4: 10 XLM at its own minute's 0.4, not spot 0.5", v, ok)
	}
	if v, ok := got[stale]; !ok || v != "" {
		t.Errorf("stale XLM leg notional = %q (present=%v), want \"\": its newest anchor is 70 min before the trade", v, ok)
	}
}
