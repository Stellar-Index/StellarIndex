// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"math/big"
	"net/http"
	"sort"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/rwa"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// GET /v1/rwa/premium: the token's price against the instrument's net asset value, as a daily
// series of /v1/rwa/assets' point-in-time `premium.pct`. It needs an observed market price and an
// oracle NAV on the same clock; a chain query has neither.
//
// Neither leg is carried across a silent day, unlike /v1/rwa/history's supply: carrying the market
// leg draws a discount nobody traded, and carrying the NAV leg a premium against a value nobody
// published. A point exists only on a day both legs were observed.
//
// The market leg is held to a floor of the live thin-market gate's shape
// ([Server.rwaPremiumDayFloorFor]); a raw daily VWAP would publish, for past days, the
// attacker-seeded dust rate the live surface refuses. The floor uses hourly data because minute
// history depends on retention ([timescale.Store.DailyMarketDays]).
//
// Only `rwa:` feeds in the curated binding (internal/rwa/oracle_reference.go) that trade against a
// dollar can carry a premium; `excluded[]` names every member left out.

// rwaPremiumHistoryTTL bounds reuse of one assembled series; it matches [rwaHistoryTTL] since a
// daily grain cannot move faster than the day and both legs are scans.
const rwaPremiumHistoryTTL = 10 * time.Minute

// rwaPremiumHistoryBudget bounds the two scans together, tied to [maxHandlerBudget] as [rwaHistoryBudget] is: the handler builds
// synchronously on a miss, so a larger budget can never fire.
const rwaPremiumHistoryBudget = maxHandlerBudget

// rwaPremiumHistoryMaxPoints caps the served point count per series.
const rwaPremiumHistoryMaxPoints = 4096

// rwaPremiumHistoryQuote is the denominator of both legs. Fixed, not a parameter: the admitted
// reference feeds are USD-denominated and the market leg is read against dollar spellings only.
var rwaPremiumHistoryQuote = canonical.Asset{Type: canonical.AssetFiat, Code: "USD"}

// rwaPremiumDayFloorFor derives the per-day market-substance floor from the SAME policy /v1/price serves against (s.RWAPremiumSubstance),
// evaluated by [pricingguard.SubstanceOK]. MinVolumeUSD and MinSpan are
// the serving values UNCHANGED but MinBuckets is NOT: serving counts MINUTE buckets (1440/day), this counts HOUR buckets (24/day), the
// coarsest grain that still says WHEN ([timescale.Store.DailyMarketDays]); inheriting it would blank every series.
func (s *Server) rwaPremiumDayFloorFor() pricingguard.SubstancePolicy {
	minVolumeUSD := s.RWAPremiumSubstance.MinVolumeUSD
	if minVolumeUSD == nil {
		minVolumeUSD = new(big.Rat).SetInt64(pricingguard.DefaultSubstanceMinVolumeUSD)
	}
	minSpan := s.RWAPremiumSubstance.MinSpan
	if minSpan == 0 {
		minSpan = pricingguard.DefaultSubstanceMinSpan
	}
	return pricingguard.SubstancePolicy{
		MinVolumeUSD: minVolumeUSD,
		MinBuckets:   2,
		MinSpan:      minSpan,
		Window:       24 * time.Hour,
	}
}

// ─── wire ───────────────────────────────────────────────────────────

// RWAPremiumHistoryView is the payload for GET /v1/rwa/premium.
type RWAPremiumHistoryView struct {
	// Basis states what was measured, on which basis, and what the
	// figures are not.
	Basis string `json:"basis"`
	// Granularity is the bucket width. Always "1d".
	Granularity string `json:"granularity"`
	// Timeframe echoes the requested window token.
	Timeframe string `json:"timeframe"`
	// Quote is the denominator both legs are in.
	Quote string `json:"quote"`
	// Assets and Issuers size the WHOLE RWA set, including members this surface cannot compare;
	// `coverage[].assets_unmeasured` is counted against Assets.
	Assets  int `json:"assets"`
	Issuers int `json:"issuers"`
	// Bound is how many set members carry a curated (code, issuer) → instrument-feed binding at
	// all: the ceiling on coverage.
	Bound int `json:"bound"`
	// Members is how many set members COULD be compared at all: `members` plus every
	// `excluded[].assets` is exactly `assets`. It is not windowed, so a narrower `timeframe` can
	// leave `series` shorter than this.
	Members int `json:"members"`
	// MembershipAsOf is when the set was decided. Every point is measured against THIS membership,
	// including points that predate an asset's admission.
	MembershipAsOf WireTime `json:"membership_as_of"`
	// Excluded names the set members with no series and why, tallied by
	// reason.
	Excluded []RWAHistoryExcluded `json:"excluded,omitempty"`
	// Sources names the distinct oracles behind the reference leg,
	// sorted.
	Sources []string `json:"sources,omitempty"`
	// Series is one premium history per comparable member.
	//
	// There is deliberately NO total series: a premium is a percentage of a different denominator
	// per member, and an average would need an unpublished weighting and hide the dispersion.
	Series []RWAPremiumSeries `json:"series"`
	// Coverage is the daily account of how much of the set could be compared, so a reader can tell
	// "the sector moved" from "we could see less of it".
	Coverage []RWAPremiumCoveragePoint `json:"coverage,omitempty"`
	// Truncated reports that a cap bound a series.
	Truncated bool `json:"truncated,omitempty"`
}

// RWAPremiumSeries is one member's premium history.
type RWAPremiumSeries struct {
	// AssetID is the canonical `CODE-ISSUER` identity.
	AssetID string `json:"asset_id"`
	Code    string `json:"code,omitempty"`
	Issuer  string `json:"issuer,omitempty"`
	// Label is the SEP-1 currency name where one is known. Display
	// text, never identity.
	Label string `json:"label,omitempty"`
	// Feed is the instrument feed (canonical `rwa:` form) the market price was measured against;
	// Source is the oracle whose series was read.
	Feed   string `json:"feed"`
	Source string `json:"source"`
	// Points is the series, ascending by day. A day either leg was not
	// observed on is ABSENT.
	Points []RWAPremiumHistoryPoint `json:"points"`
	// MarketWithheldDays is how many days carried observed trades that did NOT clear the day
	// floor; "saw trades, refused to price them" and "no trades" are opposite findings that an
	// absent point renders identically.
	MarketWithheldDays int `json:"market_withheld_days,omitempty"`
	// ReferenceOnlyDays is how many days carried a published NAV and no market at all, the ordinary
	// case for held instruments. Counted only from this member's FIRST observed market day onward,
	// and never on a day already in MarketWithheldDays: earlier, "did not trade" and "aggregate does
	// not reach back" are indistinguishable.
	ReferenceOnlyDays int `json:"reference_only_days,omitempty"`
}

// RWAPremiumHistoryPoint is one day of one member's premium, with both prices served beside the ratio so a reader can tell which side moved.
type RWAPremiumHistoryPoint struct {
	// T is the UTC day the bucket opens.
	T WireTime `json:"t"`
	// PremiumPct is (market − reference) / reference × 100 as a signed 4-dp decimal STRING,
	// positive when the token traded ABOVE the independent valuation; same arithmetic and scale
	// as `premium.pct` on /v1/rwa/assets.
	PremiumPct string `json:"premium_pct"`
	// MarketUSD is the day's volume-weighted average price, exact.
	MarketUSD string `json:"market_usd"`
	// ReferenceUSD is the day's closing oracle value at the feed's own
	// scale, exact.
	ReferenceUSD string `json:"reference_usd"`
	// VolumeUSD and Trades are the market leg's evidence, the figures the day floor was applied
	// to: a premium from a day that barely cleared the floor is a weaker claim than one from a
	// deep day, and nothing else on the wire says so.
	VolumeUSD string `json:"volume_usd"`
	Trades    int64  `json:"trades"`
}

// RWAPremiumCoveragePoint is one day's account of how much of the set could be compared.
type RWAPremiumCoveragePoint struct {
	T WireTime `json:"t"`
	// AssetsMeasured and AssetsUnmeasured split the WHOLE set by whether
	// it carried a premium on this day. Their sum is `assets`.
	AssetsMeasured   int `json:"assets_measured"`
	AssetsUnmeasured int `json:"assets_unmeasured"`
}

// Premium-series exclusion reasons the value series has no equivalent for. The shared ones
// (issuer_flagged, not_bound, contract_not_bound, no_reference_history) are reused verbatim from
// rwa_history.go: two spellings of one reason is how a funnel starts lying.
const (
	// RWAPremiumExcludedNoMarketHistory — bound to a feed the oracle has published for, but this
	// index has never observed a dollar-quoted trade in the token. The ordinary state of a
	// held-to-maturity instrument, not a defect.
	RWAPremiumExcludedNoMarketHistory = "no_market_history"
	// RWAPremiumExcludedMarketBelowFloor — dollar-quoted trades exist but no day cleared the
	// thin-market floor, so no day may carry an aggregated price claim.
	RWAPremiumExcludedMarketBelowFloor = "market_below_floor"
	// RWAPremiumExcludedNoOverlap — both legs exist and never on the same day. A ratio needs one
	// clock; interpolating either side onto the other's days is the fabrication this surface refuses.
	RWAPremiumExcludedNoOverlap = "no_same_day_observation"
)

var rwaPremiumExcludedDetail = map[string]string{
	RWAHistoryExcludedNotBound:           rwaHistoryExcludedDetail[RWAHistoryExcludedNotBound],
	RWAHistoryExcludedContract:           rwaHistoryExcludedDetail[RWAHistoryExcludedContract],
	RWAHistoryExcludedIssuerFlagged:      rwaHistoryExcludedDetail[RWAHistoryExcludedIssuerFlagged],
	RWAHistoryExcludedNoReferenceHistory: rwaHistoryExcludedDetail[RWAHistoryExcludedNoReferenceHistory],
	RWAPremiumExcludedNoMarketHistory:    "No dollar-quoted trade in this token has ever been observed, so there is no market price to compare the instrument's published value against. This is the ordinary condition of a tokenized bill that is bought and held rather than traded — it is not a gap in the index, and only a market can move it.",
	RWAPremiumExcludedMarketBelowFloor:   "Trades exist, but no single day cleared the thin-market floor a published price claim requires, so no day may carry one. Honest low volume is fully visible through the raw trade surfaces; what is withheld here is only the aggregated claim that the price of this token WAS some number on that day.",
	RWAPremiumExcludedNoOverlap:          "A market price and a published value both exist for this asset, but never on the same day, and neither may be carried across a day it was not observed on. A premium needs both legs struck on one clock.",
}

// rwaPremiumBasis travels with every response.
const rwaPremiumBasis = "The token's own observed dollar price on a day, measured against what an independent oracle published that same day for the real-world instrument the issuer declares it anchors to. Positive is a premium, negative a discount. Both legs are sampled observations, so NEITHER is carried across a day it was not observed on and a day missing either leg is absent from the series rather than flat. The market leg is a day's volume-weighted average that cleared a thin-market floor of the same shape the live price surface applies; days with trades below that floor are counted, not published. Coverage is bounded by the curated set of (code, issuer) pairs bound to an instrument feed, and further by which of those tokens trade at all — most of this set is bought and held."

const rwaPremiumBasisUnavailable = "No premium series is published: membership could not be established, or neither the market read nor the oracle read answered. An empty chart would be a claim about the sector that the reads did not earn."

// ─── readers ────────────────────────────────────────────────────────

// RWAMarketHistoryReader reads the observed daily dollar market, the market leg of GET /v1/rwa/premium; nil disables the endpoint because a premium missing its market leg is no series.
type RWAMarketHistoryReader interface {
	DailyMarketDays(ctx context.Context, assets, quotes []canonical.Asset, from, to time.Time) ([]timescale.MarketDay, error)
}

// ─── assembled series ───────────────────────────────────────────────

// rwaPremiumDay is one member's premium on one day.
type rwaPremiumDay struct {
	day       time.Time
	pct       string
	market    string
	reference string
	volumeUSD string
	trades    int64
}

// rwaPremiumMember is one constituent's assembled series.
type rwaPremiumMember struct {
	assetID   string
	code      string
	issuer    string
	label     string
	feed      string
	source    string
	days      []rwaPremiumDay
	withheld  []time.Time
	refOnly   []time.Time
	published map[time.Time]struct{}
}

// rwaPremiumHistory is one assembly.
type rwaPremiumHistory struct {
	available  bool
	setAssets  int
	setIssuers int
	bound      int
	members    []rwaPremiumMember
	excluded   map[string]int
	sources    []string
	builtAt    time.Time
	// stale marks a cached assembly served past its TTL because the
	// rebuild failed or has not answered; builtAt is then its real age.
	stale bool
}

// ─── handler ────────────────────────────────────────────────────────

// handleRWAPremiumHistory serves GET /v1/rwa/premium.
func (s *Server) handleRWAPremiumHistory(w http.ResponseWriter, r *http.Request) {
	tf, ok := parseRWAPremiumParams(w, r)
	if !ok {
		return
	}
	view := RWAPremiumHistoryView{
		Basis:       rwaPremiumBasis,
		Granularity: "1d",
		Timeframe:   tf,
		Quote:       rwaPremiumHistoryQuote.String(),
		Series:      []RWAPremiumSeries{},
	}
	if s.AssetsReader == nil || s.OracleHistory == nil || s.MarketHistory == nil {
		writeRWASeriesUnavailable(w, r, rwaPremiumBasisUnavailable)
		return
	}

	hist := s.cachedRWAPremiumHistory(r.Context())
	if !hist.available {
		w.Header().Set("Retry-After", rwaSeriesRetryAfter)
		writeRWASeriesUnavailable(w, r, rwaPremiumBasisUnavailable)
		return
	}

	from := rwaHistoryWindowStart(tf, hist.builtAt)
	view.Assets = hist.setAssets
	view.Issuers = hist.setIssuers
	view.Bound = hist.bound
	view.MembershipAsOf = WireTime(hist.builtAt)
	view.Excluded = rwaPremiumExcludedRows(hist.excluded)
	view.Sources = hist.sources
	view.Series, view.Truncated = rwaPremiumSeriesRows(hist.members, from)
	// Members counts the ASSEMBLY, not the window, so `members` + every `excluded[].assets` closes
	// on `assets`; a narrower window may leave `series` shorter without the account lying.
	view.Members = len(hist.members)
	view.Coverage = rwaPremiumCoverage(view.Series, hist.setAssets)
	writeEnvelope(w, Envelope{Data: view, AsOf: WireTime(hist.builtAt), Flags: Flags{Stale: hist.stale, Degraded: hist.stale}})
}

// parseRWAPremiumParams validates `timeframe` against [rwaHistoryTimeframes] verbatim: the same
// daily grain, so the same refusal of sub-daily tokens.
func parseRWAPremiumParams(w http.ResponseWriter, r *http.Request) (string, bool) {
	tf := r.URL.Query().Get("timeframe")
	if tf == "" {
		tf = "1y"
	}
	if _, known := rwaHistoryTimeframes[tf]; !known {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-timeframe",
			"Invalid timeframe", http.StatusBadRequest,
			"the premium series is daily; timeframe must be one of: 1w, 1mo, 1y, all (got "+tf+")")
		return "", false
	}
	return tf, true
}

// ─── series + coverage ──────────────────────────────────────────────

// rwaPremiumSeriesRows windows each member's series and drops those the window leaves empty, ordered by the member's LAST premium, widest
// ABSOLUTE dispersion first (a signed sort would bury a 4% discount), then asset id.
func rwaPremiumSeriesRows(members []rwaPremiumMember, from time.Time) ([]RWAPremiumSeries, bool) {
	truncated := false
	out := make([]RWAPremiumSeries, 0, len(members))
	for i := range members {
		m := &members[i]
		pts := make([]RWAPremiumHistoryPoint, 0, len(m.days))
		for _, d := range m.days {
			if !from.IsZero() && d.day.Before(from) {
				continue
			}
			pts = append(pts, RWAPremiumHistoryPoint{
				T:            WireTime(d.day),
				PremiumPct:   d.pct,
				MarketUSD:    d.market,
				ReferenceUSD: d.reference,
				VolumeUSD:    d.volumeUSD,
				Trades:       d.trades,
			})
		}
		if len(pts) == 0 {
			continue
		}
		if len(pts) > rwaPremiumHistoryMaxPoints {
			pts = pts[len(pts)-rwaPremiumHistoryMaxPoints:]
			truncated = true
		}
		out = append(out, RWAPremiumSeries{
			AssetID:            m.assetID,
			Code:               m.code,
			Issuer:             m.issuer,
			Label:              m.label,
			Feed:               "rwa:" + m.feed,
			Source:             m.source,
			Points:             pts,
			MarketWithheldDays: countFrom(m.withheld, from),
			ReferenceOnlyDays:  countFrom(m.refOnly, from),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		li, lj := lastAbsPremium(out[i]), lastAbsPremium(out[j])
		if c := li.Cmp(lj); c != 0 {
			return c > 0
		}
		return out[i].AssetID < out[j].AssetID
	})
	return out, truncated
}

// countFrom counts the days at or after `from`.
func countFrom(days []time.Time, from time.Time) int {
	n := 0
	for _, d := range days {
		if from.IsZero() || !d.Before(from) {
			n++
		}
	}
	return n
}

func lastAbsPremium(s RWAPremiumSeries) *big.Rat {
	if len(s.Points) == 0 {
		return new(big.Rat)
	}
	v, ok := new(big.Rat).SetString(s.Points[len(s.Points)-1].PremiumPct)
	if !ok {
		return new(big.Rat)
	}
	return v.Abs(v)
}

// rwaPremiumCoverage counts per day how much of the WHOLE set carried a premium, taken against `setAssets` rather than the comparable
// members: a point states coverage of the sector, not of what was readable.
func rwaPremiumCoverage(series []RWAPremiumSeries, setAssets int) []RWAPremiumCoveragePoint {
	byDay := map[time.Time]int{}
	for _, s := range series {
		for _, p := range s.Points {
			byDay[time.Time(p.T)]++
		}
	}
	if len(byDay) == 0 {
		return nil
	}
	days := make([]time.Time, 0, len(byDay))
	for d := range byDay {
		days = append(days, d)
	}
	sort.Slice(days, func(i, j int) bool { return days[i].Before(days[j]) })
	out := make([]RWAPremiumCoveragePoint, 0, len(days))
	for _, d := range days {
		measured := byDay[d]
		unmeasured := setAssets - measured
		if unmeasured < 0 {
			unmeasured = 0
		}
		out = append(out, RWAPremiumCoveragePoint{
			T:                WireTime(d),
			AssetsMeasured:   measured,
			AssetsUnmeasured: unmeasured,
		})
	}
	return out
}

func rwaPremiumExcludedRows(tally map[string]int) []RWAHistoryExcluded {
	if len(tally) == 0 {
		return nil
	}
	out := make([]RWAHistoryExcluded, 0, len(tally))
	for reason, n := range tally {
		if n == 0 {
			continue
		}
		out = append(out, RWAHistoryExcluded{
			Reason: reason,
			Assets: n,
			Detail: rwaPremiumExcludedDetail[reason],
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Assets != out[j].Assets {
			return out[i].Assets > out[j].Assets
		}
		return out[i].Reason < out[j].Reason
	})
	return out
}

// ─── assembly ───────────────────────────────────────────────────────

// cachedRWAPremiumHistory returns the assembled series, rebuilt at most once per TTL behind a single-flight gate (both legs are scans),
// as [Server.cachedRWAValueHistory]. On a failed assembly the last good series is served,
// else an UNAVAILABLE one: an empty series would claim no token ever traded away from its NAV, which a read that did not answer may not
// claim.
func (s *Server) cachedRWAPremiumHistory(ctx context.Context) rwaPremiumHistory {
	s.rwaPremMu.Lock()
	if s.rwaPremCache != nil && time.Since(s.rwaPremAt) < rwaPremiumHistoryTTL {
		h := *s.rwaPremCache
		s.rwaPremMu.Unlock()
		return h
	}
	ch := s.rwaPremFlight
	if ch == nil {
		ch = make(chan struct{})
		s.rwaPremFlight = ch
		//nolint:gosec,contextcheck // G118 / contextcheck: intentional
		// detached build — see runDetachedFill.
		go runDetachedFill(s.logger, "api-rwa-premium-build", rwaPremiumHistoryBudget, ch,
			func(ctx context.Context) (rwaPremiumHistory, error) { return s.buildRWAPremiumHistory(ctx), nil }, //nolint:unparam // runDetachedFill's signature; failure is .available
			s.settleRWAPremiumHistory)
	}
	s.rwaPremMu.Unlock()
	select {
	case <-ch:
	case <-ctx.Done():
	}
	return s.lastRWAPremiumHistory()
}

// settleRWAPremiumHistory caches a build that answered and releases the flight.
func (s *Server) settleRWAPremiumHistory(built rwaPremiumHistory, err error) {
	s.rwaPremMu.Lock()
	defer s.rwaPremMu.Unlock()
	if err == nil && built.available {
		s.rwaPremCache = &built
		s.rwaPremAt = time.Now()
	}
	s.rwaPremFlight = nil
}

// lastRWAPremiumHistory returns the cached assembly, stale once past its
// TTL, or an unavailable one when no build has ever succeeded.
func (s *Server) lastRWAPremiumHistory() rwaPremiumHistory {
	s.rwaPremMu.Lock()
	defer s.rwaPremMu.Unlock()
	if s.rwaPremCache == nil {
		return rwaPremiumHistory{}
	}
	h := *s.rwaPremCache
	h.stale = time.Since(s.rwaPremAt) >= rwaPremiumHistoryTTL
	return h
}

// buildRWAPremiumHistory assembles one series from today's membership, the oracle day buckets for
// the members' instruments, and the days their tokens traded against a dollar.
func (s *Server) buildRWAPremiumHistory(ctx context.Context) rwaPremiumHistory {
	ctx, cancel := context.WithTimeout(ctx, rwaPremiumHistoryBudget)
	defer cancel()

	m := s.cachedRWAMembership(ctx)
	// Unavailable means NEITHER arm answered, as on the other RWA surfaces. One arm reporting
	// while the other failed is a partial measurement, shown in the excluded tally.
	if !m.available && !m.contractCensus.available &&
		len(m.members) == 0 && len(m.contracts) == 0 {
		return rwaPremiumHistory{}
	}
	out := rwaPremiumHistory{
		setAssets:  len(m.members) + len(m.contracts),
		setIssuers: rwaHistoryIssuerCount(m),
		excluded:   map[string]int{},
		builtAt:    time.Now().UTC(),
	}
	out.excluded[RWAHistoryExcludedContract] = len(m.contracts)

	cands := rwaPremiumCandidates(m, out.excluded)
	out.bound = len(cands)
	if len(cands) == 0 {
		// Nothing to compare, but the membership read DID answer, so
		// the account of who was excluded is itself the finding.
		out.available = true
		return out
	}

	// Complete UTC days only, both legs: a partial day's VWAP is not the day's price, and holding it
	// to a whole-day floor would report every morning as a thin market.
	to := rwaClosedDayCeiling(time.Now())
	floor := s.rwaPremiumDayFloorFor()

	prices, ok := s.rwaPremiumReferenceDays(ctx, cands, to)
	if !ok {
		return rwaPremiumHistory{}
	}
	market, ok := s.rwaPremiumMarketDays(ctx, cands, to)
	if !ok {
		return rwaPremiumHistory{}
	}

	for _, c := range cands {
		series := prices[c.feed]
		if series == nil || len(series.days) == 0 {
			out.excluded[RWAHistoryExcludedNoReferenceHistory]++
			continue
		}
		days := market[c.assetID]
		if len(days) == 0 {
			out.excluded[RWAPremiumExcludedNoMarketHistory]++
			continue
		}
		member := rwaPremiumJoin(c, series, days, floor)
		if len(member.days) == 0 {
			// Trades existed: say whether every day failed the floor or the days that cleared it
			// never coincided with a published value.
			if len(member.withheld) == len(days) {
				out.excluded[RWAPremiumExcludedMarketBelowFloor]++
			} else {
				out.excluded[RWAPremiumExcludedNoOverlap]++
			}
			continue
		}
		out.members = append(out.members, member)
	}
	out.sources = rwaPremiumSources(out.members)
	out.available = true
	return out
}

// rwaPremiumCandidates applies the gates that need no read: scam-flag suppression FIRST, then the curated (code, issuer) to feed binding;
// every refusal is tallied. The order is deliberate, as in [rwaHistoryCandidates]:
// a later binding must never turn a suppression into a published claim. Unlike the value series it needs no SAC address (a premium reads
// no supply).
func rwaPremiumCandidates(m rwaMembership, excluded map[string]int) []rwaHistoryCandidate {
	out := make([]rwaHistoryCandidate, 0, len(m.members))
	for _, mem := range m.members {
		if pricingguard.IsDirectoryScamFlagged(mem.dirTags) {
			excluded[RWAHistoryExcludedIssuerFlagged]++
			continue
		}
		feed, bound := rwa.InstrumentFeed(mem.code, mem.issuer)
		if !bound {
			excluded[RWAHistoryExcludedNotBound]++
			continue
		}
		asset := canonical.Asset{Type: canonical.AssetClassic, Code: mem.code, Issuer: mem.issuer}
		out = append(out, rwaHistoryCandidate{
			assetID:  asset.String(),
			code:     mem.code,
			issuer:   mem.issuer,
			label:    rwaHistoryLabel(mem.name, mem.code),
			feed:     feed,
			decimals: 7,
		})
	}
	return out
}

// ─── reference leg ──────────────────────────────────────────────────

// rwaPremiumReferenceDays reads the oracle day buckets for the candidates' bound feeds and reduces each to ONE publisher's series via
// [rwaReduceFeedSeries] verbatim,
// so the value series and this one cannot disagree about what an instrument was worth on a day.
func (s *Server) rwaPremiumReferenceDays(
	ctx context.Context, cands []rwaHistoryCandidate, to time.Time,
) (map[string]*rwaFeedSeries, bool) {
	assets := make([]canonical.Asset, 0, len(cands))
	seen := map[string]struct{}{}
	for _, c := range cands {
		if _, dup := seen[c.feed]; dup {
			continue
		}
		seen[c.feed] = struct{}{}
		assets = append(assets, canonical.Asset{Type: canonical.AssetRWA, Code: c.feed})
	}
	// Full history on purpose: one assembly serves every timeframe including
	// `all`; the genesis floor excludes no row and keeps the range bounded.
	rows, err := s.OracleHistory.DailyOraclePrices(ctx, assets, rwaPremiumHistoryQuote, coverageFloorEpoch, to)
	if err != nil {
		s.logger.Warn("rwa premium: oracle day-bucket read failed", "err", err)
		return nil, false
	}
	return rwaReduceFeedSeries(rows), true
}

// ─── market leg ─────────────────────────────────────────────────────

// rwaPremiumMarketDays reads every candidate's observed dollar market,
// keyed by canonical asset id.
func (s *Server) rwaPremiumMarketDays(
	ctx context.Context, cands []rwaHistoryCandidate, to time.Time,
) (map[string][]timescale.MarketDay, bool) {
	assets := make([]canonical.Asset, 0, len(cands))
	for _, c := range cands {
		assets = append(assets, canonical.Asset{
			Type: canonical.AssetClassic, Code: c.code, Issuer: c.issuer,
		})
	}
	// Full history for the same reason as the reference leg; the first
	// observed day also bounds the reference-only count.
	rows, err := s.MarketHistory.DailyMarketDays(
		ctx, assets, s.rwaPremiumUSDQuotes(), coverageFloorEpoch, to)
	if err != nil {
		s.logger.Warn("rwa premium: market day read failed", "err", err)
		return nil, false
	}
	out := map[string][]timescale.MarketDay{}
	for _, r := range rows {
		out[r.AssetID] = append(out[r.AssetID], r)
	}
	return out, true
}

// rwaPremiumUSDQuotes is the set of spellings a dollar may take on the quote side of a market this surface reads. Deliberately NARROW:
// the verified USD-pegged classic assets in every canonical form plus `fiat:USD`,
// not the chart's wider stablecoin-ticker proxy set (trusting a ticker rather than the issuer let an unpegged "USDT" set XLM's all-time
// high, see [timescale.AssetATH]): a non-dollar quote leg is a false financial claim.
func (s *Server) rwaPremiumUSDQuotes() []canonical.Asset {
	seen := map[string]struct{}{}
	out := make([]canonical.Asset, 0, 4)
	add := func(a canonical.Asset) {
		k := a.String()
		if _, dup := seen[k]; dup {
			return
		}
		seen[k] = struct{}{}
		out = append(out, a)
	}
	add(rwaPremiumHistoryQuote)
	for _, peg := range s.USDPeggedClassics {
		for _, form := range assetAliases(peg) {
			add(form)
		}
	}
	return out
}

// ─── the join ───────────────────────────────────────────────────────

// rwaPremiumJoin measures one member's premium on every day BOTH legs were observed and accounts for the days only one was. A day enters
// the series only if present in BOTH maps, nothing carried;
// a day failing [Server.rwaPremiumDayFloorFor] is recorded as WITHHELD, not skipped: "too thin to publish" and "no market" differ.
func rwaPremiumJoin(
	c rwaHistoryCandidate, ref *rwaFeedSeries, market []timescale.MarketDay,
	floor pricingguard.SubstancePolicy,
) rwaPremiumMember {
	out := rwaPremiumMember{
		assetID:   c.assetID,
		code:      c.code,
		issuer:    c.issuer,
		label:     c.label,
		feed:      c.feed,
		source:    ref.source,
		published: map[time.Time]struct{}{},
	}
	if len(market) == 0 {
		// No market days at all: the caller reports no_market_history. Nothing is counted either,
		// per the reference-only bound below.
		return out
	}
	for _, d := range market {
		if !rwaPremiumDayClears(d, floor) {
			out.withheld = append(out.withheld, d.Day)
			continue
		}
		day, ok := rwaPremiumDayOf(d, ref.days[d.Day])
		if !ok {
			continue
		}
		out.days = append(out.days, day)
		out.published[d.Day] = struct{}{}
	}
	out.refOnly = rwaPremiumReferenceOnlyDays(ref, out.published, out.withheld,
		rwaPremiumFirstObserved(market))

	sortDays := func(d []time.Time) {
		sort.Slice(d, func(i, j int) bool { return d[i].Before(d[j]) })
	}
	sort.Slice(out.days, func(i, j int) bool { return out.days[i].day.Before(out.days[j].day) })
	sortDays(out.refOnly)
	sortDays(out.withheld)
	return out
}

// rwaPremiumDayClears applies the thin-market floor to one day; a volume that will not parse counts as zero and the day fails, matching
// [pricingguard.SubstanceOK]'s posture.
func rwaPremiumDayClears(d timescale.MarketDay, floor pricingguard.SubstancePolicy) bool {
	volume, ok := new(big.Rat).SetString(d.VolumeUSD)
	if !ok {
		volume = new(big.Rat)
	}
	return pricingguard.SubstanceOK(volume, d.Hours, d.SpanSeconds, floor)
}

// rwaPremiumDayOf measures one day's premium against that day's published value. ok=false (not an error) means no comparison: the oracle
// published nothing or a non-positive value (never divided by), or the market
// figure is not positive. Exact rationals; the 4-dp scale is `premium.pct`'s on /v1/rwa/assets so the two surfaces cannot round one
// quantity two ways.
func rwaPremiumDayOf(d timescale.MarketDay, refPrice *big.Rat) (rwaPremiumDay, bool) {
	if refPrice == nil || refPrice.Sign() <= 0 {
		return rwaPremiumDay{}, false
	}
	marketPrice, ok := new(big.Rat).SetString(d.VWAP)
	if !ok || marketPrice.Sign() <= 0 {
		return rwaPremiumDay{}, false
	}
	volume, ok := new(big.Rat).SetString(d.VolumeUSD)
	if !ok {
		volume = new(big.Rat)
	}
	pct := new(big.Rat).Sub(marketPrice, refPrice)
	pct.Quo(pct, refPrice)
	pct.Mul(pct, new(big.Rat).SetInt64(100))
	return rwaPremiumDay{
		day:       d.Day,
		pct:       pct.FloatString(4),
		market:    marketPrice.FloatString(10),
		reference: refPrice.FloatString(10),
		volumeUSD: volume.FloatString(2),
		trades:    d.Trades,
	}, true
}

// rwaPremiumFirstObserved is the earliest day the market read answered for, taken as a minimum rather than assumed from row order, so an
// infrastructure gap is not reported as a market fact.
func rwaPremiumFirstObserved(market []timescale.MarketDay) time.Time {
	first := market[0].Day
	for _, d := range market {
		if d.Day.Before(first) {
			first = d.Day
		}
	}
	return first
}

// rwaPremiumReferenceOnlyDays lists the days the oracle published on and no premium came out BECAUSE THERE WAS NO MARKET. A WITHHELD day
// had a refused market, the opposite finding; counting it in both would sum past the window.
// Only days from `firstObserved` onward count: earlier, "did not trade" and "hour aggregate not materialised" are indistinguishable.
func rwaPremiumReferenceOnlyDays(
	ref *rwaFeedSeries,
	published map[time.Time]struct{},
	withheld []time.Time,
	firstObserved time.Time,
) []time.Time {
	refused := make(map[time.Time]struct{}, len(withheld))
	for _, d := range withheld {
		refused[d] = struct{}{}
	}
	var out []time.Time
	for day := range ref.days {
		if day.Before(firstObserved) {
			continue
		}
		if _, done := published[day]; done {
			continue
		}
		if _, skip := refused[day]; skip {
			continue
		}
		out = append(out, day)
	}
	return out
}

func rwaPremiumSources(members []rwaPremiumMember) []string {
	seen := map[string]struct{}{}
	out := []string{}
	for _, m := range members {
		if m.source == "" {
			continue
		}
		if _, dup := seen[m.source]; dup {
			continue
		}
		seen[m.source] = struct{}{}
		out = append(out, m.source)
	}
	sort.Strings(out)
	return out
}
