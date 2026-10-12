package v1_test

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// Wallets need XLM/BRL, XLM/JPY etc., but only USD, EUR and GBP are quoted by any ingested venue. The USD-anchored
// layer composes price(asset, CCY) = price(asset, USD) × rate_usd[CCY] (fx_quotes) from two fresh halves. See ADR-0051.

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
}

func (r *usdLegReader) LatestPrice(
	_ context.Context, a, q canonical.Asset,
) (v1.PriceSnapshot, []string, bool, error) {
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
	// UpdatedAt is "now" — these tests exercise the derivation itself,
	// not the fx-cross staleness guard (see
	// usd_anchored_fiat_cross_stale_test.go for that).
	now := time.Now().UTC()
	return &stubCurrenciesReader{
		snap: &v1.CurrenciesSnapshot{
			Currencies: []v1.CurrencyEntry{
				{Ticker: "BRL", Name: "Brazilian real", RateUSD: brlRateUSD, UpdatedAt: now},
				{Ticker: "JPY", Name: "Japanese yen", RateUSD: 147.2, UpdatedAt: now},
			},
			PublishedAt: now,
		},
	}
}

// TestPriceDerivesAnyFiatThroughUSD is the headline case: a currency
// with no market of its own, priced through the USD anchor.
func TestPriceDerivesAnyFiatThroughUSD(t *testing.T) {
	reader := &usdLegReader{usdPriceFor: "native", price: xlmUSDPrice}
	srv := v1.New(v1.Options{Prices: reader, Currencies: brlCurrencies(), FXFixings: brlFixings()})
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

// TestPriceWithheldUSDLegIsNotLaunderedThroughFX is the key guard: withholding decisions (scam issuer, decimals
// guard) are made on the USD leg, so a cross-rate layer that re-reads that leg and multiplies by FX would be a side
// door publishing what policy declined.
func TestPriceWithheldUSDLegIsNotLaunderedThroughFX(t *testing.T) {
	reader := &usdLegReader{usdPriceFor: "native", price: xlmUSDPrice, withheld: true}
	srv := v1.New(v1.Options{Prices: reader, Currencies: brlCurrencies(), FXFixings: brlFixings()})
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

// TestPriceUnknownFiatStillMisses — the layer must not invent a rate it
// does not have. A currency absent from the FX snapshot is an honest
// 404, not a fabricated number.
func TestPriceUnknownFiatStillMisses(t *testing.T) {
	reader := &usdLegReader{usdPriceFor: "native", price: xlmUSDPrice}
	srv := v1.New(v1.Options{Prices: reader, Currencies: brlCurrencies(), FXFixings: brlFixings()})
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
	srv := v1.New(v1.Options{Prices: reader, Currencies: brlCurrencies(), FXFixings: brlFixings()})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price?asset=native&quote=fiat:BRL")
	if resp.StatusCode != http.StatusNotFound {
		body, _ := readAll(resp)
		t.Errorf("status = %d, want 404 — no USD leg means no derivation. Body: %s",
			resp.StatusCode, body)
	}
}

// TestPriceBatchDerivesAnyFiatThroughUSD: /v1/price/batch shares priceFallback, so the local-currency answer must
// appear there too; wallets call it to price a whole portfolio in one round trip.
func TestPriceBatchDerivesAnyFiatThroughUSD(t *testing.T) {
	reader := &usdLegReader{usdPriceFor: "native", price: xlmUSDPrice}
	srv := v1.New(v1.Options{Prices: reader, Currencies: brlCurrencies(), FXFixings: brlFixings()})
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

// An ADR-0051 local-currency price is one USD-leg observation times an FX rate; FX only converts and is not a second market,
// and the aggregator freezes the USD leg, so single_source and frozen on a derived response are the USD leg's. Freeze-aware
// surfaces (/v1/price, batch, SEP-40 x_last_price) substitute a held value; the tip stays freeze-agnostic.

const (
	xlmUSDKey    = "native/fiat:USD"
	xlmUSDAlias  = "crypto:XLM/fiat:USD"
	legBucket    = "0.2000"  // the leg's newest closed bucket
	legHeld      = "0.1000"  // what a freeze on the leg is holding
	legHeldBRL   = "0.51837" // legHeld × brlRateUSD
	legBucketBRL = "1.03674" // legBucket × brlRateUSD
)

type crossEnvelope struct {
	Data    json.RawMessage `json:"data"`
	Sources []string        `json:"sources"`
	Flags   struct {
		Stale         bool `json:"stale"`
		Frozen        bool `json:"frozen"`
		FrozenChecked bool `json:"frozen_checked"`
		SingleSource  bool `json:"single_source"`
	} `json:"flags"`
}

// getCross fetches path and decodes the envelope and its (first) price.
func getCross(t *testing.T, url string) (int, crossEnvelope, string) {
	t.Helper()
	status, body := getBody(t, url)
	var env crossEnvelope
	if status != http.StatusOK {
		return status, env, body
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("%s: decode: %v: %s", url, err, body)
	}
	var row struct {
		Price string `json:"price"`
	}
	if len(env.Data) > 0 && env.Data[0] == '[' {
		var rows []struct {
			Price string `json:"price"`
		}
		if err := json.Unmarshal(env.Data, &rows); err != nil || len(rows) > 1 {
			t.Fatalf("%s: want at most one batch row, got %v: %s", url, err, body)
		}
		if len(rows) == 1 {
			row.Price = rows[0].Price
		}
	} else if err := json.Unmarshal(env.Data, &row); err != nil {
		t.Fatalf("%s: decode data: %v: %s", url, err, body)
	}
	return status, env, row.Price
}

func brlLegReaderAt(key, assetID string, sources ...string) *stubPriceReader {
	return &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			key: {AssetID: assetID, Quote: "fiat:USD", Price: legBucket, PriceType: "vwap", WindowSeconds: 60},
		},
		sources: map[string][]string{key: sources},
	}
}

func brlLegReader(sources ...string) *stubPriceReader {
	return brlLegReaderAt(xlmUSDKey, "native", sources...)
}

func assertRatEqual(t *testing.T, label, got, want string) {
	t.Helper()
	g, okG := new(big.Rat).SetString(got)
	w, okW := new(big.Rat).SetString(want)
	if !okG || !okW || g.Cmp(w) != 0 {
		t.Errorf("%s: price = %q, want %q", label, got, want)
	}
}

func TestDerivedFiatPriceSingleVenueLegIsSingleSource(t *testing.T) {
	srv := v1.New(v1.Options{Prices: brlLegReader("soroswap"), Currencies: brlCurrencies(), FXFixings: brlFixings()})
	ts := startHTTPTest(t, srv.Handler())

	for _, path := range []string{
		"/v1/price?asset=native&quote=fiat:BRL",
		"/v1/price/tip?asset=native&quote=fiat:BRL",
		"/v1/price/batch?asset_ids=native&quote=fiat:BRL",
	} {
		status, env, body := getCross(t, ts.URL+path)
		if status != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200: %s", path, status, body)
		}
		if !slices.Contains(env.Sources, "massive") || !slices.Contains(env.Sources, "soroswap") {
			t.Errorf("%s: sources must credit both the leg venue and the FX feed, got %v", path, env.Sources)
		}
		if !env.Flags.SingleSource {
			t.Errorf("%s: one venue times an FX rate is one market — single_source must be true", path)
		}
	}
}

func TestDerivedFiatPriceMultiVenueLegIsNotSingleSource(t *testing.T) {
	srv := v1.New(v1.Options{Prices: brlLegReader("binance", "sdex"), Currencies: brlCurrencies(), FXFixings: brlFixings()})
	ts := startHTTPTest(t, srv.Handler())

	for _, path := range []string{
		"/v1/price?asset=native&quote=fiat:BRL",
		"/v1/price/tip?asset=native&quote=fiat:BRL",
		"/v1/price/batch?asset_ids=native&quote=fiat:BRL",
	} {
		status, env, body := getCross(t, ts.URL+path)
		if status != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200: %s", path, status, body)
		}
		if env.Flags.SingleSource {
			t.Errorf("%s: a two-venue USD leg must not report single_source", path)
		}
	}
}

// frozenLegServer freezes the USD leg under the alias its bucket is read
// from. held is the value the freeze holds; "" means nothing is held.
func frozenLegServer(t *testing.T, key, assetID, held string) string {
	t.Helper()
	lkg := lkgPairs{}
	if held != "" {
		lkg[key+"/300"] = held
	}
	srv := v1.New(v1.Options{
		Prices:       brlLegReaderAt(key, assetID, "binance", "sdex"),
		Currencies:   brlCurrencies(),
		FXFixings:    brlFixings(),
		Freeze:       frozenPairs{key: true},
		Triangulated: lkg,
	})
	return startHTTPTest(t, srv.Handler()).URL
}

func TestDerivedFiatPriceFrozenLegServesHeldValueAndReportsFrozen(t *testing.T) {
	for _, leg := range []struct{ key, assetID string }{
		{xlmUSDKey, "native"},
		{xlmUSDAlias, "crypto:XLM"}, // governed by the alias the bucket was read under
	} {
		base := frozenLegServer(t, leg.key, leg.assetID, legHeld)
		for _, path := range []string{
			"/v1/price?asset=native&quote=fiat:BRL",
			"/v1/price/batch?asset_ids=native&quote=fiat:BRL",
			"/v1/oracle/x_last_price?base=native&quote=fiat:BRL",
		} {
			label := leg.key + " " + path
			status, env, price := getCross(t, base+path)
			if status != http.StatusOK {
				t.Fatalf("%s: status = %d, want 200: %s", label, status, price)
			}
			assertRatEqual(t, label+" (held × rate, never the refused bucket × rate)", price, legHeldBRL)
			if !env.Flags.Frozen || !env.Flags.Stale || !env.Flags.SingleSource {
				t.Errorf("%s: want frozen, stale and single_source, got %+v", label, env.Flags)
			}
			if !slices.Contains(env.Sources, "massive") {
				t.Errorf("%s: the FX feed still converted the held value — sources = %v", label, env.Sources)
			}
		}
	}
}

func TestDerivedFiatPriceFrozenLegWithNothingHeldServesNothing(t *testing.T) {
	base := frozenLegServer(t, xlmUSDKey, "native", "")

	status, _, body := getCross(t, base+"/v1/price?asset=native&quote=fiat:BRL")
	if status != http.StatusServiceUnavailable {
		t.Errorf("/v1/price: a frozen USD leg with nothing held must refuse (503) like its USD pair, got %d: %s", status, body)
	}
	usdStatus, _, _ := getCross(t, base+"/v1/price?asset=native&quote=fiat:USD")
	if usdStatus != http.StatusServiceUnavailable {
		t.Errorf("/v1/price USD leg: status = %d, want 503 (the fixture's premise)", usdStatus)
	}
	status, _, body = getCross(t, base+"/v1/oracle/x_last_price?base=native&quote=fiat:BRL")
	if status != http.StatusServiceUnavailable {
		t.Errorf("/v1/oracle/x_last_price: a frozen USD leg with nothing held must refuse (503), got %d: %s", status, body)
	}

	status, env, body := getCross(t, base+"/v1/price/batch?asset_ids=native&quote=fiat:BRL")
	if status == http.StatusOK && string(env.Data) != "[]" {
		t.Errorf("/v1/price/batch: a frozen USD leg with nothing held must omit the row, got %s", env.Data)
	} else if status != http.StatusOK {
		t.Errorf("/v1/price/batch: status = %d, want 200 with the row omitted: %s", status, body)
	}
}

func TestDerivedFiatPriceUnfrozenLegReportsFreezeChecked(t *testing.T) {
	srv := v1.New(v1.Options{
		Prices:       brlLegReader("soroswap"),
		Currencies:   brlCurrencies(),
		FXFixings:    brlFixings(),
		Freeze:       frozenPairs{},
		Triangulated: lkgPairs{},
	})
	ts := startHTTPTest(t, srv.Handler())

	status, env, price := getCross(t, ts.URL+"/v1/price?asset=native&quote=fiat:BRL")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, price)
	}
	assertRatEqual(t, "/v1/price", price, legBucketBRL)
	if env.Flags.Frozen || !env.Flags.FrozenChecked || !env.Flags.SingleSource {
		t.Errorf("want frozen=false, frozen_checked and single_source, got %+v", env.Flags)
	}
}

// The tip is freeze-agnostic (price_tip.go's handler doc): the derived BRL answer must be exactly
// what the same surface serves for USD times the rate, never a 404 while USD serves, never a
// silently substituted held value, and with the leg's venues still credited.
func TestDerivedFiatFreezeAgnosticSurfacesFollowTheirUSDLeg(t *testing.T) {
	rate := strconv.FormatFloat(brlRateUSD, 'f', -1, 64)
	for _, held := range []string{legHeld, ""} {
		base := frozenLegServer(t, xlmUSDKey, "native", held)
		for _, pair := range []struct{ usd, brl string }{
			{"/v1/price/tip?asset=native&quote=fiat:USD", "/v1/price/tip?asset=native&quote=fiat:BRL"},
		} {
			label := pair.brl + " held=" + strconv.Quote(held)
			usdStatus, _, usdPrice := getCross(t, base+pair.usd)
			if usdStatus != http.StatusOK {
				t.Fatalf("%s: USD leg status = %d, want 200 (the fixture's premise): %s", label, usdStatus, usdPrice)
			}
			status, env, price := getCross(t, base+pair.brl)
			if status != http.StatusOK {
				t.Fatalf("%s: status = %d while the USD surface serves 200: %s", label, status, price)
			}
			u, _ := new(big.Rat).SetString(usdPrice)
			r, _ := new(big.Rat).SetString(rate)
			assertRatEqual(t, label+" (USD × rate)", price, new(big.Rat).Mul(u, r).FloatString(10))
			for _, venue := range []string{"binance", "sdex", "massive"} {
				if !slices.Contains(env.Sources, venue) {
					t.Errorf("%s: sources %v missing %q", label, env.Sources, venue)
				}
			}
			if env.Flags.Frozen {
				t.Errorf("%s: a freeze-agnostic surface must not report frozen", label)
			}
		}
	}
}
