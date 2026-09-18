//go:build integration

package integration_test

import (
	"context"
	"strings"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestBespokeDEX24hVolumeCarriesXLMLeg proves the DEX block's 24h USD
// figures apply migration 0068's WHOLE read contract against a real
// TimescaleDB.
//
// source_volume_1h cannot materialize a finished USD figure (the XLM/USD
// multiply cross-references prices_1m), so it materializes the inputs and
// the reader must apply
//
//	sum_usd_priced + (sum_xlm_base + sum_xlm_quote)/10^7 * <XLM/USD vwap>
//
// Reading only sum_usd_priced serves every XLM-denominated leg the
// ingest-time valuation left unpriced as $0 — and disagrees with the
// other reader of the same CAGG (GetSourceVolumeHistory24h), which the
// SAME source page renders beside this block.
//
// Fixture (one closed minute ~2h back, all on soroswap):
//
//	XLM/USDC   100 XLM for 50 USDC → vwap 0.5, usd_volume = 50  (priced)
//	token/USDC 7 USDC quote                    usd_volume =  7  (priced)
//	token/XLM  20 XLM on the QUOTE side        usd_volume NULL → sum_xlm_quote
//	XLM/token  10 XLM on the BASE side         usd_volume NULL → sum_xlm_base
//
// priced             = 50 + 7                       = 57
// XLM legs           = (20 + 10) XLM * 0.5 USD/XLM  = 15
// 24h USD volume     =                               72
//
// The pre-fix reader returns 57.00 here; the contract's value is 72.00.
func TestBespokeDEX24hVolumeCarriesXLMLeg(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Classic USDC is the USD peg, so the two USDC-quoted legs land a
	// non-null usd_volume and the XLM legs stay NULL (the CAGG's
	// sum_xlm_base/sum_xlm_quote filters are `usd_volume IS NULL`).
	spec, err := timescale.NewUSDVolumeQuoteSpec(
		[]string{"USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"}, nil)
	if err != nil {
		t.Fatalf("spec: %v", err)
	}
	store.SetUSDVolumeQuoteSpec(spec)

	xlm := c.NativeAsset()
	usdc, err := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	token, err := c.NewSorobanAsset("CAFJZQWSED6YAWZU3GWRTOCNPPCGBN32L7QV43XX5LZLFTK6JLN34DLN")
	if err != nil {
		t.Fatal(err)
	}
	xlmUSDC, _ := c.NewPair(xlm, usdc)
	tokenUSDC, _ := c.NewPair(token, usdc)
	tokenXLM, _ := c.NewPair(token, xlm)
	xlmToken, _ := c.NewPair(xlm, token)

	ts := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Minute)
	for _, tr := range []c.Trade{
		mkIntegrationTrade("soroswap", 1, ts, xlmUSDC, 1_000_000_000, 500_000_000),
		mkIntegrationTrade("soroswap", 2, ts, tokenUSDC, 900, 70_000_000),
		mkIntegrationTrade("soroswap", 3, ts, tokenXLM, 500, 200_000_000),
		mkIntegrationTrade("soroswap", 4, ts, xlmToken, 100_000_000, 300),
	} {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade %d: %v", tr.Ledger, err)
		}
	}

	// prices_1m carries the XLM/USD vwap the read-time multiply anchors
	// on; source_volume_1h carries the pre-aggregated inputs.
	for _, cagg := range []string{"prices_1m", "source_volume_1h"} {
		if _, err := store.DB().ExecContext(ctx,
			`CALL refresh_continuous_aggregate($1, NULL, NULL)`, cagg); err != nil {
			// CALL does not accept a bind param for the view name on
			// every version — fall back to the literal form.
			if _, err2 := store.DB().ExecContext(ctx,
				`CALL refresh_continuous_aggregate('`+cagg+`', NULL, NULL)`); err2 != nil {
				t.Fatalf("refresh %s: %v / %v", cagg, err, err2)
			}
		}
	}

	// Sanity: the fixture really does park volume in the XLM legs — if it
	// did not, the assertion below would pass on the unfixed reader too.
	var priced, xlmBase, xlmQuote string
	if err := store.DB().QueryRowContext(ctx, `
		SELECT COALESCE(sum(sum_usd_priced),0)::text,
		       COALESCE(sum(sum_xlm_base),0)::text,
		       COALESCE(sum(sum_xlm_quote),0)::text
		  FROM source_volume_1h
		 WHERE source = 'soroswap' AND bucket > now() - INTERVAL '1 day'`,
	).Scan(&priced, &xlmBase, &xlmQuote); err != nil {
		t.Fatalf("read CAGG inputs: %v", err)
	}
	if mustFloat(t, priced) < 56.99 || mustFloat(t, priced) > 57.01 {
		t.Fatalf("fixture: sum_usd_priced = %s, want 57", priced)
	}
	if mustFloat(t, xlmBase) != 100_000_000 || mustFloat(t, xlmQuote) != 200_000_000 {
		t.Fatalf("fixture: XLM legs = base %s / quote %s, want 100000000 / 200000000", xlmBase, xlmQuote)
	}

	blk, err := store.BuildProtocolBespoke(ctx, "soroswap", "dex", 1)
	if err != nil {
		t.Fatalf("BuildProtocolBespoke: %v", err)
	}
	if blk == nil {
		t.Fatal("BuildProtocolBespoke returned no block for a source with 4 trades in the window")
	}

	var volKPI *timescale.BespokeKPI
	for i := range blk.KPIs {
		if blk.KPIs[i].Label == "USD volume (1d)" {
			volKPI = &blk.KPIs[i]
		}
	}
	if volKPI == nil {
		t.Fatal(`no "USD volume (1d)" KPI on the 24h DEX block`)
	}
	// The corrected VALUE: 57 priced + 15 XLM-anchored. The unfixed
	// reader returns "57.00" here.
	if volKPI.Value != "72.00" {
		t.Errorf("24h USD volume KPI = %s, want 72.00 (57 priced + 30 XLM at 0.5)", volKPI.Value)
	}
	if !strings.Contains(volKPI.Hint, "XLM") {
		t.Errorf("24h USD volume KPI hint must disclose the XLM-anchored leg, got %q", volKPI.Hint)
	}

	// The hourly series carries the same derivation (all four trades sit
	// in one hour bucket, so its single point is the whole window).
	var series *timescale.BespokeSeries
	for i := range blk.Series {
		if blk.Series[i].Name == "USD volume" {
			series = &blk.Series[i]
		}
	}
	if series == nil || len(series.Points) == 0 {
		t.Fatal("no hourly USD volume series on the 24h DEX block")
	}
	var total float64
	for _, p := range series.Points {
		total += mustFloat(t, p.Value)
	}
	if total < 71.99 || total > 72.01 {
		t.Errorf("hourly USD volume series totals %.4f, want ~72 (57 priced + 15 XLM-anchored)", total)
	}

	// The block's served note must describe what it actually serves: the
	// old note promised "never ad-hoc pricing", which the read-time XLM
	// multiply is.
	joined := strings.Join(blk.Notes, "\n")
	if !strings.Contains(joined, "XLM-denominated legs") {
		t.Errorf("24h block note must disclose the XLM-anchored leg, got %q", joined)
	}
	if strings.Contains(joined, "never ad-hoc pricing") {
		t.Errorf("24h block note must not claim there is no ad-hoc pricing, got %q", joined)
	}

	// Cross-surface parity — the whole point of the finding: the other
	// reader of this CAGG (the source page's own 24h chart, /v1/sources)
	// must report the SAME 24h volume for the same source.
	hist, err := store.GetSourceVolumeHistory24h(ctx)
	if err != nil {
		t.Fatalf("GetSourceVolumeHistory24h: %v", err)
	}
	var histTotal float64
	for _, b := range hist {
		if b.Source == "soroswap" {
			histTotal += mustFloat(t, b.VolumeUSD)
		}
	}
	if histTotal < 71.99 || histTotal > 72.01 {
		t.Fatalf("fixture check: GetSourceVolumeHistory24h totals %.4f, want ~72", histTotal)
	}
	if diff := histTotal - total; diff > 0.01 || diff < -0.01 {
		t.Errorf("the two readers of source_volume_1h disagree on soroswap's 24h volume: bespoke block %.4f vs source chart %.4f", total, histTotal)
	}
}
