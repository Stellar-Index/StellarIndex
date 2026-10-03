// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"net/http"
	"strings"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// A declared 1:1 peg is neither an observation nor a value that survives an
// FX multiplication, so no fiat cross is served from it, on any surface.
func TestDeclaredPegIsNotCrossedThroughFX(t *testing.T) {
	usdc, _ := oraclePegs(t)
	srv := v1.New(v1.Options{
		Prices:            &stubPriceReader{err: v1.ErrPriceNotFound},
		USDPeggedClassics: []canonical.Asset{usdc},
		Currencies:        brlCurrencies(),
		FXFixings:         brlFixings(),
	})
	ts := startHTTPTest(t, srv.Handler())

	status, body := getBody(t, ts.URL+"/v1/price?asset="+usdc.String()+"&quote=fiat:USD")
	if status != http.StatusOK || !strings.Contains(body, `"price_type":"peg"`) {
		t.Fatalf("precondition: the declaration serves as peg against USD, got %d: %s", status, body)
	}
	for _, path := range []string{
		"/v1/price?asset=" + usdc.String() + "&quote=fiat:BRL",
		"/v1/price/tip?asset=" + usdc.String() + "&quote=fiat:BRL",
		"/v1/oracle/x_last_price?base=" + usdc.String() + "&quote=fiat:BRL",
	} {
		status, body := getBody(t, ts.URL+path)
		if status != http.StatusNotFound || strings.Contains(body, `"price"`) {
			t.Errorf("%s: status = %d, want 404 with no price: %s", path, status, body)
		}
	}
}
