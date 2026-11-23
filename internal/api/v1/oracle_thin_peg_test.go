// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
)

const (
	usdcUSDMarket = "crypto:USDC/fiat:USD"
	pegPrice      = "0.1626"
	directPrice   = "0.1700"
)

// thinPegFixture: crypto:XLM's own fiat:USD book is refused with direct (nil: served at directPrice); the native/USDC book
// serves pegPrice unless pegErr refuses it; priceAt observes the USDC dollar market (nil: no point-in-time reader).
type thinPegFixture struct {
	direct, pegErr error
	priceAt        v1.PriceAtReader
	history        v1.HistoryReader
	pegBook        []v1.PriceSnapshot
}

func (f thinPegFixture) server(t *testing.T) string {
	t.Helper()
	usdc, _ := oraclePegs(t)
	pegKey := "native/" + usdc.String()
	book := f.pegBook
	if book == nil {
		book = pegSeries(usdc.String(), pegPrice)
	}
	direct := v1.PriceSnapshot{
		AssetID: "crypto:XLM", Quote: "fiat:USD", Price: directPrice, PriceType: "vwap", WindowSeconds: 60,
		ObservedAt: v1.WireTime(time.Now().UTC().Truncate(time.Minute)),
	}
	reader := &stubPriceReader{
		errByPair: map[string]error{xlmUSD: f.direct, "native/fiat:USD": f.direct, pegKey: f.pegErr},
		snapshots: map[string]v1.PriceSnapshot{pegKey: book[0]},
		recent:    map[string][]v1.PriceSnapshot{pegKey: book},
		sources:   map[string][]string{pegKey: {"sdex"}},
	}
	if f.direct == nil {
		reader.snapshots[xlmUSD] = direct
		reader.recent[xlmUSD] = []v1.PriceSnapshot{direct}
	}
	opts := v1.Options{Prices: reader, USDPeggedClassics: []canonical.Asset{usdc}, VerifiedCurrencies: newTestCatalogue(t)}
	if f.priceAt != nil {
		opts.PriceAt = f.priceAt
	}
	if f.history != nil {
		opts.History = f.history
	}
	return startHTTPTest(t, v1.New(opts).Handler()).URL
}

// usdcObserved is a point-in-time reader observing USDC's dollar market at price ("": not observed).
func usdcObserved(price string) v1.PriceAtReader {
	byPair := map[string]string{}
	if price != "" {
		byPair[usdcUSDMarket] = price
	}
	return &recordingPriceAtReader{byPair: byPair}
}

// TestOracleThinDirect_PegBookQualifiesOnBothRoutes: a direct fiat:USD book refused only as thin is priced through a USD-peg
// book iff that book passed the reader's gates and the peg is checked inside its band; lastprice and prices() give the same
// answer in every case, and a peg answer is flagged triangulated (and stale, as every peg-walk answer is).
func TestOracleThinDirect_PegBookQualifiesOnBothRoutes(t *testing.T) {
	thin := v1.PriceWithheldError(pricingguard.WithheldThinMarket)
	cases := []struct {
		name   string
		fx     thinPegFixture
		status int
		price  string
	}{
		{"thin direct, deep in-band peg", thinPegFixture{direct: thin, priceAt: usdcObserved("1.0")}, http.StatusOK, pegPrice},
		{"thin direct, thin peg", thinPegFixture{direct: thin, pegErr: thin, priceAt: usdcObserved("1.0")}, http.StatusNotFound, ""},
		{"thin direct, peg depegged", thinPegFixture{direct: thin, priceAt: usdcObserved("0.95")}, http.StatusNotFound, ""},
		{"thin direct, peg unobserved", thinPegFixture{direct: thin, priceAt: usdcObserved("")}, http.StatusNotFound, ""},
		{"thin direct, no peg checker", thinPegFixture{direct: thin}, http.StatusNotFound, ""},
		{"thin direct, peg book excluded", thinPegFixture{
			direct: thin, pegErr: v1.PriceWithheldError(pricingguard.WithheldSourceExcluded), priceAt: usdcObserved("1.0"),
		}, http.StatusNotFound, ""},
		{"flagged issuer, in-band peg", thinPegFixture{
			direct: v1.PriceWithheldError(pricingguard.WithheldFlaggedIssuer), priceAt: usdcObserved("1.0"),
		}, http.StatusNotFound, ""},
		{"exclusions unread, in-band peg", thinPegFixture{
			direct: v1.PriceWithheldError(pricingguard.WithheldExclusionsUnread), priceAt: usdcObserved("1.0"),
		}, http.StatusNotFound, ""},
		{"healthy direct", thinPegFixture{priceAt: usdcObserved("1.0")}, http.StatusOK, directPrice},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := tc.fx.server(t)
			got := map[string]int{}
			for _, path := range []string{lastPriceURL, pricesURL} {
				status, body := getBody(t, base+path)
				got[path] = status
				if status != tc.status {
					t.Fatalf("%s = %d, want %d: %s", path, status, tc.status, body)
				}
				if tc.status == http.StatusNotFound {
					if !strings.Contains(body, "errors/price-withheld") {
						t.Fatalf("%s: want price-withheld: %s", path, body)
					}
					continue
				}
				peg := tc.price == pegPrice
				checkBody(t, body, []string{`"price":"` + tc.price + `"`}, nil)
				if got := strings.Contains(body, `"triangulated":true`); got != peg {
					t.Errorf("%s: triangulated = %v, want %v: %s", path, got, peg, body)
				}
			}
			if got[lastPriceURL] != got[pricesURL] {
				t.Errorf("lastprice = %d, prices() = %d: the routes disagree", got[lastPriceURL], got[pricesURL])
			}
		})
	}
}

// TestOraclePrices_ThinDirectPegJudgedOverTheSeriesSpan: a peg that holds at the newest bucket but left its band between the
// series' buckets does not qualify; the same series with the peg in band throughout does.
func TestOraclePrices_ThinDirectPegJudgedOverTheSeriesSpan(t *testing.T) {
	usdc, _ := oraclePegs(t)
	newest := time.Unix(1_770_000_000, 0).UTC()
	oldest := newest.Add(-time.Hour)
	book := []v1.PriceSnapshot{
		{AssetID: "native", Quote: usdc.String(), Price: pegPrice, PriceType: "vwap", ObservedAt: v1.WireTime(newest)},
		{AssetID: "native", Quote: usdc.String(), Price: "0.1620", PriceType: "vwap", ObservedAt: v1.WireTime(oldest)},
	}
	for _, dip := range []bool{true, false} {
		low := "1.0"
		if dip {
			low = "0.95"
		}
		bars := []v1.OHLCSeriesBar{
			{T: v1.WireTime(oldest), O: "1.0", H: "1.0", L: "1.0", C: "1.0"},
			{T: v1.WireTime(oldest.Add(30 * time.Minute)), O: "1.0", H: "1.0", L: low, C: "1.0"},
		}
		base := thinPegFixture{
			direct:  v1.PriceWithheldError(pricingguard.WithheldThinMarket),
			priceAt: usdcObserved("1.0"),
			history: &stubHistoryReader{ohlcByPair: map[string][]v1.OHLCSeriesBar{usdcUSDMarket: bars}},
			pegBook: book,
		}.server(t)
		status, body := getBody(t, base+"/v1/oracle/prices?asset=crypto:XLM&records=2")
		if want := map[bool]int{true: http.StatusNotFound, false: http.StatusOK}[dip]; status != want {
			t.Fatalf("dip=%v: prices() = %d, want %d: %s", dip, status, want, body)
		}
	}
}

// TestOracleThinDirect_UnreadPegBandCheckIsRetryable: a band check that could not be read leaves the peg unjudged, so both routes
// answer a retryable, uncached 5xx, never a 404 price-withheld verdict nor the peg book.
func TestOracleThinDirect_UnreadPegBandCheckIsRetryable(t *testing.T) {
	base := thinPegFixture{direct: v1.PriceWithheldError(pricingguard.WithheldThinMarket), priceAt: failingPriceAt{}}.server(t)
	for _, path := range []string{lastPriceURL, pricesURL} {
		resp := mustGet(t, base+path)
		body, _ := readAll(resp)
		if resp.StatusCode < 500 || !strings.Contains(resp.Header.Get("Cache-Control"), "no-store") {
			t.Fatalf("%s = %d (Cache-Control %q), want an uncached 5xx: %s",
				path, resp.StatusCode, resp.Header.Get("Cache-Control"), body)
		}
	}
}
