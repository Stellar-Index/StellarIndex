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

// outsideDepegBand reports whether a decimal USD price of a peg sits more
// than [depegBand] from $1. An unparsable or non-positive price is no
// evidence either way, so it never trips the band.
func outsideDepegBand(price string) bool {
	p, ok := new(big.Rat).SetString(price)
	if !ok || p.Sign() <= 0 {
		return false
	}
	dev := p.Sub(p, big.NewRat(1, 1))
	return dev.Abs(dev).Cmp(depegBand) > 0
}

// pegUSDObservationPairs lists the directly observed `crypto:<STABLE>/fiat:USD`
// market of each declared USD peg. The proxy serves asset/<peg> as asset/USD,
// so the peg's own dollar market is the only check that assumption has.
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

// proxyDeviation reports whether any declared USD peg's observed dollar price
// at `at` is outside the [depegBand]. False also covers "no observation": the
// flag is omitted rather than asserting the peg held.
func (s *Server) proxyDeviation(ctx context.Context, at time.Time) bool {
	if s.PriceAt == nil {
		return false
	}
	for _, pair := range s.pegUSDObservationPairs() {
		value, _, _, err := s.PriceAt.PriceAt(ctx, pair, at, priceAtMaxLookback)
		if err == nil && outsideDepegBand(value) {
			return true
		}
	}
	return false
}
