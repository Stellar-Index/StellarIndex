package v1_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// A wallet showing a balance in its user's local currency needs
// XLM/BRL, XLM/JPY, XLM/ZAR. Before this layer, only three fiats
// resolved for a crypto asset — USD, EUR and GBP — because those are
// the only ones any venue we ingest quotes directly. Every other
// currency 404'd, even though both halves of the answer were on the
// box and fresh: XLM/USD from the market data, and USD→CCY for ~130
// currencies in fx_quotes. Nothing composed them.
//
// This is the composition: price(asset, CCY) = price(asset, USD) ×
// rate_usd[CCY]. See ADR-0051.

const (
	xlmUSDPrice = "0.17794602537043946501"
	brlRateUSD  = 5.1837 // 1 USD = 5.1837 BRL, live rate 2026-08-31
)

// usdLegReader serves a USD price for one asset and nothing else, so a
// test can prove the BRL answer was DERIVED rather than read.
type usdLegReader struct {
	usdPriceFor string // asset id that has a fiat:USD price
	price       string
	withheld    bool // the USD leg is withheld by policy
	// directPairs lets a test supply a real observed market, to prove
	// the derived layer does not shadow one.
	directPairs map[string]string
}

func (r *usdLegReader) LatestPrice(
	_ context.Context, a, q canonical.Asset,
) (v1.PriceSnapshot, []string, bool, error) {
	key := a.String() + "/" + q.String()
	if p, ok := r.directPairs[key]; ok {
		return v1.PriceSnapshot{
			AssetID: a.String(), Quote: q.String(),
			Price: p, PriceType: "vwap",
		}, []string{"binance"}, false, nil
	}
	if a.String() == r.usdPriceFor && q.String() == "fiat:USD" {
		if r.withheld {
			return v1.PriceSnapshot{}, nil, false, v1.ErrPriceWithheld
		}
		return v1.PriceSnapshot{
			AssetID: a.String(), Quote: q.String(),
			Price: r.price, PriceType: "vwap",
		}, []string{"sdex", "binance"}, false, nil
	}
	return v1.PriceSnapshot{}, nil, false, v1.ErrPriceNotFound
}

func (r *usdLegReader) RecentClosedSnapshots(
	_ context.Context, _, _ canonical.Asset, _ int,
) ([]v1.PriceSnapshot, error) {
	return []v1.PriceSnapshot{}, nil
}

func brlCurrencies() *stubCurrenciesReader {
	return &stubCurrenciesReader{
		snap: &v1.CurrenciesSnapshot{
			Currencies: []v1.CurrencyEntry{
				{Ticker: "BRL", Name: "Brazilian real", RateUSD: brlRateUSD},
				{Ticker: "JPY", Name: "Japanese yen", RateUSD: 147.2},
			},
			PublishedAt: time.Unix(1_770_000_000, 0).UTC(),
		},
	}
}

// TestPriceDerivesAnyFiatThroughUSD is the headline case: a currency
// with no market of its own, priced through the USD anchor.
func TestPriceDerivesAnyFiatThroughUSD(t *testing.T) {
	reader := &usdLegReader{usdPriceFor: "native", price: xlmUSDPrice}
	srv := v1.New(v1.Options{Prices: reader, Currencies: brlCurrencies()})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price?asset=native&quote=fiat:BRL")
	body, _ := readAll(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — XLM/USD is known and USD→BRL is "+
			"known, so XLM/BRL is derivable. A wallet cannot show a Brazilian "+
			"user their balance without it. Body: %s", resp.StatusCode, body)
	}
	// 0.17794602537043946501 × 5.1837 = 0.92240345...
	if !strings.Contains(body, `"price":"0.922`) {
		t.Errorf("price is not usd_price × rate_usd[BRL] (~0.9224): %s", body)
	}
	// The value is synthesised, not observed. Saying otherwise would
	// misrepresent it as a market print.
	if !strings.Contains(body, `"triangulated":true`) {
		t.Errorf("a derived price must carry triangulated=true: %s", body)
	}
	// The FX feed is part of the provenance of this number.
	if !strings.Contains(body, "massive") {
		t.Errorf("sources must credit the FX feed the rate came from: %s", body)
	}
	// …and so is the market that produced the USD leg. Dropping it
	// would hide which venues actually set the price.
	if !strings.Contains(body, "binance") {
		t.Errorf("sources must retain the USD leg's own sources: %s", body)
	}
}

// TestPriceWithheldUSDLegIsNotLaunderedThroughFX is the guard that
// matters most. Every withholding decision (scam-flagged issuer,
// decimals guard) is made on the USD leg. If the cross-rate layer
// re-reads that leg and multiplies it by an FX rate, it becomes a side
// door that publishes exactly the price policy declined to publish —
// the MSP-02 / MSP-06 class, where a fallback chain re-served a
// withheld market through a route nobody had gated.
func TestPriceWithheldUSDLegIsNotLaunderedThroughFX(t *testing.T) {
	reader := &usdLegReader{usdPriceFor: "native", price: xlmUSDPrice, withheld: true}
	srv := v1.New(v1.Options{Prices: reader, Currencies: brlCurrencies()})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price?asset=native&quote=fiat:BRL")
	body, _ := readAll(resp)
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("a WITHHELD USD leg was published in BRL — the cross-rate layer "+
			"is a side door around the withholding decision. Body: %s", body)
	}
	if !strings.Contains(body, "errors/price-withheld") {
		t.Errorf("the withheld verdict must propagate through the derivation, so "+
			"the customer is told we hold a price and decline to publish it — not "+
			"that none exists. Body: %s", body)
	}
}

// TestPriceDerivedFiatDoesNotShadowARealMarket pins the ordering. XLM
// is quoted directly in EUR by the CEX feeds; that observed print must
// win over usd_price × rate_usd[EUR]. Deriving on top of a real market
// would replace measured data with an estimate.
func TestPriceDerivedFiatDoesNotShadowARealMarket(t *testing.T) {
	reader := &usdLegReader{
		usdPriceFor: "native", price: xlmUSDPrice,
		directPairs: map[string]string{"native/fiat:EUR": "0.15312172801139768016"},
	}
	currencies := &stubCurrenciesReader{
		snap: &v1.CurrenciesSnapshot{
			Currencies: []v1.CurrencyEntry{{Ticker: "EUR", RateUSD: 0.86}},
		},
	}
	srv := v1.New(v1.Options{Prices: reader, Currencies: currencies})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price?asset=native&quote=fiat:EUR")
	body, _ := readAll(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200. Body: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "0.15312172801139768016") {
		t.Errorf("the OBSERVED EUR market must win over the derived cross "+
			"(0.17794… × 0.86 = 0.1530…): %s", body)
	}
	if strings.Contains(body, `"triangulated":true`) {
		t.Errorf("a directly observed market must not be flagged triangulated: %s", body)
	}
}

// TestPriceUnknownFiatStillMisses — the layer must not invent a rate it
// does not have. A currency absent from the FX snapshot is an honest
// 404, not a fabricated number.
func TestPriceUnknownFiatStillMisses(t *testing.T) {
	reader := &usdLegReader{usdPriceFor: "native", price: xlmUSDPrice}
	srv := v1.New(v1.Options{Prices: reader, Currencies: brlCurrencies()})
	ts := startHTTPTest(t, srv.Handler())

	// KRW is a valid ADR-0010 fiat code but carries no rate in this
	// snapshot. (An unparseable code would 400 at the boundary and never
	// reach this layer, so it would not exercise the guard.)
	resp := mustGet(t, ts.URL+"/v1/price?asset=native&quote=fiat:KRW")
	if resp.StatusCode != http.StatusNotFound {
		body, _ := readAll(resp)
		t.Errorf("status = %d, want 404 for a currency with no rate — the layer "+
			"must never invent one. Body: %s", resp.StatusCode, body)
	}
}

// TestPriceNoUSDLegStillMisses — an asset we cannot price in USD at all
// cannot be priced in BRL either. The derivation must not manufacture a
// USD leg it does not have.
func TestPriceNoUSDLegStillMisses(t *testing.T) {
	reader := &usdLegReader{usdPriceFor: "crypto:BTC", price: "78812.87"}
	srv := v1.New(v1.Options{Prices: reader, Currencies: brlCurrencies()})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price?asset=native&quote=fiat:BRL")
	if resp.StatusCode != http.StatusNotFound {
		body, _ := readAll(resp)
		t.Errorf("status = %d, want 404 — no USD leg means no derivation. Body: %s",
			resp.StatusCode, body)
	}
}

// TestPriceBatchDerivesAnyFiatThroughUSD — /v1/price/batch shares
// priceFallback, so the local-currency answer must appear there too.
// That is the endpoint a wallet actually calls to price a whole
// portfolio in one round trip.
func TestPriceBatchDerivesAnyFiatThroughUSD(t *testing.T) {
	reader := &usdLegReader{usdPriceFor: "native", price: xlmUSDPrice}
	srv := v1.New(v1.Options{Prices: reader, Currencies: brlCurrencies()})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price/batch?asset_ids=native&quote=fiat:BRL")
	body, _ := readAll(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200. Body: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, `"price":"0.922`) {
		t.Errorf("batch omitted the derived row — a wallet pricing a portfolio in "+
			"BRL would get an empty envelope: %s", body)
	}
}
