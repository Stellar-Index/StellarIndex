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

// CoverageFloorReader answers one question for a pair: when does this
// deployment's served history for it BEGIN? It exists because an empty
// series is two completely different answers wearing the same wire
// shape — "the market was quiet across the window" and "the window is
// before anything held for this pair" — and no serving read can tell
// them apart, because both return zero rows.
//
// Production wiring is timescale.Store: one bounded, index-backed
// `min(bucket)` over prices_<granularity>. The three methods differ
// only in WHICH stored rows they span, and a surface picks the one its
// own serving read spans — never a wider one, because a floor measured
// over rows a surface cannot serve is a coverage claim about bars that
// will never arrive:
//
//   - EarliestBucket folds both legs' alias families and both stored
//     directions, matching a read that walks the spellings of both
//     legs (chartPointsWithAliases, lookupPriceAt, the non-fiat
//     ohlcSeriesWithAliases) over a store read that combines
//     base/quote and quote/base rows into the requested orientation.
//   - EarliestBucketAsStored reads the requested orientation only,
//     matching the raw-trade page read (TradesInRangeAfter), which
//     never flips.
//   - EarliestBucketLiteralQuote drops the alias fold on the quote leg,
//     matching the fiat combine (ohlcSeriesFiatCombined), which reads
//     each USD-pegged constituent under the one quote spelling the peg
//     expansion named it in and never walks that quote's family.
//
// [from, to) is half-open and `to` MUST be after `from`; the store
// rejects a degenerate window rather than reporting it empty, so a
// caller that computes a bad probe range fails loudly instead of
// silently claiming a pair has no coverage.
type CoverageFloorReader interface {
	EarliestBucket(ctx context.Context, pair canonical.Pair, granularity string, from, to time.Time) (time.Time, bool, error)
	EarliestBucketAsStored(ctx context.Context, pair canonical.Pair, granularity string, from, to time.Time) (time.Time, bool, error)
	EarliestBucketLiteralQuote(ctx context.Context, pair canonical.Pair, granularity string, from, to time.Time) (time.Time, bool, error)
}

// coverageProbeSpan names which stored rows a set's probes read. It is
// the one knob that keeps a floor a property of the read it explains:
// each surface's coverageSet declares the span its own serving read
// covers, and [Server.coverageFloor] picks the matching
// [CoverageFloorReader] method.
type coverageProbeSpan uint8

const (
	// spanAliasedBothDirections: both legs' alias families, both stored
	// directions. The default, and what a read that walks the spellings
	// of both legs over a direction-folding store read spans.
	spanAliasedBothDirections coverageProbeSpan = iota
	// spanAliasedAsStored: both legs' alias families, the requested
	// orientation only — /v1/history's page read.
	spanAliasedAsStored
	// spanLiteralQuote: the base leg's alias family and the quote leg's
	// literal spelling, both stored directions — the fiat /v1/ohlc
	// combine, which enumerates every base spelling itself and names
	// each constituent's quote in exactly one form.
	spanLiteralQuote
)

// String names the span in a probe warning, so an operator reading the
// log knows which of the three reads failed without inferring it from
// the surface.
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
	// coverageFloorGranularity is the CAGG rung the floor is measured
	// on. The daily rung, always — not the grain the request happened
	// to ask for. No price aggregate carries a retention policy
	// (migration 0031 removed the 30-day policies migration 0002 had
	// placed on prices_1m / prices_15m, and migration 0116 records the
	// tree as holding none on any reconcile target), so nothing in the
	// daily rung has been dropped by age; and prices_1d is the coarsest
	// rung, holds the fewest rows per pair, and is the cheapest to
	// prove empty on a path an anonymous caller can drive. The floor is
	// a statement about the rung the probe reads, not about the tree:
	// every prices_* view is materialized_only and the minute rungs are
	// refreshed on their own schedule (the backfill refresh set leaves
	// them out — timescale.CAGGsLiveForever), so a finer rung holds what
	// its own refreshes have materialised, which can be less of the
	// history than the daily rung holds.
	coverageFloorGranularity = "1d"

	// coverageFloorTTL is how long one answer is reused. The floor is
	// close to immutable — it moves once, when a pair's first bucket
	// materialises, and again only if a historical backfill lands
	// earlier rows — so the TTL is set by how quickly a NEW pair should
	// stop being described as uncovered, not by drift.
	coverageFloorTTL = 30 * time.Minute

	// coverageFloorProbeTimeout caps EVERY probe behind one response,
	// together. A fiat-quoted pair's floor is measured over its whole
	// constituent set (see [coverageSet]), and each constituent that
	// misses the memo is one read — measured at ~5 ms execution on r1's
	// prices_1d — so the ceiling is per response rather than per read.
	// A degraded database therefore costs the response nothing but the
	// signal, however many constituents the set holds.
	coverageFloorProbeTimeout = 2 * time.Second

	// coverageFloorCacheMax bounds the entry count. A caller can mint
	// unlimited DISTINCT pairs, so the map must not grow with them;
	// past this size admitting a key evicts one, which the `evicted`
	// metric result makes visible as the key-enumeration signature.
	coverageFloorCacheMax = 4096
)

// coverageFloorEpoch is the lower bound of every probe: the pubnet
// genesis month (the network's ledger 1 closed 2015-09-30). Nothing can
// be bucketed before it, so binding it as a literal costs no rows and
// buys plan-time chunk exclusion at the bottom end.
var coverageFloorEpoch = time.Date(2015, 9, 30, 0, 0, 0, 0, time.UTC)

// coverageFloorOutcome is what one probe established about one pair.
// The three states are distinct on purpose: a floor that was READ AND
// ABSENT can be skipped when a set of constituents is folded — the
// constituent holds no daily bucket — while a probe that FAILED means
// nothing is known about that constituent, and a fold that skipped it
// could publish a floor later than the truth, which is the one error
// direction the whole signal is built to avoid.
type coverageFloorOutcome uint8

const (
	coverageFloorFound coverageFloorOutcome = iota
	coverageFloorAbsent
	coverageFloorFailed
)

// coverageFloorEntry is one cached probe outcome. An absent or failed
// probe is cached exactly like a hit so a repeated request for an
// uncovered pair costs one read per TTL, not one per request — with one
// exception, a probe that never finished (see [Server.coverageFloor]).
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

// coverageFloorKey folds a pair onto the one identity its floor is a
// property of UNDER THE GIVEN SPAN. A leg the span reads alias-complete
// folds onto its family; a leg the span reads literally keeps its own
// spelling, or two probes with different answers would share an entry.
//
// Three folds, each load-bearing, and each done with a SINGLE registry
// lookup per leg rather than a walk over alias combinations:
//
//   - Alias family. `native`, `crypto:XLM` and the XLM SAC are the same
//     asset; a probe that reads all of their forms in one query must
//     resolve every spelling of that leg to the same cache entry or the
//     same answer would be re-read up to nine times per pair. Under
//     [spanLiteralQuote] this applies to the base leg only.
//   - Direction — for the both-orientations, both-legs-aliased probe
//     only. That probe reads both stored market directions over two
//     symmetric alias arrays, so A/B and B/A have the same floor by
//     construction; ordering the two ids makes them one entry. The
//     stored-orientation probe is a different question with a different
//     answer for A/B and B/A. So is the literal-quote probe: its two
//     legs are read with different completeness, so swapping them
//     changes the query. Both keep the order.
//   - Span. Each span gets its own prefix, so two reads that span
//     different rows can never share an entry.
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

// lookup returns a live cached entry, or ok=false when the key is
// absent or expired. Expired entries are left in place for `store` to
// sweep — a read path that deletes would need the write lock on every
// miss.
func (c *coverageFloorCache) lookup(key string, now time.Time) (coverageFloorEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || !now.Before(e.expires) {
		return coverageFloorEntry{}, false
	}
	return e, true
}

// store admits an entry, first dropping expired keys and then, if the
// map is still at its ceiling, one arbitrary key. Go's map iteration
// order is unspecified, which is the property wanted here: a caller
// enumerating pairs cannot steer which victim it evicts.
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

// coverageSet is the population a surface's serving read for one pair
// draws on — and therefore what that surface's floor is a property of.
//
// A fiat quote is the case that makes this a SET rather than a pair.
// Nothing on chain quotes in `fiat:USD`; the surfaces answer a
// fiat-quoted request from the USD-pegged constituents (the direct
// pair, the abstract stablecoin backers, the operator-declared classic
// pegs, and on /v1/chart a derivation through XLM), each surface with
// its own enumeration. A floor read on the LITERAL pair alone describes
// the wrong population twice over: a constituent whose buckets predate
// the direct pair's makes a served-and-empty window look uncovered,
// and a direct pair with buckets no constituent read would find makes
// an uncovered window look quiet. So the floor is measured over exactly
// the pairs the serving read enumerates, obtained from the same helpers
// that read uses — [Server.usdPeggedConstituents],
// [Server.chartFiatProxyPairs], [fiatCrossLegsThroughXLM],
// [Server.priceAtUSDPegPairs] — so the two cannot drift apart by
// editing one of them.
//
// Enumerating the same pairs is only half of it: a probe also spans
// more or fewer SPELLINGS of each pair than the read that requested it,
// which is what `span` pins. See [coverageProbeSpan].
type coverageSet struct {
	// direct pairs. The serving read answers from any one of them that
	// holds buckets, so the set's floor is the EARLIEST of their floors.
	direct []canonical.Pair
	// derived series. Each entry is the legs of one series composed
	// bucket by bucket, which exists only where EVERY leg does — so a
	// derived series' floor is the LATEST of its legs' floors, and that
	// single value then competes with the direct floors. Folding a leg
	// into the direct set instead would hand every non-XLM asset with
	// any XLM market the floor of XLM's own fiat series.
	derived [][]canonical.Pair
	// span is which stored rows each probe reads — matched to what the
	// surface's own serving read spans. See [coverageProbeSpan].
	span coverageProbeSpan
}

// ohlcCoverageSet mirrors [Server.ohlcSeriesWithAliases].
//
// A non-fiat quote is served by the alias walk in that function, which
// tries every spelling of both legs over a store read that folds the
// two stored directions — so the pair itself under the default span is
// exactly its population.
//
// A fiat quote is served by [Server.ohlcSeriesFiatCombined], which
// reads the LITERAL pairs [Server.usdPeggedConstituents] enumerates:
// every base spelling crossed with the peg expansion, each quote in the
// one form the expansion named it in. Those literal pairs are the set,
// and [spanLiteralQuote] is the span, because [Store.OHLCSeries] takes
// the quote spelling it is given. A quote-alias fold here would find a
// SAC-quoted Soroban pool — where a declared peg's Soroban depth lives
// — and publish its first bucket as this surface's floor, so an asset
// whose only USD venue is such a pool would serve `intervals: []` and
// call it quiet. It serves an empty series with NO floor instead, which
// is the truth about this surface; closing the gap is launch-plan row
// 1.15. The base leg keeps its fold because the combine enumerates
// every base spelling itself, and the memo collapses the repeats to one
// read per quote spelling. A test pins the probed and requested
// populations equal.
func (s *Server) ohlcCoverageSet(pair canonical.Pair) coverageSet {
	if pair.Quote.Type != canonical.AssetFiat {
		return coverageSet{direct: []canonical.Pair{pair}}
	}
	return coverageSet{direct: s.usdPeggedConstituents(pair), span: spanLiteralQuote}
}

// chartCoverageSet mirrors [Server.handleChart]'s default vwap path:
// the pair itself ([Server.chartPointsWithAliases]), then for a fiat
// quote the proxy list [Server.chartStablecoinFallback] walks, then the
// XLM cross [Server.fiatSeriesThroughXLM] derives — its two legs as one
// derived entry, since the cross exists only where both legs do.
//
// The default span (both legs' aliases, both directions) is what this
// chain spans: every entry is read through
// [Server.chartPointsWithAliases] or, for a proxy entry, is one of the
// literal pairs [Server.chartFiatProxyPairs] already enumerated across
// both base aliases AND every canonical form of each declared peg
// ([Server.usdPegProxyQuotes]'s classic pass then SAC pass) — so this
// surface's read does reach a SAC-quoted pool, and a floor that spans
// one is a floor over bars it can serve.
func (s *Server) chartCoverageSet(pair canonical.Pair) coverageSet {
	set := coverageSet{direct: []canonical.Pair{pair}}
	if pair.Quote.Type != canonical.AssetFiat {
		return set
	}
	set.direct = append(set.direct, s.chartFiatProxyPairs(pair)...)
	if legs, ok := fiatCrossLegsThroughXLM(pair); ok {
		set.derived = append(set.derived, legs[:])
	}
	return set
}

// priceAtCoverageSet mirrors [Server.handlePriceAt]: the pair itself
// ([Server.lookupPriceAt]), then for a `fiat:USD` quote the declared
// USD pegs [Server.lookupPriceAtStablecoinFallback] retries.
//
// Every entry is read through [Server.lookupPriceAt], which walks both
// legs' alias families over a store read
// ([Store.ClosedVWAPAtOrBefore]) that folds the two stored directions
// — the default span exactly.
func (s *Server) priceAtCoverageSet(pair canonical.Pair) coverageSet {
	return coverageSet{direct: append([]canonical.Pair{pair}, s.priceAtUSDPegPairs(pair.Base, pair.Quote)...)}
}

// historyCoverageSet mirrors [Server.tradesInRangeAfterWithAliases]:
// the pair itself, in the orientation it was asked for. The raw-trade
// page has no fiat fallback, and its read
// ([HistoryReader.TradesInRangeAfter]) spans ONE stored orientation and
// is never flipped by its caller, so a market the decoder recorded only
// as USDC/AQUA answers `base=AQUA&quote=USDC` with an empty page every
// time. A both-orientations floor would find the USDC/AQUA bucket and
// describe that page as quiet since then — a coverage claim about rows
// the page can never return — so the probe spans what the read spans.
// The read's single-orientation semantics are a separate matter, left
// as they are here.
func historyCoverageSet(pair canonical.Pair) coverageSet {
	return coverageSet{direct: []canonical.Pair{pair}, span: spanAliasedAsStored}
}

// coverageFloor is one memoised probe: the instant the daily rung's
// history for `pair` begins under the requested orientation span, and
// whether that is known at all.
//
// `ctx` carries the response-wide probe deadline set by
// [Server.coverageFloorOf]. A probe the database answered with an error
// is cached as failed for the TTL, so a broken read is not re-attempted
// on every subsequent empty window for the pair. A probe that did NOT
// finish — the caller went away, or the response-wide deadline ran out
// — is not cached: it established nothing about the pair, and a memo
// entry written from one client's disconnect would describe every
// request for that pair as unknowable for the next thirty minutes. Not
// caching it costs at most one more bounded attempt per empty answer,
// under the same deadline.
func (s *Server) coverageFloor(ctx context.Context, pair canonical.Pair, span coverageProbeSpan) (time.Time, coverageFloorOutcome) {
	key := coverageFloorKey(pair, span)
	now := time.Now().UTC()
	if e, ok := s.coverageFloorCache.lookup(key, now); ok {
		obs.APICoverageFloorProbesTotal.WithLabelValues("hit").Inc()
		return e.floor, e.outcome
	}

	read := s.coverageFloorReader.EarliestBucket
	switch span {
	case spanAliasedAsStored:
		read = s.coverageFloorReader.EarliestBucketAsStored
	case spanLiteralQuote:
		read = s.coverageFloorReader.EarliestBucketLiteralQuote
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
			expires: now.Add(coverageFloorTTL),
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

// coverageFloorOf returns the instant this deployment's served history
// for `set` begins, and whether it is known at all.
//
// Called ONLY from an already-empty answer, so the probes ride on a
// read that has just proved it has nothing to return — they add a
// bounded second read per constituent to a request that was going to
// be cheap-and-empty or expensive-and-empty either way, and the TTL
// cache collapses a repeat of the same question to nothing.
//
// The fold is strict about what it does not know. Every direct
// constituent that was read and holds no bucket is skipped; a derived
// series with any leg absent cannot be served and is skipped too. But
// a single FAILED probe anywhere in the set makes the whole floor
// unknown: the constituent that failed might have held the earliest
// bucket, and a floor computed without it could be too LATE — the one
// direction that turns a served-and-quiet window into a false coverage
// claim. Staying silent costs an unannotated empty window, exactly what
// callers had before the signal existed.
func (s *Server) coverageFloorOf(ctx context.Context, set coverageSet) (time.Time, bool) {
	// The memo is required, not optional: without it the probes are
	// reads per request on a path an anonymous caller controls. A
	// Server assembled field-by-field rather than through [New] has no
	// cache, and the honest answer there is no signal.
	if s.coverageFloorReader == nil || s.coverageFloorCache == nil {
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

// coverageAnnotation is what a handler stamps on an empty answer: the
// floor to echo as `coverage_from`, and whether the requested range
// provably sits below it.
//
// `end` is the requested range's EXCLUSIVE upper bound — a series
// window's `to`, or, for a point-in-time lookup, the instant itself
// (the daily bucket that STARTS at the floor has not closed at that
// instant, so an instant exactly at the floor is still uncovered).
//
// outside is true ONLY when a floor is known AND the whole range ends
// at or before it. A range that STRADDLES the floor is not flagged: it
// contains covered time, so its emptiness is a genuine quiet-market
// answer for that part and mislabelling it would be the same lie in the
// other direction. A set with NO known floor is not flagged either —
// absence of a daily bucket is not proof of absence of history (a pair
// whose first trades landed an hour ago has none yet).
func (s *Server) coverageAnnotation(ctx context.Context, set coverageSet, end time.Time) (from *time.Time, outside bool) {
	floor, known := s.coverageFloorOf(ctx, set)
	if !known {
		return nil, false
	}
	f := floor
	return &f, !end.After(floor)
}

// coverageAnnotationIfEmpty is [Server.coverageAnnotation] with the
// gate that decides whether to probe at all folded in, so every call
// site is one statement.
//
// `empty` is the handler's own judgement that this response is the
// ambiguous one — no bars, no points, no rows, no price — and it is the
// cost bound of the whole signal: a populated answer explains itself,
// and the probes never run behind one.
func (s *Server) coverageAnnotationIfEmpty(
	ctx context.Context, set coverageSet, end time.Time, empty bool,
) (from *time.Time, outside bool) {
	if !empty {
		return nil, false
	}
	return s.coverageAnnotation(ctx, set, end)
}

// historyPageIsAmbiguous reports whether an empty /v1/history page is
// the ambiguous kind.
//
// An empty FIRST page means the caller's window held no trades — the
// answer that reads identically whether the market was dead or the
// window predates the held history. An empty page reached by CURSOR
// means something else entirely: the caller paginated past the last
// row. The cursor also shadows `from`, so the window the signal would
// describe is not the window that was read.
func historyPageIsAmbiguous(rows int, afterTS time.Time) bool {
	return rows == 0 && afterTS.IsZero()
}

// writeChartSeries emits a /v1/chart response with its coverage
// annotation attached. It is one call rather than an annotate-then-write
// pair because the retention-truncation signal handleChart already
// carries is computed from points[0] and so says nothing at all about an
// EMPTY series — this is the surface's only account of that case, and it
// belongs with the write.
//
// The chart's window always ends at NOW, so it structurally cannot sit
// below the floor and `outside_coverage` never fires here. The floor
// itself is the whole answer on this surface: an empty 24h series with
// `coverage_from: 2018-07-01` says the pair has been quiet; the same
// series with the field absent says this deployment holds nothing for
// the pair at all. Pre-signal both rendered as `points: []`.
func (s *Server) writeChartSeries(
	w http.ResponseWriter, r *http.Request,
	pair canonical.Pair, series ChartSeries, triangulated bool,
) {
	coverageFrom, outside := s.coverageAnnotationIfEmpty(
		r.Context(), s.chartCoverageSet(pair), time.Now().UTC(), len(series.Points) == 0)
	writeJSONCoverage(w, series,
		Flags{Triangulated: triangulated, OutsideCoverage: outside}, coverageFrom)
}
