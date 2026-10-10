//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// flakeyFXResolver implements timescale.USDVolumeFXResolver. It resolves the
// prices it knows unless `fail` is set, in which case it returns a transient
// error — the exact class (deadlock / statement-timeout / pool blip / Patroni
// failover) that VWAPUSDFXResolver.USDPriceAt surfaces and does NOT
// negative-cache, so each independent write is a fresh chance to miss.
type flakeyFXResolver struct {
	prices map[string]string
	fail   bool
}

func (r *flakeyFXResolver) USDPriceAt(_ context.Context, asset c.Asset, _ time.Time) (string, bool, error) {
	if r.fail {
		return "", false, errors.New("transient pg fault (deadlock/statement-timeout)")
	}
	p, ok := r.prices[asset.String()]
	if !ok {
		return "", false, nil
	}
	return p, true, nil
}

// TestUSDVolumeGenerationAwareNullPreservation is the proven-red regression:
// in production persist_per_source=true, a DEX trade
// is DOUBLE-WRITTEN at deriveGeneration=0 by both the dispatcher's
// BatchInsertTrades and the projector's InsertTrade on the same store. If one
// writer resolves a usd_volume and the other races the same PK while its FX
// resolver hits a transient fault (returning nil), an upsert
// (`usd_volume = EXCLUDED.usd_volume` gated only by
// `derive_generation <= EXCLUDED.derive_generation`, equal included) overwrote
// the populated value with NULL — permanently deflating the pair's volume.
//
// The fix is GENERATION-AWARE, not a blanket COALESCE: at the LIVE / equal
// generation a NULL incoming must NOT regress a populated value, but a
// strictly-HIGHER-generation re-derive is still allowed to write an honest
// NULL (store.go:147-155, the tier-3b de-poisoning case). This test pins both:
//
//   - EQUAL (gen-0) NULL must PRESERVE the stored value. FAILS on the unfixed
//     `usd_volume = EXCLUDED.usd_volume` (row goes NULL). To reproduce red:
//     revert the CASE in both writers to `usd_volume = EXCLUDED.usd_volume`.
//   - HIGHER-generation NULL must CLEAR the stored value. FAILS on the naive
//     unconditional `COALESCE(EXCLUDED.usd_volume, trades.usd_volume)` the
//     skeptic panel rejected (which would block the honest de-poisoning NULL).
func TestUSDVolumeGenerationAwareNullPreservation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// A fresh SEP-41 base with no market (never resolvable) and a quote
	// token the resolver prices — the tier-3 FX path, where a transient miss
	// yields a nil usd_volume for the racing writer.
	base, err := c.NewSorobanAsset("CAQQR5SWBXKIGZKPBZDH3KM5GQ5GUTPKB7JAFCINLZBC5WXPJKRG3IM7")
	if err != nil {
		t.Fatalf("base asset: %v", err)
	}
	quote, err := c.NewSorobanAsset("CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75")
	if err != nil {
		t.Fatalf("quote asset: %v", err)
	}
	pair, err := c.NewPair(base, quote)
	if err != nil {
		t.Fatalf("pair: %v", err)
	}
	ts := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

	const (
		source = "soroswap" // SubclassDEX
		nonce  = 42
	)
	ledger := int64(50_000_000 + nonce)

	// 2.5 quote units (25,000,000 stroops at 1e7) x $2.00 = $5.00 — a small,
	// below-ceiling single-leg print, so W1-flow-price-serve-1's bound does
	// not apply and the value populates.
	res := &flakeyFXResolver{prices: map[string]string{quote.String(): "2.00"}}
	store.SetUSDVolumeFXResolver(res)

	readUSD := func() sql.NullString {
		const q = `SELECT usd_volume::text FROM trades WHERE source = $1 AND ledger = $2`
		var uv sql.NullString
		if err := store.DB().QueryRowContext(ctx, q, source, ledger).Scan(&uv); err != nil {
			t.Fatalf("read usd_volume: %v", err)
		}
		return uv
	}
	isFive := func(uv sql.NullString) bool {
		return uv.Valid && (uv.String == "5.00000000" || uv.String == "5")
	}

	// Writer A, gen 0: resolves the quote leg → usd_volume = $5.00 lands.
	trA := mkIntegrationTrade(source, nonce, ts, pair, 1_000_000_000, 25_000_000)
	if err := store.InsertTrade(ctx, trA); err != nil {
		t.Fatalf("InsertTrade (writer A): %v", err)
	}
	if uv := readUSD(); !isFive(uv) {
		t.Fatalf("writer A: usd_volume = %v, want $5.00 populated", uv)
	}

	// Writer B, gen 0: same PK, but its resolver hits a transient fault →
	// computes NULL. At EQUAL generation the fix must PRESERVE $5.00.
	// Overwriting with `usd_volume = EXCLUDED.usd_volume` would null this row.
	res.fail = true
	trB := mkIntegrationTrade(source, nonce, ts, pair, 1_000_000_000, 25_000_000)
	if err := store.InsertTrade(ctx, trB); err != nil {
		t.Fatalf("InsertTrade (writer B, transient miss): %v", err)
	}
	if uv := readUSD(); !isFive(uv) {
		t.Fatalf("EQUAL-generation NULL must NOT regress a populated value: "+
			"usd_volume = %v, want $5.00 preserved (W1-flowtradeingest-1)", uv)
	}

	// Higher-generation honest NULL (the tier-3b de-poisoning case): a
	// properly-wired re-derive at gen 5 that legitimately computes NULL must
	// be allowed to CLEAR the value. The reDeriveNullVolumeGuard requires
	// resolution to be installed for a gen>0 NULL write; install it (no-pegs
	// no-op is enough to flip the installed flag).
	if err := timescale.InstallUSDVolumeResolution(store, nil, nil); err != nil {
		t.Fatalf("InstallUSDVolumeResolution: %v", err)
	}
	store.SetDeriveGeneration(5)
	// Honest NULL = the re-derive legitimately finds no price (not-found,
	// nil error). A resolver ERROR is refused in re-derive mode instead.
	res.fail = false
	delete(res.prices, quote.String())
	trC := mkIntegrationTrade(source, nonce, ts, pair, 1_000_000_000, 25_000_000)
	if err := store.InsertTrade(ctx, trC); err != nil {
		t.Fatalf("InsertTrade (writer C, gen-5 honest NULL): %v", err)
	}
	if uv := readUSD(); uv.Valid {
		t.Fatalf("HIGHER-generation re-derive must be able to write an honest NULL "+
			"(de-poisoning): usd_volume = %q, want NULL — a blanket COALESCE would "+
			"wrongly preserve it", uv.String)
	}
}

// TestInsertTrade_PopulatesUSDVolume proves the L2.2 caveat fix
// shipped end-to-end: a binance + fiat:USD trade lands with a
// non-NULL `usd_volume` column matching the expected sum/1e8
// conversion, and an on-chain trade lands with `usd_volume IS NULL`.
func TestInsertTrade_PopulatesUSDVolume(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	usd, err := c.NewFiatAsset("USD")
	if err != nil {
		t.Fatal(err)
	}
	xlm, err := c.NewCryptoAsset("XLM")
	if err != nil {
		t.Fatal(err)
	}
	xlmUSD, _ := c.NewPair(xlm, usd)
	xlmUSDC, _ := c.NewPair(xlm,
		func() c.Asset {
			a, _ := c.NewCryptoAsset("USDC")
			return a
		}())

	// Anchor in the past for deterministic queries.
	ts := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)

	// Binance + fiat:USD: usd_volume = 12_000_000 / 1e8 = 0.12.
	binTrade := mkIntegrationTrade("binance", 1, ts, xlmUSD, 100_000_000, 12_000_000)
	// Soroswap + USDC (on-chain DEX): out of scope → usd_volume NULL.
	swapTrade := mkIntegrationTrade("soroswap", 2, ts, xlmUSDC, 100_000_000, 12_000_000)

	for _, tr := range []c.Trade{binTrade, swapTrade} {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade %s: %v", tr.Source, err)
		}
	}

	const q = `SELECT usd_volume FROM trades WHERE source = $1 AND ledger = $2`

	t.Run("binance + fiat:USD: usd_volume populated", func(t *testing.T) {
		var v sql.NullString
		if err := store.DB().QueryRowContext(ctx, q, "binance", binTrade.Ledger).Scan(&v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if !v.Valid {
			t.Fatal("usd_volume = NULL, want a populated value")
		}
		// FloatString(8) on 12_000_000/1e8 → "0.12000000"; Postgres
		// NUMERIC may render as "0.12000000" or trim trailing zeros
		// depending on the column scale — accept either form.
		if v.String != "0.12000000" && v.String != "0.12" {
			t.Errorf("usd_volume = %q, want 0.12 or 0.12000000", v.String)
		}
	})

	t.Run("soroswap + USDC (on-chain): usd_volume NULL", func(t *testing.T) {
		var v sql.NullString
		if err := store.DB().QueryRowContext(ctx, q, "soroswap", swapTrade.Ledger).Scan(&v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if v.Valid {
			t.Errorf("usd_volume = %q, want NULL (on-chain source out of scope)", v.String)
		}
	})
}

// TestInsertTrade_L76XLMBaseAnchorPopulatesUSDVolume proves the XLM-base
// anchor end-to-end: with the FX resolver wired (the r1
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
// SorobanVolume24hUSDForAsset reader's per-trade COALESCE(usd_volume, …) takes
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
	plain, _, err := store.Volume24hUSDForAsset(ctx, token.String())
	if err != nil {
		t.Fatalf("Volume24hUSDForAsset: %v", err)
	}
	if got := mustFloat(t, plain); got < 4.99 || got > 5.01 {
		t.Errorf("plain Volume24hUSDForAsset = %s (%.4f), want ~5.00", plain, got)
	}

	// Anchored reader takes the stored usd_volume for the SAME row —
	// no double count against its own base_asset='native' CASE.
	anchored, _, err := store.SorobanVolume24hUSDForAsset(ctx, token.String())
	if err != nil {
		t.Fatalf("SorobanVolume24hUSDForAsset: %v", err)
	}
	if got := mustFloat(t, anchored); got < 4.99 || got > 5.01 {
		t.Errorf("SorobanVolume24hUSDForAsset = %s (%.4f), want ~5.00 (no double count)", anchored, got)
	}
}
