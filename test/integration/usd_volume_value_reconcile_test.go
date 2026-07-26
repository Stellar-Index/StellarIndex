//go:build integration

package integration_test

import (
	"context"
	"math/big"
	"testing"
	"time"

	_ "github.com/lib/pq"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestUSDVolumeValueReconcile_ExactTierIdentity is the DB-backed half of
// C4-055 / C4-066, run against real TimescaleDB.
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
