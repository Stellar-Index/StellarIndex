// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"net/http"
	"strings"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
)

// The SEP-40 single-price surfaces read the same closed prices_1m
// bucket /v1/price does, but never asked whether the pair was frozen.
// On a frozen pair that newest bucket is the one the anomaly checker
// refused, so lastprice/x_last_price published it to oracle integrators
// — the consumers least able to second-guess it — while /v1/price
// served the held last-known-good. These drive the production handlers
// with the fixture (moved bucket vs held value) and assert the
// VALUE.

const (
	xlmUSD       = "crypto:XLM/fiat:USD"
	heldUSD      = "0.1011"
	movedUSD     = "0.1377"
	lastPriceURL = "/v1/oracle/lastprice?asset=crypto:XLM"
	xLastURL     = "/v1/oracle/x_last_price?base=crypto:XLM&quote=fiat:USD"
)

func movedUSDBucketReader() *stubPriceReader {
	return &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			xlmUSD: {AssetID: "crypto:XLM", Quote: "fiat:USD", Price: movedUSD, PriceType: "vwap", WindowSeconds: 60},
		},
		sources: map[string][]string{xlmUSD: {"kraken", "coinbase"}},
	}
}

func TestOracleSEP40_FrozenPairServesLastKnownGood(t *testing.T) {
	for _, path := range []string{lastPriceURL, xLastURL} {
		t.Run(path, func(t *testing.T) {
			srv := v1.New(v1.Options{
				Prices:       movedUSDBucketReader(),
				Freeze:       frozenPairs{xlmUSD: true},
				Triangulated: lkgPairs{xlmUSD + "/300": heldUSD},
			})
			ts := startHTTPTest(t, srv.Handler())

			status, body := getBody(t, ts.URL+path)
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", status, body)
			}
			if strings.Contains(body, movedUSD) {
				t.Fatalf("frozen pair served the bucket the freeze refused (%s): %s", movedUSD, body)
			}
			for _, want := range []string{`"price":"` + heldUSD + `"`, `"frozen":true`, `"stale":true`, `"single_source":true`} {
				if !strings.Contains(body, want) {
					t.Errorf("body missing %s: %s", want, body)
				}
			}
			if strings.Contains(body, "kraken") {
				t.Errorf("sources still credit the refused bucket's venues: %s", body)
			}
		})
	}
}

func TestOracleSEP40_FrozenPairWithNothingHeldRefuses(t *testing.T) {
	for _, path := range []string{lastPriceURL, xLastURL} {
		t.Run(path, func(t *testing.T) {
			srv := v1.New(v1.Options{
				Prices:       movedUSDBucketReader(),
				Freeze:       frozenPairs{xlmUSD: true},
				Triangulated: lkgPairs{},
			})
			ts := startHTTPTest(t, srv.Handler())

			status, body := getBody(t, ts.URL+path)
			if status != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503: %s", status, body)
			}
			if strings.Contains(body, movedUSD) {
				t.Fatalf("refusal leaked the withheld bucket: %s", body)
			}
		})
	}
}

// The substitution discards the bucket, so pin where it must not fire.
func TestOracleSEP40_UnfrozenPairServesTheBucketUntouched(t *testing.T) {
	for _, path := range []string{lastPriceURL, xLastURL} {
		t.Run(path, func(t *testing.T) {
			srv := v1.New(v1.Options{
				Prices:       movedUSDBucketReader(),
				Freeze:       frozenPairs{},
				Triangulated: lkgPairs{xlmUSD + "/300": heldUSD},
			})
			ts := startHTTPTest(t, srv.Handler())

			status, body := getBody(t, ts.URL+path)
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", status, body)
			}
			for _, want := range []string{`"price":"` + movedUSD + `"`, `"stale":false`, `"frozen_checked":true`} {
				if !strings.Contains(body, want) {
					t.Errorf("body missing %s: %s", want, body)
				}
			}
			if strings.Contains(body, heldUSD) || strings.Contains(body, `"frozen":true`) {
				t.Errorf("unfrozen pair must not be touched by the freeze path: %s", body)
			}
		})
	}
}
