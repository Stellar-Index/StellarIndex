// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"math/big"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// depegBand is the deviation from $1 beyond which a USD peg's own market is
// taken to disagree with the proxy's assumption that the peg is worth $1.
var depegBand = big.NewRat(2, 100)

// outsideDepegBand reports whether a peg's decimal USD price is more than [depegBand] from $1; an unparsable or non-positive
// price is no evidence and never trips it.
func outsideDepegBand(price string) bool {
	p, ok := new(big.Rat).SetString(price)
	if !ok || p.Sign() <= 0 {
		return false
	}
	dev := p.Sub(p, big.NewRat(1, 1))
	return dev.Abs(dev).Cmp(depegBand) > 0
}

// pegUSDObservationPairs lists the directly observed `crypto:<STABLE>/fiat:USD` market of each declared USD peg; the proxy
// serves asset/<peg> as asset/USD, so this is the only check on that assumption.
func (s *Server) pegUSDObservationPairs() []canonical.Pair {
	usd, err := canonical.NewFiatAsset("USD")
	if err != nil {
		return nil
	}
	var out []canonical.Pair
	seen := make(map[string]struct{}, len(s.USDPeggedClassics))
	for _, peg := range s.USDPeggedClassics {
		ticker, err := canonical.NewCryptoAsset(peg.Code)
		if err != nil || !aggregate.IsFiatProxyFor(ticker, "USD") {
			continue
		}
		if _, dup := seen[ticker.Code]; dup {
			continue
		}
		pair, err := canonical.NewPair(ticker, usd)
		if err != nil {
			continue
		}
		seen[ticker.Code] = struct{}{}
		out = append(out, pair)
	}
	return out
}

// proxyDeviationChecked reports whether any declared USD peg's observed price at `at` is outside the [depegBand]. When none is
// seen outside it, unchecked says some peg had no usable observation (missed or withheld) and failed says some read errored;
// either way false is not a "peg holds" verdict.
func (s *Server) proxyDeviationChecked(ctx context.Context, at time.Time) (deviated, unchecked, failed bool) {
	if s.PriceAt == nil {
		return false, false, false
	}
	for _, pair := range s.pegUSDObservationPairs() {
		value, _, _, err := s.PriceAt.PriceAt(ctx, pair, at, priceAtMaxLookback)
		switch {
		case err == nil:
			if outsideDepegBand(value) {
				return true, false, false
			}
		case isPriceAtMiss(err):
			unchecked = true
		default:
			unchecked, failed = true, true
		}
	}
	return false, unchecked, failed
}

// withProxyDeviation stamps a triangulated answer's depeg flag. One whose peg check did not answer says so on the wire; a failed
// read is also degraded (no-store), while a miss is a standing state the cache may hold.
func (s *Server) withProxyDeviation(ctx context.Context, f Flags, at time.Time) Flags {
	if !f.Triangulated {
		return f
	}
	deviated, unchecked, failed := s.proxyDeviationChecked(ctx, at)
	f.ProxyDeviation = deviated
	f.ProxyDeviationUnchecked = unchecked
	f.Degraded = f.Degraded || failed
	return f
}
