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

// CoverageFloorReader answers when this deployment's served history for a pair BEGINS. An empty
// series is otherwise ambiguous ("the market was quiet" vs "the window predates anything held").
// Production wiring is timescale.Store: one bounded, index-backed `min(bucket)` over
// prices_<granularity>. The methods differ only in which stored rows they span, and a surface
// picks the one its own serving read spans, never a wider one: a floor over rows a surface cannot
// serve is a coverage claim about bars that will never arrive.
//   - EarliestBucket folds both legs' alias families and both stored directions, matching reads
//     that walk both legs' spellings (chartMergeAliasPairs, lookupPriceAt, the non-fiat
//     ohlcSeriesWithAliases, tradesInRangeAfterWithAliases).
//   - EarliestBucketAsStored reads the requested orientation only (no surface uses it today).
//   - EarliestBucketLiteralQuote drops the alias fold on the quote leg, matching the fiat combine
//     (ohlcSeriesFiatCombined), which reads each constituent under one named quote spelling.
//
// [from, to) is half-open and `to` MUST be after `from`; the store rejects a degenerate window so a
// bad probe range fails loudly instead of claiming a pair has no coverage.
type CoverageFloorReader interface {
	EarliestBucket(ctx context.Context, pair canonical.Pair, granularity string, from, to time.Time) (time.Time, bool, error)
	EarliestBucketAsStored(ctx context.Context, pair canonical.Pair, granularity string, from, to time.Time) (time.Time, bool, error)
	EarliestBucketLiteralQuote(ctx context.Context, pair canonical.Pair, granularity string, from, to time.Time) (time.Time, bool, error)
}

// coverageProbeSpan names which stored rows a set's probes read, keeping a floor a property of the
// read it explains. Each coverageSet declares its serving read's span and [Server.coverageFloor]
// picks the matching [CoverageFloorReader] method.
type coverageProbeSpan uint8

const (
	// spanAliasedBothDirections: both legs' alias families, both stored directions. The default.
	spanAliasedBothDirections coverageProbeSpan = iota
	// spanAliasedAsStored: both legs' alias families, the requested orientation only. No surface
	// selects it today; it is the capability a single-orientation read would need.
	spanAliasedAsStored
	// spanLiteralQuote: the base leg's alias family and the quote leg's literal spelling, both
	// stored directions. Used by the fiat /v1/ohlc combine, which names each constituent's quote
	// in exactly one form.
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

// Coverage-floor probe envelope. Every constant here exists to bound
// what an ANONYMOUS caller can spend by asking for empty windows.
const (
	// coverageFloorGranularity is the CAGG rung the floor is measured on: always the daily rung,
	// not the grain requested. prices_1d has no retention policy (only prices_1m can, and ships
	// disabled), and it is the coarsest rung with the fewest rows per pair, so it is the cheapest to
	// prove empty on a path an anonymous caller can drive. Finer rungs are materialized_only and
	// refreshed on their own schedule (timescale.CAGGsLiveForever), so they can hold less history.
	coverageFloorGranularity = "1d"

	// coverageFloorTTL is how long one answer is reused. The floor is close to immutable (it moves
	// when a pair's first bucket materialises or a backfill lands earlier rows), so the TTL is set
	// by how quickly a NEW pair should stop being described as uncovered.
	coverageFloorTTL = 30 * time.Minute

	// coverageFloorFailedTTL is how long a probe error is memoised. Short, because a transient
	// store error is not a fact about the pair and must not poison the advisory annotation for the
	// full TTL, yet it still bounds re-issuing the failing read on every empty window.
	coverageFloorFailedTTL = 45 * time.Second

	// coverageFloorProbeTimeout caps ALL probes behind one response together. A fiat-quoted pair's
	// floor spans its whole constituent set (see [coverageSet]) and each memo miss is one read, so
	// the ceiling is per response. A degraded database costs the response only the signal.
	coverageFloorProbeTimeout = 2 * time.Second

	// coverageFloorCacheMax bounds the entry count, since a caller can mint unlimited distinct
	// pairs. Past it, admitting a key evicts one; the `evicted` metric shows key enumeration.
	coverageFloorCacheMax = 4096
)

// coverageFloorEpoch is the lower bound of every probe: the pubnet
// genesis month. Nothing can be bucketed before it, so binding it as a
// literal costs no rows and buys plan-time chunk exclusion at the bottom
// end.
var coverageFloorEpoch = time.Date(2015, 9, 30, 0, 0, 0, 0, time.UTC)

// coverageFloorOutcome is what one probe established about one pair. A floor read and absent can
// be skipped when folding a set, but a FAILED probe means nothing is known, and skipping it could
// publish a floor later than the truth, the one error direction this signal must avoid.
type coverageFloorOutcome uint8

const (
	coverageFloorFound coverageFloorOutcome = iota
	coverageFloorAbsent
	coverageFloorFailed
)

// coverageFloorEntry is one cached probe outcome. Absent and failed probes are cached like hits so
// an uncovered pair costs one read per TTL, except a probe that never finished (see
// [Server.coverageFloor]).
type coverageFloorEntry struct {
	floor   time.Time
	outcome coverageFloorOutcome
	expires time.Time
}

// coverageFloorCache is the per-process memo in front of
// [CoverageFloorReader]. Bounded, TTL'd, and keyed by the pair's
// identity under the span the probe used — see [coverageFloorKey].
type coverageFloorCache struct {
	mu      sync.Mutex
	entries map[string]coverageFloorEntry
}

// coverageFloorKey folds a pair onto the one identity its floor is a property of under the given
// span. A leg read alias-complete folds onto its family; a leg read literally keeps its spelling,
// or two probes with different answers would share an entry. Each fold uses a single registry
// lookup per leg:
//   - Alias family. `native`, `crypto:XLM` and the XLM SAC are one asset, so every spelling must
//     resolve to the same entry or the same answer is re-read up to nine times. Under
//     [spanLiteralQuote] this applies to the base leg only.
//   - Direction, for the both-orientations, both-legs-aliased probe only: it reads both stored
//     directions over symmetric alias arrays, so A/B and B/A share a floor and are ordered into one
//     entry. The stored-orientation and literal-quote probes answer differently per orientation and
//     keep the order.
//   - Span. Each span has its own prefix so reads spanning different rows never share an entry.
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

// store admits an entry, first dropping expired keys and then, if still at the ceiling, one
// arbitrary key. Unspecified map iteration order is wanted here: a caller enumerating pairs cannot
// steer the victim.
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

// coverageSet is the population a surface's serving read for one pair draws on, and so what that
// surface's floor is a property of.
//
// A fiat quote makes this a SET: nothing on chain quotes in `fiat:USD`, so surfaces answer from
// USD-pegged constituents (the direct pair, stablecoin backers, declared classic pegs, and on
// /v1/chart a derivation through XLM). A floor on the literal pair alone describes the wrong
// population twice over: earlier constituent buckets make a served-and-empty window look
// uncovered, and direct buckets no constituent read would find make an uncovered window look
// quiet. So the floor is measured over exactly the pairs the serving read enumerates, via the same
// helpers ([Server.usdPeggedConstituents], [Server.chartFiatProxyPairs], [fiatCrossLegsThroughXLM],
// [Server.priceAtUSDPegPairs]) so the two cannot drift apart.
//
// A probe may also span more or fewer spellings of each pair than the read; `span` pins that. See
// [coverageProbeSpan].
type coverageSet struct {
	// direct pairs. The serving read answers from any one that holds buckets, so the set's floor is
	// the EARLIEST of their floors.
	direct []canonical.Pair
	// derived series. Each entry is the legs of one series composed bucket by bucket, which exists
	// only where EVERY leg does, so its floor is the LATEST of its legs' floors and that value then
	// competes with the direct floors. Folding a leg into direct would hand every non-XLM asset with
	// any XLM market the floor of XLM's own fiat series.
	derived [][]canonical.Pair
	// span is which stored rows each probe reads, matched to the surface's serving read.
	span coverageProbeSpan
}

// ohlcCoverageSet mirrors [Server.ohlcSeriesWithAliases].
//
// A non-fiat quote is served by the alias walk over a store read that folds both stored
// directions, so the pair under the default span is exactly its population.
//
// A fiat quote is served by [Server.ohlcSeriesFiatCombined], which reads the literal pairs
// [Server.usdPeggedConstituents] enumerates: every base spelling crossed with the peg expansion,
// each quote in every canonical form (established, then held-back SAC wrappers). Those are the set
// and [spanLiteralQuote] the span, because [Store.OHLCSeries] takes the quote spelling it is given.
//
// Held-back spellings belong in the set even though the combine admits their bars only into
// buckets nothing established answered: the floor is consulted only on an empty answer, meaning
// every constituent was read, and omitting them would put the probe behind the read. A test pins
// the probed and requested populations equal.
func (s *Server) ohlcCoverageSet(pair canonical.Pair) coverageSet {
	if pair.Quote.Type != canonical.AssetFiat {
		return coverageSet{direct: []canonical.Pair{pair}}
	}
	return coverageSet{direct: s.usdPeggedConstituents(pair), span: spanLiteralQuote}
}

// chartCoverageSet mirrors [Server.chartSeriesPoints], the chain behind every CAGG-served chart:
// the requested pair's alias spellings ([Server.chartAliasPairs]), then for a fiat quote the proxy
// list [Server.chartStablecoinFallback] fills from ([Server.chartFiatProxyPairs]), then the XLM
// cross [Server.fiatSeriesThroughXLM] derives — its two legs as one derived entry, since the cross
// exists only where both legs do.
//
// The set is built from the read's own enumeration helpers so the two cannot drift. A floor is
// consulted only on an EMPTY answer, which requires that every pair in the list came back empty,
// so the read must actually read every listed pair (the merge is per bucket for that reason).
//
// The default span (both legs' aliases, both directions) is what this chain spans: every entry is
// read through [Server.chartSeriesPoints], which crosses both base aliases with every canonical
// form of each declared peg ([Server.chartFiatProxyQuotes]'s established then held-back pass).
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

// priceAtCoverageSet mirrors [Server.handlePriceAt]: the pair itself, then for a `fiat:USD` quote
// the declared USD pegs [Server.lookupPriceAtStablecoinFallback] retries. Every entry is read
// through [Server.lookupPriceAt], which walks both legs' alias families over a store read
// ([Store.ClosedVWAPAtOrBefore]) folding both stored directions: the default span exactly.
func (s *Server) priceAtCoverageSet(pair canonical.Pair) coverageSet {
	return coverageSet{direct: append([]canonical.Pair{pair}, s.priceAtUSDPegPairs(pair.Base, pair.Quote)...)}
}

// historyCoverageSet mirrors [Server.tradesInRangeAfterWithAliases]: the pair itself, since the
// raw-trade page has no fiat fallback.
//
// The default span, because that walk crosses both legs' alias families and reads each form in both
// stored directions ([Server.tradesInRangeAfterBothDirections]), re-expressing flipped rows into
// the requested orientation. A market recorded only as USDC/AQUA is thus served under
// `base=AQUA&quote=USDC`, so its floor must cover those rows: a floor over rows a surface cannot
// serve over-claims, and one over fewer rows than it serves under-reports.
func historyCoverageSet(pair canonical.Pair) coverageSet {
	return coverageSet{direct: []canonical.Pair{pair}}
}

// coverageFloor is one memoised probe: when the daily rung's history for `pair` begins under the
// span, and whether that is known at all.
//
// `ctx` carries the response-wide probe deadline set by [Server.coverageFloorOf]. A probe the
// database answered with an error is cached as failed for the short TTL. A probe that did NOT
// finish (caller went away, or the deadline ran out) is not cached: it established nothing, and
// one client's disconnect must not make every request for that pair unknowable for the full TTL.
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
		// Warn, not Error: the request itself succeeded and the caller
		// loses only an advisory annotation.
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

// coverageFloorOf returns when the served history for `set` begins, and whether that is known.
//
// Called only from an already-empty answer, so the probes add one bounded read per constituent to
// a request that was empty anyway, and the TTL cache collapses repeats.
//
// The fold is strict about what it does not know. A direct constituent read and found empty is
// skipped, as is a derived series with any leg absent. But one FAILED probe makes the whole floor
// unknown: that constituent might hold the earliest bucket, and a floor that is too LATE turns a
// served-and-quiet window into a false coverage claim. Silence only costs an unannotated window.
func (s *Server) coverageFloorOf(ctx context.Context, set coverageSet) (time.Time, bool) {
	// The memo is required: without it every probe is a read on a path an anonymous caller drives.
	// A Server assembled without [New] has no cache, and the honest answer there is no signal.
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

// coverageAnnotation is what a handler stamps on an empty answer: the floor to echo as
// `coverage_from`, and whether the requested range provably sits below it.
//
// `end` is the range's EXCLUSIVE upper bound: a series window's `to`, or for a point-in-time lookup
// the instant itself (the daily bucket starting at the floor has not closed then, so an instant
// exactly at the floor is still uncovered).
//
// outside is true only when a floor is known AND the whole range ends at or before it. A range
// straddling the floor contains covered time, so its emptiness is a genuine quiet-market answer.
// An unknown floor is not flagged either: a missing daily bucket is not proof of no history (a
// pair whose first trades landed an hour ago has none yet).
func (s *Server) coverageAnnotation(ctx context.Context, set coverageSet, end time.Time) (from *time.Time, outside bool) {
	floor, known := s.coverageFloorOf(ctx, set)
	if !known {
		return nil, false
	}
	f := floor
	return &f, !end.After(floor)
}

// coverageAnnotationIfEmpty is [Server.coverageAnnotation] gated on `empty`, the handler's
// judgement that the response is the ambiguous one. That gate is the cost bound of the whole
// signal: a populated answer explains itself and the probes never run behind one.
func (s *Server) coverageAnnotationIfEmpty(
	ctx context.Context, set coverageSet, end time.Time, empty bool,
) (from *time.Time, outside bool) {
	if !empty {
		return nil, false
	}
	return s.coverageAnnotation(ctx, set, end)
}

// historyPageIsAmbiguous reports whether an empty /v1/history page is the ambiguous kind. An empty
// first page reads the same whether the market was dead or the window predates the history. An
// empty page reached by cursor means the caller paginated past the last row, and the cursor shadows
// `from`, so the window the signal would describe is not the window that was read.
func historyPageIsAmbiguous(rows int, afterTS time.Time) bool {
	return rows == 0 && afterTS.IsZero()
}

// writeChartSeries emits a /v1/chart response with its coverage annotation and interior-gap signal.
// The truncation signal handleChart carries comes from points[0] and says nothing about an EMPTY
// series, so coverage is the only account of that case. [ChartSeries.markDiscontinuity] is stamped
// here because points[0] likewise says nothing about a hole in the MIDDLE; coverage speaks for the
// empty answer and the gap signal for the holed one, and both ride the write so neither is
// forgotten.
//
// The window always ends at NOW, so `outside_coverage` never fires here; the floor is the whole
// answer. An empty series with `coverage_from` says the pair has been quiet; with the field absent,
// this deployment holds nothing for it. Without the field both render as `points: []`.
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
