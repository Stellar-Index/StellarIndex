package v1_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/RatesEngine/rates-engine/internal/aggregate"
	v1 "github.com/RatesEngine/rates-engine/internal/api/v1"
	"github.com/RatesEngine/rates-engine/internal/canonical"
)

// stubGlobalPriceReader implements aggregate.GlobalPriceReader for
// the handler tests. Configurable per-tier.
type stubGlobalPriceReader struct {
	vwap struct {
		price      string
		asOf       time.Time
		tradeCount int64
		sources    []string
		ok         bool
	}
	agg struct {
		rows []canonical.OracleUpdate
	}
	tri struct {
		price string
		asOf  time.Time
		ok    bool
	}
}

func (s *stubGlobalPriceReader) LatestVWAP(_ context.Context, _, _ canonical.Asset) (string, time.Time, int64, []string, bool, error) {
	return s.vwap.price, s.vwap.asOf, s.vwap.tradeCount, s.vwap.sources, s.vwap.ok, nil
}

func (s *stubGlobalPriceReader) LatestAggregatorPrices(_ context.Context, _, _ canonical.Asset, _ []string) ([]canonical.OracleUpdate, error) {
	return s.agg.rows, nil
}

func (s *stubGlobalPriceReader) LookupTriangulated(_ context.Context, _, _ canonical.Asset, _ time.Duration) (string, time.Time, bool, error) {
	return s.tri.price, s.tri.asOf, s.tri.ok, nil
}

func TestAssetGet_SlugDispatch_GlobalView(t *testing.T) {
	cat := newTestCatalogue(t)
	reader := &stubGlobalPriceReader{}
	reader.vwap.price = "1.00050000000000"
	reader.vwap.asOf = time.Now().UTC().Truncate(time.Second)
	reader.vwap.tradeCount = 12
	reader.vwap.sources = []string{"coinbase", "binance"}
	reader.vwap.ok = true

	srv := v1.New(v1.Options{
		VerifiedCurrencies: cat,
		GlobalPrice:        reader,
		GlobalPriceOpts: aggregate.GlobalPriceOptions{
			AggregatorSources: []string{"coingecko"},
		},
	})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/assets/usdc")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var env struct {
		Data v1.GlobalAssetView `json:"data"`
	}
	mustDecode(t, resp, &env)
	if env.Data.Ticker != "USDC" || env.Data.Slug != "usdc" {
		t.Errorf("wrong identity: %+v", env.Data)
	}
	if env.Data.Name != "USD Coin" {
		t.Errorf("name = %q, want USD Coin", env.Data.Name)
	}
	if env.Data.VerifiedIssuer == "" {
		t.Error("verified_issuer empty")
	}
	if env.Data.PriceUSD == nil || *env.Data.PriceUSD != "1.00050000000000" {
		t.Errorf("price_usd = %v, want 1.00050000000000", env.Data.PriceUSD)
	}
	if env.Data.PriceAuthority != aggregate.AuthorityVWAPNative {
		t.Errorf("price_authority = %q, want vwap_native", env.Data.PriceAuthority)
	}
	if len(env.Data.Networks) == 0 {
		t.Error("networks empty")
	}
	// Stellar entry must carry a deep_link.
	var foundStellar bool
	for _, n := range env.Data.Networks {
		if n.Network == "stellar" {
			foundStellar = true
			if n.DataQuality != "indexed" {
				t.Errorf("stellar data_quality = %q, want indexed", n.DataQuality)
			}
			if n.DeepLink == "" {
				t.Error("stellar entry missing deep_link")
			}
			if n.AssetID == "" {
				t.Error("stellar entry missing asset_id")
			}
		}
	}
	if !foundStellar {
		t.Error("USDC catalogue entry has no stellar network — expected one")
	}
	// Non-Stellar entries must be data_quality="external" with no deep_link.
	for _, n := range env.Data.Networks {
		if n.Network != "stellar" {
			if n.DataQuality != "external" {
				t.Errorf("%s data_quality = %q, want external", n.Network, n.DataQuality)
			}
			if n.DeepLink != "" {
				t.Errorf("%s has deep_link %q; non-Stellar should not", n.Network, n.DeepLink)
			}
		}
	}
}

func TestAssetGet_SlugDispatch_StellarOnlyTokenNoPrice(t *testing.T) {
	// AQUA is in the catalogue but `crypto:AQUA` won't be on the
	// canonical crypto allow-list (it's a Stellar-only token).
	// Global view still resolves: identity + networks populate;
	// price block stays nil. Consumers drill into the Stellar
	// deep_link for the per-asset price.
	cat := newTestCatalogue(t)
	reader := &stubGlobalPriceReader{}
	// No vwap.ok, no agg.rows, no tri.ok — every tier misses.

	srv := v1.New(v1.Options{
		VerifiedCurrencies: cat,
		GlobalPrice:        reader,
	})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/assets/aqua")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var env struct {
		Data v1.GlobalAssetView `json:"data"`
	}
	mustDecode(t, resp, &env)
	if env.Data.Ticker != "AQUA" {
		t.Errorf("ticker = %q", env.Data.Ticker)
	}
	if env.Data.PriceUSD != nil {
		t.Errorf("price_usd should be nil for AQUA (no global price), got %v", env.Data.PriceUSD)
	}
	// Stellar deep_link still present — consumer's path forward.
	if len(env.Data.Networks) == 0 || env.Data.Networks[0].DeepLink == "" {
		t.Error("stellar network/deep_link missing")
	}
}

func TestAssetGet_SlugDispatch_NoGlobalPriceReader(t *testing.T) {
	// When the binary doesn't wire GlobalPrice, the slug still
	// resolves to the catalogue identity + networks; the price
	// block is just empty.
	cat := newTestCatalogue(t)
	srv := v1.New(v1.Options{
		VerifiedCurrencies: cat,
		// No GlobalPrice.
	})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/assets/usdc")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var env struct {
		Data v1.GlobalAssetView `json:"data"`
	}
	mustDecode(t, resp, &env)
	if env.Data.PriceUSD != nil {
		t.Errorf("price_usd should be nil without a reader, got %v", env.Data.PriceUSD)
	}
	if env.Data.Ticker != "USDC" {
		t.Errorf("ticker = %q", env.Data.Ticker)
	}
}

func TestAssetGet_CanonicalIDStillWorksWithCatalogue(t *testing.T) {
	// With the catalogue wired, a canonical asset_id (USDC-G...)
	// must still route to the per-Stellar-asset view, NOT to the
	// global slug view. Slug dispatch only matches bare slugs.
	cat := newTestCatalogue(t)
	srv := v1.New(v1.Options{
		VerifiedCurrencies: cat,
	})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/assets/USDC-"+testUSDCIssuer)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var env struct {
		Data v1.AssetDetail `json:"data"`
	}
	mustDecode(t, resp, &env)
	if env.Data.AssetID != "USDC-"+testUSDCIssuer {
		t.Errorf("canonical id routed wrong; got asset_id = %q", env.Data.AssetID)
	}
	if env.Data.Type != "classic" {
		t.Errorf("type = %q, want classic", env.Data.Type)
	}
}

func TestAssetGet_UnknownSlug_FallsThroughToCanonicalParse(t *testing.T) {
	// A path that's not a known slug AND not a canonical id must
	// return 400 (the existing invalid-asset-id problem). Slug
	// dispatch doesn't change that behaviour.
	cat := newTestCatalogue(t)
	srv := v1.New(v1.Options{VerifiedCurrencies: cat})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/assets/notarealthing")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestCoins_DeprecationHeaders(t *testing.T) {
	// /v1/coins and /v1/coins/{slug} must emit the Deprecation +
	// Link headers pointing at the new /v1/assets/{slug} surface
	// (R-018 Phase 1.4a).
	srv := v1.New(v1.Options{}) // no readers wired — handler degrades to 503 but headers still set
	ts := httpTestServer(t, srv)

	for _, path := range []string{"/v1/coins", "/v1/coins/usdc"} {
		t.Run(path, func(t *testing.T) {
			resp := mustGet(t, ts.URL+path)
			if got := resp.Header.Get("Deprecation"); got != "true" {
				t.Errorf("Deprecation header = %q, want true", got)
			}
			link := resp.Header.Get("Link")
			if link == "" {
				t.Error("Link header missing")
			}
			if !contains(link, `rel="successor-version"`) {
				t.Errorf("Link header missing successor-version rel: %q", link)
			}
			if !contains(link, "/v1/assets") {
				t.Errorf("Link header doesn't point at /v1/assets: %q", link)
			}
		})
	}
}

func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
