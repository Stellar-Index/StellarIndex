// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"strings"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// The detail price withholds only on positive evidence that its scale is wrong. Fixture: flaggedAsset, raw ratio 41.32, no
// nonstandard_decimals_assets row, so the price was normalised at 7.
func TestAssetDetail_PriceWithheldOnlyOnContradictedScale(t *testing.T) {
	for _, tc := range []struct {
		name      string
		dec       v1.TokenDecimalsReader
		wantPrice bool
		wantStale bool
	}{
		{"reader nil", nil, true, true},
		{"read failed", &countingDecStub{err: errDecimalsRead}, true, true},
		{"read declares nothing", &countingDecStub{}, true, false},
		{"read agrees", &countingDecStub{d: 7, found: true}, true, false},
		{"read contradicts", &countingDecStub{d: 9, found: true}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := v1.New(v1.Options{Prices: lockstepPrices(), Supply: lockstepSupply(), TokenDecimals: tc.dec})
			body := lockstepGet(t, srv)
			if got := strings.Contains(body, `"price_usd":"41.32"`); got != tc.wantPrice {
				t.Errorf("price_usd served = %v, want %v: %s", got, tc.wantPrice, body)
			}
			if got := strings.Contains(body, `"price_withheld_reason":"decimals_mismatch"`); got == tc.wantPrice {
				t.Errorf("decimals_mismatch reason = %v, want %v: %s", got, !tc.wantPrice, body)
			}
			if got := strings.Contains(body, `"stale":true`); got != tc.wantStale {
				t.Errorf("flags.stale = %v, want %v: %s", got, tc.wantStale, body)
			}
			if tc.name != "read agrees" && (strings.Contains(body, `"market_cap_usd":"`) || strings.Contains(body, `"fdv_usd":"`)) {
				t.Errorf("cap served on an unread or contradicted scale: %s", body)
			}
			if !tc.wantPrice && strings.Contains(body, `"price_history_`) {
				t.Errorf("price history outlived the withheld price: %s", body)
			}
		})
	}
}

// An unread scale is a transient outage: the body serves the price but is not replayed from the detail cache.
func TestAssetDetail_UnreadScaleNotCached(t *testing.T) {
	stub := &countingDecStub{err: errDecimalsRead}
	srv := v1.New(v1.Options{Prices: lockstepPrices(), Supply: lockstepSupply(), TokenDecimals: stub})
	tsrv := startHTTPTest(t, srv.Handler())
	for range 2 {
		resp := mustGet(t, tsrv.URL+"/v1/assets/"+flaggedAsset)
		body, _ := readAll(resp)
		if !strings.Contains(body, `"price_usd":"41.32"`) {
			t.Fatalf("price_usd withheld on an unread scale: %s", body)
		}
	}
	if n := stub.calls.Load(); n != 2 {
		t.Errorf("decimals read %d times over two requests, want 2 (a degraded body must not be cached)", n)
	}
}

// XLM is 7 by protocol under each of its three identities, with no lake reader at all.
func TestAssetDetail_XLMIdentitiesPricedWithoutReader(t *testing.T) {
	for _, id := range []string{"native", "crypto:XLM", canonical.NativeSACContractID()} {
		t.Run(id, func(t *testing.T) {
			srv := v1.New(v1.Options{Prices: &stubPriceReader{snapshots: map[string]v1.PriceSnapshot{
				"native/fiat:USD": {AssetID: "native", Quote: "fiat:USD", Price: "0.25", PriceType: "vwap"},
			}}})
			resp := mustGet(t, startHTTPTest(t, srv.Handler()).URL+"/v1/assets/"+id)
			body, _ := readAll(resp)
			if !strings.Contains(body, `"price_usd":"0.25"`) || strings.Contains(body, `"price_withheld_reason"`) {
				t.Errorf("XLM via %s: want price_usd 0.25 and no withholding: %s", id, body)
			}
			if strings.Contains(body, `"stale":true`) {
				t.Errorf("XLM via %s marked stale with a protocol-fixed scale: %s", id, body)
			}
		})
	}
}
