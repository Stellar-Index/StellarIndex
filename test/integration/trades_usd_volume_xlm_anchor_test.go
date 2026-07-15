//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestInsertTrade_L76XLMBaseAnchorPopulatesUSDVolume proves ROADMAP
// #37 / L7.6 end-to-end: with the FX resolver wired (the r1
// production shape whenever `[trades].usd_pegged_classic_assets` is
// non-empty), a pure-Soroban SEP-41 trade stored as base=XLM,
// quote=TOKEN — the orientation [timescale.Store.Volume24hUSDForAsset]'s
// insert-time tier 3 can't cover, since TOKEN has no direct
// USD-pegged market — now lands a non-NULL `usd_volume` at INSERT
// time via the tier-4 XLM-base anchor, not just via the query-time
// [timescale.Store.SorobanVolume24hUSDForAsset] fallback.
//
// Fixture (one closed 1-minute bucket ~2h back):
//   - native/USDC  vwap 0.5           → the XLM→USD anchor (1 XLM = $0.50)
//   - XLM/token    10 XLM based       → tier-4 anchor: 10 * 0.5 = $5.00
//
// Because usd_volume is now populated AT INSERT, it propagates through
// prices_1m's `volume_usd` column — so even the PLAIN
// Volume24hUSDForAsset reader (which only ever summed the insert-time
// column) now reports the anchored figure, and the anchored
// SorobanVolume24hUSDForAsset reader's `volume_usd > 0` branch picks
// the same row without re-deriving it — no double count.
func TestInsertTrade_L76XLMBaseAnchorPopulatesUSDVolume(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	usdcIssuer := "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	usdc, err := c.NewClassicAsset("USDC", usdcIssuer)
	if err != nil {
		t.Fatal(err)
	}
	xlm := c.NativeAsset()
	token, err := c.NewSorobanAsset("CAFJZQWSED6YAWZU3GWRTOCNPPCGBN32L7QV43XX5LZLFTK6JLN34DLN")
	if err != nil {
		t.Fatal(err)
	}

	// Recognise classic USDC as a USD peg so the anchor trade's own
	// leg resolves, then wire the FX resolver on the same peg list —
	// the r1 production shape (cmd/stellarindex-indexer/main.go).
	spec, err := timescale.NewUSDVolumeQuoteSpec([]string{"USDC-" + usdcIssuer}, nil)
	if err != nil {
		t.Fatalf("NewUSDVolumeQuoteSpec: %v", err)
	}
	store.SetUSDVolumeQuoteSpec(spec)

	resolver, err := timescale.NewVWAPUSDFXResolver(store, timescale.VWAPUSDFXResolverOptions{
		USDPegs:   []string{"USDC-" + usdcIssuer},
		Freshness: -1, // disabled — deterministic against the 2h-old fixture
	})
	if err != nil {
		t.Fatalf("NewVWAPUSDFXResolver: %v", err)
	}
	store.SetUSDVolumeFXResolver(resolver)

	xlmUSDC, _ := c.NewPair(xlm, usdc)   // anchor: vwap = 0.5
	xlmToken, _ := c.NewPair(xlm, token) // tier-4 case: base=XLM, quote=pure SEP-41

	ts := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Minute)

	anchorTrade := mkIntegrationTrade("soroswap", 1, ts, xlmUSDC, 1_000_000_000, 500_000_000) // 100 XLM / 50 USDC → vwap 0.5
	xlmBaseTrade := mkIntegrationTrade("soroswap", 2, ts, xlmToken, 100_000_000, 300)         // 10 XLM based, 300 (arbitrary) token quote

	// Insert + refresh the anchor FIRST, and only then insert the
	// tier-4 trade: [timescale.VWAPUSDFXResolver] reads `prices_1m`
	// (the CAGG), not raw `trades`, so the XLM/USD anchor must
	// already be materialised by the time InsertTrade's synchronous
	// tier-4 lookup runs — matching production, where the anchor
	// leg's bucket was refreshed well before a brand-new trade
	// arrives.
	if err := store.InsertTrade(ctx, anchorTrade); err != nil {
		t.Fatalf("InsertTrade anchor: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1m (anchor): %v", err)
	}
	if err := store.InsertTrade(ctx, xlmBaseTrade); err != nil {
		t.Fatalf("InsertTrade xlmBaseTrade: %v", err)
	}

	// The tier-4 trade's usd_volume column is populated at INSERT
	// time — not NULL, unlike the pre-L7.6 behaviour
	// (test/integration/soroban_volume_test.go's un-resolved fixture).
	const q = `SELECT usd_volume FROM trades WHERE source = $1 AND ledger = $2`
	var v sql.NullString
	if err := store.DB().QueryRowContext(ctx, q, "soroswap", xlmBaseTrade.Ledger).Scan(&v); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !v.Valid {
		t.Fatal("usd_volume = NULL, want a populated value (tier-4 XLM-base anchor)")
	}
	if got := mustFloat(t, v.String); got < 4.99 || got > 5.01 {
		t.Errorf("usd_volume = %s (%.4f), want ~5.00 (10 XLM * $0.50)", v.String, got)
	}

	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}

	// Plain reader now ALSO sees the tier-4 leg — it sums whatever
	// landed in usd_volume, and this trade's column is no longer NULL.
	plain, err := store.Volume24hUSDForAsset(ctx, token.String())
	if err != nil {
		t.Fatalf("Volume24hUSDForAsset: %v", err)
	}
	if got := mustFloat(t, plain); got < 4.99 || got > 5.01 {
		t.Errorf("plain Volume24hUSDForAsset = %s (%.4f), want ~5.00", plain, got)
	}

	// Anchored reader's volume_usd>0 branch picks up the SAME row —
	// no double count against its own base_asset='native' CASE.
	anchored, err := store.SorobanVolume24hUSDForAsset(ctx, token.String())
	if err != nil {
		t.Fatalf("SorobanVolume24hUSDForAsset: %v", err)
	}
	if got := mustFloat(t, anchored); got < 4.99 || got > 5.01 {
		t.Errorf("SorobanVolume24hUSDForAsset = %s (%.4f), want ~5.00 (no double count)", anchored, got)
	}
}
