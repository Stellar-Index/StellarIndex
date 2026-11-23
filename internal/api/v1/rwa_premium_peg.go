// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
)

// rwaPremiumRefusal names why a market day that cleared the floor may not carry a premium; "" lets it through.
type rwaPremiumRefusal func(day time.Time) string

func (r rwaPremiumRefusal) of(day time.Time) string {
	if r == nil {
		return ""
	}
	return r(day)
}

// rwaPremiumDayGuard holds the per-day judgements the market VWAP cannot carry itself: it pools every dollar spelling with
// no record of which peg or source priced it.
type rwaPremiumDayGuard struct {
	// depeg is the days on which any declared USD peg's own observed dollar market closed outside [depegBand].
	depeg map[time.Time]struct{}
	// excluded reports whether an operator source exclusion covers either leg of c's premium on day; nil judges nothing.
	excluded func(c rwaHistoryCandidate, source string, day time.Time) bool
}

// refusal is the guard for one member whose reference leg is read from source.
func (g rwaPremiumDayGuard) refusal(c rwaHistoryCandidate, source string) rwaPremiumRefusal {
	return func(day time.Time) string {
		if g.excluded != nil && g.excluded(c, source, day) {
			return RWAPremiumExcludedSourceExcluded
		}
		if _, off := g.depeg[day]; off {
			return RWAPremiumExcludedPegOutsideBand
		}
		return ""
	}
}

// rwaPremiumDayGuard reads both judgements once per assembly; ok=false when either cannot be read, since an unjudged day
// would be published as if it had been judged.
func (s *Server) rwaPremiumDayGuard(ctx context.Context, to time.Time) (rwaPremiumDayGuard, bool) {
	depeg, ok := s.rwaPremiumDepegDays(ctx, to)
	if !ok {
		return rwaPremiumDayGuard{}, false
	}
	set, judge, v := s.exclusionSnapshot(ctx)
	if v != pricingguard.NotWithheld {
		s.logger.Warn("rwa premium: source exclusions unreadable", "withholding", v)
		return rwaPremiumDayGuard{}, false
	}
	g := rwaPremiumDayGuard{depeg: depeg}
	if !judge {
		return g, true
	}
	quotes := s.rwaPremiumUSDQuotes()
	g.excluded = func(c rwaHistoryCandidate, source string, day time.Time) bool {
		end := day.Add(24 * time.Hour)
		asset := canonical.Asset{Type: canonical.AssetClassic, Code: c.code, Issuer: c.issuer}
		var books []canonical.Pair
		for _, form := range assetAliases(asset) {
			for _, q := range quotes {
				books = appendBook(books, canonical.Pair{Base: form, Quote: q})
			}
		}
		// The market day records no sources, so any exclusion naming the token withholds it.
		if judgeBucket(ctx, set, books, nil, day, end) != pricingguard.NotWithheld {
			return true
		}
		var sources []string
		if source != "" {
			sources = []string{source}
		}
		ref := canonical.Pair{Base: canonical.Asset{Type: canonical.AssetRWA, Code: c.feed}, Quote: rwaPremiumHistoryQuote}
		return judgeBucket(ctx, set, []canonical.Pair{ref}, sources, day, end) != pricingguard.NotWithheld
	}
	return g, true
}

// rwaPremiumDepegDays reads each declared USD peg's own observed `crypto:<STABLE>/fiat:USD` day and returns the days any closed
// outside [depegBand]: the market leg pools those pegs as dollars, so on such a day its VWAP is not a dollar price. A peg with
// no verified ticker cannot be observed and is not judged here.
func (s *Server) rwaPremiumDepegDays(ctx context.Context, to time.Time) (map[time.Time]struct{}, bool) {
	tickers := make([]canonical.Asset, 0, len(s.USDPeggedClassics))
	want := map[string]struct{}{}
	for _, peg := range s.USDPeggedClassics {
		ticker, ok := s.verifiedPegTicker(peg)
		if _, dup := want[ticker.String()]; !ok || dup {
			continue
		}
		want[ticker.String()] = struct{}{}
		tickers = append(tickers, ticker)
	}
	out := map[time.Time]struct{}{}
	if len(tickers) == 0 {
		return out, true
	}
	rows, err := s.MarketHistory.DailyMarketDays(ctx, tickers, []canonical.Asset{rwaPremiumHistoryQuote}, coverageFloorEpoch, to)
	if err != nil {
		s.logger.Warn("rwa premium: peg day read failed", "err", err)
		return nil, false
	}
	for _, r := range rows {
		if _, peg := want[r.AssetID]; peg && outsideDepegBand(r.VWAP) {
			out[r.Day.UTC()] = struct{}{}
		}
	}
	return out, true
}
