package v1_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/holds"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
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
	if env.Data.Kind != "catalogue" {
		t.Errorf("kind = %q, want \"catalogue\" (ADR-0042 LC-040 wire-shape discriminator)", env.Data.Kind)
	}
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
}

// TestAssetGet_SlugDispatch_GlobalView_SubstanceWithheld pins that
// the global CEX/aggregator tier (populateGlobalCryptoPrice) must be
// held to the same substance gate as the classic /v1/assets listing and
// /v1/price, not just the on-chain fallback tier below it. Before the
// fix this tier had no gate at all, so a thin/dust market the rest of
// the API withholds still headlined on /v1/assets/{slug}.
func TestAssetGet_SlugDispatch_GlobalView_SubstanceWithheld(t *testing.T) {
	cat := newTestCatalogue(t)
	reader := &stubGlobalPriceReader{}
	reader.vwap.price = "1.00050000000000"
	reader.vwap.asOf = time.Now().UTC().Truncate(time.Second)
	reader.vwap.tradeCount = 12
	reader.vwap.sources = []string{"coinbase", "binance"}
	reader.vwap.ok = true

	gate := &stubSubstanceGate{allow: false}
	srv := v1.New(v1.Options{
		VerifiedCurrencies: cat,
		GlobalPrice:        reader,
		GlobalPriceOpts: aggregate.GlobalPriceOptions{
			AggregatorSources: []string{"coingecko"},
		},
		Substance: gate,
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
		t.Errorf("price_usd = %v, want withheld — the substance gate refuses this pair", *env.Data.PriceUSD)
	}
	if len(gate.surfaces) == 0 {
		t.Error("substance gate was never consulted for the global asset view")
	}
}

func TestAssetGet_SlugDispatch_StellarOnlyTokenNoPrice(t *testing.T) {
	// AQUA is in the catalogue but `crypto:AQUA` won't be on the
	// canonical crypto allow-list (it's a Stellar-only token).
	// Global view still resolves: identity populates; price block
	// stays nil. Consumers drill into the canonical asset_id surface
	// for the per-asset price.
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
}

func TestAssetGet_SlugDispatch_NoGlobalPriceReader(t *testing.T) {
	// When the binary doesn't wire GlobalPrice, the slug still
	// resolves to the catalogue identity; the price block is just
	// empty.
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
	if env.Data.Kind != "catalogue" {
		t.Errorf("kind = %q, want \"catalogue\" (ADR-0042 LC-040 wire-shape discriminator)", env.Data.Kind)
	}
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
	if env.Data.Kind != "stellar_asset" {
		t.Errorf("kind = %q, want \"stellar_asset\" (ADR-0042 LC-040 wire-shape discriminator) — proves the same oneOf route discriminates both branches correctly", env.Data.Kind)
	}
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

// TestAssetGet_Fiat_USDIdentity — /v1/assets/us-dollar synthesises
// a 1.00 price (identity) without hitting the global-price reader,
// and computes market_cap_usd directly from the catalogue's M2 figure.
func TestAssetGet_Fiat_USDIdentity(t *testing.T) {
	cat := newTestCatalogue(t)
	srv := v1.New(v1.Options{
		VerifiedCurrencies: cat,
		// Provide a stub reader so the price block isn't short-
		// circuited by the nil-guard; the USD path doesn't call
		// the reader but the handler checks s.globalPrice != nil.
		GlobalPrice: &stubGlobalPriceReader{},
	})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/external/assets/us-dollar")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var env struct {
		Data v1.GlobalAssetView `json:"data"`
	}
	mustDecode(t, resp, &env)
	d := env.Data

	if d.Class != "fiat" {
		t.Errorf("class = %q, want fiat", d.Class)
	}
	if d.PriceUSD == nil || *d.PriceUSD != "1.00000000000000" {
		t.Errorf("price_usd = %v, want 1.00000000000000", d.PriceUSD)
	}
	if d.CirculatingSupply == nil {
		t.Errorf("circulating_supply missing for USD")
	}
	// USD M2 × 1.00 → "21700000000000.00" (seed M2 value × identity).
	if d.MarketCapUSD == nil {
		t.Fatalf("market_cap_usd missing for USD")
	}
	if *d.MarketCapUSD != "21700000000000.00" {
		t.Errorf("USD market_cap_usd = %q, want 21700000000000.00", *d.MarketCapUSD)
	}
}

// TestAssetGet_Fiat_CNY_MarketCap — non-USD fiat: handler reads
// the FX rate from PriceReader (s.Prices) for the fiat:CNY/fiat:USD
// pair, then multiplies M2 by the result. PriceReader is used
// rather than ComputeGlobalPrice because the FX-rate Redis
// triangulated fallback (which prices_1m doesn't carry for
// fiat:fiat) is in scope of the production PriceReader.
func TestAssetGet_Fiat_CNY_MarketCap(t *testing.T) {
	cat := newTestCatalogue(t)
	priceStub := &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			"fiat:CNY/fiat:USD": {
				AssetID: "fiat:CNY", Quote: "fiat:USD",
				Price: "0.14000000000000", PriceType: "vwap",
			},
		},
	}

	srv := v1.New(v1.Options{
		VerifiedCurrencies: cat,
		Prices:             priceStub,
	})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/external/assets/chinese-yuan")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var env struct {
		Data v1.GlobalAssetView `json:"data"`
	}
	mustDecode(t, resp, &env)
	d := env.Data

	if d.Class != "fiat" {
		t.Errorf("class = %q, want fiat", d.Class)
	}
	if d.PriceUSD == nil || *d.PriceUSD != "0.14000000000000" {
		t.Errorf("price_usd = %v, want 0.14000000000000", d.PriceUSD)
	}
	// CNY M2 = 302_000_000_000_000; × 0.14 = 42_280_000_000_000.00
	if d.MarketCapUSD == nil || *d.MarketCapUSD != "42280000000000.00" {
		t.Errorf("CNY market_cap_usd = %v, want 42280000000000.00", d.MarketCapUSD)
	}
}

// TestAssetMetadataRouteWorks — verifies the
// /v1/assets/{asset_id}/metadata route serves the AssetMetadata body.
func TestAssetMetadataRouteWorks(t *testing.T) {
	cat := newTestCatalogue(t)
	srv := v1.New(v1.Options{VerifiedCurrencies: cat})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/assets/native/metadata")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/v1/assets/native/metadata status = %d", resp.StatusCode)
	}
	// Body shape: AssetMetadata.
	var env struct {
		Data map[string]any `json:"data"`
	}
	mustDecode(t, resp, &env)
	if _, ok := env.Data["sep1_status"]; !ok {
		t.Errorf("body missing sep1_status: %+v", env.Data)
	}
}

// /v1/coins removed (no production consumers); deprecation-header
// test deleted along with the routes.

// TestExternalAssetList_InvalidClass_400s — an unrecognised
// asset_class must reject with 400, not silently fall through to the
// unfiltered listing (the sibling /v1/assets?asset_class= path already
// 400s via validAssetClass; /v1/external/assets was missing the gate).
func TestExternalAssetList_InvalidClass_400s(t *testing.T) {
	cat := newTestCatalogue(t)
	srv := v1.New(v1.Options{
		VerifiedCurrencies: cat,
		GlobalPrice:        &stubGlobalPriceReader{},
	})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/external/assets?asset_class=bogus")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("asset_class=bogus status = %d, want 400", resp.StatusCode)
	}
}

// TestAssetGet_StellarExternalGate pins the detail split: an external
// asset (fiat) 404s on /v1/assets/{slug}, and a Stellar asset (usdc) 404s on
// /v1/external/assets/{slug}. No redirect — each lives on exactly one path.
func TestAssetGet_StellarExternalGate(t *testing.T) {
	cat := newTestCatalogue(t)
	srv := v1.New(v1.Options{
		VerifiedCurrencies: cat,
		GlobalPrice:        &stubGlobalPriceReader{},
	})
	ts := httpTestServer(t, srv)

	// External (fiat) must NOT resolve on the Stellar detail route.
	if resp := mustGet(t, ts.URL+"/v1/assets/us-dollar"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("/v1/assets/us-dollar (external) status = %d, want 404", resp.StatusCode)
	}
	// External (fiat) DOES resolve on the external detail route.
	if resp := mustGet(t, ts.URL+"/v1/external/assets/us-dollar"); resp.StatusCode != http.StatusOK {
		t.Errorf("/v1/external/assets/us-dollar status = %d, want 200", resp.StatusCode)
	}
	// Stellar (usdc) must NOT resolve on the external detail route.
	if resp := mustGet(t, ts.URL+"/v1/external/assets/usdc"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("/v1/external/assets/usdc (Stellar) status = %d, want 404", resp.StatusCode)
	}
	// Stellar (usdc) DOES resolve on the Stellar detail route.
	if resp := mustGet(t, ts.URL+"/v1/assets/usdc"); resp.StatusCode != http.StatusOK {
		t.Errorf("/v1/assets/usdc status = %d, want 200", resp.StatusCode)
	}
}

// Fiat prices are not the vwap_native tier: a non-USD fiat is an FX
// reference rate served as its stored NUMERIC text (never re-rendered
// from a float), and USD is an identity.
func TestAssetGet_FiatPriceAuthorityAndExactRate(t *testing.T) {
	const exact = "1.1700000000000000000123"
	fx := &stubFXHistoryReader{points: []v1.FXQuotePoint{
		{Bucket: time.Now().UTC().Add(-24 * time.Hour), InverseUSDText: exact},
	}}
	srv := v1.New(v1.Options{
		VerifiedCurrencies: newTestCatalogue(t),
		FXHistory:          fx,
		GlobalPrice:        &stubGlobalPriceReader{},
	})
	ts := httpTestServer(t, srv)

	for _, tc := range []struct {
		slug, price string
		authority   aggregate.PriceAuthority
	}{
		{"euro", exact, aggregate.AuthorityReferenceRate},
		{"us-dollar", "1.00000000000000", aggregate.AuthorityIdentity},
	} {
		resp := mustGet(t, ts.URL+"/v1/external/assets/"+tc.slug)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status = %d", tc.slug, resp.StatusCode)
		}
		var env struct {
			Data v1.GlobalAssetView `json:"data"`
		}
		mustDecode(t, resp, &env)
		d := env.Data
		if d.PriceUSD == nil || *d.PriceUSD != tc.price {
			got := "<nil>"
			if d.PriceUSD != nil {
				got = *d.PriceUSD
			}
			t.Errorf("%s: price_usd = %q, want %q", tc.slug, got, tc.price)
		}
		if d.PriceAuthority != tc.authority {
			t.Errorf("%s: price_authority = %q, want %q", tc.slug, d.PriceAuthority, tc.authority)
		}
	}
}

// The overlay's row price, both price histories and the ATH are RAW CAGG
// ratios. For a confirmed 9-decimals token they were published verbatim
// — 100x low — beside a price_usd the canonical path normalises, so one
// payload disagreed with itself by a power of ten.
func TestAssetGet_AssetExtension_NormalisesNonstandardDecimals(t *testing.T) {
	d := assetExtensionDetail(t, flaggedAsset,
		nonstandardDecimalsCacheWith(t, flaggedAsset, 9), rawOverlay(flaggedAsset))

	if d.PriceUSD == nil || *d.PriceUSD != "4132.0000000000" {
		t.Errorf("price_usd = %v, want 4132.0000000000", deref(d.PriceUSD))
	}
	if len(d.PriceHistory24h) != 3 {
		t.Fatalf("price_history_24h has %d points, want 3 (the bucket grid must not change)", len(d.PriceHistory24h))
	}
	if got := deref(d.PriceHistory24h[0].P); got != "4132.0000000000" {
		t.Errorf("price_history_24h[0] = %q, want 4132.0000000000", got)
	}
	if d.PriceHistory24h[1].P != nil {
		t.Errorf("price_history_24h[1] = %q, want a null gap", *d.PriceHistory24h[1].P)
	}
	if got := deref(d.PriceHistory24h[2].P); got != "4000.0000000000" {
		t.Errorf("price_history_24h[2] = %q, want 4000.0000000000", got)
	}
	if len(d.PriceHistory7d) != 1 || deref(d.PriceHistory7d[0].P) != "3950.0000000000" {
		t.Errorf("price_history_7d = %+v, want one point at 3950.0000000000", d.PriceHistory7d)
	}
	if d.ATH == nil || d.ATH.USD != "5512.5000000000" {
		t.Errorf("ath = %+v, want usd 5512.5000000000", d.ATH)
	}
	if d.ATH != nil && d.ATH.At != "2026-08-01T00:00:00Z" {
		t.Errorf("ath.at = %q, want it carried through unchanged", d.ATH.At)
	}
}

// The same overlay for an asset with NO confirmed row must leave the wire
// bytes alone, even with the table populated for someone else: the
// catalogue's text rendering is not ratToDecimal's, so reformatting
// unconditionally would move every already-correct 7dp price.
func TestAssetGet_AssetExtension_SevenDecimalsByteIdentical(t *testing.T) {
	const plain = "CAUP7NFABXE5TJRL3FKTPMWRLC7IAXYDCTHQRFSCLR5TMGKHOOQO772J"
	overlay := rawOverlay(plain)
	overlay.row.PriceUSD = sptr("41.32")
	d := assetExtensionDetail(t, plain, nonstandardDecimalsCacheWith(t, flaggedAsset, 9), overlay)

	if got := deref(d.PriceUSD); got != "41.32" {
		t.Errorf("price_usd = %q, want 41.32 byte-identical", got)
	}
	if len(d.PriceHistory24h) != 3 || deref(d.PriceHistory24h[0].P) != "41.3200000000" ||
		d.PriceHistory24h[1].P != nil || deref(d.PriceHistory24h[2].P) != "40.0000000000" {
		t.Errorf("price_history_24h = %+v, want it byte-identical", d.PriceHistory24h)
	}
	if len(d.PriceHistory7d) != 1 || deref(d.PriceHistory7d[0].P) != "39.5000000000" {
		t.Errorf("price_history_7d = %+v, want it byte-identical", d.PriceHistory7d)
	}
	if d.ATH == nil || d.ATH.USD != "55.125" {
		t.Errorf("ath = %+v, want usd 55.125 byte-identical", d.ATH)
	}
}

func TestAssetGet_HoldMarksUnderReview(t *testing.T) {
	usdc, err := canonical.NewClassicAsset("USDC", testUSDCIssuer)
	if err != nil {
		t.Fatalf("NewClassicAsset: %v", err)
	}
	reader := &stubAssetReader{
		byID: map[string]v1.AssetDetail{
			usdc.String(): {AssetID: usdc.String(), Type: "classic", Code: "USDC"},
		},
	}
	srv := v1.New(v1.Options{Assets: reader, AssetsReader: &stubAssetsReaderExt{}})
	list, err := holds.Parse([]byte("[[hold]]\nasset = \"" + usdc.String() + "\"\nreason = \"r\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	srv.SetHolds(list)
	resp := mustGet(t, httpTestServer(t, srv).URL+"/v1/assets/"+usdc.String())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var body struct {
		Flags  map[string]any `json:"flags"`
		Reason string         `json:"under_review_reason"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Flags["under_review"] != true || body.Reason != "r" {
		t.Errorf("not marked: flags=%v reason=%q", body.Flags, body.Reason)
	}
}

func TestAssetGet_AssetExtension_Populates(t *testing.T) {
	price := sptr("1.0008")
	ch1h := sptr("+0.11")
	ch7d := sptr("-0.04")
	assetsReader := &stubAssetsReaderExt{
		row: timescale.AssetRow{
			Slug:        "USDC",
			AssetID:     "USDC-" + testUSDCIssuer,
			Code:        "USDC",
			PriceUSD:    price,
			Change1hPct: ch1h,
			Change7dPct: ch7d,
		},
		topMarkets: []timescale.AssetTopMarket{
			{Counterparty: "native", Side: "quote", Volume24hUSD: sptr("1000.0"), TradeCount24h: 5},
		},
		hist24:   []timescale.AssetPricePoint{{T: "2026-05-11T00:00:00Z", P: sptr("1.0001")}},
		hist7d:   []timescale.AssetPricePoint{{T: "2026-05-05T00:00:00Z", P: sptr("1.0010")}},
		marketsN: 12,
		tradeN:   456,
		ath:      &timescale.AssetATH{USD: "6.39", At: "2026-05-04T00:00:00Z"},
	}
	usdc, err := canonical.NewClassicAsset("USDC", testUSDCIssuer)
	if err != nil {
		t.Fatalf("NewClassicAsset: %v", err)
	}
	reader := &stubAssetReader{
		byID: map[string]v1.AssetDetail{
			usdc.String(): {AssetID: usdc.String(), Type: "classic", Code: "USDC"},
		},
	}
	srv := v1.New(v1.Options{Assets: reader, AssetsReader: assetsReader})
	ts := httpTestServer(t, srv)
	resp := mustGet(t, ts.URL+"/v1/assets/"+usdc.String())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var env struct {
		Data v1.AssetDetail `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	d := env.Data
	if d.PriceUSD == nil || *d.PriceUSD != "1.0008" {
		t.Errorf("price_usd: got %v want 1.0008", d.PriceUSD)
	}
	if d.Change1hPct == nil || *d.Change1hPct != "+0.11" {
		t.Errorf("change_1h_pct: got %v", d.Change1hPct)
	}
	if d.Change7dPct == nil || *d.Change7dPct != "-0.04" {
		t.Errorf("change_7d_pct: got %v", d.Change7dPct)
	}
	if len(d.TopMarkets) != 1 || d.TopMarkets[0].Counterparty != "native" {
		t.Errorf("top_markets unexpected: %+v", d.TopMarkets)
	}
	if len(d.PriceHistory24h) != 1 {
		t.Errorf("price_history_24h: %d points want 1", len(d.PriceHistory24h))
	}
	if len(d.PriceHistory7d) != 1 {
		t.Errorf("price_history_7d: %d points want 1", len(d.PriceHistory7d))
	}
	if d.MarketsCount == nil || *d.MarketsCount != 12 {
		t.Errorf("markets_count: %v", d.MarketsCount)
	}
	if d.TradeCount24h == nil || *d.TradeCount24h != 456 {
		t.Errorf("trade_count_24h: %v", d.TradeCount24h)
	}
	if d.ATH == nil || d.ATH.USD != "6.39" {
		t.Errorf("ath: %+v", d.ATH)
	}
}

func TestAssetGet_AssetExtension_NoAssetReader_NoOp(t *testing.T) {
	// No AssetsReader wired — asset-extension fields stay nil; rest
	// of the response is unchanged.
	usdc, _ := canonical.NewClassicAsset("USDC", testUSDCIssuer)
	reader := &stubAssetReader{
		byID: map[string]v1.AssetDetail{
			usdc.String(): {AssetID: usdc.String(), Type: "classic", Code: "USDC"},
		},
	}
	srv := v1.New(v1.Options{Assets: reader})
	ts := httpTestServer(t, srv)
	resp := mustGet(t, ts.URL+"/v1/assets/"+usdc.String())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var env struct {
		Data v1.AssetDetail `json:"data"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&env)
	if env.Data.PriceUSD != nil || len(env.Data.TopMarkets) != 0 {
		t.Errorf("expected nil extension fields, got %+v", env.Data)
	}
}

func TestAssetGet_AssetExtension_FiatAsset_Skipped(t *testing.T) {
	// fiat:* assets have no asset-catalogue row; the extension is a no-op even
	// when an AssetsReader is wired.
	assetsReader := &stubAssetsReaderExt{
		row:        timescale.AssetRow{PriceUSD: sptr("999")}, // would leak through if not gated
		topMarkets: []timescale.AssetTopMarket{{Counterparty: "should_not_appear"}},
	}
	usd, _ := canonical.ParseAsset("fiat:USD")
	reader := &stubAssetReader{
		byID: map[string]v1.AssetDetail{
			usd.String(): {AssetID: "fiat:USD", Type: "fiat", Code: "USD"},
		},
	}
	srv := v1.New(v1.Options{Assets: reader, AssetsReader: assetsReader})
	ts := httpTestServer(t, srv)
	resp := mustGet(t, ts.URL+"/v1/assets/fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var env struct {
		Data v1.AssetDetail `json:"data"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&env)
	if env.Data.PriceUSD != nil {
		t.Errorf("fiat asset should NOT get asset extension; got price_usd=%v", env.Data.PriceUSD)
	}
	if len(env.Data.TopMarkets) != 0 {
		t.Errorf("fiat asset should NOT get top_markets; got %+v", env.Data.TopMarkets)
	}
}

// TestAssetGet_ZeroObservationCountIsServedNotDropped.
//
// `observation_count` is a COUNT column declared `NOT NULL DEFAULT 0`
// (migrations/0023), so a registered asset that hasn't traded in the
// window legitimately reads 0. An overlay that gated the field on
// `!= 0`, which dropped it from the response — leaving a client unable
// to distinguish "this asset has zero observations" from "we have no
// catalogue row for this asset at all", the two states the field's own
// `omitempty` is supposed to separate.
//
// The ledger fields are deliberately NOT included in that change:
// ledger 0 does not exist on Stellar, so 0 there really does mean
// unset and must stay omitted. This test pins both halves.
func TestAssetGet_ZeroObservationCountIsServedNotDropped(t *testing.T) {
	usdc, err := canonical.NewClassicAsset("USDC", testUSDCIssuer)
	if err != nil {
		t.Fatalf("NewClassicAsset: %v", err)
	}
	assetsReader := &stubAssetsReaderExt{
		row: timescale.AssetRow{
			Slug:    "USDC",
			AssetID: usdc.String(),
			Code:    "USDC",
			// A catalogued asset with no observations yet.
			ObservationCount: 0,
			FirstSeenLedger:  0,
			LastSeenLedger:   0,
		},
	}
	reader := &stubAssetReader{
		byID: map[string]v1.AssetDetail{
			usdc.String(): {AssetID: usdc.String(), Type: "classic", Code: "USDC"},
		},
	}
	srv := v1.New(v1.Options{Assets: reader, AssetsReader: assetsReader})
	ts := httpTestServer(t, srv)
	resp := mustGet(t, ts.URL+"/v1/assets/"+usdc.String())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var env struct {
		Data v1.AssetDetail `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Data.ObservationCount == nil {
		t.Fatalf("observation_count dropped for a catalogued zero-observation asset — "+
			"indistinguishable from 'no catalogue row'. body: %s", body)
	}
	if got := *env.Data.ObservationCount; got != 0 {
		t.Errorf("observation_count = %d, want 0", got)
	}
	if !bytes.Contains(body, []byte(`"observation_count":0`)) {
		t.Errorf("wire body must carry observation_count:0, got: %s", body)
	}
	// Ledger 0 is not a real Stellar ledger — those stay omitted.
	if env.Data.FirstSeenLedger != nil {
		t.Errorf("first_seen_ledger = %v, want absent (ledger 0 does not exist)", *env.Data.FirstSeenLedger)
	}
	if env.Data.LastSeenLedger != nil {
		t.Errorf("last_seen_ledger = %v, want absent (ledger 0 does not exist)", *env.Data.LastSeenLedger)
	}
}

// TestAssetGet_NativeObservationCountIsAbsent pins that Native
// XLM has no registry row, so its row holds no trade count;
// observation_count must be absent rather than whatever figure the
// synthetic row carries (it once served a 24 h prices_1m bucket count
// under a field documented as an all-time trade count).
func TestAssetGet_NativeObservationCountIsAbsent(t *testing.T) {
	assetsReader := &stubAssetsReaderExt{
		row: timescale.AssetRow{
			Slug:                       "XLM",
			AssetID:                    "native",
			Code:                       "XLM",
			ObservationCount:           2880,
			ObservationCountUnmeasured: true,
		},
	}
	reader := &stubAssetReader{
		byID: map[string]v1.AssetDetail{
			"native": {AssetID: "native", Type: "native", Code: "XLM"},
		},
	}
	srv := v1.New(v1.Options{Assets: reader, AssetsReader: assetsReader})
	ts := httpTestServer(t, srv)
	resp := mustGet(t, ts.URL+"/v1/assets/native")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !bytes.Contains(body, []byte(`"asset_id":"native"`)) {
		t.Fatalf("fixture did not reach the native detail path: %s", body)
	}
	if bytes.Contains(body, []byte(`"observation_count"`)) {
		t.Errorf("observation_count served for native XLM, which has no trade count: %s", body)
	}
}

// The curated scam warning is a static lookup on the issuer, so it must
// survive every way the catalogue-row read can come back empty, and the
// cached replay must carry it too.
func TestAssetGet_IssuerScamReason_SurvivesCatalogueRowMiss(t *testing.T) {
	const scamIssuer = "GDOEVDDBU6OBWKL7VHDAOKD77UP4DKHQYKOKJJT5PR3WRDBTX35HUEUX"
	const want = "Scam (stellar.expert)"
	asset, err := canonical.NewClassicAsset("SCAM", scamIssuer)
	if err != nil {
		t.Fatalf("NewClassicAsset: %v", err)
	}
	cases := []struct {
		name   string
		assets v1.AssetsReader
	}{
		{"row read deadline", &stubAssetsReaderExt{rowErr: context.DeadlineExceeded}},
		{"no catalogue row", &stubAssetsReaderExt{rowErr: sql.ErrNoRows}},
		{"no assets reader", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader := &stubAssetReader{byID: map[string]v1.AssetDetail{
				asset.String(): {AssetID: asset.String(), Type: "classic", Code: "SCAM", Issuer: sptr(scamIssuer)},
			}}
			srv := v1.New(v1.Options{Assets: reader, AssetsReader: tc.assets})
			ts := httpTestServer(t, srv)
			for _, pass := range []string{"fresh", "cached"} {
				resp := mustGet(t, ts.URL+"/v1/assets/"+asset.String())
				var env struct {
					Data v1.AssetDetail `json:"data"`
				}
				if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
					t.Fatalf("%s: decode: %v", pass, err)
				}
				_ = resp.Body.Close()
				if env.Data.IssuerScamReason != want {
					t.Errorf("%s: issuer_scam_reason = %q, want %q", pass, env.Data.IssuerScamReason, want)
				}
			}
		})
	}
}

// TestAssetGet_IssuerDirectoryTags_Surfaced — the scam AUD's issuer
// directory label lands on the detail payload with the exact tags,
// domain, and name from account_directory.
func TestAssetGet_IssuerDirectoryTags_Surfaced(t *testing.T) {
	srv, aud := audDetailServer(t, v1.Options{Directory: scamAUDDirectoryStub()})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/assets/"+aud.String())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var env struct {
		Data v1.AssetDetail `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	d := env.Data
	if got := d.IssuerDirectoryTags; len(got) != 2 || got[0] != "malicious" || got[1] != "unsafe" {
		t.Errorf("issuer_directory_tags = %v, want [malicious unsafe]", got)
	}
	if d.IssuerDirectoryDomain != scamAUDDomain {
		t.Errorf("issuer_directory_domain = %q, want %q", d.IssuerDirectoryDomain, scamAUDDomain)
	}
	if d.IssuerDirectoryName != "Fake AUD" {
		t.Errorf("issuer_directory_name = %q, want %q", d.IssuerDirectoryName, "Fake AUD")
	}
}

// TestAssetGet_ScamDirectoryTag_WithholdsPrice — the scam-pricing gate
// (deliberately overturning the old display-only
// invariant): a scam-class-tagged issuer (malicious/unsafe/fraud/scam/
// hack/phishing) has its published price_usd + market_cap WITHHELD, so a
// scam token can't show a value that lends it legitimacy — even when its
// market is liquid. The suppression is directory-driven: with no directory
// wired, the same asset prices normally. Raw trades stay on /v1/ohlc etc.
func TestAssetGet_ScamDirectoryTag_WithholdsPrice(t *testing.T) {
	get := func(t *testing.T, directory v1.Options) v1.AssetDetail {
		t.Helper()
		srv, aud := audDetailServer(t, directory)
		ts := httpTestServer(t, srv)
		resp := mustGet(t, ts.URL+"/v1/assets/"+aud.String())
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		var env struct {
			Data v1.AssetDetail `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return env.Data
	}

	withDir := get(t, v1.Options{Directory: scamAUDDirectoryStub()})
	withoutDir := get(t, v1.Options{})

	// The tag surfaced only on the directory-wired server.
	if len(withDir.IssuerDirectoryTags) == 0 {
		t.Fatalf("precondition: expected the malicious tag to surface with a directory wired")
	}
	if len(withoutDir.IssuerDirectoryTags) != 0 {
		t.Fatalf("precondition: no directory wired must omit the tags, got %v", withoutDir.IssuerDirectoryTags)
	}
	// Scam-flagged: price + market cap are WITHHELD (the gate).
	if withDir.PriceUSD != nil {
		t.Errorf("price_usd (scam-tagged) = %q, want withheld (nil) — the scam gate must suppress it", *withDir.PriceUSD)
	}
	if withDir.MarketCapUSD != nil {
		t.Errorf("market_cap_usd (scam-tagged) = %q, want withheld (nil)", *withDir.MarketCapUSD)
	}
	// Directory-driven: with no directory wired, the same asset prices
	// normally — proving the suppression is the flag, not a coincidence.
	if withoutDir.PriceUSD == nil || *withoutDir.PriceUSD != "0.65" {
		t.Errorf("price_usd (no directory) = %v, want 0.65 — suppression must be directory-driven", withoutDir.PriceUSD)
	}
}

// TestAssetGet_DirectoryReadFailure_WithholdsPrice — a failed
// directory read means nobody checked the issuer for a scam flag, so the
// detail page must not publish the price it would have withheld had the
// read answered. The labels stay omitted: a failed read accuses no one.
func TestAssetGet_DirectoryReadFailure_WithholdsPrice(t *testing.T) {
	down := &stubDirectoryReader{err: errors.New("account_directory: connection refused")}
	srv, aud := audDetailServer(t, v1.Options{Directory: down})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/assets/"+aud.String())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a directory outage must not fail the asset view)", resp.StatusCode)
	}
	var env struct {
		Data v1.AssetDetail `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Data.PriceUSD != nil {
		t.Errorf("price_usd = %q, want withheld — the scam check did not run", *env.Data.PriceUSD)
	}
	if len(env.Data.IssuerDirectoryTags) != 0 {
		t.Errorf("issuer_directory_tags = %v, want omitted", env.Data.IssuerDirectoryTags)
	}
}

func TestAssetGet_VolumeCharacter_Surfaced(t *testing.T) {
	stub := &stubVolumeCharacterReader{vc: scamAUDVolumeCharacter()}
	srv, aud := audDetailServer(t, v1.Options{VolumeCharacter: stub})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/assets/"+aud.String())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var env struct {
		Data v1.AssetDetail `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	d := env.Data
	if d.VolumeCharacter != "concentrated" {
		t.Errorf("volume_character = %q, want concentrated", d.VolumeCharacter)
	}
	if d.VolumeCharacterSignals == nil {
		t.Fatalf("volume_character_signals missing")
	}
	sig := d.VolumeCharacterSignals
	if sig.TopAccountPairVolShare != 0.99 {
		t.Errorf("top_account_pair_vol_share = %v, want 0.99", sig.TopAccountPairVolShare)
	}
	if !sig.IsMarketStyled {
		t.Errorf("is_market_styled = false, want true")
	}
	if sig.DistinctMakers != 2 || sig.DistinctTakers != 1 {
		t.Errorf("distinct makers/takers = %d/%d, want 2/1", sig.DistinctMakers, sig.DistinctTakers)
	}
	if sig.WindowDays != 14 {
		t.Errorf("window_days = %d, want 14", sig.WindowDays)
	}
	if sig.VolumeUSD != "2870000.00" {
		t.Errorf("volume_usd = %q, want 2870000.00", sig.VolumeUSD)
	}
}

// TestAssetGet_VolumeCharacter_VolumeUSDExact: volume_usd is rendered to
// 2dp from the rollup's NUMERIC text in big.Rat (ADR-0003). A float hop
// loses the cents above 2^53 cents and rounds the exact half-cent 1234.125
// to even ("1234.12"); an unparseable value omits the signals.
func TestAssetGet_VolumeCharacter_VolumeUSDExact(t *testing.T) {
	cases := []struct {
		name, stored, want string
	}{
		{"above_2^53_cents", "90071992547409.93", "90071992547409.93"},
		{"half_cent_rounds_away_from_zero", "1234.125", "1234.13"},
		{"extra_scale_truncated_to_cents", "5000.00449999", "5000.00"},
		{"unparseable_omits_signals", "NaN", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			vc := scamAUDVolumeCharacter()
			vc.VolumeUSD = c.stored
			srv, aud := audDetailServer(t, v1.Options{VolumeCharacter: &stubVolumeCharacterReader{vc: vc}})
			ts := httpTestServer(t, srv)
			resp := mustGet(t, ts.URL+"/v1/assets/"+aud.String())
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", resp.StatusCode)
			}
			var env struct {
				Data v1.AssetDetail `json:"data"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
				t.Fatalf("decode: %v", err)
			}
			sig := env.Data.VolumeCharacterSignals
			if c.want == "" {
				if sig != nil || env.Data.VolumeCharacter != "" {
					t.Errorf("unparseable volume_usd served signals %+v / %q, want omitted", sig, env.Data.VolumeCharacter)
				}
				return
			}
			if sig == nil {
				t.Fatalf("volume_character_signals missing")
			}
			if sig.VolumeUSD != c.want {
				t.Errorf("volume_usd = %q, want %q", sig.VolumeUSD, c.want)
			}
		})
	}
}

// TestAssetGet_VolumeCharacter_DoesNotAffectPrice — volume_character is
// analytics-only: a `concentrated` verdict must not suppress or move the
// asset's price. price_usd is byte-identical with and without the reader,
// and carries the real market price either way.
func TestAssetGet_VolumeCharacter_DoesNotAffectPrice(t *testing.T) {
	get := func(t *testing.T, opts v1.Options) v1.AssetDetail {
		t.Helper()
		srv, aud := audDetailServer(t, opts)
		ts := httpTestServer(t, srv)
		resp := mustGet(t, ts.URL+"/v1/assets/"+aud.String())
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		var env struct {
			Data v1.AssetDetail `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return env.Data
	}

	withVC := get(t, v1.Options{VolumeCharacter: &stubVolumeCharacterReader{vc: scamAUDVolumeCharacter()}})
	withoutVC := get(t, v1.Options{})

	if withVC.VolumeCharacter != "concentrated" {
		t.Fatalf("precondition: expected concentrated with the reader wired, got %q", withVC.VolumeCharacter)
	}
	if withoutVC.VolumeCharacter != "" {
		t.Fatalf("precondition: no reader wired must omit volume_character, got %q", withoutVC.VolumeCharacter)
	}
	if withVC.PriceUSD == nil || *withVC.PriceUSD != "0.65" {
		t.Errorf("price_usd (concentrated) = %v, want 0.65 — analytics must not suppress pricing", withVC.PriceUSD)
	}
	if withoutVC.PriceUSD == nil || *withoutVC.PriceUSD != "0.65" {
		t.Errorf("price_usd (no reader) = %v, want 0.65", withoutVC.PriceUSD)
	}
}

// TestAssetGet_DeclaredPeg_DustCataloguePriceReplacedByPeg — the
// detail-path half of the AUDD fix, end-to-end through
// handleAssetGet: the asset-catalogue overlay carries the dust-authored
// $0.78 price the substance gate withholds on the listing, and without the gate
// applyAssetRowToDetail would copy it onto the detail UNGATED — so the
// listing served the $0.655 peg while the detail presented $0.78 as a
// market price, and the nil-only peg fill never ran. With the overlay
// gated by the same per-pair verdict, the detail now serves the
// declared-peg price with its provenance, matching the listing.
func TestAssetGet_DeclaredPeg_DustCataloguePriceReplacedByPeg(t *testing.T) {
	srv := pegHandlerTestServer(t,
		false, // gate denies every pair — AUDD's USD books are bot dust
		timescale.AssetRow{
			AssetID:     pegHandlerTestAUDD,
			Code:        "AUDD",
			Slug:        "AUDD",
			PriceUSD:    sptr("0.7763"),
			Change1hPct: sptr("+3.10"),
			Change7dPct: sptr("-9.90"),
		})
	d := getPegAssetDetail(t, srv, pegHandlerTestAUDD)
	if d.PriceUSD == nil || *d.PriceUSD != "0.655" {
		t.Fatalf("price_usd = %s, want 0.655 (declared AUD peg × fresh AUD/USD rate, not the $0.7763 dust)", derefOrNil(d.PriceUSD))
	}
	if d.PriceBasis != "declared_peg" {
		t.Errorf("price_basis = %q, want declared_peg", d.PriceBasis)
	}
	if d.Change1hPct != nil || d.Change7dPct != nil {
		t.Errorf("gated-out row must lose the overlay change pills (got 1h=%v 7d=%v) — dust pills must not outlive their price",
			d.Change1hPct, d.Change7dPct)
	}
}

// TestAssetGet_SubstanceGate_NonPeggedDustDetailWithheld —
// closure for every asset, not just pegged ones: a NON-configured
// asset whose only price is the ungated catalogue overlay now has that
// price (and the pills derived from it) withheld on the detail path,
// matching the listing's verdict for the same pair set.
func TestAssetGet_SubstanceGate_NonPeggedDustDetailWithheld(t *testing.T) {
	dustID := "SCAM-GCQTGZQQ5G4PTM2GL7CDIFKUBIPEC52BROAQIAPW53XBRJVN6ZJVTG6V"
	srv := pegHandlerTestServer(t,
		false, // gate denies every pair
		timescale.AssetRow{
			AssetID:     dustID,
			Code:        "SCAM",
			Slug:        "SCAM",
			PriceUSD:    sptr("123.45"),
			Change1hPct: sptr("+99.00"),
			Change7dPct: sptr("+400.00"),
		})
	d := getPegAssetDetail(t, srv, dustID)
	if d.PriceUSD != nil {
		t.Fatalf("price_usd = %v, want null (substance-withheld, no peg configured)", *d.PriceUSD)
	}
	if d.PriceBasis != "" {
		t.Errorf("price_basis = %q, want absent (no fill happened)", d.PriceBasis)
	}
	if d.Change1hPct != nil || d.Change7dPct != nil {
		t.Errorf("withheld row must lose its change pills (got 1h=%v 7d=%v)", d.Change1hPct, d.Change7dPct)
	}
}

// TestAssetGet_SubstanceGate_MarketPriceSurvivesAndPegDoesNotFill —
// the floor-passing case: a real market keeps its catalogue price +
// pills on the detail, and the peg fill (nil-only) never overwrites it
// or stamps a basis, even for a peg-configured asset.
func TestAssetGet_SubstanceGate_MarketPriceSurvivesAndPegDoesNotFill(t *testing.T) {
	srv := pegHandlerTestServer(t,
		true, // a real market cleared the substance floor
		timescale.AssetRow{
			AssetID:     pegHandlerTestAUDD,
			Code:        "AUDD",
			Slug:        "AUDD",
			PriceUSD:    sptr("0.652"),
			Change1hPct: sptr("+0.11"),
			Change7dPct: sptr("-0.04"),
		})
	d := getPegAssetDetail(t, srv, pegHandlerTestAUDD)
	if d.PriceUSD == nil || *d.PriceUSD != "0.652" {
		t.Fatalf("price_usd = %s, want the surviving market price 0.652", derefOrNil(d.PriceUSD))
	}
	if d.PriceBasis != "" {
		t.Errorf("price_basis = %q, want absent (market-derived price must carry no basis)", d.PriceBasis)
	}
	if d.Change1hPct == nil || *d.Change1hPct != "+0.11" {
		t.Errorf("change_1h_pct = %v, want +0.11 (allowed row keeps its pills)", d.Change1hPct)
	}
	if d.Change7dPct == nil || *d.Change7dPct != "-0.04" {
		t.Errorf("change_7d_pct = %v, want -0.04", d.Change7dPct)
	}
}

// The issuer's standing and backing declarations reach both the asset
// detail and the metadata-only surface, with a declared false served as
// false rather than dropped.
func TestAssetGet_Sep1OverlayCurrencyAuthority(t *testing.T) {
	yes, no := true, false
	base := currencyAuthorityFixture(t, timescale.IssuerSep1Currency{
		Status:                 "live",
		IsAssetAnchored:        &yes,
		AttestationOfReserve:   "https://issuer.example.com/reserves.pdf",
		RedemptionInstructions: "Redeem through the issuer's portal.",
		Regulated:              &no,
		ApprovalServer:         "https://issuer.example.com/tx_approve",
		ApprovalCriteria:       "KYC required.",
	})

	for _, path := range []string{"/v1/assets/USDC-" + testUSDCIssuer, "/v1/assets/USDC-" + testUSDCIssuer + "/metadata"} {
		resp := mustGet(t, base+path)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status = %d", path, resp.StatusCode)
		}
		var env struct {
			Data v1.Sep1CurrencyAuthority `json:"data"`
		}
		mustDecode(t, resp, &env)
		d := env.Data
		if d.CurrencyStatus == nil || *d.CurrencyStatus != "live" {
			t.Errorf("%s: currency_status = %v, want live", path, d.CurrencyStatus)
		}
		if d.IsAssetAnchored == nil || !*d.IsAssetAnchored {
			t.Errorf("%s: is_asset_anchored = %v, want true", path, d.IsAssetAnchored)
		}
		if d.Regulated == nil || *d.Regulated {
			t.Errorf("%s: regulated = %v, want a declared false", path, d.Regulated)
		}
		if d.AttestationOfReserve == nil || *d.AttestationOfReserve != "https://issuer.example.com/reserves.pdf" {
			t.Errorf("%s: attestation_of_reserve = %v", path, d.AttestationOfReserve)
		}
		if d.RedemptionInstructions == nil || *d.RedemptionInstructions != "Redeem through the issuer's portal." {
			t.Errorf("%s: redemption_instructions = %v", path, d.RedemptionInstructions)
		}
		if d.ApprovalServer == nil || *d.ApprovalServer != "https://issuer.example.com/tx_approve" {
			t.Errorf("%s: approval_server = %v", path, d.ApprovalServer)
		}
		if d.ApprovalCriteria == nil || *d.ApprovalCriteria != "KYC required." {
			t.Errorf("%s: approval_criteria = %v", path, d.ApprovalCriteria)
		}
	}
}

// Clients render the two URL fields as links, so a non-http(s) value from
// the issuer's document is dropped while the rest of the entry still serves.
func TestAssetGet_Sep1OverlayDropsUnsafeAuthorityURLs(t *testing.T) {
	base := currencyAuthorityFixture(t, timescale.IssuerSep1Currency{
		Status:               "live",
		AttestationOfReserve: "javascript:alert(1)",
		ApprovalServer:       "https://x.example.com/\"><script>",
	})
	resp := mustGet(t, base+"/v1/assets/USDC-"+testUSDCIssuer)
	var env struct {
		Data v1.AssetDetail `json:"data"`
	}
	mustDecode(t, resp, &env)
	if env.Data.AttestationOfReserve != nil {
		t.Errorf("attestation_of_reserve = %q, want dropped", *env.Data.AttestationOfReserve)
	}
	if env.Data.ApprovalServer != nil {
		t.Errorf("approval_server = %q, want dropped", *env.Data.ApprovalServer)
	}
	if env.Data.CurrencyStatus == nil {
		t.Error("currency_status dropped alongside the unsafe URLs; the guard should be URL-only")
	}
	if env.Data.IsAssetAnchored != nil || env.Data.Regulated != nil {
		t.Errorf("undeclared booleans served as declared: is_asset_anchored=%v regulated=%v",
			env.Data.IsAssetAnchored, env.Data.Regulated)
	}
}

func TestAssetGet_Sep1OverlayVerified(t *testing.T) {
	issuer := testUSDCIssuer
	domain := "circle.com"
	reader := &stubAssetReader{
		byID: map[string]v1.AssetDetail{
			"USDC-" + testUSDCIssuer: {
				AssetID:    "USDC-" + testUSDCIssuer,
				Type:       "classic",
				Code:       "USDC",
				Issuer:     &issuer,
				HomeDomain: &domain,
				Decimals:   7,
			},
		},
	}
	sep1 := &stubSep1Cache{
		byIssuer: map[string]*timescale.IssuerSep1Cached{
			testUSDCIssuer: {
				OrgName: "Circle Internet Financial Limited",
				Currencies: []timescale.IssuerSep1Currency{{
					Code:            "USDC",
					Issuer:          testUSDCIssuer,
					Name:            "USD Coin",
					Description:     "Dollar-denominated stablecoin",
					Image:           "https://circle.com/usdc-logo.svg",
					AnchorAsset:     "USD",
					AnchorAssetType: "fiat",
					Decimals:        7,
					DisplayDecimals: 2,
				}},
			},
		},
	}

	srv := v1.New(v1.Options{Assets: reader, Sep1Cache: sep1})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/assets/USDC-"+testUSDCIssuer)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var env struct {
		Data v1.AssetDetail `json:"data"`
	}
	mustDecode(t, resp, &env)

	if env.Data.Sep1Status != "verified" {
		t.Errorf("sep1_status = %q, want verified", env.Data.Sep1Status)
	}
	if env.Data.OrgName == nil || *env.Data.OrgName != "Circle Internet Financial Limited" {
		t.Errorf("org_name not overlaid: %+v", env.Data.OrgName)
	}
	if env.Data.Name == nil || *env.Data.Name != "USD Coin" {
		t.Errorf("name not overlaid: %+v", env.Data.Name)
	}
	if env.Data.AnchorAsset == nil || *env.Data.AnchorAsset != "USD" {
		t.Errorf("anchor_asset not overlaid: %+v", env.Data.AnchorAsset)
	}
	// `decimals` is the on-chain unit scale (7 for classic) and
	// must NOT be overwritten by the issuer's display_decimals — doing so
	// would inflate market-cap math by 10^5×. The rounding hint rides
	// display_decimals instead.
	if env.Data.Decimals != 7 {
		t.Errorf("decimals = %d, want 7 (on-chain scale, NOT display_decimals)", env.Data.Decimals)
	}
	if env.Data.DisplayDecimals == nil || *env.Data.DisplayDecimals != 2 {
		t.Errorf("display_decimals = %v, want 2", env.Data.DisplayDecimals)
	}
}

func TestAssetGet_Sep1OverlayRejectsHostileImageURL(t *testing.T) {
	// An issuer's stellar.toml is attacker-controlled ground for any
	// asset on its domain. A hostile image like "javascript:alert(1)"
	// could be served back to browser-based API consumers. Verify
	// non-http(s) schemes are dropped, not propagated.
	issuer := testUSDCIssuer
	domain := "evil.example.com"
	reader := &stubAssetReader{
		byID: map[string]v1.AssetDetail{
			"USDC-" + testUSDCIssuer: {
				AssetID: "USDC-" + testUSDCIssuer, Type: "classic",
				Code: "USDC", Issuer: &issuer, HomeDomain: &domain,
				Decimals: 7,
			},
		},
	}

	for _, badImage := range []string{
		"javascript:alert(1)",
		"data:text/html,<script>alert(1)</script>",
		"file:///etc/passwd",
		"blob:abc",
		"//protocol-relative.example.com/x.png",
		"   not a url   ",
	} {
		sep1 := &stubSep1Cache{
			byIssuer: map[string]*timescale.IssuerSep1Cached{
				testUSDCIssuer: {Currencies: []timescale.IssuerSep1Currency{{
					Code: "USDC", Issuer: testUSDCIssuer,
					Name: "X", Image: badImage,
				}}},
			},
		}
		srv := v1.New(v1.Options{Assets: reader, Sep1Cache: sep1})
		ts := httpTestServer(t, srv)

		resp := mustGet(t, ts.URL+"/v1/assets/USDC-"+testUSDCIssuer)
		var env struct {
			Data v1.AssetDetail `json:"data"`
		}
		mustDecode(t, resp, &env)
		if env.Data.Image != nil {
			t.Errorf("image = %q; hostile URL %q should have been dropped",
				*env.Data.Image, badImage)
		}
		if env.Data.Name == nil {
			t.Errorf("name dropped alongside hostile image %q — guard should be image-only", badImage)
		}
	}
}

func TestAssetGet_Sep1OverlayNoMatch(t *testing.T) {
	// SEP-1 loads, but the currency under a different issuer.
	issuer := testUSDCIssuer
	domain := "example.com"
	reader := &stubAssetReader{
		byID: map[string]v1.AssetDetail{
			"USDC-" + testUSDCIssuer: {
				AssetID: "USDC-" + testUSDCIssuer, Type: "classic", Code: "USDC",
				Issuer: &issuer, HomeDomain: &domain, Decimals: 7,
			},
		},
	}
	sep1 := &stubSep1Cache{
		byIssuer: map[string]*timescale.IssuerSep1Cached{
			testUSDCIssuer: {
				OrgName: "Someone Else",
				Currencies: []timescale.IssuerSep1Currency{{
					Code: "USDC", Issuer: "GSOMEONEELSEXXXXXXX", Name: "Fake",
				}},
			},
		},
	}

	srv := v1.New(v1.Options{Assets: reader, Sep1Cache: sep1})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/assets/USDC-"+testUSDCIssuer)
	var env struct {
		Data v1.AssetDetail `json:"data"`
	}
	mustDecode(t, resp, &env)

	if env.Data.Sep1Status != "no_match" {
		t.Errorf("sep1_status = %q, want no_match", env.Data.Sep1Status)
	}
	if env.Data.Name != nil {
		t.Errorf("should NOT overlay currency fields on issuer mismatch")
	}
	if env.Data.OrgName == nil || *env.Data.OrgName != "Someone Else" {
		t.Errorf("org_name should be surfaced even on no_match: %+v", env.Data.OrgName)
	}
}

func TestAssetGet_Sep1OverlayRefusesNonClassicMatch(t *testing.T) {
	// Soroban / native / fiat assets must NOT match any SEP-1 currency
	// entry — the cached overlay short-circuits on non-classic types
	// before even hitting the lookup.
	domain := "circle.com"
	// A GENERIC Soroban token — deliberately NOT the XLM SAC
	// (CAS3J7GY…), which the /v1/assets handler normalizes to native as
	// an XLM alias (so it would not exercise the Soroban path).
	sorobanContract := "CCT4ZYIYZ3TUO2AWQFEOFGBZ6HQP3GW5TA37CK7CRZVFRDXYTHTYX7KP"
	reader := &stubAssetReader{
		byID: map[string]v1.AssetDetail{
			sorobanContract: {
				AssetID:    sorobanContract,
				Type:       "soroban",
				ContractID: &sorobanContract,
				HomeDomain: &domain,
				Decimals:   7,
			},
		},
	}
	sep1 := &stubSep1Cache{
		byIssuer: map[string]*timescale.IssuerSep1Cached{
			testUSDCIssuer: {
				OrgName: "Circle",
				Currencies: []timescale.IssuerSep1Currency{{
					Code: "USDC", Issuer: testUSDCIssuer, Name: "USD Coin",
				}},
			},
		},
	}

	srv := v1.New(v1.Options{Assets: reader, Sep1Cache: sep1})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/assets/"+sorobanContract)
	var env struct {
		Data v1.AssetDetail `json:"data"`
	}
	mustDecode(t, resp, &env)

	if env.Data.Sep1Status != "not_applicable" {
		t.Errorf("sep1_status = %q, want not_applicable (Soroban has no issuer to look up)", env.Data.Sep1Status)
	}
	if env.Data.Name != nil {
		t.Errorf("Soroban asset should NOT inherit USDC's Name: %v", env.Data.Name)
	}
}

// TestAssetGet_Sep1NotFetched: an issuer the sep1-refresh cron has not visited
// yet, or a server with no Sep1Cache at all, surfaces "not_fetched" with a 200
// rather than crashing or stalling.
func TestAssetGet_Sep1NotFetched(t *testing.T) {
	issuer := testUSDCIssuer
	domain := "circle.com"
	reader := &stubAssetReader{
		byID: map[string]v1.AssetDetail{
			"USDC-" + testUSDCIssuer: {
				AssetID: "USDC-" + testUSDCIssuer, Type: "classic", Code: "USDC",
				Issuer: &issuer, HomeDomain: &domain, Decimals: 7,
			},
		},
	}
	for _, tc := range []struct {
		name string
		opts v1.Options
	}{
		{"issuer not in cache", v1.Options{Assets: reader, Sep1Cache: &stubSep1Cache{err: errors.New("dns: nxdomain")}}},
		{"cache unwired", v1.Options{Assets: reader}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := httpTestServer(t, v1.New(tc.opts))
			resp := mustGet(t, ts.URL+"/v1/assets/USDC-"+testUSDCIssuer)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200 (cache miss is not fatal)", resp.StatusCode)
			}
			var body struct {
				Data v1.AssetDetail `json:"data"`
			}
			mustDecode(t, resp, &body)
			if body.Data.Sep1Status != "not_fetched" {
				t.Errorf("sep1_status = %q, want not_fetched", body.Data.Sep1Status)
			}
		})
	}
}

// With no reader wired, GET /v1/assets/{id} echoes the canonical decode.
func TestAssetGet_CanonicalEcho(t *testing.T) {
	cases := []struct {
		name, path, wantType, wantCode string
		wantIssuer                     string // empty: not asserted
		wantAssetID                    string // empty: not asserted
	}{
		{"native", "native", "native", "", "", "native"},
		{"classic", "USDC-" + testUSDCIssuer, "classic", "USDC", testUSDCIssuer, ""},
		{"fiat (ADR-0010)", "fiat:USD", "fiat", "USD", "", ""},
	}
	ts := httpTestServer(t, v1.New(v1.Options{}))
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := mustGet(t, ts.URL+"/v1/assets/"+tc.path)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", resp.StatusCode)
			}
			var body struct {
				Data v1.AssetDetail `json:"data"`
			}
			mustDecode(t, resp, &body)
			d := body.Data
			if d.Type != tc.wantType || (tc.wantCode != "" && d.Code != tc.wantCode) ||
				(tc.wantAssetID != "" && d.AssetID != tc.wantAssetID) {
				t.Errorf("wrong decode: %+v", d)
			}
			if tc.wantIssuer != "" && (d.Issuer == nil || *d.Issuer != tc.wantIssuer) {
				t.Errorf("issuer missing: %+v", d.Issuer)
			}
		})
	}
}

func TestAssetGet_invalidIdReturns400(t *testing.T) {
	srv := v1.New(v1.Options{})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/assets/garbage-but-not-any-format")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("content-type = %q, want problem+json", ct)
	}
}

func TestAssetGet_notFound(t *testing.T) {
	reader := &stubAssetReader{byID: map[string]v1.AssetDetail{}}
	srv := v1.New(v1.Options{Assets: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/assets/native")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, "asset-not-found") {
		t.Errorf("body missing error type: %s", body)
	}
}

func TestAssetGet_readerPopulatesSep1Status(t *testing.T) {
	// When the reader returns a detail, we use its fields verbatim
	// (vs canonical-echo which fills defaults).
	issuer := testUSDCIssuer
	domain := "circle.com"
	reader := &stubAssetReader{
		byID: map[string]v1.AssetDetail{
			"USDC-" + testUSDCIssuer: {
				AssetID:    "USDC-" + testUSDCIssuer,
				Type:       "classic",
				Code:       "USDC",
				Issuer:     &issuer,
				HomeDomain: &domain,
				Decimals:   7,
				Sep1Status: "verified",
			},
		},
	}
	srv := v1.New(v1.Options{Assets: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/assets/USDC-"+testUSDCIssuer)
	var env struct {
		Data v1.AssetDetail `json:"data"`
	}
	mustDecode(t, resp, &env)
	if env.Data.Sep1Status != "verified" || env.Data.HomeDomain == nil {
		t.Fatalf("reader fields lost: %+v", env.Data)
	}
}

// TestAssetGet_Kind_SetForReaderPathAndSurvivesResponseCache proves
// the ADR-0042 cache trap is closed. stubAssetReader — like
// the real storage.timescale AssetReader implementation — has no
// reason to know about the `kind` wire-shape discriminator, so its
// fixture row below deliberately carries a zero-value Kind, the same
// as a not-yet-updated storage layer would. handleAssetGet must stamp
// Kind AFTER resolveAssetDetail returns but BEFORE renderAssetDetailEnvelope
// caches the rendered bytes (assets.go's 30s assetDetailCache) — a fix
// applied only on the FIRST response would leave the cached bytes
// permanently missing `kind` for the remainder of the TTL window. This
// test issues the request twice: once to populate the cache, once to
// hit it, and requires `kind` on both.
func TestAssetGet_Kind_SetForReaderPathAndSurvivesResponseCache(t *testing.T) {
	reader := &stubAssetReader{
		byID: map[string]v1.AssetDetail{
			"native": {
				// Kind intentionally left zero-valued — simulates a
				// storage-layer AssetDetail that doesn't set it.
				AssetID:    "native",
				Type:       "native",
				Code:       "XLM",
				Decimals:   7,
				Sep1Status: "not_applicable",
			},
		},
	}
	srv := v1.New(v1.Options{Assets: reader})
	ts := httpTestServer(t, srv)

	for i, label := range []string{"first (uncached)", "second (cache hit)"} {
		resp := mustGet(t, ts.URL+"/v1/assets/native")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status = %d", label, resp.StatusCode)
		}
		var env struct {
			Data v1.AssetDetail `json:"data"`
		}
		mustDecode(t, resp, &env)
		if env.Data.Kind != "stellar_asset" {
			t.Errorf("%s request (i=%d): kind = %q, want \"stellar_asset\" — the reader.GetAsset path must have Kind stamped before the response is cached, not left to the (Kind-unaware) storage layer", label, i, env.Data.Kind)
		}
	}
}

// TestAssetGet_DeadContextBodyNotCached is the regression proof for
// handleAssetGet needs a liveness gate between its cache miss
// and its assetDetailCache.put, so a request whose context died mid-
// chain (client gone, or the blanket request-timeout deadline) still
// cached whatever best-effort body it had assembled and replayed it
// as a fresh 200 for the full 120s TTL. A second, healthy request for
// the same asset_id must therefore recompute rather than replay the
// first (dead-context) render.
func TestAssetGet_DeadContextBodyNotCached(t *testing.T) {
	reader := &ctxDeadCountingReader{}
	srv := v1.New(v1.Options{Assets: reader})
	h := srv.Handler()

	// Request 1: context already dead by the time it reaches the
	// handler — simulating a client abort or a fired request-timeout
	// deadline partway through the enrichment chain.
	deadCtx, cancel := context.WithCancel(context.Background())
	cancel()
	req1 := httptest.NewRequest(http.MethodGet, "/v1/assets/native", nil).WithContext(deadCtx)
	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("request 1 (dead context): status = %d", rec1.Code)
	}

	// Request 2: healthy context, same asset_id, well inside the 120s
	// TTL. Must NOT be served from a cache entry request 1 wrote.
	req2 := httptest.NewRequest(http.MethodGet, "/v1/assets/native", nil)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("request 2 (healthy context): status = %d", rec2.Code)
	}

	var env struct {
		Data v1.AssetDetail `json:"data"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode request 2 body: %v", err)
	}
	if reader.calls != 2 {
		t.Fatalf("GetAsset called %d times, want 2 — the second request must recompute, not replay a body cached from the dead-context request", reader.calls)
	}
	if env.Data.Code != "CALL2" {
		t.Fatalf("second request served code %q, want CALL2 — it must reflect the SECOND (healthy-context) GetAsset call, not a cached body from the dead-context request",
			env.Data.Code)
	}
}

// TestAssetGet_ClassicSlugResolves — /v1/assets/{slug} must resolve a
// migration-0134 public slug to its (code, issuer) identity when the
// wired reader offers the capability, and keep the 400 for genuinely
// unresolvable ids. Without this, slug URLs would be resolvable ONLY through
// the explorer's build cache, so any page not baked at build time
// 404 (operator report: /assets/usdt-gasu4kif).
func TestAssetGet_ClassicSlugResolves(t *testing.T) {
	const usdtID = "USDT-GCQTGZQQ5G4PTM2GL7CDIFKUBIPEC52BROAQIAPW53XBRJVN6ZJVTG6V"
	reader := slugStubAssetReader{
		AssetReader: &stubAssetReader{byID: map[string]v1.AssetDetail{
			usdtID: {AssetID: usdtID, Type: "classic", Code: "USDT", Decimals: 7},
		}},
		code:   "USDT",
		issuer: "GCQTGZQQ5G4PTM2GL7CDIFKUBIPEC52BROAQIAPW53XBRJVN6ZJVTG6V",
	}
	srv := v1.New(v1.Options{Assets: reader})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/assets/usdt-gcqtgzqq")
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "USDT-GCQTGZQQ5G4PTM2GL7CDIFKUBIPEC52BROAQIAPW53XBRJVN6ZJVTG6V") {
		t.Errorf("resolved detail must carry the full asset_id: %s", body)
	}

	resp2 := mustGet(t, ts.URL+"/v1/assets/definitely-not-a-slug")
	if resp2.StatusCode != http.StatusBadRequest {
		t.Errorf("unresolvable id status = %d, want 400", resp2.StatusCode)
	}
}

func TestAssetGet_VerifiedAsset_NoWarning(t *testing.T) {
	// The real Circle USDC matches the catalogue's verified entry
	// exactly — no warning, no flag.
	srv := v1.New(v1.Options{VerifiedCurrencies: newTestCatalogue(t)})
	ts := httpTestServer(t, srv)

	url := ts.URL + "/v1/assets/USDC-" + testUSDCIssuer
	resp := mustGet(t, url)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var env struct {
		Data  v1.AssetDetail `json:"data"`
		Flags v1.Flags       `json:"flags"`
	}
	mustDecode(t, resp, &env)
	if env.Data.UnverifiedWarning != nil {
		t.Errorf("UnverifiedWarning attached to verified USDC: %+v", env.Data.UnverifiedWarning)
	}
	if env.Flags.UnverifiedTickerCollision {
		t.Error("flags.unverified_ticker_collision = true on verified asset")
	}
}

func TestAssetGet_TickerCollision_AttachesWarning(t *testing.T) {
	// USDC ticker with a fake issuer — warning + flag should fire.
	srv := v1.New(v1.Options{VerifiedCurrencies: newTestCatalogue(t)})
	ts := httpTestServer(t, srv)

	url := ts.URL + "/v1/assets/USDC-" + otherRealIssuer
	resp := mustGet(t, url)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	var env struct {
		Data  v1.AssetDetail `json:"data"`
		Flags v1.Flags       `json:"flags"`
	}
	mustDecode(t, resp, &env)

	if !env.Flags.UnverifiedTickerCollision {
		t.Error("flags.unverified_ticker_collision = false on collision")
	}
	if env.Data.UnverifiedWarning == nil {
		t.Fatal("UnverifiedWarning not attached")
	}
	w := env.Data.UnverifiedWarning
	if w.VerifiedSlug != "usdc" {
		t.Errorf("verified_slug = %q, want usdc", w.VerifiedSlug)
	}
	if w.VerifiedAssetID != "USDC-"+testUSDCIssuer {
		t.Errorf("verified_asset_id = %q, want USDC-%s", w.VerifiedAssetID, testUSDCIssuer)
	}
	if w.VerifiedName != "USD Coin" {
		t.Errorf("verified_name = %q, want USD Coin", w.VerifiedName)
	}
	if w.VerifiedIssuer == "" {
		t.Error("verified_issuer empty — expected an attribution label")
	}
	if !strings.Contains(w.Note, "USDC") || !strings.Contains(w.Note, testUSDCIssuer) {
		t.Errorf("note doesn't mention USDC + issuer: %q", w.Note)
	}
}

func TestAssetGet_NoCatalogue_NoWarning(t *testing.T) {
	// When the catalogue isn't wired (operator hasn't set
	// VerifiedCurrencies on Options), no warning surface appears —
	// even for known collisions. This is the pre-Phase-1.1 behaviour.
	srv := v1.New(v1.Options{}) // no VerifiedCurrencies
	ts := httpTestServer(t, srv)

	url := ts.URL + "/v1/assets/USDC-" + otherRealIssuer
	resp := mustGet(t, url)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var env struct {
		Data  v1.AssetDetail `json:"data"`
		Flags v1.Flags       `json:"flags"`
	}
	mustDecode(t, resp, &env)
	if env.Data.UnverifiedWarning != nil {
		t.Errorf("UnverifiedWarning attached without a catalogue: %+v", env.Data.UnverifiedWarning)
	}
	if env.Flags.UnverifiedTickerCollision {
		t.Error("flags.unverified_ticker_collision = true without a catalogue")
	}
}

// A code no verified currency claims on Stellar gets no warning either, even
// with a syntactically valid but unknown issuer.
func TestAssetGet_NativeFiatAndUnknownCode_NoWarning(t *testing.T) {
	srv := v1.New(v1.Options{VerifiedCurrencies: newTestCatalogue(t)})
	ts := httpTestServer(t, srv)

	for _, path := range []string{"/v1/assets/native", "/v1/assets/fiat:USD", "/v1/assets/XYZWHATEVER-" + otherRealIssuer} {
		t.Run(path, func(t *testing.T) {
			resp := mustGet(t, ts.URL+path)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", resp.StatusCode)
			}
			var env struct {
				Data  v1.AssetDetail `json:"data"`
				Flags v1.Flags       `json:"flags"`
			}
			mustDecode(t, resp, &env)
			if env.Data.UnverifiedWarning != nil {
				t.Errorf("warning attached: %+v", env.Data.UnverifiedWarning)
			}
			if env.Flags.UnverifiedTickerCollision {
				t.Error("collision flag set")
			}
		})
	}
}

func TestAssetGet_WarningSerialisationShape(t *testing.T) {
	// Lock the exact JSON keys the explorer + Freighter will consume.
	// Renaming any field is a wire-shape break.
	srv := v1.New(v1.Options{VerifiedCurrencies: newTestCatalogue(t)})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/assets/USDC-"+otherRealIssuer)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(raw["data"], &data); err != nil {
		t.Fatalf("decode data: %v", err)
	}
	warning, ok := data["unverified_warning"]
	if !ok {
		t.Fatal("data.unverified_warning missing from JSON body")
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(warning, &keys); err != nil {
		t.Fatalf("decode warning: %v", err)
	}
	for _, k := range []string{"verified_slug", "verified_asset_id", "verified_name", "verified_issuer", "note"} {
		if _, present := keys[k]; !present {
			t.Errorf("warning missing key %q", k)
		}
	}

	var flags map[string]json.RawMessage
	if err := json.Unmarshal(raw["flags"], &flags); err != nil {
		t.Fatalf("decode flags: %v", err)
	}
	if _, ok := flags["unverified_ticker_collision"]; !ok {
		t.Error("flags.unverified_ticker_collision missing from JSON body")
	}
}

// TestAssetGet_NonUSDFiat_ServesPriceFromFXQuotes pins that the
// asset DETAIL page for a non-USD fiat must resolve price_usd (and hence
// market_cap_usd) through the same fx_quotes-first chain the asset LISTING
// already used.
//
// The two paths must not drift. The listing path tries fx_quotes
// before PriceReader; populateFiatView must too rather than
// calling PriceReader alone. storePriceReader fast-paths ANY
// fiat-quoted request to ErrPriceNotFound — no on-chain trades exist for a
// fiat/fiat pair — so the detail endpoint served price_usd: null and
// market_cap_usd: null for every non-USD currency, while the listing beside
// it showed correct values for the same asset.
//
// Fiats are non-Stellar, so the detail page is /v1/external/assets/{slug}
// (they are routed off /v1/assets). This test wires an FX reader and NO PriceReader at all, so a non-null price
// proves the fx_quotes path ran rather than a price reader happening to
// answer.
//
// Without the fx_quotes-first chain in populateFiatView both fields would be null.
func TestAssetGet_NonUSDFiat_ServesPriceFromFXQuotes(t *testing.T) {
	// 1 EUR = 1.17 USD.
	fx := &stubFXHistoryReader{points: []v1.FXQuotePoint{
		{Bucket: time.Now().UTC().Add(-24 * time.Hour), InverseUSDText: "1.17"},
	}}
	srv := v1.New(v1.Options{
		VerifiedCurrencies: newTestCatalogue(t),
		FXHistory:          fx,
		// Prices deliberately nil: the fiat->USD answer must come from
		// fx_quotes, which is the whole point of the fix.
	})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/external/assets/euro")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data struct {
			PriceUSD     *string  `json:"price_usd"`
			MarketCapUSD *string  `json:"market_cap_usd"`
			PriceSources []string `json:"price_sources"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Data.PriceUSD == nil {
		t.Fatalf("price_usd = null — the fiat detail page must resolve a price via " +
			"fx_quotes; PriceReader alone always fails for a fiat/fiat pair (COR-14)")
	}
	if got := *env.Data.PriceUSD; !strings.HasPrefix(got, "1.17") {
		t.Errorf("price_usd = %q, want ~1.17 (the fx_quotes InverseUSD, not RateUSD — "+
			"swapping them is a 24000x error for JPY)", got)
	}
	// market_cap_usd is derived from the same price, so it must follow.
	if env.Data.MarketCapUSD == nil {
		t.Errorf("market_cap_usd = null despite a resolved price_usd")
	}
}

// TestAssetGet_OnChainHomeDomainBeatsCuratedMap — when the storage
// row has no home_domain, the backfill must consult the live
// on-chain account state BEFORE the curated knownIssuers map.
func TestAssetGet_OnChainHomeDomainBeatsCuratedMap(t *testing.T) {
	issuer := testUSDCIssuer // present in knownIssuers as circle.com
	reader := &stubAssetReader{
		byID: map[string]v1.AssetDetail{
			"USDC-" + testUSDCIssuer: {
				AssetID:  "USDC-" + testUSDCIssuer,
				Type:     "classic",
				Code:     "USDC",
				Issuer:   &issuer,
				Decimals: 7,
			},
		},
	}
	explorer := &stubExplorerReader{
		accountState: clickhouse.AccountState{
			Exists:     true,
			HomeDomain: "live-onchain.example",
		},
	}
	srv := v1.New(v1.Options{Assets: reader, Explorer: explorer})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/assets/USDC-"+testUSDCIssuer)
	var env struct {
		Data v1.AssetDetail `json:"data"`
	}
	mustDecode(t, resp, &env)

	if env.Data.HomeDomain == nil || *env.Data.HomeDomain != "live-onchain.example" {
		t.Errorf("HomeDomain = %v, want live-onchain.example (on-chain must beat the curated map)",
			env.Data.HomeDomain)
	}
}

// TestAssetGet_CuratedMapStillFillsWhenChainSilent — no explorer
// reader wired (or the account is unobserved) keeps the curated map
// as the working fallback; this is the established behavior
// the precedence change must not regress.
func TestAssetGet_CuratedMapStillFillsWhenChainSilent(t *testing.T) {
	issuer := testUSDCIssuer
	reader := &stubAssetReader{
		byID: map[string]v1.AssetDetail{
			"USDC-" + testUSDCIssuer: {
				AssetID:  "USDC-" + testUSDCIssuer,
				Type:     "classic",
				Code:     "USDC",
				Issuer:   &issuer,
				Decimals: 7,
			},
		},
	}
	explorer := &stubExplorerReader{
		accountState: clickhouse.AccountState{Exists: false},
	}
	srv := v1.New(v1.Options{Assets: reader, Explorer: explorer})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/assets/USDC-"+testUSDCIssuer)
	var env struct {
		Data v1.AssetDetail `json:"data"`
	}
	mustDecode(t, resp, &env)

	if env.Data.HomeDomain == nil || *env.Data.HomeDomain != "circle.com" {
		t.Errorf("HomeDomain = %v, want circle.com (curated fallback when chain has no observation)",
			env.Data.HomeDomain)
	}
}

// TestAssetGet_TickerCollision_NetworkScoped pins that the look-alike
// warning, which points at a pubnet issuer, is not stamped on a test net.
func TestAssetGet_TickerCollision_NetworkScoped(t *testing.T) {
	opts := v1.Options{VerifiedCurrencies: newTestCatalogue(t)}
	decode := func(body string) (bool, bool) {
		var env struct {
			Data  v1.AssetDetail `json:"data"`
			Flags v1.Flags       `json:"flags"`
		}
		if err := json.Unmarshal([]byte(body), &env); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return env.Flags.UnverifiedTickerCollision, env.Data.UnverifiedWarning != nil
	}
	path := "/v1/assets/USDC-" + otherRealIssuer
	_, body := networkGet(t, opts, "pubnet", path)
	if flag, warning := decode(body); !flag || !warning {
		t.Fatalf("pubnet: collision flag=%v warning=%v on a USDC look-alike, want both", flag, warning)
	}
	for _, network := range testNets {
		status, body := networkGet(t, opts, network, path)
		if status != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", network, status)
		}
		if flag, warning := decode(body); flag || warning {
			t.Errorf("%s: collision flag=%v warning=%v, want neither", network, flag, warning)
		}
	}
}
