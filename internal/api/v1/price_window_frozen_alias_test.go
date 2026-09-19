// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"net/http"
	"strings"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
)

// K037's class sweep ("any branch setting Frozen whose served value did
// not come from the pair the freeze governs") has one hit the F013 fix
// did not reach: /v1/price?window=N. It reads the VWAP cache under
// whichever ALIAS spelling the aggregator published — and then asked the
// freeze marker about the spelling the CLIENT used. The marker is keyed
// on the literal pair the aggregator prices (cachekeys.Freeze), so the
// two questions have different answers whenever the request is answered
// from an alias:
//
//   - the served pair is frozen, the requested literal is not: the
//     response carries a value a freeze is holding and says frozen=false;
//   - the requested literal is frozen, the served pair is healthy: the
//     response carries an accepted value and says frozen=true — the
//     false flag F013R removed from the default path.
//
// Both fakes key on the LITERAL pair, exactly as Redis does, so a test
// here cannot pass by accident of a fake that ignores the spelling.
// Every case drives the production handler through the router.

const (
	windowedValue = "0.2511"
	btcXLM        = "crypto:BTC/crypto:XLM"
)

// isFlaggedFrozen reads the flag the way a client does. `frozen` is
// omitempty on the wire, so "not frozen" is the key's ABSENCE — a test
// looking for `"frozen":false` would never match either way.
func isFlaggedFrozen(body string) bool {
	return strings.Contains(body, `"frozen":true`)
}

func windowedBody(t *testing.T, srv *v1.Server, query string) string {
	t.Helper()
	ts := startHTTPTest(t, srv.Handler())
	status, body := getBody(t, ts.URL+"/v1/price?"+query)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	if !strings.Contains(body, `"price":"`+windowedValue+`"`) {
		t.Fatalf("fixture did not serve the windowed value %s — the flag assertion below would be vacuous: %s", windowedValue, body)
	}
	return body
}

func TestPriceWindowed_FrozenFlagFollowsTheAliasTheValueWasReadFrom(t *testing.T) {
	// asset=native; the aggregator publishes (and freezes) crypto:XLM.
	srv := v1.New(v1.Options{
		Prices:       &stubPriceReader{}, // production always wires one; ?window= never reads it
		Freeze:       frozenPairs{xlmGBP: true},
		Triangulated: lkgPairs{xlmGBP + "/300": windowedValue},
	})
	body := windowedBody(t, srv, "asset=native&quote=fiat:GBP&window=300")
	if !strings.Contains(body, `"frozen":true`) {
		t.Errorf("value read from the frozen %s, but the flag was taken from the unfrozen requested literal: %s", xlmGBP, body)
	}
	// The echo contract is untouched: the client still sees its own id.
	if !strings.Contains(body, `"asset_id":"native"`) {
		t.Errorf("asset_id must echo the requested id: %s", body)
	}
}

func TestPriceWindowed_FreezeOnRequestedLiteralDoesNotFlagAHealthyAliasValue(t *testing.T) {
	// The marker is on native/fiat:GBP only; nothing is published under
	// it. The value comes from the healthy crypto:XLM market.
	srv := v1.New(v1.Options{
		Prices:       &stubPriceReader{}, // production always wires one; ?window= never reads it
		Freeze:       frozenPairs{nativeGBP: true},
		Triangulated: lkgPairs{xlmGBP + "/300": windowedValue},
	})
	body := windowedBody(t, srv, "asset=native&quote=fiat:GBP&window=300")
	if isFlaggedFrozen(body) {
		t.Errorf("healthy %s value flagged frozen from a marker on a different venue population (%s): %s", xlmGBP, nativeGBP, body)
	}
}

func TestPriceWindowed_FrozenFlagFollowsTheQuoteAliasToo(t *testing.T) {
	// The alias walk is two-dimensional. quote=native answered from the
	// crypto:XLM-quoted key is governed by THAT pair's marker.
	srv := v1.New(v1.Options{
		Prices:       &stubPriceReader{}, // production always wires one; ?window= never reads it
		Freeze:       frozenPairs{btcXLM: true},
		Triangulated: lkgPairs{btcXLM + "/3600": windowedValue},
	})
	body := windowedBody(t, srv, "asset=crypto:BTC&quote=native&window=3600")
	if !strings.Contains(body, `"frozen":true`) {
		t.Errorf("value read from the frozen %s, flag taken from the requested quote spelling: %s", btcXLM, body)
	}
}

// Non-regression guards — green before and after. The literal spelling
// is the common case and must keep its flag in both directions, and the
// first alias that HOLDS a value is the one whose marker is asked: a
// frozen sibling further down the walk says nothing about it.
func TestPriceWindowed_LiteralPairKeepsItsOwnFlag(t *testing.T) {
	for _, tc := range []struct {
		name   string
		freeze frozenPairs
		want   bool
	}{
		{"frozen literal", frozenPairs{xlmGBP: true}, true},
		{"unfrozen literal", frozenPairs{}, false},
		{"only a later sibling alias is frozen", frozenPairs{nativeGBP: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := v1.New(v1.Options{
				Prices:       &stubPriceReader{}, // production always wires one; ?window= never reads it
				Freeze:       tc.freeze,
				Triangulated: lkgPairs{xlmGBP + "/300": windowedValue, nativeGBP + "/300": "0.9999"},
			})
			body := windowedBody(t, srv, "asset=crypto:XLM&quote=fiat:GBP&window=300")
			if got := isFlaggedFrozen(body); got != tc.want {
				t.Errorf("frozen = %v, want %v: %s", got, tc.want, body)
			}
		})
	}
}
