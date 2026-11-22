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

// pegObservationMarkets lists, per peg a triangulated answer in quote may have been served through, the alias forms of that
// peg's directly observed `crypto:<STABLE>/fiat:<CODE>` market: every declared USD peg (a non-USD answer can ride a USD
// anchor), plus each stablecoin backer of a non-USD fiat quote. unbound counts declared pegs with no such market.
func (s *Server) pegObservationMarkets(quote canonical.Asset) (markets [][]canonical.Pair, unbound int) {
	seen := map[string]struct{}{}
	for _, peg := range s.USDPeggedClassics {
		ticker, ok := s.verifiedPegTicker(peg)
		if !ok {
			unbound++
			continue
		}
		markets = appendPegMarket(markets, seen, ticker, "USD")
	}
	if quote.Type == canonical.AssetFiat && quote.Code != "USD" {
		for _, code := range aggregate.FiatBackers(quote.Code) {
			if ticker, err := canonical.NewCryptoAsset(code); err == nil {
				markets = appendPegMarket(markets, seen, ticker, quote.Code)
			}
		}
	}
	return markets, unbound
}

// verifiedPegTicker binds a declared classic peg to the crypto:<STABLE> ticker the verified catalogue issues under that exact
// identity (any alias form). A peg sharing only the code is not that ticker, so its own peg is not observed by that market.
func (s *Server) verifiedPegTicker(peg canonical.Asset) (canonical.Asset, bool) {
	for _, alias := range canonical.AssetAliasStrings(peg) {
		vc, ok := s.VerifiedCurrencies.LookupByStellarAssetID(alias)
		if !ok {
			continue
		}
		ticker, err := canonical.NewCryptoAsset(vc.Ticker)
		if err != nil || !aggregate.IsFiatProxyFor(ticker, "USD") {
			return canonical.Asset{}, false
		}
		return ticker, true
	}
	return canonical.Asset{}, false
}

// appendPegMarket adds ticker's observed market against fiat once per alias class, with every alias form of the ticker.
func appendPegMarket(markets [][]canonical.Pair, seen map[string]struct{}, ticker canonical.Asset, fiat string) [][]canonical.Pair {
	quote, err := canonical.NewFiatAsset(fiat)
	if err != nil {
		return markets
	}
	var forms []canonical.Pair
	for _, alias := range canonical.AssetAliases(ticker) {
		if _, dup := seen[alias.String()+"/"+fiat]; dup {
			return markets
		}
		if pair, err := canonical.NewPair(alias, quote); err == nil {
			forms = append(forms, pair)
		}
	}
	for _, pair := range forms {
		seen[pair.Base.String()+"/"+fiat] = struct{}{}
	}
	if len(forms) == 0 {
		return markets
	}
	return append(markets, forms)
}

// proxyDeviationChecked reports whether any peg a triangulated answer in quote may rely on has an observed price at `at`
// outside the [depegBand]. When none is seen outside it, unchecked says some peg had no usable observation (unbound, missed or
// withheld) and failed says some read errored; either way false is not a "peg holds" verdict.
func (s *Server) proxyDeviationChecked(ctx context.Context, quote canonical.Asset, at time.Time) (deviated, unchecked, failed bool) {
	if s.PriceAt == nil {
		return false, false, false
	}
	markets, unbound := s.pegObservationMarkets(quote)
	unchecked = unbound > 0
	for _, forms := range markets {
		value, observed, readFailed := s.observePeg(ctx, forms, at)
		switch {
		case observed:
			if outsideDepegBand(value) {
				return true, false, false
			}
		case readFailed:
			unchecked, failed = true, true
		default:
			unchecked = true
		}
	}
	return false, unchecked, failed
}

// observePeg reads one peg's observed price under the first alias form that answers.
func (s *Server) observePeg(ctx context.Context, forms []canonical.Pair, at time.Time) (value string, observed, readFailed bool) {
	for _, pair := range forms {
		v, _, _, err := s.PriceAt.PriceAt(ctx, pair, at, priceAtMaxLookback)
		switch {
		case err == nil:
			return v, true, false
		case !isPriceAtMiss(err):
			readFailed = true
		}
	}
	return "", false, readFailed
}

// withProxyDeviation stamps a triangulated answer's depeg flag. One whose peg check did not answer says so on the wire; a failed
// read is also degraded (no-store), while a miss is a standing state the cache may hold.
func (s *Server) withProxyDeviation(ctx context.Context, f Flags, quote canonical.Asset, at time.Time) Flags {
	if !f.Triangulated {
		return f
	}
	deviated, unchecked, failed := s.proxyDeviationChecked(ctx, quote, at)
	f.ProxyDeviation = deviated
	f.ProxyDeviationUnchecked = unchecked
	f.Degraded = f.Degraded || failed
	return f
}
