// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"errors"
	"strings"
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
// Until RLT-090 / RLT-239 (#580) rateFor folded both into the first. The
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
	return src, pricer, asset.String()
}

// TestDEXTVLCache_PriceReadErrorCarriesThePreviousFigureForward is the
// defect. Every protocol on the shared fixture holds XLM, so a failed
// XLM read must carry all four forward at their previous figures and
// surface as a refresh error — not publish soroswap at $10.50 (its
// pegged leg alone) where it was $25.50.
func TestDEXTVLCache_PriceReadErrorCarriesThePreviousFigureForward(t *testing.T) {
	src, pricer, _ := priceReadErrorSources(t)
	c := NewDEXTVLCache(src)

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("healthy Refresh: %v", err)
	}
	before, _ := c.Snapshot()
	if got := before["soroswap"].TVLUSD; got != "25.50" {
		t.Fatalf("fixture drifted: healthy soroswap TVL = %q, want 25.50", got)
	}

	pricer.arm("native", errTVLPriceRead)
	err := c.Refresh(context.Background())
	// Not Fatal: the figures below are the harm, and they are reported
	// whether or not the error surfaced.
	var errText string
	if err == nil {
		t.Error("Refresh returned nil: a failed price read was admitted as a successful refresh")
	} else {
		errText = err.Error()
		if !errors.Is(err, errTVLPriceRead) {
			t.Errorf("Refresh error does not wrap the read failure: %v", err)
		}
	}

	after, _ := c.Snapshot()
	for _, name := range []string{"soroswap", "aquarius", "phoenix", "comet"} {
		if after[name] != before[name] {
			t.Errorf("%s: want the previous figure carried forward %+v, got %+v",
				name, before[name], after[name])
		}
		if !strings.Contains(errText, name) {
			t.Errorf("Refresh error does not name %s: %q", name, errText)
		}
		if snap, ok := c.Protocol(name); !ok || !snap.CarriedForward {
			t.Errorf("%s: published as fresh (CarriedForward=false) on a failed price read", name)
		}
	}
	if got := after["soroswap"].TVLUSD; got != "25.50" {
		t.Errorf("soroswap TVL = %q after a failed XLM price read, want the carried 25.50", got)
	}

	// The failure is memoised for the refresh like any other verdict:
	// four protocols asked about XLM, the failing store was asked once.
	if n := pricer.callsFor("native"); n != 1 {
		t.Errorf("failing price read issued %d times in one refresh, want 1", n)
	}

	// And it does not outlive the refresh that saw it.
	pricer.arm("native", nil)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("recovered Refresh: %v", err)
	}
	recovered, _ := c.Snapshot()
	if got := recovered["soroswap"].TVLUSD; got != "25.50" {
		t.Errorf("recovered soroswap TVL = %q, want 25.50", got)
	}
}

// TestDEXTVLCache_PriceReadErrorIsScopedToTheProtocolsThatHoldTheToken
// keeps the blast radius honest: only soroswap's pair B holds the
// no-market token, so only soroswap carries forward. The other three
// never asked the failing question and publish this cycle's figure.
func TestDEXTVLCache_PriceReadErrorIsScopedToTheProtocolsThatHoldTheToken(t *testing.T) {
	src, pricer, noMarketID := priceReadErrorSources(t)
	c := NewDEXTVLCache(src)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("healthy Refresh: %v", err)
	}
	if n := pricer.callsFor(noMarketID); n == 0 {
		t.Fatalf("fixture drifted: the pricer was never asked about %s, so a fault armed on it could not fire", noMarketID)
	}

	pricer.arm(noMarketID, errTVLPriceRead)
	err := c.Refresh(context.Background())
	if err == nil {
		t.Fatal("Refresh returned nil for a failed price read")
	}
	if !strings.Contains(err.Error(), "soroswap") {
		t.Errorf("error does not name soroswap: %v", err)
	}
	if snap, ok := c.Protocol("soroswap"); !ok || !snap.CarriedForward {
		t.Error("soroswap: published as fresh on a failed price read")
	}
	for _, name := range []string{"aquarius", "phoenix", "comet"} {
		if strings.Contains(err.Error(), name) {
			t.Errorf("%s holds no failing token but was failed: %v", name, err)
		}
		if snap, ok := c.Protocol(name); !ok || snap.CarriedForward {
			t.Errorf("%s: carried forward though it never read the failing token", name)
		}
	}
}

// TestDEXTVLCache_UnpricedTokenIsStillNotAnError is the other side of
// the distinction, and the reason the fix cannot simply fail a protocol
// whenever a leg has no rate. The no-market token answers (ok=false, nil
// error) on the healthy fixture: that is a fact about the token, the
// refresh succeeds, and the leg reads no_served_price exactly as before.
func TestDEXTVLCache_UnpricedTokenIsStillNotAnError(t *testing.T) {
	src, _, _ := priceReadErrorSources(t)
	c := NewDEXTVLCache(src)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh with an unpriced (not erroring) token: %v", err)
	}
	snap, ok := c.Protocol("soroswap")
	if !ok {
		t.Fatal("no soroswap snapshot")
	}
	var found bool
	for _, pool := range snap.Pools {
		for _, leg := range pool.Legs {
			if leg.Token != tvlTestNoMarketToken {
				continue
			}
			found = true
			if leg.Excluded != DEXTVLLegNoServedPrice {
				t.Errorf("unpriced leg excluded = %q, want %q", leg.Excluded, DEXTVLLegNoServedPrice)
			}
		}
	}
	if !found {
		t.Fatal("fixture drifted: no leg holds tvlTestNoMarketToken")
	}
}
