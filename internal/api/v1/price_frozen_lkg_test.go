// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// F013 (MNY-22): `flags.frozen=true` promises that the response carries
// the last-known-good value the freeze is holding (ADR-0019, and the
// `frozen` flag's own wording in openapi). The default /v1/price path
// read the newest closed prices_1m bucket — the bucket the anomaly
// detector had just refused — and stamped the flag on it from an
// independent marker read. These tests drive the production handler
// with a frozen pair whose prices_1m bucket has MOVED away from the
// held value, and assert the VALUE, not the flag.

// lkgPairs is the aggregator's VWAP cache, keyed
// "<asset>/<quote>/<window-seconds>" — the per-(pair, window) value a
// freeze keeps alive as its last-known-good.
type lkgPairs map[string]string

func (l lkgPairs) LookupTriangulatedVWAP(
	_ context.Context, base, quote canonical.Asset, window time.Duration,
) (string, bool, bool, error) {
	v, ok := l[base.String()+"/"+quote.String()+"/"+strconv.Itoa(int(window/time.Second))]
	return v, false, ok, nil
}

// frozenPairs is the freeze-marker set, keyed "<asset>/<quote>" exactly
// as the aggregator keys it: the LITERAL pair it prices, not every
// alias of it.
type frozenPairs map[string]bool

func (f frozenPairs) FrozenForPair(_ context.Context, asset, quote canonical.Asset) (bool, error) {
	return f[asset.String()+"/"+quote.String()], nil
}

const (
	heldLKG     = "0.2511"
	movedBucket = "0.3199"
	xlmGBP      = "crypto:XLM/fiat:GBP"
)

// movedBucketReader is the prices_1m side of a frozen XLM/GBP: the
// newest closed minute printed movedBucket, three sources, not stale —
// everything about it says "serve me".
func movedBucketReader() *stubPriceReader {
	return &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			xlmGBP: {AssetID: "crypto:XLM", Quote: "fiat:GBP", Price: movedBucket, PriceType: "vwap", WindowSeconds: 60},
		},
		sources: map[string][]string{xlmGBP: {"kraken", "coinbase", "bitstamp"}},
	}
}

func getBody(t *testing.T, url string) (int, string) {
	t.Helper()
	resp := mustGet(t, url)
	body, _ := readAll(resp)
	return resp.StatusCode, body
}

func TestPrice_FrozenPairServesLastKnownGoodNotTheMovedBucket(t *testing.T) {
	srv := v1.New(v1.Options{
		Prices:       movedBucketReader(),
		Freeze:       frozenPairs{xlmGBP: true},
		Triangulated: lkgPairs{xlmGBP + "/300": heldLKG},
	})
	ts := startHTTPTest(t, srv.Handler())

	status, body := getBody(t, ts.URL+"/v1/price?asset=crypto:XLM&quote=fiat:GBP")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	if strings.Contains(body, movedBucket) {
		t.Fatalf("frozen pair served the prices_1m bucket the freeze refused (%s): %s", movedBucket, body)
	}
	for _, want := range []string{
		`"price":"` + heldLKG + `"`,
		`"window_seconds":300`, // the value's real window, not the 60 it replaced
		`"frozen":true`,
		`"single_source":true`,
		`"stale":true`, // below the closed-1m-bucket contract, like every other non-bucket serve
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %s: %s", want, body)
		}
	}
	// The refused bucket's venues must not be credited for a value they
	// did not produce.
	if strings.Contains(body, "kraken") {
		t.Errorf("sources still credit the refused bucket's venues: %s", body)
	}
}

// The freeze lifecycle is per (pair, window) while the marker is per
// pair: the 5m key can be cold (a thin window is dropped before the
// freeze step) while 1h holds the value. The held value is served with
// ITS window, never relabelled as something it is not.
func TestPrice_FrozenPairFallsToTheWindowThatHoldsAValue(t *testing.T) {
	srv := v1.New(v1.Options{
		Prices:       movedBucketReader(),
		Freeze:       frozenPairs{xlmGBP: true},
		Triangulated: lkgPairs{xlmGBP + "/3600": heldLKG},
	})
	ts := startHTTPTest(t, srv.Handler())

	status, body := getBody(t, ts.URL+"/v1/price?asset=crypto:XLM&quote=fiat:GBP")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	if strings.Contains(body, movedBucket) {
		t.Fatalf("served the refused bucket: %s", body)
	}
	for _, want := range []string{`"price":"` + heldLKG + `"`, `"window_seconds":3600`, `"frozen":true`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %s: %s", want, body)
		}
	}
}

// Frozen with no held value anywhere: the only price on hand is the one
// the freeze exists to withhold. Refuse loudly rather than publish it
// under a flag that says it is something else.
func TestPrice_FrozenPairWithNoHeldValueRefusesRatherThanServeTheBucket(t *testing.T) {
	srv := v1.New(v1.Options{
		Prices:       movedBucketReader(),
		Freeze:       frozenPairs{xlmGBP: true},
		Triangulated: lkgPairs{},
	})
	ts := startHTTPTest(t, srv.Handler())

	status, body := getBody(t, ts.URL+"/v1/price?asset=crypto:XLM&quote=fiat:GBP")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", status, body)
	}
	if strings.Contains(body, movedBucket) {
		t.Fatalf("refusal leaked the withheld bucket: %s", body)
	}
	if !strings.Contains(body, "errors/price-unavailable") {
		t.Errorf("want the price-unavailable problem type: %s", body)
	}
}

// The substitution is destructive (it discards the bucket, its window
// and its sources), so pin the path where it must NOT fire: an unfrozen
// pair is served exactly as read even though a cached VWAP exists.
func TestPrice_UnfrozenPairStillServesTheClosedBucketUntouched(t *testing.T) {
	srv := v1.New(v1.Options{
		Prices:       movedBucketReader(),
		Freeze:       frozenPairs{},
		Triangulated: lkgPairs{xlmGBP + "/300": heldLKG},
	})
	ts := startHTTPTest(t, srv.Handler())

	status, body := getBody(t, ts.URL+"/v1/price?asset=crypto:XLM&quote=fiat:GBP")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	for _, want := range []string{
		`"price":"` + movedBucket + `"`, `"window_seconds":60`, `"stale":false`,
		`"sources":["kraken","coinbase","bitstamp"]`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %s: %s", want, body)
		}
	}
	if strings.Contains(body, heldLKG) || strings.Contains(body, `"frozen":true`) {
		t.Errorf("unfrozen pair must not be touched by the freeze path: %s", body)
	}
}

// XLM is asked for as `native` but priced — and frozen — as crypto:XLM.
// The alias walk reads crypto:XLM's prices_1m bucket, so the freeze
// that governs THAT pair must govern the response, or the refused
// bucket is one spelling away.
func TestPrice_FrozenAliasPairServesItsLastKnownGood(t *testing.T) {
	srv := v1.New(v1.Options{
		Prices:       movedBucketReader(), // only crypto:XLM/fiat:GBP has rows
		Freeze:       frozenPairs{xlmGBP: true},
		Triangulated: lkgPairs{xlmGBP + "/300": heldLKG},
	})
	ts := startHTTPTest(t, srv.Handler())

	status, body := getBody(t, ts.URL+"/v1/price?asset=native&quote=fiat:GBP")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	if strings.Contains(body, movedBucket) {
		t.Fatalf("alias spelling served the refused bucket: %s", body)
	}
	for _, want := range []string{`"asset_id":"native"`, `"price":"` + heldLKG + `"`, `"frozen":true`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %s: %s", want, body)
		}
	}
}

// /v1/price/batch stamps the same flag over the same read, so it owes
// the same value: the frozen row carries the held value, and a frozen
// row with nothing held is omitted (the batch contract's "no price")
// rather than shipped as the refused bucket.
func TestPriceBatch_FrozenRowCarriesLastKnownGood(t *testing.T) {
	reader := movedBucketReader()
	reader.snapshots["crypto:BTC/fiat:GBP"] = v1.PriceSnapshot{Price: "51234.5", PriceType: "vwap", WindowSeconds: 60}
	reader.sources["crypto:BTC/fiat:GBP"] = []string{"kraken", "coinbase"}

	t.Run("held value served", func(t *testing.T) {
		srv := v1.New(v1.Options{
			Prices:       reader,
			Freeze:       frozenPairs{xlmGBP: true},
			Triangulated: lkgPairs{xlmGBP + "/300": heldLKG},
		})
		ts := startHTTPTest(t, srv.Handler())
		status, body := getBody(t, ts.URL+"/v1/price/batch?asset_ids=crypto:XLM,crypto:BTC&quote=fiat:GBP")
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", status, body)
		}
		if strings.Contains(body, movedBucket) {
			t.Fatalf("batch served the refused bucket: %s", body)
		}
		for _, want := range []string{`"price":"` + heldLKG + `"`, `"price":"51234.5"`, `"frozen":true`} {
			if !strings.Contains(body, want) {
				t.Errorf("body missing %s: %s", want, body)
			}
		}
	})

	t.Run("nothing held omits the row", func(t *testing.T) {
		srv := v1.New(v1.Options{
			Prices:       reader,
			Freeze:       frozenPairs{xlmGBP: true},
			Triangulated: lkgPairs{},
		})
		ts := startHTTPTest(t, srv.Handler())
		status, body := getBody(t, ts.URL+"/v1/price/batch?asset_ids=crypto:XLM,crypto:BTC&quote=fiat:GBP")
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", status, body)
		}
		if strings.Contains(body, movedBucket) || strings.Contains(body, `"asset_id":"crypto:XLM"`) {
			t.Fatalf("frozen row with nothing held must be omitted: %s", body)
		}
		if !strings.Contains(body, `"price":"51234.5"`) {
			t.Errorf("the healthy row must still serve: %s", body)
		}
	})
}

// F013 residual: the freeze that governs a response is the freeze on the
// pair whose bucket is being SERVED. A `native` request answered from
// crypto:XLM's closed bucket is governed by crypto:XLM's marker alone; a
// marker that exists only on the requested literal (`native/fiat:GBP`)
// says nothing about the healthy bucket in hand. Before the bound, that
// literal marker discarded the healthy alias bucket anyway: with nothing
// held for `native` the request 503'd under a detail claiming a "refused
// bucket" that did not exist, the batch row vanished, and with a 24h
// value held for `native` that value replaced the healthy bucket —
// while asset=crypto:XLM served 200 / the bucket throughout.

const (
	healthyAliasBucket = "0.2500"
	literalHeld24h     = "0.2300"
	nativeGBP          = "native/fiat:GBP"
)

// healthyAliasReader has rows only under crypto:XLM/fiat:GBP — three
// venues, fresh — so every `native` request is served from that alias.
func healthyAliasReader() *stubPriceReader {
	return &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			xlmGBP: {AssetID: "crypto:XLM", Quote: "fiat:GBP", Price: healthyAliasBucket, PriceType: "vwap", WindowSeconds: 60},
		},
		sources: map[string][]string{xlmGBP: {"kraken", "coinbase", "bitstamp"}},
	}
}

// literalOnlyFreezeShapes are the two states the requested-literal marker
// can be in while the SERVED alias is unfrozen. Both must serve the
// healthy alias bucket exactly as read.
func literalOnlyFreezeShapes() map[string]lkgPairs {
	return map[string]lkgPairs{
		"nothing held for the literal": {},
		"24h held for the literal":     {nativeGBP + "/86400": literalHeld24h},
	}
}

func TestPrice_FreezeOnRequestedLiteralOnlyDoesNotDiscardTheHealthyAliasBucket(t *testing.T) {
	for name, cache := range literalOnlyFreezeShapes() {
		t.Run(name, func(t *testing.T) {
			srv := v1.New(v1.Options{
				Prices:       healthyAliasReader(),
				Freeze:       frozenPairs{nativeGBP: true}, // crypto:XLM/fiat:GBP is NOT frozen
				Triangulated: cache,
			})
			ts := startHTTPTest(t, srv.Handler())

			status, body := getBody(t, ts.URL+"/v1/price?asset=native&quote=fiat:GBP")
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200 with the healthy alias bucket: %s", status, body)
			}
			if strings.Contains(body, literalHeld24h) {
				t.Fatalf("the literal's held value replaced the healthy alias bucket: %s", body)
			}
			for _, want := range []string{
				`"asset_id":"native"`,
				`"price":"` + healthyAliasBucket + `"`,
				`"window_seconds":60`,
				`"sources":["kraken","coinbase","bitstamp"]`,
				`"stale":false`,
			} {
				if !strings.Contains(body, want) {
					t.Errorf("body missing %s: %s", want, body)
				}
			}
			// The served pair is not frozen; the flag describes the value
			// in the response, so it must not claim otherwise. (Both flags
			// are omitted from the wire when false.)
			if strings.Contains(body, `"frozen":true`) || strings.Contains(body, `"single_source":true`) {
				t.Errorf("an unfrozen three-venue bucket must not be flagged frozen/single_source: %s", body)
			}

			// Both spellings are the same market and must agree.
			_, direct := getBody(t, ts.URL+"/v1/price?asset=crypto:XLM&quote=fiat:GBP")
			if !strings.Contains(direct, `"price":"`+healthyAliasBucket+`"`) {
				t.Fatalf("control: crypto:XLM spelling should serve the bucket: %s", direct)
			}
		})
	}
}

func TestPriceBatch_FreezeOnRequestedLiteralOnlyKeepsTheHealthyAliasRow(t *testing.T) {
	for name, cache := range literalOnlyFreezeShapes() {
		t.Run(name, func(t *testing.T) {
			srv := v1.New(v1.Options{
				Prices:       healthyAliasReader(),
				Freeze:       frozenPairs{nativeGBP: true},
				Triangulated: cache,
			})
			ts := startHTTPTest(t, srv.Handler())

			status, body := getBody(t, ts.URL+"/v1/price/batch?asset_ids=native&quote=fiat:GBP")
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", status, body)
			}
			if strings.Contains(body, literalHeld24h) {
				t.Fatalf("the literal's held value replaced the healthy alias bucket: %s", body)
			}
			for _, want := range []string{
				`"asset_id":"native"`, // the row must not be omitted
				`"price":"` + healthyAliasBucket + `"`,
				`"window_seconds":60`,
				`"stale":false`,
			} {
				if !strings.Contains(body, want) {
					t.Errorf("body missing %s: %s", want, body)
				}
			}
			if strings.Contains(body, `"frozen":true`) {
				t.Errorf("an unfrozen served row must not flag the batch frozen: %s", body)
			}
		})
	}
}

// The bound must not weaken the main fix. When the SERVED alias is
// frozen, a second marker on the requested literal changes nothing: the
// served pair's held value is what goes out, never the refused bucket
// and never the literal's held value.
func TestPrice_FrozenServedAliasStillHoldsWhenTheLiteralIsAlsoFrozen(t *testing.T) {
	srv := v1.New(v1.Options{
		Prices: movedBucketReader(),
		Freeze: frozenPairs{xlmGBP: true, nativeGBP: true},
		Triangulated: lkgPairs{
			xlmGBP + "/300":      heldLKG,
			nativeGBP + "/86400": literalHeld24h,
		},
	})
	ts := startHTTPTest(t, srv.Handler())

	status, body := getBody(t, ts.URL+"/v1/price?asset=native&quote=fiat:GBP")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	if strings.Contains(body, movedBucket) || strings.Contains(body, literalHeld24h) {
		t.Fatalf("want the served pair's held value only: %s", body)
	}
	for _, want := range []string{`"price":"` + heldLKG + `"`, `"frozen":true`, `"stale":true`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %s: %s", want, body)
		}
	}
}

// ...and a frozen served alias with nothing held still refuses, even
// though the literal holds a value: one venue population's held value is
// never substituted for another's.
func TestPrice_FrozenServedAliasWithNothingHeldStillRefuses(t *testing.T) {
	srv := v1.New(v1.Options{
		Prices:       movedBucketReader(),
		Freeze:       frozenPairs{xlmGBP: true, nativeGBP: true},
		Triangulated: lkgPairs{nativeGBP + "/86400": literalHeld24h},
	})
	ts := startHTTPTest(t, srv.Handler())

	status, body := getBody(t, ts.URL+"/v1/price?asset=native&quote=fiat:GBP")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", status, body)
	}
	if strings.Contains(body, movedBucket) || strings.Contains(body, literalHeld24h) {
		t.Fatalf("refusal leaked a value: %s", body)
	}
}

// No closed-bucket read served the response (served alias is zero): the
// requested literal's marker is the only one there is, and it still
// governs — the bound applies only when a bucket was actually read.
func TestPrice_FreezeOnLiteralStillGovernsWhenNoBucketWasServed(t *testing.T) {
	srv := v1.New(v1.Options{
		Prices:       &stubPriceReader{}, // every alias misses -> fallback chain
		Freeze:       frozenPairs{nativeGBP: true},
		Triangulated: lkgPairs{nativeGBP + "/300": literalHeld24h},
	})
	ts := startHTTPTest(t, srv.Handler())

	status, body := getBody(t, ts.URL+"/v1/price?asset=native&quote=fiat:GBP")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	for _, want := range []string{`"price":"` + literalHeld24h + `"`, `"frozen":true`, `"single_source":true`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %s: %s", want, body)
		}
	}
}
