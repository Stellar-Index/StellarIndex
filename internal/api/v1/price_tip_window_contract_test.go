// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

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

// usdOnlyVWAPLooker is a Redis VWAP cache holding only X/fiat:USD, so a
// non-USD fiat quote can only be served through the USD-anchored cross.
type usdOnlyVWAPLooker struct{ value string }

func (l usdOnlyVWAPLooker) LookupTriangulatedVWAP(
	_ context.Context, _, quote canonical.Asset, _ time.Duration,
) (v1.CachedVWAP, bool, error) {
	if quote.String() != "fiat:USD" {
		return v1.CachedVWAP{}, false, nil
	}
	return v1.CachedVWAP{Value: l.value, ObservedAt: time.Now().UTC()}, true, nil
}

// TestPriceTip_FallbackBranchesOmitWindowSeconds pins the tip surface's
// window_seconds contract: it names the rolling window the tip was
// computed over, clamped to [1,60]. A fallback is not that window — the
// Redis VWAP cache is a 300s read and the closed bucket a 60s one — so
// every fallback branch omits the field rather than report its source's
// own resolution as if it were the caller's window.
func TestPriceTip_FallbackBranchesOmitWindowSeconds(t *testing.T) {
	cases := []struct {
		name string
		opts v1.Options
		url  string
	}{
		{
			// Only the fiat:USD leg is cached; BRL is derived from it.
			name: "usd_anchored_fiat_cross_over_cached_usd_leg",
			opts: v1.Options{
				Prices:       &stubPriceReader{err: v1.ErrPriceNotFound},
				History:      &stubHistoryReader{},
				Triangulated: usdOnlyVWAPLooker{value: "0.2"},
				Currencies:   brlCurrencies(),
			},
			url: "/v1/price/tip?asset=native&quote=fiat:BRL",
		},
		{
			name: "direct_redis_vwap_cache",
			opts: v1.Options{
				Prices:       &stubPriceReader{err: v1.ErrPriceNotFound},
				History:      &stubHistoryReader{},
				Triangulated: usdOnlyVWAPLooker{value: "0.2"},
			},
			url: "/v1/price/tip?asset=native&quote=fiat:USD&window_seconds=5",
		},
		{
			name: "closed_bucket",
			opts: v1.Options{
				Prices: &stubPriceReader{snapshots: map[string]v1.PriceSnapshot{
					"native/fiat:USD": {
						AssetID: "native", Quote: "fiat:USD", Price: "0.2",
						PriceType: "vwap", WindowSeconds: 60,
					},
				}},
				History: &stubHistoryReader{},
			},
			url: "/v1/price/tip?asset=native&quote=fiat:USD&window_seconds=5",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := startHTTPTest(t, v1.New(tc.opts).Handler())
			resp := mustGet(t, ts.URL+tc.url)
			body, _ := readAll(resp)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
			}
			if strings.Contains(body, `"window_seconds"`) {
				t.Errorf("a fallback tip must omit window_seconds — it was not "+
					"computed over the caller's [1,60] rolling window: %s", body)
			}
		})
	}
}
