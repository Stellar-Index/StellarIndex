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

// GET /v1/rwa/premium — the token's price against the instrument's net
// asset value, over time.
//
// # What this is
//
// /v1/rwa/assets publishes `premium.pct` per asset: the token's market
// price measured against an independent oracle's valuation of the
// instrument the issuer declares it anchors to. That figure is
// point-in-time. This surface is the same measurement as a daily
// series, and it is the one chart a chain-query tool structurally
// cannot draw: it needs both an observed market price and an oracle's
// published NAV on the same clock, and a chain query has neither leg.
//
// # The two legs, and why NEITHER may be carried forward
//
// The sibling /v1/rwa/history carries ONE of its legs across a silent
// day, and the asymmetry there is load-bearing: supply is cumulated
// from an append-only log that records every event able to move the
// level, so a day with no entry is a day the level did not change —
// carrying it is arithmetic. Its price leg is a sampled observation and
// is never carried.
//
// A premium has no such leg. It is a ratio of two SAMPLED observations:
//
//   - The MARKET leg is the day's volume-weighted average of what
//     buyers were observed paying. A day with no trade is a day nobody
//     transacted, not a day the price stayed put — a held-to-maturity
//     instrument can sit untraded for weeks while the thing it tracks
//     moves daily.
//   - The REFERENCE leg is the day's closing publication by an oracle.
//     A day the oracle was silent is a day nobody stated a value.
//
// Carrying EITHER manufactures the number outright. Carry the market
// leg over a week in which the NAV rose and the chart draws a widening
// discount that nobody traded; carry the NAV leg and it draws a premium
// against a valuation nobody published. So a point exists only on a day
// BOTH legs were independently observed, and every other day is a hole.
// The response says how many assets were measurable on each day so a
// reader can tell a quiet market from a quiet sector.
//
// # The market leg is gated, not merely read
//
// The premium on /v1/rwa/assets compares against the SERVED price,
// which has already passed the thin-market substance gate: an
// aggregated price claim is withheld for a pair whose trailing market
// activity is below the operator's floor, because on a permissionless
// DEX an attacker can mint a token, seed a handful of dust trades and
// have their own rate published as ours (the 2026-08-04 valuation
// incident). A history built on the raw daily VWAP would publish, for
// every past day, exactly the claim the live surface refuses — the gate
// routed around by changing the time axis.
//
// So each day's market leg is measured and then held to a floor of the
// same shape ([rwaPremiumDayFloor]), and a day that fails it is
// withheld and COUNTED rather than dropped in silence.
//
// The floor is not the live gate and cannot be. The live gate measures
// a trailing 24 hours ending now at MINUTE grain; a past day has no
// "trailing 24 hours", and the minute aggregate's reach back through
// history is a deployment setting rather than a property of the schema
// (see [timescale.Store.DailyMarketDays] — a retention policy on it
// ships disarmed and would truncate the series the day it is armed).
// What survives is the same three legs at the coarsest grain history
// reliably keeps, which is the hour. The
// consequence to state plainly: an asset can carry a premium on a past
// day and none today, because its market was substantial then and is
// too thin now. That is not an inconsistency between the two surfaces —
// it is the two surfaces measuring two different days.
//
// # Coverage is small, and the response says how small
//
// Only a curated (code, issuer) → feed binding may value an instrument
// (internal/rwa/oracle_reference.go), and the curated set binds SEVEN
// pairs of the fourteen `rwa:` feeds the registry carries — XAU and
// SPXU are deliberately refused as off-chain reference codes, and the
// rest have no admitted Stellar issuer. Of those seven, only the ones
// that actually TRADE against a dollar can carry a premium at all: a
// tokenized bill that is bought and held has a NAV every day and a
// market price on none. `excluded[]` names every member left out and
// who can move it.

// rwaPremiumHistoryTTL bounds the reuse of one assembled series. It
// matches [rwaHistoryTTL] for the same reason: a daily grain cannot
// move faster than the day, and both legs are scans.
const rwaPremiumHistoryTTL = 10 * time.Minute

// rwaPremiumHistoryBudget bounds the two scans together, so a slow
// store cannot hold a request open for the handler's whole budget.
const rwaPremiumHistoryBudget = 20 * time.Second

// rwaPremiumHistoryMaxPoints caps the served point count per series.
const rwaPremiumHistoryMaxPoints = 4096

// rwaPremiumHistoryQuote is the denominator of both legs. Fixed, not a
// parameter: the reference feeds this surface admits are the
// USD-denominated ones, and the market leg is read against dollar
// spellings only.
var rwaPremiumHistoryQuote = canonical.Asset{Type: canonical.AssetFiat, Code: "USD"}

// rwaPremiumDayFloor is the per-day market-substance floor.
//
// It is [pricingguard.SubstancePolicy] — the same three legs, evaluated
// by the same pure decision ([pricingguard.SubstanceOK]) — with each
// number chosen against the serving gate's default and the difference
// stated:
//
//   - MinVolumeUSD is the serving default, UNCHANGED. Both windows are
//     24 hours long, so the number means the same thing.
//   - MinSpan is the serving default, UNCHANGED. It is the
//     "a market must have existed at more than one point in time" leg,
//     and it is grain-independent.
//   - MinBuckets is NOT the serving default and must not be. The
//     serving gate counts distinct MINUTE buckets, of which a day holds
//     1440; this counts distinct HOUR buckets, of which a day holds 24,
//     because the hour aggregate is the coarsest-reaching grain that
//     still says WHEN inside a past day the trading happened (see
//     [timescale.Store.DailyMarketDays]). Inheriting 20 here would
//     demand 20 of 24 hours and blank every series. Two hours is the
//     weakest form of the property the span leg already carries, kept
//     explicit so the policy has all three legs rather than two and a
//     silence.
//
// TestRWAPremiumDayFloorTracksServingDefaults pins the two shared
// numbers to the pricingguard constants, so lowering the serving floor
// without deciding about this one fails the build rather than letting
// the two drift apart quietly.
var rwaPremiumDayFloor = pricingguard.SubstancePolicy{
	MinVolumeUSD: new(big.Rat).SetInt64(pricingguard.DefaultSubstanceMinVolumeUSD),
	MinBuckets:   2,
	MinSpan:      pricingguard.DefaultSubstanceMinSpan,
	Window:       24 * time.Hour,
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
	// Assets and Issuers size the WHOLE RWA set, including every member
	// this surface cannot compare. `coverage[].assets_unmeasured` is
	// counted against Assets.
	Assets  int `json:"assets"`
	Issuers int `json:"issuers"`
	// Bound is how many set members carry a curated (code, issuer) →
	// instrument-feed binding at all — the ceiling on coverage, and a
	// number a reader would otherwise have to fetch the definition to
	// learn.
	Bound int `json:"bound"`
	// Members is how many set members COULD be compared at all —
	// `members` plus every `excluded[].assets` is exactly `assets`, so
	// the account closes. It is not windowed: a narrower `timeframe`
	// can leave `series` shorter than this, because a member with no
	// observation inside the window is not a member that could not be
	// compared.
	Members int `json:"members"`
	// MembershipAsOf is when the set was decided. Every point is
	// measured against THIS membership, including points that predate
	// an asset's admission — the same caveat the sibling series carries.
	MembershipAsOf WireTime `json:"membership_as_of"`
	// Excluded names the set members with no series and why, tallied by
	// reason.
	Excluded []RWAHistoryExcluded `json:"excluded,omitempty"`
	// Sources names the distinct oracles behind the reference leg,
	// sorted.
	Sources []string `json:"sources,omitempty"`
	// Series is one premium history per comparable member.
	//
	// There is deliberately NO total series. A premium is a percentage
	// of a different denominator for every member, so the set has no
	// sum; an average would need a weighting nobody published and would
	// hide the dispersion, which on this measurement is the finding.
	Series []RWAPremiumSeries `json:"series"`
	// Coverage is the daily account of how much of the set could be
	// compared, for the days any member could. It is the strip a reader
	// needs to tell "the sector moved" from "we could see less of it".
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
	// Feed is the ADR-0028 instrument feed the market price was
	// measured against, in canonical `rwa:` form; Source is the oracle
	// whose series was read.
	Feed   string `json:"feed"`
	Source string `json:"source"`
	// Points is the series, ascending by day. A day either leg was not
	// observed on is ABSENT.
	Points []RWAPremiumHistoryPoint `json:"points"`
	// MarketWithheldDays is how many days inside the window carried
	// observed trades that did NOT clear the day floor. Served because
	// "we saw trades and refused to price them" and "there were no
	// trades" are opposite findings that an absent point renders
	// identically.
	MarketWithheldDays int `json:"market_withheld_days,omitempty"`
	// ReferenceOnlyDays is how many days inside the window carried a
	// published NAV and no market at all. On this asset class that is
	// the ordinary case, not a defect — the instruments are bought and
	// held.
	//
	// Counted only from this member's FIRST observed market day onward,
	// and never on a day already counted in MarketWithheldDays. Before
	// the first observed day "did not trade", "did not exist yet" and
	// "the aggregate does not reach back that far" are one thing from
	// here, and a tally that conflated them would report an
	// infrastructure gap as a fact about the market.
	ReferenceOnlyDays int `json:"reference_only_days,omitempty"`
}

// RWAPremiumHistoryPoint is one day of one member's premium.
//
// Both prices are served beside the ratio, not just the ratio: the
// point-in-time surface publishes the reference and the market figure
// separately for the same reason, because a reader who can see only
// their quotient cannot tell which side moved.
type RWAPremiumHistoryPoint struct {
	// T is the UTC day the bucket opens.
	T WireTime `json:"t"`
	// PremiumPct is (market − reference) / reference × 100 as a signed
	// 4-dp decimal STRING (ADR-0003) — positive when the token traded
	// ABOVE the instrument's independent valuation. The same arithmetic
	// and the same scale as `premium.pct` on /v1/rwa/assets.
	PremiumPct string `json:"premium_pct"`
	// MarketUSD is the day's volume-weighted average price, exact.
	MarketUSD string `json:"market_usd"`
	// ReferenceUSD is the day's closing oracle value at the feed's own
	// scale, exact.
	ReferenceUSD string `json:"reference_usd"`
	// VolumeUSD and Trades are the market leg's evidence — the figures
	// the day floor was applied to. A premium drawn from a day that
	// barely cleared the floor is a weaker claim than one drawn from a
	// deep day, and nothing else on the wire would say so.
	VolumeUSD string `json:"volume_usd"`
	Trades    int64  `json:"trades"`
}

// RWAPremiumCoveragePoint is one day's account of how much of the set
// could be compared.
type RWAPremiumCoveragePoint struct {
	T WireTime `json:"t"`
	// AssetsMeasured and AssetsUnmeasured split the WHOLE set by whether
	// it carried a premium on this day. Their sum is `assets`.
	AssetsMeasured   int `json:"assets_measured"`
	AssetsUnmeasured int `json:"assets_unmeasured"`
}

// Premium-series exclusion reasons that the value series has no
// equivalent for. The shared ones — issuer_flagged, not_bound,
// contract_not_bound, no_reference_history — are reused verbatim from
// rwa_history.go, because they are the same refusal by the same rule
// and two spellings of one reason is how a funnel starts lying.
const (
	// RWAPremiumExcludedNoMarketHistory — the member is bound to a feed
	// and the oracle has published for it, but this index has never
	// observed a dollar-quoted trade in the token. The ordinary state
	// of a held-to-maturity instrument, and not a defect.
	RWAPremiumExcludedNoMarketHistory = "no_market_history"
	// RWAPremiumExcludedMarketBelowFloor — dollar-quoted trades exist
	// but not one day of them cleared the thin-market floor, so no day
	// may carry an aggregated price claim.
	RWAPremiumExcludedMarketBelowFloor = "market_below_floor"
	// RWAPremiumExcludedNoOverlap — both legs exist and never on the
	// same day. A ratio needs one clock, and interpolating either side
	// onto the other's days is the fabrication this whole surface
	// refuses.
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

// RWAMarketHistoryReader reads the observed daily dollar market. Backs
// the market leg of GET /v1/rwa/premium.
//
// Declared apart from the other price seams for [RWAOracleHistoryReader]'s
// reason: this is a history capability, and putting it on a live-read
// interface would make it a nil-returning stub in every implementation
// that does not need it.
//
// Production wiring is *timescale.Store directly. Nil disables the
// endpoint — a premium with the market leg missing is not a shorter
// series, it is no series.
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
	if s.assetsReader == nil || s.oracleHistory == nil || s.marketHistory == nil {
		view.Basis = rwaPremiumBasisUnavailable
		writeEnvelope(w, Envelope{Data: view, Flags: Flags{}})
		return
	}

	hist := s.cachedRWAPremiumHistory(r.Context())
	if !hist.available {
		view.Basis = rwaPremiumBasisUnavailable
		writeEnvelope(w, Envelope{Data: view, Flags: Flags{}})
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
	// Members counts the ASSEMBLY, not the window, so that
	// `members` + every `excluded[].assets` closes exactly on `assets`.
	// A narrower window can leave `series` shorter than `members` — a
	// member with no observation inside it is not a member that could
	// not be compared — and the two numbers being allowed to differ is
	// what keeps the account from having to lie about one of them.
	view.Members = len(hist.members)
	view.Coverage = rwaPremiumCoverage(view.Series, hist.setAssets)
	writeEnvelope(w, Envelope{Data: view, Flags: Flags{}})
}

// parseRWAPremiumParams validates `timeframe`. The window vocabulary is
// [rwaHistoryTimeframes] verbatim — the same daily grain, so the same
// refusal of the sub-daily tokens.
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

// rwaPremiumSeriesRows windows each member's series and drops the ones
// the window leaves empty.
//
// Ordered by the member's LAST premium, widest ABSOLUTE dispersion
// first, ties broken on asset id so the order never depends on map
// iteration. Absolute because a 4% discount and a 4% premium are the
// same size of finding and a signed sort would bury one of them.
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

// rwaPremiumCoverage counts, per day, how much of the WHOLE set carried
// a premium.
//
// Taken against `setAssets` rather than against the members that
// happened to be comparable, for the reason every count on the sibling
// series is: a point states its coverage of the sector, not of whatever
// was readable.
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

// cachedRWAPremiumHistory returns the assembled series, rebuilt at most
// once per TTL and shared across concurrent requests by a single-flight
// gate — the shape [Server.cachedRWAValueHistory] uses, for the same
// reason: both legs are scans.
//
// On a failed assembly the last good series is served when there is
// one, and an UNAVAILABLE one when there is not. Unavailable is not
// empty: an empty series would say no token has ever traded away from
// its instrument's value, which a read that did not answer may not
// claim.
func (s *Server) cachedRWAPremiumHistory(ctx context.Context) rwaPremiumHistory {
	s.rwaPremMu.Lock()
	if s.rwaPremCache != nil && time.Since(s.rwaPremAt) < rwaPremiumHistoryTTL {
		h := *s.rwaPremCache
		s.rwaPremMu.Unlock()
		return h
	}
	if ch := s.rwaPremFlight; ch != nil {
		s.rwaPremMu.Unlock()
		select {
		case <-ch:
			s.rwaPremMu.Lock()
			var h rwaPremiumHistory
			if s.rwaPremCache != nil {
				h = *s.rwaPremCache
			}
			s.rwaPremMu.Unlock()
			return h
		case <-ctx.Done():
			return rwaPremiumHistory{}
		}
	}
	done := make(chan struct{})
	s.rwaPremFlight = done
	s.rwaPremMu.Unlock()

	built := s.buildRWAPremiumHistory(ctx)

	s.rwaPremMu.Lock()
	if built.available {
		s.rwaPremCache = &built
		s.rwaPremAt = time.Now()
	} else if s.rwaPremCache != nil {
		built = *s.rwaPremCache
	}
	s.rwaPremFlight = nil
	s.rwaPremMu.Unlock()
	close(done)
	return built
}

// buildRWAPremiumHistory assembles one series: today's membership, the
// oracle day buckets for the members' bound instruments, and the days
// their tokens were observed trading against a dollar.
func (s *Server) buildRWAPremiumHistory(ctx context.Context) rwaPremiumHistory {
	ctx, cancel := context.WithTimeout(ctx, rwaPremiumHistoryBudget)
	defer cancel()

	m := s.cachedRWAMembership(ctx)
	// Unavailable means NEITHER arm answered — the same test the other
	// two RWA surfaces apply. One arm reporting a set while the other
	// failed is a partial measurement, and the excluded tally is where
	// that shows up.
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

	// Complete UTC days only, both legs. A premium is a ratio of two
	// figures measured over the SAME day, and today is a day neither
	// leg has finished: a partial day's VWAP is not the day's price,
	// and holding it to a floor measured over a whole day would report
	// every morning as a thin market.
	to := time.Now().UTC().Truncate(24 * time.Hour).Add(-24 * time.Hour)

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
		member := rwaPremiumJoin(c, series, days)
		if len(member.days) == 0 {
			// Trades existed. Say which of the two ways it failed:
			// every day was refused by the floor, or the days that
			// cleared it never coincided with a published value.
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

// rwaPremiumCandidates applies the gates that need no read: the
// scam-flag suppression FIRST, then the curated (code, issuer) → feed
// binding. Every refusal is tallied rather than dropped.
//
// The scam flag is checked before the binding is consulted, and the
// order is deliberate: a flagged issuer must be suppressed whether or
// not anything would have priced it, so that adding a binding later can
// never turn a suppression into a published claim. It is the same order
// [rwaHistoryCandidates] applies, and the same reason.
//
// Unlike the value series this needs no Stellar Asset Contract address:
// a premium compares two prices and reads no supply at all.
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

// rwaPremiumReferenceDays reads the oracle day buckets for the
// candidates' bound feeds and reduces each feed to ONE publisher's
// series.
//
// The reduction is [rwaReduceFeedSeries] verbatim — the same
// oracle-class gate, the same non-positive refusal, the same
// pick-one-source-for-the-whole-series rule that keeps a publisher
// switch from rendering as a price move. Two reductions of one quantity
// is how the value series and this one would come to disagree about
// what an instrument was worth on a day.
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
	rows, err := s.oracleHistory.DailyOraclePrices(ctx, assets, rwaPremiumHistoryQuote, time.Time{}, to)
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
	rows, err := s.marketHistory.DailyMarketDays(
		ctx, assets, s.rwaPremiumUSDQuotes(), time.Time{}, to)
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

// rwaPremiumUSDQuotes is the set of spellings a dollar may be written
// as on the quote side of a market this surface will read.
//
// Deliberately the NARROW set — the operator's verified USD-pegged
// classic assets in every canonical form, plus the synthetic `fiat:USD`
// an off-chain venue quotes in — and NOT the wider proxy set the chart
// walks. That wider set includes abstract stablecoin tickers, and
// [timescale.AssetATH] records what a token wearing one of them did the
// last time a surface trusted the ticker rather than the issuer: an
// unpegged asset calling itself USDT put XLM's all-time high at $1.03.
// A premium divides by an oracle's NAV, so a quote leg that is not
// actually a dollar does not produce a noisy number, it produces a
// false financial claim about a real instrument.
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
	for _, peg := range s.usdPeggedClassics {
		for _, form := range assetAliases(peg) {
			add(form)
		}
	}
	return out
}

// ─── the join ───────────────────────────────────────────────────────

// rwaPremiumJoin measures one member's premium on every day BOTH legs
// were observed, and accounts for the days only one of them was.
//
// The market days drive the walk because they are the scarcer leg by a
// wide margin on this asset class, but the rule is symmetric and stated
// as such: a day enters the series only if the day is present in BOTH
// maps. Nothing is carried in either direction. A day whose market did
// not clear [rwaPremiumDayFloor] is recorded as WITHHELD rather than
// simply skipped, because "the market was too thin to publish" and
// "there was no market" are different findings.
func rwaPremiumJoin(
	c rwaHistoryCandidate, ref *rwaFeedSeries, market []timescale.MarketDay,
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
		// No market days at all: the caller reports this as
		// no_market_history and never reaches the series. Nothing here
		// may be counted either — see the reference-only bound below
		// for why a token with no observed market gets no tally of
		// days it "did not trade".
		return out
	}
	for _, d := range market {
		if !rwaPremiumDayClears(d) {
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

// rwaPremiumDayClears applies the thin-market floor to one day.
//
// A volume that will not parse cannot be held to the floor, and the
// floor is what licenses the price claim — so it counts as zero and the
// day fails. That is [pricingguard.SubstanceOK]'s own posture for an
// unverifiable volume, stated here because the parse happens on this
// side of the call.
func rwaPremiumDayClears(d timescale.MarketDay) bool {
	volume, ok := new(big.Rat).SetString(d.VolumeUSD)
	if !ok {
		volume = new(big.Rat)
	}
	return pricingguard.SubstanceOK(volume, d.Hours, d.SpanSeconds, rwaPremiumDayFloor)
}

// rwaPremiumDayOf measures one day's premium against that day's
// published value, or reports that there is none to measure.
//
// ok=false covers three refusals that are all the same statement — this
// day carries no comparison — and none of which is an error: the oracle
// published nothing, it published a non-positive value (bad data, never
// a valuation of zero, and nothing is divided by it), or the market
// figure is not a positive number.
//
// Exact rational arithmetic throughout (ADR-0003); the 4-dp scale is
// `premium.pct`'s on /v1/rwa/assets, so the two surfaces cannot round
// one quantity two ways.
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

// rwaPremiumFirstObserved is the earliest day the market read answered
// for. The rows arrive ordered, but the minimum is taken rather than
// assumed: this bound is what keeps an infrastructure gap from being
// reported as a fact about the market, and it should not rest on an
// ORDER BY in another package.
func rwaPremiumFirstObserved(market []timescale.MarketDay) time.Time {
	first := market[0].Day
	for _, d := range market {
		if d.Day.Before(first) {
			first = d.Day
		}
	}
	return first
}

// rwaPremiumReferenceOnlyDays lists the days the oracle published on
// and no premium came out of BECAUSE THERE WAS NO MARKET.
//
// Two bounds, each closing a way the count could state something it
// does not know:
//
//   - A day already counted as WITHHELD is a day the market existed and
//     was refused, which is the opposite finding. Counting it in both
//     would let a reader add the two tallies and get more days than the
//     window holds.
//   - Only days from `firstObserved` onward are counted. Before it,
//     "the token did not trade", "the token did not exist yet" and "the
//     hour aggregate is not materialised that far back" are
//     indistinguishable from here, and a count that conflates them
//     would report a materialisation gap as a fact about the market.
//     After it the market read has demonstrably answered for the span,
//     so a silent day inside it is a silent market.
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
