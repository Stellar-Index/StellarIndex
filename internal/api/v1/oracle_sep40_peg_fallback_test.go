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
)

// GH-819: /v1/oracle/prices walks the declared USD pegs when the literal
// asset/fiat:USD series is empty. A peg leg the withholding gate refused
// was skipped like a miss, so a withheld asset answered 200 [] — "no
// data" — where every sibling surface answers the price-withheld 404.
// And a peg-served series was stamped triangulated but not stale, unlike
// the same fallback on lastprice/x_last_price.

func oraclePegs(t *testing.T) (usdc, usdt canonical.Asset) {
	t.Helper()
	usdc, err := canonical.ParseAsset("USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	usdt, err = canonical.ParseAsset("USDT-GCQTGZQQ5G4PTM2GL7CDIFKUBIPEC52BROAQIAPW53XBRJVN6ZJVTG6V")
	if err != nil {
		t.Fatal(err)
	}
	return usdc, usdt
}

func pegSeries(quote, price string) []v1.PriceSnapshot {
	at := time.Unix(1_770_000_000, 0).UTC()
	return []v1.PriceSnapshot{{
		AssetID: "native", Quote: quote, Price: price, PriceType: "vwap", ObservedAt: v1.WireTime(at),
	}}
}

func TestOraclePrices_WithheldPegLegAnswersWithheldNotEmpty(t *testing.T) {
	usdc, _ := oraclePegs(t)
	reader := &stubPriceReader{
		errByPair: map[string]error{"native/" + usdc.String(): v1.ErrPriceWithheld},
	}
	srv := v1.New(v1.Options{Prices: reader, USDPeggedClassics: []canonical.Asset{usdc}})
	ts := startHTTPTest(t, srv.Handler())

	status, body := getBody(t, ts.URL+"/v1/oracle/prices?asset=native&records=5")
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 price-withheld (a refused peg leg is not a miss): %s", status, body)
	}
	if !strings.Contains(body, "errors/price-withheld") {
		t.Errorf("body missing the price-withheld problem type: %s", body)
	}
}

// The destructive branch's boundary: a withheld verdict on one peg is a
// verdict on THAT pair, so a later peg that serves still wins, exactly as
// walkUSDPegs does for lastprice.
func TestOraclePrices_WithheldPegLegDoesNotHideALaterServingPeg(t *testing.T) {
	usdc, usdt := oraclePegs(t)
	reader := &stubPriceReader{
		errByPair: map[string]error{"native/" + usdc.String(): v1.ErrPriceWithheld},
		recent:    map[string][]v1.PriceSnapshot{"native/" + usdt.String(): pegSeries(usdt.String(), "0.1631")},
	}
	srv := v1.New(v1.Options{Prices: reader, USDPeggedClassics: []canonical.Asset{usdc, usdt}})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/oracle/prices?asset=native&records=5")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 via the second peg", resp.StatusCode)
	}
	var env struct {
		Data  []v1.SEP40Price `json:"data"`
		Flags v1.Flags        `json:"flags"`
	}
	mustDecode(t, resp, &env)
	if len(env.Data) != 1 || env.Data[0].Price != "0.1631" {
		t.Fatalf("data = %+v, want the USDT peg's single 0.1631 record", env.Data)
	}
}

func TestOraclePrices_PegFallbackIsStale(t *testing.T) {
	usdc, _ := oraclePegs(t)
	cases := []struct {
		name   string
		recent map[string][]v1.PriceSnapshot
		want   bool
	}{
		{"direct fiat:USD series", map[string][]v1.PriceSnapshot{"native/fiat:USD": pegSeries("fiat:USD", "0.1620")}, false},
		{"peg-proxied series", map[string][]v1.PriceSnapshot{"native/" + usdc.String(): pegSeries(usdc.String(), "0.1626")}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := v1.New(v1.Options{
				Prices:            &stubPriceReader{recent: tc.recent},
				USDPeggedClassics: []canonical.Asset{usdc},
			})
			ts := startHTTPTest(t, srv.Handler())
			resp := mustGet(t, ts.URL+"/v1/oracle/prices?asset=native&records=5")
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			var env struct {
				Data  []v1.SEP40Price `json:"data"`
				Flags v1.Flags        `json:"flags"`
			}
			mustDecode(t, resp, &env)
			if len(env.Data) != 1 {
				t.Fatalf("got %d records, want 1", len(env.Data))
			}
			if env.Flags.Stale != tc.want || env.Flags.Triangulated != tc.want {
				t.Errorf("flags stale=%v triangulated=%v, want both %v",
					env.Flags.Stale, env.Flags.Triangulated, tc.want)
			}
		})
	}
}

// A declared peg is an assumption, not a record: SEP-40's answer for "no
// record" is None, and SEP40Price has no field to tell a declaration from
// a print. Both point reads refuse it where /v1/price serves it as "peg".
func TestOracleSEP40PointReads_RefuseADeclaredPeg(t *testing.T) {
	usdc, _ := oraclePegs(t)
	srv := v1.New(v1.Options{
		Prices:            &stubPriceReader{err: v1.ErrPriceNotFound},
		USDPeggedClassics: []canonical.Asset{usdc},
	})
	ts := startHTTPTest(t, srv.Handler())

	status, body := getBody(t, ts.URL+"/v1/price?asset="+usdc.String()+"&quote=fiat:USD")
	if status != http.StatusOK || !strings.Contains(body, `"price_type":"peg"`) {
		t.Fatalf("precondition: /v1/price must serve the declaration as price_type peg, got %d: %s", status, body)
	}
	for _, path := range []string{
		"/v1/oracle/lastprice?asset=" + usdc.String(),
		"/v1/oracle/lastprice?asset=crypto:USDC",
		"/v1/oracle/x_last_price?base=" + usdc.String() + "&quote=fiat:USD",
		"/v1/oracle/x_last_price?base=crypto:USDC&quote=fiat:USD",
		"/v1/oracle/x_last_price?base=crypto:EURC&quote=fiat:EUR",
	} {
		status, body := getBody(t, ts.URL+path)
		if status != http.StatusNotFound || !strings.Contains(body, "errors/price-not-found") {
			t.Errorf("%s: status = %d, want 404 price-not-found (a declaration is not a SEP-40 record): %s", path, status, body)
		}
	}
}
