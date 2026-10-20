// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"math/big"
	"net/http"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// cryptoCrossLegs are the two USD legs a crypto-quoted cross divides, each as read; the freeze
// markers and venue counts that govern the cross are theirs.
type cryptoCrossLegs struct {
	asset, quote crossLeg
}

// crossLeg is one leg of a cross: the snapshot read, its venues, and the alias it was read under
// (zero when a fallback answered).
type crossLeg struct {
	snap    PriceSnapshot
	sources []string
	served  canonical.Asset
}

// closedUSDAnchoredFiatCross prices a NON-fiat asset in a fiat for closed surfaces: price(asset, USD) x the fixing binding at the
// USD leg's bucket end (ADR-0051, ADR-0018), so the answer never depends on when or where it is read. The USD leg resolves first
// so a withheld leg propagates (a rate onto it would publish it through an ungated door).
func (s *Server) closedUSDAnchoredFiatCross(ctx context.Context, asset, quote canonical.Asset) fallbackResult {
	if asset.Type == canonical.AssetFiat || !asset.IsMapped() || quote.Type != canonical.AssetFiat || quote.Code == "USD" {
		return fallbackResult{}
	}
	usdSnap, usdSources, usdServed, usdStale, ok, withheld := s.resolveUSDLeg(ctx, asset)
	if withheld {
		return fallbackResult{withheld: PriceWithheldUnattributed}
	}
	if !ok || s.fxFixings == nil || isDeclaredPeg(usdSnap) {
		return fallbackResult{}
	}
	e := time.Time(usdSnap.ObservedAt)
	bound, err := s.bindFXFixings(ctx, []string{quote.Code}, e)
	if miss, done := fxBindFailure(err); done {
		return miss
	}
	b := bound[quote.Code]
	out, ok := convertAtFixing(usdSnap, usdSources, quote, b)
	if !ok {
		return fallbackResult{withheld: PriceWithheldFXLeg}
	}
	out.AssetID = asset.String()
	out.usdLeg = &usdLegFacts{sources: usdSources, served: usdServed}
	return fallbackResult{
		snap: out, sources: appendFXSource(usdSources, b.Source), triangulated: true, ok: true,
		stale: usdStale || fxFixingStale(e, b),
	}
}

// closedUSDAnchoredCryptoCross prices a non-fiat asset in a crypto ticker it has no market against: price(asset, USD) / price(quote, USD)
// in exact rationals (invariant 1).
// Both legs resolve through [Server.resolveUSDLeg], so a withheld leg withholds the cross; legs from different buckets are stale (a ratio
// of two moments is a price at neither).
func (s *Server) closedUSDAnchoredCryptoCross(ctx context.Context, asset, quote canonical.Asset) fallbackResult {
	if asset.Type == canonical.AssetFiat || !asset.IsMapped() || quote.Type != canonical.AssetCrypto || sameAsset(asset, quote) {
		return fallbackResult{}
	}
	var legs cryptoCrossLegs
	var staleLeg bool
	for _, l := range []struct {
		a   canonical.Asset
		leg *crossLeg
	}{{asset, &legs.asset}, {quote, &legs.quote}} {
		snap, sources, served, stale, ok, withheld := s.resolveUSDLeg(ctx, l.a)
		if withheld {
			return fallbackResult{withheld: PriceWithheldUnattributed}
		}
		// A declared peg is a 1:1 claim against USD, not an observation to divide by.
		if !ok || isDeclaredPeg(snap) {
			return fallbackResult{}
		}
		*l.leg = crossLeg{snap: snap, sources: sources, served: served}
		staleLeg = staleLeg || stale
	}
	out, ok := composeCryptoCross(asset, quote, legs.asset.snap, legs.quote.snap)
	if !ok {
		return fallbackResult{}
	}
	out.cryptoCross = &legs
	aligned := time.Time(legs.asset.snap.ObservedAt).Equal(time.Time(legs.quote.snap.ObservedAt))
	return fallbackResult{
		snap: out, sources: unionSources(legs.asset.sources, legs.quote.sources), triangulated: true, ok: true,
		stale: staleLeg || !aligned,
	}
}

// composeCryptoCross divides two USD legs; a zero, negative or unparsable leg is a miss.
// observed_at is the older leg's and the wider window governs.
func composeCryptoCross(asset, quote canonical.Asset, a, q PriceSnapshot) (PriceSnapshot, bool) {
	num, okA := new(big.Rat).SetString(a.Price)
	den, okQ := new(big.Rat).SetString(q.Price)
	if !okA || !okQ || num.Sign() <= 0 || den.Sign() <= 0 {
		return PriceSnapshot{}, false
	}
	out := a
	out.AssetID, out.Quote = asset.String(), quote.String()
	out.Price = formatCrossRate(new(big.Rat).Quo(num, den))
	out.ObservedAt = WireTime(olderNonZero(time.Time(a.ObservedAt), time.Time(q.ObservedAt)))
	if q.WindowSeconds > out.WindowSeconds {
		out.WindowSeconds = q.WindowSeconds
	}
	return out, true
}

// resolveFrozenCryptoCross governs a crypto-quoted cross by both legs' markers, read as /v1/price?quote=fiat:USD reads them; a
// frozen leg contributes its held value, or serves nothing if none is held, never the refused bucket divided through.
func (s *Server) resolveFrozenCryptoCross(r *http.Request, c *cryptoCrossLegs, requested, quote canonical.Asset) frozenResolution {
	a := s.resolveFrozenServe(r, requested, c.asset.served, defaultPriceQuote)
	q := s.resolveFrozenServe(r, quote, c.quote.served, defaultPriceQuote)
	checked := a.checked && q.checked
	switch {
	case a.outcome == frozenServeNothingHeld || q.outcome == frozenServeNothingHeld:
		return frozenResolution{outcome: frozenServeNothingHeld, checked: checked}
	case a.outcome == frozenServeNotFrozen && q.outcome == frozenServeNotFrozen:
		return frozenResolution{outcome: frozenServeNotFrozen, checked: checked}
	}
	aSnap, aSources, pairBase := c.asset.snap, c.asset.sources, c.asset.served
	if a.outcome == frozenServeHeld {
		aSnap, aSources, pairBase = a.snapshot, a.sources, a.pairBase
	}
	qSnap, qSources := c.quote.snap, c.quote.sources
	if q.outcome == frozenServeHeld {
		qSnap, qSources = q.snapshot, q.sources
	}
	out, ok := composeCryptoCross(requested, quote, aSnap, qSnap)
	if !ok {
		return frozenResolution{outcome: frozenServeNothingHeld, checked: checked}
	}
	return frozenResolution{
		outcome: frozenServeHeld, checked: checked, snapshot: out,
		sources: unionSources(aSources, qSources), triangulated: true, pairBase: pairBase,
	}
}
