// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"math/big"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
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
// anchor), each stablecoin backer of a non-USD fiat quote, and the backer quoting each fiat-combine book that fed the
// answer (books; nil off the combine), since a USD combine reads X/crypto:<STABLE> books plain triangulation never does.
// unbound counts declared pegs with no such market.
func (s *Server) pegObservationMarkets(quote canonical.Asset, books []canonical.Pair) (markets [][]canonical.Pair, unbound int) {
	seen := map[string]struct{}{}
	for _, peg := range s.USDPeggedClassics {
		ticker, ok := s.verifiedPegTicker(peg)
		if !ok {
			unbound++
			continue
		}
		markets = appendPegMarket(markets, seen, ticker, "USD")
	}
	if quote.Type != canonical.AssetFiat {
		return markets, unbound
	}
	if quote.Code != "USD" {
		for _, code := range aggregate.FiatBackers(quote.Code) {
			if ticker, err := canonical.NewCryptoAsset(code); err == nil {
				markets = appendPegMarket(markets, seen, ticker, quote.Code)
			}
		}
	}
	for _, book := range books {
		ticker := book.Quote
		if t, ok := s.verifiedPegTicker(book.Quote); ok {
			ticker = t // a classic spelling of a USD stablecoin
		}
		if ticker.Type == canonical.AssetCrypto && aggregate.IsFiatProxyFor(ticker, quote.Code) {
			markets = appendPegMarket(markets, seen, ticker, quote.Code)
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
	return s.pegDeviation(ctx, quote, at, nil)
}

// pegDeviation is [Server.proxyDeviationChecked] also covering the pegs quoting the fiat-combine books that fed the answer.
func (s *Server) pegDeviation(ctx context.Context, quote canonical.Asset, at time.Time, books []canonical.Pair) (deviated, unchecked, failed bool) {
	if s.PriceAt == nil {
		return false, false, false
	}
	markets, unbound := s.pegObservationMarkets(quote, books)
	unchecked = unbound > 0
	for _, forms := range markets {
		value, observed, stale, readFailed := s.observePeg(ctx, forms, at)
		switch {
		case observed && !stale:
			if outsideDepegBand(value) {
				return true, false, false
			}
		case observed:
			// A stale reading only adds doubt: out of band it still warns, in band it is never an all-clear.
			deviated = deviated || outsideDepegBand(value)
			unchecked = true
			failed = failed || readFailed
		case readFailed:
			unchecked, failed = true, true
		default:
			unchecked = true
		}
	}
	return deviated, unchecked, failed
}

// pegEvidenceMaxAge is how long before `at` a peg reading's bucket may have OPENED and still count as an observation at `at`:
// one 1m bucket plus 15m grace past its close, whatever rung served it. A stalled feed's last in-band bucket (often a 1d
// rung the price-at ladder falls back to) is not evidence that the peg holds now.
const pegEvidenceMaxAge = 17 * time.Minute

// pegReadingCurrent reports whether a reading that closed at observedAt, from a resolutionSeconds-wide bucket, observes `at`.
func pegReadingCurrent(at, observedAt time.Time, resolutionSeconds int) bool {
	if observedAt.IsZero() {
		return false
	}
	opened := observedAt.Add(-time.Duration(resolutionSeconds) * time.Second)
	return at.Sub(opened) <= pegEvidenceMaxAge
}

// observePeg reads one peg's observed price under the first alias form that answers currently. When only a reading too old
// to observe `at` answers, it is returned with stale set: it can still show a depeg, never that the peg holds.
func (s *Server) observePeg(ctx context.Context, forms []canonical.Pair, at time.Time) (value string, observed, stale, readFailed bool) {
	for _, pair := range forms {
		v, observedAt, resolution, _, err := s.PriceAt.PriceAt(ctx, pair, at, priceAtMaxLookback)
		switch {
		case err == nil && pegReadingCurrent(at, observedAt, resolution):
			return v, true, false, false
		case err == nil:
			if !observed || outsideDepegBand(v) {
				value, observed, stale = v, true, true
			}
		case !isPriceAtMiss(err):
			readFailed = true
		}
	}
	return value, observed, stale, readFailed
}

// withProxyDeviation stamps a triangulated answer's depeg flag, judged at every instant the answer was priced at. One whose peg
// check did not answer says so on the wire; a failed read is also degraded (no-store), while a miss is a standing state the cache
// may hold.
func (s *Server) withProxyDeviation(ctx context.Context, f Flags, quote canonical.Asset, instants ...time.Time) Flags {
	if !f.Triangulated {
		return f
	}
	f.ProxyDeviation, f.ProxyDeviationUnchecked = false, false
	for _, at := range instants {
		deviated, unchecked, failed := s.pegDeviation(ctx, quote, at, nil)
		f.ProxyDeviation = f.ProxyDeviation || deviated
		f.ProxyDeviationUnchecked = f.ProxyDeviationUnchecked || unchecked
		f.Degraded = f.Degraded || failed
	}
	return f
}

// withFiatCombineProxyDeviation stamps an answer from [Server.tradesInRangeWithStablecoinFallback] over [from, to): books are
// the constituents that fed it, whose stablecoin quotes are pegs the answer rode on too.
func (s *Server) withFiatCombineProxyDeviation(ctx context.Context, f Flags, quote canonical.Asset, from, to time.Time, books []canonical.Pair) Flags {
	if !f.Triangulated {
		return f
	}
	deviated, unchecked, failed := s.pegRangeDeviation(ctx, quote, from, to, books)
	f.ProxyDeviation = deviated
	f.ProxyDeviationUnchecked = unchecked
	f.Degraded = f.Degraded || failed
	return f
}

// pegRangeMaxBars bounds the bars one peg's window read may return; a window it truncates is unchecked.
const pegRangeMaxBars = 1440

// pegHourRungMaxAge is how far back prices_1h answers (priceAtResolutionLadder's second rung); older windows read daily bars.
const pegHourRungMaxAge = 45 * 24 * time.Hour

// pegRangeDeviation is [Server.pegDeviation] over a window of trades: the peg is judged at `to` as before, and any bar of a peg
// market's own series in [from, to) whose high or low leaves the band is a deviation (a bar's extremes keep every print inside
// it, so a coarse rung still sees a short depeg). A peg with no bars in the window, or no series reader, is unchecked: the
// reading at `to` says nothing about the trades before it.
func (s *Server) pegRangeDeviation(ctx context.Context, quote canonical.Asset, from, to time.Time, books []canonical.Pair) (deviated, unchecked, failed bool) {
	deviated, unchecked, failed = s.pegDeviation(ctx, quote, to, books)
	if deviated || s.PriceAt == nil || !from.Before(to) {
		return deviated, unchecked, failed
	}
	if s.History == nil {
		return false, true, failed
	}
	markets, _ := s.pegObservationMarkets(quote, books)
	interval := pegRangeInterval(s.now(), from, to)
	for _, forms := range markets {
		out, seen, readFailed := s.pegSeriesLeavesBand(ctx, forms, interval, from, to)
		if out {
			return true, false, false
		}
		unchecked = unchecked || !seen
		failed = failed || readFailed
	}
	return false, unchecked, failed
}

// pegRangeInterval is the finest rung still retained at from that spans [from, to) in at most pegRangeMaxBars bars.
func pegRangeInterval(now, from, to time.Time) string {
	age, window := now.Sub(from), to.Sub(from)
	switch {
	case age <= timescale.PriceAtMinuteRungMaxAge && window <= pegRangeMaxBars*time.Minute:
		return "1m"
	case age <= pegHourRungMaxAge && window <= pegRangeMaxBars*time.Hour:
		return "1h"
	default:
		return "1d"
	}
}

// pegSeriesLeavesBand reads one peg's bars in [from, to) under every alias form: out when any bar leaves the band, seen when a
// form answered with a complete (untruncated) series.
func (s *Server) pegSeriesLeavesBand(
	ctx context.Context, forms []canonical.Pair, interval string, from, to time.Time,
) (out, seen, readFailed bool) {
	for _, pair := range forms {
		bars, err := s.History.OHLCSeries(ctx, pair, interval, from, to, pegRangeMaxBars+1)
		if err != nil {
			readFailed = true
			continue
		}
		for _, b := range bars {
			if outsideDepegBand(b.H) || outsideDepegBand(b.L) {
				return true, true, readFailed
			}
		}
		seen = seen || (len(bars) > 0 && len(bars) <= pegRangeMaxBars)
	}
	return false, seen, readFailed
}
