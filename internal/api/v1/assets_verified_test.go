package v1_test

import (
	"net/http"
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
	if usdc.AssetID != "USDC-"+testUSDCIssuer || usdc.Issuer != testUSDCIssuer {
		t.Errorf("usdc identity: asset_id=%q issuer=%q", usdc.AssetID, usdc.Issuer)
	}

	xlm, ok := bySlug["xlm"]
	if !ok {
		t.Fatal("xlm entry missing")
	}
	if xlm.Ticker != "XLM" {
		t.Errorf("xlm ticker = %q", xlm.Ticker)
	}
	if xlm.AssetID != "native" || xlm.Issuer != "" {
		t.Errorf("xlm identity: asset_id=%q issuer=%q, want native and no issuer", xlm.AssetID, xlm.Issuer)
	}

	// Every Stellar-issued row serves the seed's asset_id, derived from
	// its (code, issuer); rows without a Stellar issuance serve none.
	cat := newTestCatalogue(t)
	for _, e := range env.Data {
		vc, _ := cat.LookupBySlug(e.Slug)
		want := ""
		if se := vc.StellarEntry(); se != nil {
			want = se.AssetID
		}
		if e.AssetID != want {
			t.Errorf("%s: asset_id=%q, want %q", e.Slug, e.AssetID, want)
		}
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
// guard that attachFiatMarketCaps does not skip its ENTIRE
// fan-out whenever PriceReader (s.Prices) was nil, even though
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
		{Bucket: time.Now().UTC(), InverseUSDText: "0.14"},
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
