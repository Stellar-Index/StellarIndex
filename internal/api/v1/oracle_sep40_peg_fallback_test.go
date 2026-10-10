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

// /v1/oracle/prices walks the declared USD pegs when the literal
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
