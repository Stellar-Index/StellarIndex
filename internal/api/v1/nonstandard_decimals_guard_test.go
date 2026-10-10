package v1_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// flaggedAsset is a fixed C-strkey used across the guard tests — the SAME
// contract id named in the runbook / migration 0093 header (harmless as a
// test fixture: it's a real on-chain public contract id, not a secret).
const flaggedAsset = "CC2RBGYNCFBCVENIDL5BFBWPH4OUZM2UA3OD2K2N54GLMWCC4KWPVAGO"

// nonstandardDecimalsCacheWith builds a *v1.NonstandardDecimalsCache
// pre-populated (via one Refresh) with a single flagged asset.
func nonstandardDecimalsCacheWith(t *testing.T, asset string, decimals int) *v1.NonstandardDecimalsCache {
	t.Helper()
	reader := &stubNonstandardDecimalsReader{
		rows: []timescale.NonstandardDecimalsAsset{{Asset: asset, Decimals: decimals, Source: "aquarius"}},
	}
	c := v1.NewNonstandardDecimalsCache(reader, nil)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("cache refresh: %v", err)
	}
	return c
}

// classicUSDC is a 7dp classic quote leg for series-mode fixtures — a
// non-fiat quote takes ohlcSeriesWithAliases' first-hit path (no
// fiat-combine fan-out), so the stub's bars map 1:1 onto the response.
const classicUSDC = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

// globalViewPrice serves /v1/assets/usdc against a tier-1 VWAP of vwap and
// returns price_usd (nil when every tier missed).
func globalViewPrice(t *testing.T, vwap string, decimals *v1.NonstandardDecimalsCache) *string {
	t.Helper()
	reader := &stubGlobalPriceReader{}
	reader.vwap.price = vwap
	reader.vwap.asOf = time.Now().UTC().Truncate(time.Second)
	reader.vwap.tradeCount = 12
	reader.vwap.ok = true
	srv := v1.New(v1.Options{
		VerifiedCurrencies:  newTestCatalogue(t),
		GlobalPrice:         reader,
		NonstandardDecimals: decimals,
	})
	ts := httpTestServer(t, srv)
	resp := mustGet(t, ts.URL+"/v1/assets/usdc")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data v1.GlobalAssetView `json:"data"`
	}
	mustDecode(t, resp, &env)
	return env.Data.PriceUSD
}

// TestGlobalAsset_NonstandardDecimals_NormalizesVWAPTier pins that the
// global view's vwap_native tier serves the corrected price, not the raw
// prices_1m ratio its reader returns. The tier reads `crypto:USDC` /
// `fiat:USD`; flagging the base at 9dp gives K = 10^(9−7) = 100.
func TestGlobalAsset_NonstandardDecimals_NormalizesVWAPTier(t *testing.T) {
	got := globalViewPrice(t, "41.32", nonstandardDecimalsCacheWith(t, "crypto:USDC", 9))
	if got == nil || *got != "4132.0000000000" {
		t.Errorf("price_usd = %s, want 4132.0000000000 (raw 41.32 × 100)", deref(got))
	}
}

// TestGlobalAsset_NonstandardDecimals_UnflaggedByteIdentical — with the
// cache wired but neither leg flagged, the tier-1 string passes through
// byte-identical (no re-render at a fixed digit count).
func TestGlobalAsset_NonstandardDecimals_UnflaggedByteIdentical(t *testing.T) {
	got := globalViewPrice(t, "1.00050000000000", nonstandardDecimalsCacheWith(t, flaggedAsset, 9))
	if got == nil || *got != "1.00050000000000" {
		t.Errorf("price_usd = %s, want byte-identical 1.00050000000000", deref(got))
	}
}

// TestGlobalAsset_NonstandardDecimals_UnparseableFlaggedWithheld — a
// flagged pair's ratio that cannot be parsed cannot be corrected, and
// serving it raw is the defect, so the tier reads as a miss.
func TestGlobalAsset_NonstandardDecimals_UnparseableFlaggedWithheld(t *testing.T) {
	got := globalViewPrice(t, "not-a-number", nonstandardDecimalsCacheWith(t, "crypto:USDC", 9))
	if got != nil {
		t.Errorf("price_usd = %q, want nil (an uncorrectable flagged ratio must not be served)", *got)
	}
}
