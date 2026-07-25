//go:build integration

package integration_test

import (
	"context"
	"strings"
	"testing"
	"time"

	_ "github.com/lib/pq"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestVWAPUSDFXResolver_QueriesPrices1m exercises the F-1268
// production path against a real postgres: seed an EURC/USDC
// trade, refresh prices_1m, then call USDPriceAt(EURC, now+1m) and
// verify the resolver picks up the VWAP through the USDC peg.
func TestVWAPUSDFXResolver_QueriesPrices1m(t *testing.T) {
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
	eurcIssuer := "GDHU6WRG4IEQXM5NZ4BMPKOXHW76MZM4Y2IEMFDVXBSDP6SJY4ITNPP2"
	usdc, _ := c.NewClassicAsset("USDC", usdcIssuer)
	eurc, _ := c.NewClassicAsset("EURC", eurcIssuer)
	pair, _ := c.NewPair(eurc, usdc)

	// Anchor 2h ago so the trade lands inside the prices_1m window
	// the CAGG materialises by default. Single trade is enough —
	// the resolver only needs one VWAP row.
	t0 := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Minute)
	trade := mkIntegrationTrade("sdex", 1, t0,
		pair,
		1_000_000_000, // 100 EURC at 7-decimals
		1_085_000_000) // 108.5 USDC at 7-decimals → 1.085 EUR/USD
	if err := store.InsertTrade(ctx, trade); err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`,
	); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}

	// Resolver with USDC's classic asset key on the peg list.
	resolver, err := timescale.NewVWAPUSDFXResolver(store, timescale.VWAPUSDFXResolverOptions{
		USDPegs: []string{"USDC-" + usdcIssuer},
		// F-1251 (codex audit-2026-05-12): -1 = freshness check
		// disabled. The previous `0` form was silently overridden
		// to the 1h default by the constructor, which still happened
		// to pass for this test (1m gap < 1h) but would fail any
		// historical-replay test where the trade was older than 1h.
		Freshness: -1,
	})
	if err != nil {
		t.Fatalf("NewVWAPUSDFXResolver: %v", err)
	}

	// Query at the bucket-end timestamp. The resolver should hit
	// the seeded row.
	got, ok, err := resolver.USDPriceAt(ctx, eurc, t0.Add(time.Minute))
	if err != nil {
		t.Fatalf("USDPriceAt: %v", err)
	}
	if !ok {
		t.Fatalf("expected resolver to find EURC/USDC VWAP, got ok=false")
	}
	// VWAP = quote/base = 1.085. NUMERIC is exact; CAGG-rendered
	// text strips trailing zeros but preserves precision.
	if got != "1.085" {
		t.Errorf("USDPriceAt = %q, want %q", got, "1.085")
	}
}

// TestVWAPUSDFXResolver_DustBucketDoesNotOutrankRealBucket is the
// Postgres-semantics proof for the tier-3a dust floor (MNY-22, re-fixed
// after the circular first attempt was reverted at 7b69cd33).
//
// queryDB takes the FRESHEST qualifying bucket, so a single sub-cent
// fill landing in a newer minute than the real market sets the USD
// valuation rate for every trade quoted in that asset for the whole
// freshness window. Seeded here at production shape: a real 108.50-USDC
// bucket, then five minutes later a 2-stroop/40000-stroop crumb whose
// "price" is 20000 USDC per EURC — an artifact of dividing two tiny
// integers, worth $0.004.
//
// Pre-fix this returns "20000". Post-fix the crumb is excluded and the
// real bucket's 1.085 is served.
//
// The dust bucket is deliberately made of trades that DO carry
// usd_volume semantics identical to the real one (both are NULL here —
// see [TestVWAPUSDFXResolver_BootstrapsWithoutUSDVolume]), so this also
// demonstrates the floor discriminates without a USD-valued column.
func TestVWAPUSDFXResolver_DustBucketDoesNotOutrankRealBucket(t *testing.T) {
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
	eurcIssuer := "GDHU6WRG4IEQXM5NZ4BMPKOXHW76MZM4Y2IEMFDVXBSDP6SJY4ITNPP2"
	usdc, _ := c.NewClassicAsset("USDC", usdcIssuer)
	eurc, _ := c.NewClassicAsset("EURC", eurcIssuer)
	pair, _ := c.NewPair(eurc, usdc)

	t0 := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Minute)
	// The real market: 100 EURC for 108.50 USDC → 1.085.
	realBucket := mkIntegrationTrade("sdex", 1, t0, pair, 1_000_000_000, 1_085_000_000)
	if err := store.InsertTrade(ctx, realBucket); err != nil {
		t.Fatalf("InsertTrade(real): %v", err)
	}
	// The dust: 2 stroops of EURC for 40000 stroops of USDC — $0.004 of
	// the peg, five minutes FRESHER than the real bucket.
	dustAt := t0.Add(5 * time.Minute)
	dust := mkIntegrationTrade("sdex", 2, dustAt, pair, 2, 40_000)
	if err := store.InsertTrade(ctx, dust); err != nil {
		t.Fatalf("InsertTrade(dust): %v", err)
	}
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`,
	); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}

	// Sanity-check the fixture against the CAGG itself: the dust bucket
	// must really be the freshest row and must really carry the absurd
	// VWAP, or the test would pass for the wrong reason.
	var (
		freshestBucket time.Time
		freshestVWAP   string
	)
	if err := store.DB().QueryRowContext(ctx,
		`SELECT bucket, vwap::text FROM prices_1m
		  WHERE base_asset = $1 AND quote_asset = $2
		  ORDER BY bucket DESC LIMIT 1`,
		eurc.String(), usdc.String(),
	).Scan(&freshestBucket, &freshestVWAP); err != nil {
		t.Fatalf("fixture check: %v", err)
	}
	if !freshestBucket.UTC().Equal(dustAt) {
		t.Fatalf("fixture: freshest bucket = %s, want the dust bucket %s", freshestBucket.UTC(), dustAt)
	}
	if got := trimTrailingZeros(freshestVWAP); got != "20000" {
		t.Fatalf("fixture: dust bucket vwap = %q, want 20000", got)
	}

	resolver, err := timescale.NewVWAPUSDFXResolver(store, timescale.VWAPUSDFXResolverOptions{
		USDPegs: []string{"USDC-" + usdcIssuer},
		// -1 disables the freshness bound so BOTH buckets are eligible;
		// the choice between them is the floor's job, not staleness'.
		Freshness: -1,
	})
	if err != nil {
		t.Fatalf("NewVWAPUSDFXResolver: %v", err)
	}

	got, ok, err := resolver.USDPriceAt(ctx, eurc, dustAt.Add(time.Minute))
	if err != nil {
		t.Fatalf("USDPriceAt: %v", err)
	}
	if !ok {
		t.Fatalf("expected the real EURC/USDC bucket to still resolve, got ok=false")
	}
	if got != "1.085" {
		t.Errorf("USDPriceAt = %q, want %q (the real bucket; %q is the $0.004 dust crumb)",
			got, "1.085", "20000")
	}
}

// TestVWAPUSDFXResolver_BootstrapsWithoutUSDVolume pins the
// non-circularity of the tier-3a dust floor, and is the regression
// guard against re-introducing the `volume_usd` form reverted at
// 7b69cd33.
//
// `trades.usd_volume` is written by the USD-resolution step, and tier
// 3a IS that step for a pair quoted in a peg — so a floor keyed on
// `prices_1m.volume_usd` can never be cleared by a pair the resolver
// has not already priced. The assertion below is deliberately
// two-sided: the seeded bucket carries volume_usd = 0 (no resolver
// installed on this store, so every trade inserted NULL and the CAGG
// coalesced it), AND the resolver still returns the rate.
func TestVWAPUSDFXResolver_BootstrapsWithoutUSDVolume(t *testing.T) {
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
	eurcIssuer := "GDHU6WRG4IEQXM5NZ4BMPKOXHW76MZM4Y2IEMFDVXBSDP6SJY4ITNPP2"
	usdc, _ := c.NewClassicAsset("USDC", usdcIssuer)
	eurc, _ := c.NewClassicAsset("EURC", eurcIssuer)
	pair, _ := c.NewPair(eurc, usdc)

	t0 := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Minute)
	trade := mkIntegrationTrade("sdex", 1, t0, pair, 1_000_000_000, 1_085_000_000)
	if err := store.InsertTrade(ctx, trade); err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`,
	); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}

	// The premise: the bucket has NOT been USD-valued. If this ever
	// stops holding, the circularity argument needs re-deriving before
	// anyone reaches for volume_usd again.
	var nullUSDVolumes int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM trades WHERE usd_volume IS NULL`,
	).Scan(&nullUSDVolumes); err != nil {
		t.Fatalf("usd_volume check: %v", err)
	}
	if nullUSDVolumes != 1 {
		t.Fatalf("fixture: %d trades with NULL usd_volume, want 1", nullUSDVolumes)
	}
	var volumeUSD string
	if err := store.DB().QueryRowContext(ctx,
		`SELECT volume_usd::text FROM prices_1m
		  WHERE base_asset = $1 AND quote_asset = $2`,
		eurc.String(), usdc.String(),
	).Scan(&volumeUSD); err != nil {
		t.Fatalf("volume_usd check: %v", err)
	}
	if v := trimTrailingZeros(volumeUSD); v != "0" {
		t.Fatalf("fixture: prices_1m.volume_usd = %q, want 0 (nothing has valued this pair yet)", v)
	}

	resolver, err := timescale.NewVWAPUSDFXResolver(store, timescale.VWAPUSDFXResolverOptions{
		USDPegs:   []string{"USDC-" + usdcIssuer},
		Freshness: -1,
	})
	if err != nil {
		t.Fatalf("NewVWAPUSDFXResolver: %v", err)
	}

	got, ok, err := resolver.USDPriceAt(ctx, eurc, t0.Add(time.Minute))
	if err != nil {
		t.Fatalf("USDPriceAt: %v", err)
	}
	if !ok {
		t.Fatalf("resolver could not bootstrap a never-priced pair (ok=false) — the tier-3a floor is circular again")
	}
	if got != "1.085" {
		t.Errorf("USDPriceAt = %q, want %q", got, "1.085")
	}
}

// trimTrailingZeros renders a Postgres NUMERIC::text in the same
// canonical form the resolver returns, so fixture assertions can be
// written against the arithmetic value rather than the column scale.
func trimTrailingZeros(s string) string {
	if !strings.ContainsRune(s, '.') {
		return s
	}
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

// TestVWAPUSDFXResolver_NoMatchReturnsOk False — asset with no
// against-peg row produces (`""`, ok=false, nil err). Pre-Phase-2
// behaviour preserved for assets we don't cover yet.
func TestVWAPUSDFXResolver_NoMatchReturnsOk(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	usdc, _ := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")

	resolver, _ := timescale.NewVWAPUSDFXResolver(store, timescale.VWAPUSDFXResolverOptions{
		USDPegs: []string{"USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"},
	})

	// No trades inserted; asking for an obscure asset against the
	// peg → no match. The boundary is (empty rate, ok=false, nil err).
	obscure, _ := c.NewClassicAsset("AQUA", "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA")
	_ = usdc
	got, ok, err := resolver.USDPriceAt(ctx, obscure, time.Now().UTC())
	if err != nil {
		t.Errorf("err = %v, want nil for no-match case", err)
	}
	if ok {
		t.Errorf("expected ok=false for no-data asset, got rate=%q ok=true", got)
	}
	if got != "" {
		t.Errorf("expected empty rate, got %q", got)
	}
}
