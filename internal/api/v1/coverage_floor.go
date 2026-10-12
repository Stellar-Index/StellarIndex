// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// CoverageFloorReader answers when served history for a pair BEGINS (min bucket over prices_<granularity>); [from, to) is half-open and `to` MUST be after `from`.
type CoverageFloorReader interface {
	EarliestBucket(ctx context.Context, pair canonical.Pair, granularity string, from, to time.Time) (time.Time, bool, error)
	EarliestBucketAsStored(ctx context.Context, pair canonical.Pair, granularity string, from, to time.Time) (time.Time, bool, error)
	EarliestBucketLiteralQuote(ctx context.Context, pair canonical.Pair, granularity string, from, to time.Time) (time.Time, bool, error)
}

// coverageProbeSpan names which stored rows a set's probes read, keeping a floor a property of the
// read it explains; [Server.coverageFloor] picks the matching [CoverageFloorReader] method.
type coverageProbeSpan uint8

const (
	// spanAliasedBothDirections: both legs' alias families, both stored directions. The default.
	spanAliasedBothDirections coverageProbeSpan = iota
	// spanAliasedAsStored: both legs' alias families, the requested orientation only.
	spanAliasedAsStored
	// spanLiteralQuote: the base leg's alias family and the quote leg's literal spelling, both
	// stored directions (the fiat /v1/ohlc combine).
	spanLiteralQuote
)

// String names the span in a probe warning so an operator can tell which read failed.
func (s coverageProbeSpan) String() string {
	switch s {
	case spanAliasedAsStored:
		return "as_stored"
	case spanLiteralQuote:
		return "literal_quote"
	case spanAliasedBothDirections:
	}
	return "aliased"
}

// Coverage-floor probe constants. Each bounds what an ANONYMOUS caller can spend by asking for
// empty windows.
const (
	// coverageFloorGranularity is the daily rung regardless of the requested grain: prices_1d has no retention policy and the
	// fewest rows per pair; finer rungs are materialized_only and can hold less history.
	coverageFloorGranularity = "1d"

	// coverageFloorTTL: the floor is close to immutable, so the TTL is set by how quickly a NEW pair
	// should stop being described as uncovered.
	coverageFloorTTL = 30 * time.Minute

	// coverageFloorFailedTTL is short, since a transient store error is not a fact about the pair,
	// yet it still bounds re-issuing the failing read.
	coverageFloorFailedTTL = 45 * time.Second

	// coverageFloorProbeTimeout caps ALL probes behind one response together (a fiat-quoted pair
	// probes its whole constituent set). A degraded database costs the response only the signal.
	coverageFloorProbeTimeout = 2 * time.Second

	// coverageFloorCacheMax bounds the entry count, since a caller can mint unlimited distinct
	// pairs. Past it, admitting a key evicts one; the `evicted` metric shows key enumeration.
	coverageFloorCacheMax = 4096
)

// coverageFloorEpoch is the lower bound of every probe: the pubnet genesis month. Binding it as a
// literal buys plan-time chunk exclusion at the bottom end.
var coverageFloorEpoch = time.Date(2015, 9, 30, 0, 0, 0, 0, time.UTC)

// coverageFloorOutcome is what one probe established about a pair. A FAILED probe means nothing is known and must not be
// skipped when folding: that could publish a floor later than the truth, the error direction this signal must avoid.
type coverageFloorOutcome uint8

const (
	coverageFloorFound coverageFloorOutcome = iota
	coverageFloorAbsent
	coverageFloorFailed
)

// coverageFloorEntry is one cached probe outcome. Absent and failed probes are cached like hits so
// an uncovered pair costs one read per TTL, except a probe that never finished.
type coverageFloorEntry struct {
	floor   time.Time
	outcome coverageFloorOutcome
	expires time.Time
}

// coverageFloorCache is the bounded, TTL'd per-process memo in front of [CoverageFloorReader].
type coverageFloorCache struct {
	mu      sync.Mutex
	entries map[string]coverageFloorEntry
}

// coverageFloorKey folds a pair onto the identity its floor belongs to under the span. Alias family (`native`, `crypto:XLM`, XLM SAC share
// one entry; under [spanLiteralQuote] the base leg only); direction (A/B and B/A share a floor for the both-orientations probe only); span
// (own prefix, so reads over different rows never share an entry). A literally-read leg keeps its spelling.
func coverageFloorKey(pair canonical.Pair, span coverageProbeSpan) string {
	base := canonical.CanonicalAsset(pair.Base).String()
	switch span {
	case spanAliasedAsStored:
		return "stored\x00" + base + "\x00" + canonical.CanonicalAsset(pair.Quote).String()
	case spanLiteralQuote:
		return "literalq\x00" + base + "\x00" + pair.Quote.String()
	case spanAliasedBothDirections:
	}
	quote := canonical.CanonicalAsset(pair.Quote).String()
	if base > quote {
		base, quote = quote, base
	}
	return "either\x00" + base + "\x00" + quote
}

// lookup returns a live cached entry, or ok=false when absent or expired. Expired entries are left
// for `store` to sweep, since deleting here would need the write lock on every miss.
func (c *coverageFloorCache) lookup(key string, now time.Time) (coverageFloorEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || !now.Before(e.expires) {
		return coverageFloorEntry{}, false
	}
	return e, true
}

// store admits an entry, dropping expired keys then, if still at the ceiling, one arbitrary key; map order is wanted so a
// caller enumerating pairs cannot steer the victim.
func (c *coverageFloorCache) store(key string, e coverageFloorEntry, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= coverageFloorCacheMax {
		for k, v := range c.entries {
			if !now.Before(v.expires) {
				delete(c.entries, k)
			}
		}
	}
	if len(c.entries) >= coverageFloorCacheMax {
		for k := range c.entries {
			delete(c.entries, k)
			obs.APICoverageFloorProbesTotal.WithLabelValues("evicted").Inc()
			break
		}
	}
	c.entries[key] = e
}

// coverageSet is the population a surface's serving read draws on, which its floor is a property of. A fiat quote makes it a SET (nothing
// quotes `fiat:USD` on chain: direct pair, stablecoin backers, declared pegs, on /v1/chart the XLM cross). A floor on the literal pair alone
// errs both ways, so it is measured over exactly the pairs the serving read enumerates, via the same helpers.
type coverageSet struct {
	// direct pairs: the serving read answers from any one that holds buckets, so the set's floor is
	// the EARLIEST of their floors.
	direct []canonical.Pair
	// derived series: each entry is the legs of one series composed bucket by bucket, which exists
	// only where EVERY leg does, so its floor is the LATEST of its legs' floors. Folding a leg into
	// direct would hand every non-XLM asset with any XLM market the floor of XLM's own fiat series.
	derived [][]canonical.Pair
	// span is which stored rows each probe reads, matched to the surface's serving read.
	span coverageProbeSpan
}

// ohlcCoverageSet mirrors [Server.ohlcSeriesWithAliases]: a non-fiat quote is the pair under the default span; a fiat quote
// reads literal pairs ([Server.usdPeggedConstituents]), hence [spanLiteralQuote]. Held-back SAC spellings belong in the set, as
// the floor is consulted only on an empty answer; a test pins probed and requested populations equal.
func (s *Server) ohlcCoverageSet(pair canonical.Pair) coverageSet {
	if pair.Quote.Type != canonical.AssetFiat {
		return coverageSet{direct: []canonical.Pair{pair}}
	}
	return coverageSet{direct: s.usdPeggedConstituents(pair), span: spanLiteralQuote}
}

// chartCoverageSet mirrors [Server.chartSeriesPoints] (alias spellings, fiat-quote proxies, the XLM cross as one derived entry)
// from the read's own helpers. The floor is consulted only on an EMPTY answer, so the read must read every listed pair (merge
// is per bucket for that reason).
func (s *Server) chartCoverageSet(pair canonical.Pair) coverageSet {
	set := coverageSet{direct: s.chartAliasPairs(pair)}
	if pair.Quote.Type != canonical.AssetFiat {
		return set
	}
	set.direct = append(set.direct, s.chartFiatProxyPairs(pair)...)
	if legs, ok := fiatCrossLegsThroughXLM(pair); ok {
		set.derived = append(set.derived, legs[:])
	}
	return set
}

// priceAtCoverageSet mirrors [Server.handlePriceAt]: the pair, then for a `fiat:USD` quote the declared USD pegs it retries,
// each via [Server.lookupPriceAt] (both stored directions, default span).
func (s *Server) priceAtCoverageSet(pair canonical.Pair) coverageSet {
	return coverageSet{direct: append([]canonical.Pair{pair}, s.priceAtUSDPegPairs(pair.Base, pair.Quote)...)}
}

// historyCoverageSet mirrors [Server.tradesInRangeAfterWithAliases]: the pair itself under the default span, since that walk
// re-expresses flipped rows into the requested orientation. A floor over unservable rows over-claims, over fewer under-reports.
func historyCoverageSet(pair canonical.Pair) coverageSet {
	return coverageSet{direct: []canonical.Pair{pair}}
}

// coverageFloor is one memoised probe: when the daily rung's history for `pair` begins, and whether that is known. A DB error
// caches as failed for the short TTL; an unfinished probe (caller left, deadline) is not cached, so one disconnect cannot make
// the pair unknowable for the full TTL.
func (s *Server) coverageFloor(ctx context.Context, pair canonical.Pair, span coverageProbeSpan) (time.Time, coverageFloorOutcome) {
	key := coverageFloorKey(pair, span)
	now := time.Now().UTC()
	if e, ok := s.coverageFloorCache.lookup(key, now); ok {
		obs.APICoverageFloorProbesTotal.WithLabelValues("hit").Inc()
		return e.floor, e.outcome
	}

	read := s.CoverageFloor.EarliestBucket
	switch span {
	case spanAliasedAsStored:
		read = s.CoverageFloor.EarliestBucketAsStored
	case spanLiteralQuote:
		read = s.CoverageFloor.EarliestBucketLiteralQuote
	case spanAliasedBothDirections:
	}
	floor, found, err := read(ctx, pair, coverageFloorGranularity, coverageFloorEpoch, now)
	if err != nil {
		obs.APICoverageFloorProbesTotal.WithLabelValues("error").Inc()
		switch {
		case errors.Is(err, context.Canceled):
			// The caller went away; nothing was learned, nothing to log.
			return time.Time{}, coverageFloorFailed
		case errors.Is(err, context.DeadlineExceeded):
			s.logger.Warn("coverage-floor probe ran out of time",
				"base", pair.Base.String(), "quote", pair.Quote.String(), "span", span.String())
			return time.Time{}, coverageFloorFailed
		}
		// Warn, not Error: the caller loses only an advisory annotation.
		s.logger.Warn("coverage-floor probe failed",
			"err", err, "base", pair.Base.String(), "quote", pair.Quote.String(), "span", span.String())
		s.coverageFloorCache.store(key, coverageFloorEntry{
			outcome: coverageFloorFailed,
			expires: now.Add(coverageFloorFailedTTL),
		}, now)
		return time.Time{}, coverageFloorFailed
	}
	outcome, result := coverageFloorAbsent, "absent"
	if found {
		outcome, result = coverageFloorFound, "found"
	}
	obs.APICoverageFloorProbesTotal.WithLabelValues(result).Inc()
	s.coverageFloorCache.store(key, coverageFloorEntry{
		floor:   floor,
		outcome: outcome,
		expires: now.Add(coverageFloorTTL),
	}, now)
	return floor, outcome
}

// coverageFloorOf returns when served history for `set` begins, and whether known. Called only from an already-empty answer. An
// absent constituent is skipped, but one FAILED probe makes the floor unknown: it might hold the earliest bucket, and a floor
// too LATE falsely claims coverage.
func (s *Server) coverageFloorOf(ctx context.Context, set coverageSet) (time.Time, bool) {
	// The memo is required: without it every probe is a read on a path an anonymous caller drives.
	if s.CoverageFloor == nil || s.coverageFloorCache == nil {
		return time.Time{}, false
	}
	probeCtx, cancel := context.WithTimeout(ctx, coverageFloorProbeTimeout)
	defer cancel()

	var (
		floor time.Time
		known bool
	)
	fold := func(candidate time.Time) {
		if !known || candidate.Before(floor) {
			floor, known = candidate, true
		}
	}
	for _, pair := range set.direct {
		f, outcome := s.coverageFloor(probeCtx, pair, set.span)
		switch outcome {
		case coverageFloorFailed:
			return time.Time{}, false
		case coverageFloorFound:
			fold(f)
		case coverageFloorAbsent:
		}
	}
	for _, legs := range set.derived {
		var (
			latest   time.Time
			complete = len(legs) > 0
		)
		for _, leg := range legs {
			f, outcome := s.coverageFloor(probeCtx, leg, set.span)
			switch outcome {
			case coverageFloorFailed:
				return time.Time{}, false
			case coverageFloorAbsent:
				complete = false
			case coverageFloorFound:
				if f.After(latest) {
					latest = f
				}
			}
		}
		if complete {
			fold(latest)
		}
	}
	return floor, known
}

// coverageAnnotation is what a handler stamps on an empty answer: the floor as `coverage_from`, and whether the range provably sits below
// it. `end` is the range's EXCLUSIVE upper bound (for a point lookup the instant itself). outside is true only when a floor is known AND the
// whole range ends at or before it; an unknown floor is not flagged (a missing daily bucket proves no absence of history).
func (s *Server) coverageAnnotation(ctx context.Context, set coverageSet, end time.Time) (from *time.Time, outside bool) {
	floor, known := s.coverageFloorOf(ctx, set)
	if !known {
		return nil, false
	}
	f := floor
	return &f, !end.After(floor)
}

// coverageAnnotationIfEmpty is [Server.coverageAnnotation] gated on `empty`. That gate is the cost
// bound of the whole signal: a populated answer explains itself and the probes never run.
func (s *Server) coverageAnnotationIfEmpty(
	ctx context.Context, set coverageSet, end time.Time, empty bool,
) (from *time.Time, outside bool) {
	if !empty {
		return nil, false
	}
	return s.coverageAnnotation(ctx, set, end)
}

// historyPageIsAmbiguous reports whether an empty /v1/history page is ambiguous: reached by cursor means the caller paged past
// the last row, and the cursor shadows `from`, so the signal would describe an unread window.
func historyPageIsAmbiguous(rows int, afterTS time.Time) bool {
	return rows == 0 && afterTS.IsZero()
}

// writeChartSeries emits /v1/chart with its coverage annotation and gap signal ([ChartSeries.markDiscontinuity]); truncation
// says nothing about an EMPTY series or a mid-series hole. The window ends at NOW, so `outside_coverage` never fires; an empty
// series with `coverage_from` means a quiet pair, without it this deployment holds nothing.
func (s *Server) writeChartSeries(
	w http.ResponseWriter, r *http.Request,
	pair canonical.Pair, series ChartSeries, walk chartWalkResult,
) {
	series.markDiscontinuity()
	coverageFrom, outside := s.coverageAnnotationIfEmpty(
		r.Context(), s.chartCoverageSet(pair), time.Now().UTC(), len(series.Points) == 0)
	writeJSONCoverage(w, series, Flags{
		Triangulated:    walk.proxied,
		Stale:           walk.degraded,
		Degraded:        walk.degraded,
		OutsideCoverage: outside,
	}, coverageFrom)
}
