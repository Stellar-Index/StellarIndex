// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"encoding/json"
	"math/big"
	"net/http"
	"slices"
	"strconv"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
)

// An ADR-0051 local-currency price is one USD-leg market observation
// times an FX rate. The FX feed converts that observation; it is not a
// second market, and the pair the aggregator freezes is the USD leg —
// there is no marker for the derived fiat pair. So single_source and
// frozen on a derived response are the USD leg's (GH-953). Only the
// freeze-aware surfaces (/v1/price, /v1/price/batch) substitute a held
// value; the tip and the SEP-40 oracle stay freeze-agnostic.

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
	srv := v1.New(v1.Options{Prices: brlLegReader("soroswap"), Currencies: brlCurrencies()})
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
	srv := v1.New(v1.Options{Prices: brlLegReader("binance", "sdex"), Currencies: brlCurrencies()})
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

// The tip and the SEP-40 oracle are freeze-agnostic (price_tip.go's
// handler doc): the derived BRL answer must be exactly what the same
// surface serves for USD, times the rate — never a 404 while USD
// serves, never a silently substituted held value, and with the leg's
// venues still credited.
func TestDerivedFiatFreezeAgnosticSurfacesFollowTheirUSDLeg(t *testing.T) {
	rate := strconv.FormatFloat(brlRateUSD, 'f', -1, 64)
	for _, held := range []string{legHeld, ""} {
		base := frozenLegServer(t, xlmUSDKey, "native", held)
		for _, pair := range []struct{ usd, brl string }{
			{"/v1/price/tip?asset=native&quote=fiat:USD", "/v1/price/tip?asset=native&quote=fiat:BRL"},
			{"/v1/oracle/x_last_price?base=native&quote=fiat:USD", "/v1/oracle/x_last_price?base=native&quote=fiat:BRL"},
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
