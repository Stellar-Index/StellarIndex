package v1_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/currency"
)

// otherRealIssuer is a real, CRC-valid Stellar G-strkey unrelated to
// Circle. The collision tests reuse it as a stand-in "someone else
// issuing USDC on Stellar" so the asset_id passes the canonical
// strkey validator (which checks the CRC, not just the prefix).
// Borrowed from internal/api/v1/known_issuers.go — Aquarius's AQUA
// issuer; we're not testing AQUA here, just needing a different
// real G-strkey to pair with the USDC code.
const otherRealIssuer = "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"

func newTestCatalogue(t *testing.T) *currency.Catalogue {
	t.Helper()
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatalf("currency.LoadEmbedded: %v", err)
	}
	return cat
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

func TestAssetGet_NativeAndFiat_NoWarning(t *testing.T) {
	srv := v1.New(v1.Options{VerifiedCurrencies: newTestCatalogue(t)})
	ts := httpTestServer(t, srv)

	for _, path := range []string{"/v1/assets/native", "/v1/assets/fiat:USD"} {
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

func TestAssetGet_UnknownCode_NoWarning(t *testing.T) {
	// A code that no verified currency claims on Stellar → no
	// warning, even with a syntactically-valid-but-unknown issuer.
	srv := v1.New(v1.Options{VerifiedCurrencies: newTestCatalogue(t)})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/assets/XYZWHATEVER-"+otherRealIssuer)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var env struct {
		Data  v1.AssetDetail `json:"data"`
		Flags v1.Flags       `json:"flags"`
	}
	mustDecode(t, resp, &env)
	if env.Data.UnverifiedWarning != nil {
		t.Errorf("warning attached on unknown code: %+v", env.Data.UnverifiedWarning)
	}
	if env.Flags.UnverifiedTickerCollision {
		t.Error("collision flag set on unknown code")
	}
}

func TestAssetsVerified_ListsCatalogue(t *testing.T) {
	srv := v1.New(v1.Options{VerifiedCurrencies: newTestCatalogue(t)})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/assets/verified")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var env struct {
		Data []v1.VerifiedCurrencyListItem `json:"data"`
	}
	mustDecode(t, resp, &env)

	if len(env.Data) < 10 {
		t.Fatalf("got %d entries; seed has at least 10", len(env.Data))
	}

	bySlug := map[string]v1.VerifiedCurrencyListItem{}
	for _, e := range env.Data {
		bySlug[e.Slug] = e
	}
	usdc, ok := bySlug["usdc"]
	if !ok {
		t.Fatal("usdc entry missing from /v1/assets/verified")
	}
	if usdc.Ticker != "USDC" || usdc.Name != "USD Coin" {
		t.Errorf("usdc entry: %+v", usdc)
	}

	xlm, ok := bySlug["xlm"]
	if !ok {
		t.Fatal("xlm entry missing")
	}
	if xlm.Ticker != "XLM" {
		t.Errorf("xlm ticker = %q", xlm.Ticker)
	}
}

func TestAssetsVerified_NoCatalogue_503(t *testing.T) {
	srv := v1.New(v1.Options{})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/assets/verified")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}

// TestAssetsVerified_FiatMarketCap — verifies fiat rows in the
// listing carry computed market_cap_usd; crypto/stablecoin rows
// don't. The fiat fan-out queries the global-price reader; we
// stub a fixed FX rate to make the math predictable.
//
// Pulls every USD row (price = 1.00 → cap = supply) and one
// non-USD row to exercise the reader path.
func TestAssetsVerified_FiatMarketCap(t *testing.T) {
	cat := newTestCatalogue(t)
	// Stub PriceReader with a 0.14 FX rate for every fiat:CCY/fiat:USD
	// pair. Production reads the same path /v1/price uses, which
	// includes the Redis-triangulated fallback when prices_1m
	// misses for fiat:fiat pairs.
	snapshots := map[string]v1.PriceSnapshot{}
	for _, vc := range cat.ByClass(currency.ClassFiat) {
		if vc.Ticker == "USD" {
			continue
		}
		key := "fiat:" + vc.Ticker + "/fiat:USD"
		snapshots[key] = v1.PriceSnapshot{
			AssetID: "fiat:" + vc.Ticker, Quote: "fiat:USD",
			Price: "0.14000000000000", PriceType: "vwap",
		}
	}
	priceStub := &stubPriceReader{snapshots: snapshots}

	srv := v1.New(v1.Options{
		VerifiedCurrencies: cat,
		Prices:             priceStub,
	})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/assets/verified")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var env struct {
		Data []v1.VerifiedCurrencyListItem `json:"data"`
	}
	mustDecode(t, resp, &env)

	bySlug := map[string]v1.VerifiedCurrencyListItem{}
	for _, e := range env.Data {
		bySlug[e.Slug] = e
	}

	// USD: identity → cap = supply = "21700000000000.00"
	usd := bySlug["us-dollar"]
	if usd.MarketCapUSD != "21700000000000.00" {
		t.Errorf("USD market_cap_usd = %q, want 21700000000000.00", usd.MarketCapUSD)
	}
	// CNY: supply=302T × 0.14 → "42280000000000.00"
	cny := bySlug["chinese-yuan"]
	if cny.MarketCapUSD != "42280000000000.00" {
		t.Errorf("CNY market_cap_usd = %q, want 42280000000000.00", cny.MarketCapUSD)
	}
	// Crypto rows: no market_cap_usd (catalogue has no supply for crypto)
	xlm := bySlug["xlm"]
	if xlm.MarketCapUSD != "" {
		t.Errorf("XLM (crypto) market_cap_usd = %q; want empty (catalogue carries no supply)", xlm.MarketCapUSD)
	}
	usdc := bySlug["usdc"]
	if usdc.MarketCapUSD != "" {
		t.Errorf("USDC (stablecoin) market_cap_usd = %q; want empty", usdc.MarketCapUSD)
	}
}

// TestAssetsVerified_FiatMarketCap_FXHistoryOnlyNoPriceReader is the
// COR-14 regression: attachFiatMarketCaps used to skip its ENTIRE
// fan-out whenever PriceReader (s.prices) was nil, even though
// fiatMarketCapUSD tries fxHistory FIRST and only falls back to
// PriceReader. A deployment that wires FXHistory but not PriceReader —
// or even the USD row itself, which needs NEITHER reader (identity
// price) — got every fiat market_cap_usd silently blanked out. Here
// only FXHistory is wired (Prices is omitted/nil) and both USD
// (identity path, no reader needed at all) and CNY (fxHistory path)
// must still carry a market_cap_usd.
func TestAssetsVerified_FiatMarketCap_FXHistoryOnlyNoPriceReader(t *testing.T) {
	cat := newTestCatalogue(t)
	// USD-base rate for CNY: 1 USD = 7.14286 CNY → InverseUSD ~= 0.14.
	fx := &stubFXHistoryReader{points: []v1.FXQuotePoint{
		{Bucket: time.Now().UTC(), RateUSD: 1 / 0.14, InverseUSD: 0.14},
	}}

	srv := v1.New(v1.Options{
		VerifiedCurrencies: cat,
		FXHistory:          fx,
		// Prices deliberately omitted (nil) — attachFiatMarketCaps must
		// not gate its fan-out on PriceReader alone.
	})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/assets/verified")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var env struct {
		Data []v1.VerifiedCurrencyListItem `json:"data"`
	}
	mustDecode(t, resp, &env)

	bySlug := map[string]v1.VerifiedCurrencyListItem{}
	for _, e := range env.Data {
		bySlug[e.Slug] = e
	}

	// USD needs no reader at all (identity price) — must still be
	// populated even though PriceReader is nil.
	usd := bySlug["us-dollar"]
	if usd.MarketCapUSD != "21700000000000.00" {
		t.Errorf("USD market_cap_usd = %q, want 21700000000000.00 (no reader needed at all — COR-14)", usd.MarketCapUSD)
	}
	// CNY resolves via fxHistory alone: supply=302T × 0.14 → 42280000000000.00
	cny := bySlug["chinese-yuan"]
	if cny.MarketCapUSD != "42280000000000.00" {
		t.Errorf("CNY market_cap_usd = %q, want 42280000000000.00 (fxHistory-only path — COR-14)", cny.MarketCapUSD)
	}
}

func TestAssetsVerified_StaticPathDoesNotShadowSlugDispatch(t *testing.T) {
	// /v1/assets/verified must route to the catalogue listing
	// handler, NOT collapse onto /v1/assets/{asset_id} where
	// "verified" would be parsed as an asset_id (and 400 on the
	// canonical-id check). Go 1.22+ ServeMux picks the more-
	// specific pattern; this test pins that behaviour.
	srv := v1.New(v1.Options{VerifiedCurrencies: newTestCatalogue(t)})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/assets/verified")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (slug dispatch shadowed the static route?)", resp.StatusCode)
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

// TestAssetGet_NonUSDFiat_ServesPriceFromFXQuotes pins the COR-14 fix: the
// asset DETAIL page for a non-USD fiat must resolve price_usd (and hence
// market_cap_usd) through the same fx_quotes-first chain the asset LISTING
// already used.
//
// The two paths had drifted. The listing path was fixed under COR-14
// (50c93ecd) to try fx_quotes before PriceReader; populateFiatView was never
// updated and called PriceReader alone. storePriceReader fast-paths ANY
// fiat-quoted request to ErrPriceNotFound — no on-chain trades exist for a
// fiat/fiat pair — so the detail endpoint served price_usd: null and
// market_cap_usd: null for every non-USD currency, while the listing beside
// it showed correct values for the same asset.
//
// Fiats are non-Stellar, so the detail page is /v1/external/assets/{slug}
// (LC-001 routes them off /v1/assets). This test wires an FX reader and NO PriceReader at all, so a non-null price
// proves the fx_quotes path ran rather than a price reader happening to
// answer.
//
// Proven red: against the pre-fix populateFiatView both fields are null.
func TestAssetGet_NonUSDFiat_ServesPriceFromFXQuotes(t *testing.T) {
	// 1 EUR = 1.17 USD.
	const inverse = 1.17
	fx := &stubFXHistoryReader{points: []v1.FXQuotePoint{
		{Bucket: time.Now().UTC().Add(-24 * time.Hour), RateUSD: 1 / inverse, InverseUSD: inverse},
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
