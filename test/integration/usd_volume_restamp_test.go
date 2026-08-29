//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"math/big"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestUSDVolumeRestamp_ExactTierRepair is the DB-backed proof for the W5.3
// `usd-volume-restamp` write path, on real TimescaleDB, against rows the
// REAL insert path wrote with the REAL peg configuration:
//
//  1. the SQL identity the tool evaluates (`round(leg / 10^d, 8)`) lands
//     the SAME value the Go formula [timescale.ExactTierUSDVolume] and
//     therefore [tradeUSDVolume] produce — no SQL reimplementation drift;
//  2. DIFFERENTIAL: a correctly-stamped row is untouched — value AND
//     derive_generation — so a re-run is a no-op (idempotent);
//  3. the dry-run count is the write's exact preview;
//  4. INV-3: a row at a HIGHER generation is never clawed back, and every
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
	// USDC/XLM — the dollar leg is the BASE: tier 2b, the exact class the
	// 2026-07-30 sweep found dirty on every one of its 66 days.
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

	// ── apply ──
	n, err := store.RestampExactTierUSDVolume(ctx, params(false))
	if err != nil {
		t.Fatalf("RestampExactTierUSDVolume: %v", err)
	}
	if n != 1 {
		t.Fatalf("restamped %d row(s), want 1", n)
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

	// 4b. INV-3 guard: a newer generation is never clawed back.
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
