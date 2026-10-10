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

// `flags.frozen=true` promises that the response carries
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
) (v1.CachedVWAP, bool, error) {
	v, ok := l[base.String()+"/"+quote.String()+"/"+strconv.Itoa(int(window/time.Second))]
	return v1.CachedVWAP{Value: v, ObservedAt: time.Now().UTC()}, ok, nil
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

// heldSincePairs is lkgPairs with a fixed observation stamp, so a test
// can tell the held value's own observed_at from the read time.
type heldSincePairs struct {
	values lkgPairs
	at     time.Time
}

func (h heldSincePairs) LookupTriangulatedVWAP(
	ctx context.Context, base, quote canonical.Asset, window time.Duration,
) (v1.CachedVWAP, bool, error) {
	v, ok, err := h.values.LookupTriangulatedVWAP(ctx, base, quote, window)
	v.ObservedAt = h.at
	return v, ok, err
}

// A freeze serves its held value for the whole hold, so the read time
// can be tens of minutes past the value's observation. Every surface
// serving a held value must stamp the value's own observed_at, or a
// client reads a frozen price as current.
func TestFrozenHeldValueCarriesItsOwnObservedAt(t *testing.T) {
	heldSince := time.Now().UTC().Add(-40 * time.Minute).Truncate(time.Second)
	stamp := heldSince.Format(time.RFC3339)
	for _, tc := range []struct{ url, want string }{
		{"/v1/price?asset=crypto:XLM&quote=fiat:GBP", `"observed_at":"` + stamp + `"`},
		{"/v1/price/batch?asset_ids=crypto:XLM&quote=fiat:GBP", `"observed_at":"` + stamp + `"`},
		{lastPriceURL, `"timestamp":"` + stamp + `"`},
	} {
		t.Run(tc.url, func(t *testing.T) {
			reader := movedBucketReader()
			reader.snapshots[xlmUSD] = movedUSDBucketReader().snapshots[xlmUSD]
			srv := v1.New(v1.Options{
				Prices: reader,
				Freeze: frozenPairs{xlmGBP: true, xlmUSD: true},
				Triangulated: heldSincePairs{
					values: lkgPairs{xlmGBP + "/300": heldLKG, xlmUSD + "/300": heldUSD},
					at:     heldSince,
				},
			})
			ts := startHTTPTest(t, srv.Handler())

			status, body := getBody(t, ts.URL+tc.url)
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", status, body)
			}
			for _, want := range []string{tc.want, `"frozen":true`, `"stale":true`} {
				if !strings.Contains(body, want) {
					t.Errorf("body missing %s: %s", want, body)
				}
			}
		})
	}
}

// The freeze that governs a response is the freeze on the
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
