// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// rwaPremiumMemberGuard judges one member's days; a nil guard, or a nil func in it, judges nothing.
type rwaPremiumMemberGuard struct {
	// refuse names why a market day that cleared the floor may not carry a premium; "" lets it through.
	refuse func(day time.Time) string
	// refExcluded reports an exclusion over the reference series on day, so that day's value may not anchor the band.
	refExcluded func(day time.Time) bool
}

func (g *rwaPremiumMemberGuard) of(day time.Time) string {
	if g == nil || g.refuse == nil {
		return ""
	}
	return g.refuse(day)
}

func (g *rwaPremiumMemberGuard) anchors(day time.Time) bool {
	return g == nil || g.refExcluded == nil || !g.refExcluded(day)
}

// rwaPegDays is one declared USD peg's own observed dollar market, judged.
type rwaPegDays struct {
	// first is the earliest day the read returned for the peg, usable or not; before it there was no feed to judge with.
	first time.Time
	// inBand holds only the USABLE days (cleared the thin-market floor, no exclusion covers the peg's book) and whether
	// each closed inside [depegBand]; a later day absent from it is unobserved, never assumed at par.
	inBand map[time.Time]bool
}

// rwaPremiumDayGuard holds the per-day judgements the market VWAP cannot carry itself: it pools every dollar spelling with
// no record of which peg or source priced it.
type rwaPremiumDayGuard struct {
	pegs []rwaPegDays
	// market and reference report an operator source exclusion over that leg of c's premium on day; nil judges nothing.
	market    func(c rwaHistoryCandidate, day time.Time) bool
	reference func(c rwaHistoryCandidate, source string, day time.Time) bool
}

// member is the guard for one member whose reference leg is read from source. An exclusion is judged first, then a
// depegged dollar, then a dollar that could not be observed: a positive finding outranks a missing one.
func (g rwaPremiumDayGuard) member(c rwaHistoryCandidate, source string) *rwaPremiumMemberGuard {
	refExcluded := func(day time.Time) bool { return g.reference != nil && g.reference(c, source, day) }
	return &rwaPremiumMemberGuard{
		refExcluded: refExcluded,
		refuse: func(day time.Time) string {
			if (g.market != nil && g.market(c, day)) || refExcluded(day) {
				return RWAPremiumExcludedSourceExcluded
			}
			return rwaPremiumPegRefusal(g.pegs, day)
		},
	}
}

// rwaPremiumPegRefusal judges day against every peg whose market had been observed by then; like [proxyDeviationChecked], a
// missing observation is unchecked, never all-clear.
func rwaPremiumPegRefusal(pegs []rwaPegDays, day time.Time) string {
	unobserved := false
	for _, p := range pegs {
		if day.Before(p.first) {
			continue
		}
		in, seen := p.inBand[day]
		if !seen {
			unobserved = true
		} else if !in {
			return RWAPremiumExcludedPegOutsideBand
		}
	}
	if unobserved {
		return RWAPremiumExcludedPegUnobserved
	}
	return ""
}

// rwaPremiumDayGuard reads both judgements once per assembly; ok=false when either cannot be read, since an unjudged day
// would be published as if it had been judged.
func (s *Server) rwaPremiumDayGuard(ctx context.Context, to time.Time, floor rwaPremiumDayFloor) (rwaPremiumDayGuard, bool) {
	tickers, rows, ok := s.rwaPremiumPegMarkets(ctx, to)
	if !ok {
		return rwaPremiumDayGuard{}, false
	}
	set, judge, v := s.exclusionSnapshot(ctx)
	if v != pricingguard.NotWithheld {
		s.logger.Warn("rwa premium: source exclusions unreadable", "withholding", v)
		return rwaPremiumDayGuard{}, false
	}
	// The market day records no sources, so any exclusion naming a book withholds it.
	covers := func(books []canonical.Pair, sources []string, day time.Time) bool {
		return judge && judgeBucket(ctx, set, books, sources, day, day.Add(24*time.Hour)) != pricingguard.NotWithheld
	}
	g := rwaPremiumDayGuard{}
	for _, ticker := range tickers {
		var books []canonical.Pair
		for _, form := range assetAliases(ticker) {
			books = appendBook(books, canonical.Pair{Base: form, Quote: rwaPremiumHistoryQuote})
		}
		if p, ok := rwaPremiumJudgePeg(rows[ticker.String()], floor, func(day time.Time) bool { return covers(books, nil, day) }); ok {
			g.pegs = append(g.pegs, p)
		}
	}
	if !judge {
		return g, true
	}
	quotes := s.rwaPremiumUSDQuotes()
	g.market = func(c rwaHistoryCandidate, day time.Time) bool {
		asset := canonical.Asset{Type: canonical.AssetClassic, Code: c.code, Issuer: c.issuer}
		var books []canonical.Pair
		for _, form := range assetAliases(asset) {
			for _, q := range quotes {
				books = appendBook(books, canonical.Pair{Base: form, Quote: q})
			}
		}
		return covers(books, nil, day)
	}
	g.reference = func(c rwaHistoryCandidate, source string, day time.Time) bool {
		var sources []string
		if source != "" {
			sources = []string{source}
		}
		ref := canonical.Pair{Base: canonical.Asset{Type: canonical.AssetRWA, Code: c.feed}, Quote: rwaPremiumHistoryQuote}
		return covers([]canonical.Pair{ref}, sources, day)
	}
	return g, true
}

// rwaPremiumJudgePeg applies the market leg's own thin-market floor and the exclusion set to one peg's days, so a single
// thin or excluded print can neither pass nor blank a day; ok=false when the peg was never observed.
func rwaPremiumJudgePeg(days []timescale.MarketDay, floor rwaPremiumDayFloor, excluded func(day time.Time) bool) (rwaPegDays, bool) {
	if len(days) == 0 {
		return rwaPegDays{}, false
	}
	p := rwaPegDays{first: rwaPremiumFirstObserved(days), inBand: map[time.Time]bool{}}
	for _, d := range days {
		day := d.Day.UTC()
		if rwaPremiumDayClears(d, floor) && !excluded(day) {
			p.inBand[day] = !outsideDepegBand(d.VWAP)
		}
	}
	p.first = p.first.UTC()
	return p, true
}

// rwaPremiumPegMarkets reads each declared USD peg's own observed `crypto:<STABLE>/fiat:USD` days, keyed by ticker: the
// market leg pools those pegs as dollars, so a day its peg was not a dollar is not a dollar price. A peg with no verified
// ticker cannot be observed and is not judged here.
func (s *Server) rwaPremiumPegMarkets(ctx context.Context, to time.Time) ([]canonical.Asset, map[string][]timescale.MarketDay, bool) {
	tickers := make([]canonical.Asset, 0, len(s.USDPeggedClassics))
	out := map[string][]timescale.MarketDay{}
	for _, peg := range s.USDPeggedClassics {
		ticker, ok := s.verifiedPegTicker(peg)
		if _, dup := out[ticker.String()]; !ok || dup {
			continue
		}
		out[ticker.String()] = nil
		tickers = append(tickers, ticker)
	}
	if len(tickers) == 0 {
		return nil, out, true
	}
	rows, err := s.MarketHistory.DailyMarketDays(ctx, tickers, []canonical.Asset{rwaPremiumHistoryQuote}, coverageFloorEpoch, to)
	if err != nil {
		s.logger.Warn("rwa premium: peg day read failed", "err", err)
		return nil, nil, false
	}
	for _, r := range rows {
		if _, peg := out[r.AssetID]; peg {
			out[r.AssetID] = append(out[r.AssetID], r)
		}
	}
	return tickers, out, true
}
