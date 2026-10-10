// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// A USD price read can fail in two ways that mean opposite things.
// "Nobody prices this token" is a fact about the token: the leg is
// honestly excluded as no_served_price and the figure is a lower bound.
// "The price read ERRORED" is a fact about this refresh: the token is
// exactly as priced as it was a minute ago, and we just could not ask.
//
// If rateFor folded both into the first, the
// error was memoised as "unpriceable" for the whole refresh, value()
// returned no_served_price with no error, every protocol's refresh
// therefore SUCCEEDED, and Refresh's carry-forward — which only runs
// when a protocol refresh returns an error — could not fire. One
// transient Postgres error on the XLM rate silently removed every
// XLM leg from the published DEX TVL and admitted the shrunken total as
// fresh.

// erroringTVLPricer is stubTVLPricer with a fault that can be armed per
// asset between refreshes, and a per-asset call count.
type erroringTVLPricer struct {
	mu     sync.Mutex
	rates  map[string]string
	failOn map[string]error
	calls  map[string]int
}

func (p *erroringTVLPricer) USDPriceAt(_ context.Context, asset canonical.Asset, _ time.Time) (string, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	id := asset.String()
	if p.calls == nil {
		p.calls = map[string]int{}
	}
	p.calls[id]++
	if err := p.failOn[id]; err != nil {
		return "", false, err
	}
	r, ok := p.rates[id]
	return r, ok, nil
}

func (p *erroringTVLPricer) arm(id string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failOn = map[string]error{}
	if err != nil {
		p.failOn[id] = err
	}
	p.calls = map[string]int{}
}

func (p *erroringTVLPricer) callsFor(id string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[id]
}

var errTVLPriceRead = errors.New("pq: canceling statement due to statement timeout")

// priceReadErrorSources is the shared fixture with soroswap pair B's
// first leg swapped for a WELL-FORMED token nobody prices. The shared
// fixture's tvlTestUnpriced cannot stand in: it fails strkey validation,
// so it is excluded as malformed_token before any price is read (see
// dex_tvl_pools_internal_test.go) and a fault armed on it never fires.
// Only soroswap holds the swapped-in token; its healthy figure is still
// $25.50 because an unpriced leg contributes nothing.
func priceReadErrorSources(t *testing.T) (DEXTVLSources, *erroringTVLPricer, string) {
	t.Helper()
	src := tvlTestSources()
	reserves, ok := src.SoroswapReserves.(stubTVLReserveReader)
	if !ok {
		t.Fatalf("fixture drifted: SoroswapReserves is %T", src.SoroswapReserves)
	}
	pairB := reserves.states[tvlTestPairB]
	pairB.Token0 = tvlTestNoMarketToken
	reserves.states[tvlTestPairB] = pairB

	asset, ok := tvlAssetForToken(tvlTestNoMarketToken)
	if !ok {
		t.Fatalf("fixture drifted: %s is not a well-formed token", tvlTestNoMarketToken)
	}
	pricer := &erroringTVLPricer{rates: map[string]string{"native": "0.5"}}
	src.Pricer = pricer
	src.Verified = tvlTestCatalogue(tvlTestNoMarketToken)
	return src, pricer, asset.String()
}
