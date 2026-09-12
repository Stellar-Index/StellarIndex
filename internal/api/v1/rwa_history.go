// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"math/big"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/rwa"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// GET /v1/rwa/history — the tokenized-real-world-asset set valued over
// time, on the reference basis.
//
// # What this series is, and the one thing it is not
//
// /v1/rwa/assets publishes two totals over the set: a MARKET CAP, summed
// from prices buyers were observed paying, and a REFERENCE VALUATION,
// summed from what independent oracles say the underlying instruments
// are worth. This surface is the second of those over time, and it is
// deliberately not the first. Most of the set is held to maturity and
// has never traded — the market-cap total covers a handful of rows while
// the reference total covers nearly all of them — so a market series
// would be a chart of the small overlap, drawn under a headline about
// the sector. A per-asset market-cap history already exists for the rows
// that do trade, at /v1/chart?price_type=market_cap.
//
// # The two legs, and why only one of them may be carried forward
//
// A day's value for one member is `circulating supply × the day's
// closing oracle value of the instrument`. The legs come from different
// stores and have different rights:
//
//   - SUPPLY comes from the lake's append-only mint/burn/clawback log
//     (`stellar.supply_flows`, keyed on the asset's deterministic SAC
//     address), cumulated from the contract's first flow. Because the
//     log records EVERY event that can move the level, a day with no row
//     is a day the supply did not change — not a day it was not
//     observed. Carrying the running total across it is arithmetic.
//
//   - PRICE comes from the `oracle_prices_1d` continuous aggregate
//     (migration 0034), keyed `rwa:<CODE>` / `fiat:USD`. It is an
//     observation of a quantity that moves on its own, so a day the
//     oracle was silent is a GAP. The member contributes nothing that
//     day, the point says so in `assets_valued`, and no value is
//     invented for it.
//
// A day on which NO member can be valued produces NO POINT AT ALL. The
// series has holes in it rather than zeros, for the reason every money
// surface here withholds rather than defaults: a reader cannot tell a
// zero that means "worthless" from a zero that means "we could not see".
//
// # Why the supply leg is not supply_1d
//
// `supply_1d` is the CAGG the crypto market-cap chart uses, and it is
// the obvious candidate. It holds nothing for this set: it rolls up
// `asset_supply_history`, which the aggregator writes only for the
// operator-curated `watched_classic_assets` list (USDC, EURC, AQUA,
// yXLM, VELO, BLND, PHO, KALE), and no RWA issuer is on it. Reading it
// here would have produced an empty chart that looked like a finding
// about the sector rather than a gap in a watch list.
//
// # Membership is today's, applied backwards
//
// The set is rebuilt from TODAY's SEP-1 attestations and TODAY's curated
// directory. An asset that qualifies now is valued back to its first
// flow, and an asset that would have qualified last year but does not
// now is absent for the whole window. This is stated on the wire
// (`membership_as_of`, and the `basis` prose) rather than left for a
// reader to assume, because the alternative — reconstructing membership
// per day — needs an attestation history the index does not keep.

// rwaHistoryTTL bounds the reuse of one assembled history.
//
// It matches [rwaMembershipTTL] rather than [rwaReferenceTTL]: the
// series is a DAILY grain, so its newest bucket cannot move faster than
// the oracle publishes into today, and the reads behind it — a
// ClickHouse scan over the flow log and a CAGG scan over every day the
// oracles have published — are far too heavy to run per request.
const rwaHistoryTTL = 10 * time.Minute

// rwaHistoryBudget is the assembly's own deadline. Both legs are scans;
// this bounds them together so a slow lake cannot hold a request open
// for the handler's whole budget.
const rwaHistoryBudget = 20 * time.Second

// rwaHistoryMaxPoints caps the served point count per series. The daily
// grain over the longest window this API offers is a few thousand
// points; the cap exists so a future grain change cannot turn this into
// an unbounded response.
const rwaHistoryMaxPoints = 4096

// rwaHistoryQuote is the denominator. Fixed, not a parameter: the
// reference feeds this surface admits are the USD-denominated ones
// (R-A in rwa_reference.go), so a quote parameter could only ever be
// answered with "fiat:USD" or an error.
var rwaHistoryQuote = canonical.Asset{Type: canonical.AssetFiat, Code: "USD"}

// ─── wire ───────────────────────────────────────────────────────────

// RWAHistoryView is the payload for GET /v1/rwa/history.
type RWAHistoryView struct {
	// Basis states what was measured, on which basis, and what the
	// figures are not — the same posture the point-in-time summary
	// takes, because a total over time is no less in need of it.
	Basis string `json:"basis"`
	// Granularity is the bucket width. Always "1d": both legs are daily
	// and a finer grain would interpolate one of them.
	Granularity string `json:"granularity"`
	// Timeframe echoes the requested window token.
	Timeframe string `json:"timeframe"`
	// Quote is the denominator every figure is in.
	Quote string `json:"quote"`
	// Assets and Issuers size the WHOLE RWA set, including the members
	// this series cannot value. `points[].assets_unvalued` is counted
	// against Assets, so a reader can see the coverage of every point
	// without holding the other endpoint's response.
	Assets  int `json:"assets"`
	Issuers int `json:"issuers"`
	// Members is how many of those assets contribute to the series at
	// all — the ones carrying both a curated instrument binding and a
	// readable supply history.
	Members int `json:"members"`
	// MembershipAsOf is when the set was decided. Every point in the
	// series is valued against THIS membership, including points that
	// predate an asset's admission. See the file comment.
	MembershipAsOf WireTime `json:"membership_as_of"`
	// Excluded names the set members left out of the series and why,
	// tallied by reason. Served for the reason the assets funnel is: a
	// total that silently dropped rows is a smaller claim wearing the
	// same label.
	Excluded []RWAHistoryExcluded `json:"excluded,omitempty"`
	// Sources names the distinct oracles behind the values, sorted. A
	// dollar figure that cannot be traced to a publisher is worse than
	// an absent one.
	Sources []string `json:"sources,omitempty"`
	// Points is the total series, ascending by day. A day no member
	// could be valued on is ABSENT — never a zero and never a repeat of
	// the previous day.
	Points []RWAHistoryPoint `json:"points"`
	// Groups is the same series decomposed, present only when `group_by`
	// asked for one. The group series are NOT guaranteed to sum to
	// `points` on every day, because a member that drops out of one day
	// drops out of both — which is exactly what the per-point counts are
	// there to expose.
	Groups []RWAHistoryGroup `json:"groups,omitempty"`
	// Truncated reports that a cap bound the series, so it is known to
	// be shorter than the data behind it.
	Truncated bool `json:"truncated,omitempty"`
}

// RWAHistoryPoint is one day of the series.
type RWAHistoryPoint struct {
	// T is the UTC day the bucket opens.
	T WireTime `json:"t"`
	// ValueUSD is the exact sum of the contributing members' values on
	// this day, as a 2-dp decimal string (ADR-0003 — rational
	// arithmetic throughout, never a float). Add up the group series
	// for the same day and you land on this number.
	ValueUSD string `json:"value_usd"`
	// AssetsValued and AssetsUnvalued split the WHOLE set by whether it
	// contributed to this day's figure. Their sum is `assets`.
	AssetsValued   int `json:"assets_valued"`
	AssetsUnvalued int `json:"assets_unvalued"`
	// LowerBound is true whenever any set member is unvalued on this
	// day — i.e. whenever the figure is less than the reference-priced
	// value of the set on that day. It is the same claim
	// `reference_valuation.lower_bound` makes on /v1/rwa/assets.
	LowerBound bool `json:"lower_bound"`
}

// RWAHistoryGroup is one decomposition of the total series.
type RWAHistoryGroup struct {
	// Key is the asset_id under group_by=asset and the issuer G-address
	// under group_by=issuer.
	Key string `json:"key"`
	// Label is a human name where one is known — the SEP-1 currency
	// name or the curated directory's attribution. Display text, never
	// identity.
	Label string `json:"label,omitempty"`
	// Code and Issuer carry the classic identity of an asset group.
	Code   string `json:"code,omitempty"`
	Issuer string `json:"issuer,omitempty"`
	// Feed is the ADR-0028 instrument feed an asset group was valued
	// against, in its canonical `rwa:` form; Source is the oracle whose
	// series was read. Both absent on an issuer group, which may span
	// several of each.
	Feed   string `json:"feed,omitempty"`
	Source string `json:"source,omitempty"`
	// Members is how many set members this group's series draws on.
	Members int               `json:"members"`
	Points  []RWAHistoryPoint `json:"points"`
}

// RWAHistoryExcluded is one reason a set member is absent from the
// series, with how many members it accounts for.
type RWAHistoryExcluded struct {
	Reason string `json:"reason"`
	Assets int    `json:"assets"`
	// Detail says in prose what the reason means and, where it matters,
	// who can move it — the same obligation the assets funnel's `actor`
	// carries.
	Detail string `json:"detail"`
}

// RWAHistoryExcluded reasons. Each names a DIFFERENT absence, because a
// member nobody has bound to a feed and a member whose flow log is
// incompletely seeded are opposite findings that one count would render
// identically.
const (
	// RWAHistoryExcludedNotBound — no curated (code, issuer) → feed
	// binding, so nothing may value the member's instrument. The same
	// refusal `premium.status` reports as `not_bound`.
	RWAHistoryExcludedNotBound = "not_bound"
	// RWAHistoryExcludedContract — a contract-issued member. The curated
	// binding table is keyed on (code, issuer), an identity a bare
	// contract does not have.
	RWAHistoryExcludedContract = "contract_not_bound"
	// RWAHistoryExcludedIssuerFlagged — the issuer acquired a scam-class
	// directory tag. The point-in-time surface withholds its valuation;
	// a history that kept publishing one would route around that.
	RWAHistoryExcludedIssuerFlagged = "issuer_flagged"
	// RWAHistoryExcludedNoSupplyHistory — the lake holds no flows for
	// the member's Stellar Asset Contract, so no supply level can be
	// reconstructed for any day.
	RWAHistoryExcludedNoSupplyHistory = "no_supply_history"
	// RWAHistoryExcludedSupplyIncomplete — the running total of the
	// member's flows goes NEGATIVE, which is physically impossible for a
	// real token and means the contract's flows are incompletely seeded
	// in the lake. The same refusal [clickhouse.TokenSupply.Incomplete]
	// makes on the point-in-time path.
	RWAHistoryExcludedSupplyIncomplete = "supply_incomplete"
	// RWAHistoryExcludedNoReferenceHistory — the member is bound to a
	// feed the oracles have never published a USD day bucket for.
	RWAHistoryExcludedNoReferenceHistory = "no_reference_history"
)

var rwaHistoryExcludedDetail = map[string]string{
	RWAHistoryExcludedNotBound:           "No curated binding ties this exact (code, issuer) to an oracle feed, so nothing may value its instrument. A binding is a reviewed code change, not a runtime match on the asset code — see definition.bound_instruments on /v1/rwa/assets.",
	RWAHistoryExcludedContract:           "A contract-issued member. The curated binding table is keyed on (code, issuer), and a bare contract address has no such identity.",
	RWAHistoryExcludedIssuerFlagged:      "Withheld — the issuer carries a scam-class directory flag, so no valuation is published for it anywhere on this site, past or present.",
	RWAHistoryExcludedNoSupplyHistory:    "The lake holds no mint/burn record for this asset's Stellar Asset Contract, so no supply level can be reconstructed for any day. An operator can move this by seeding the contract's flows.",
	RWAHistoryExcludedSupplyIncomplete:   "The running total of this asset's recorded flows goes negative, which no real token can do. Its flow log is incompletely seeded in the lake, and a series built on it would understate the supply by an unknown amount. An operator can move this by re-seeding the contract.",
	RWAHistoryExcludedNoReferenceHistory: "The oracles have published no dollar-denominated day bucket for this asset's bound instrument, so there is no value to multiply the supply by.",
}

// rwaHistoryBasis is the prose that travels with every response. It says
// what the number is, what it is not, and the two structural caveats a
// reader cannot recover from the figures.
const rwaHistoryBasis = "Circulating supply, reconstructed from the lake's complete mint/burn record, times the day's closing value published by an independent oracle for the real-world instrument each token declares it anchors to. It is NOT a market capitalisation: nobody was observed paying it, and the price and liquidity gates behind the market-cap column cannot check it. Membership is today's set applied backwards — an asset that qualifies now is valued back to its first mint. A day on which no member could be valued is absent from the series rather than plotted as zero."

const rwaHistoryBasisUnavailable = "No series is published: membership could not be established, or neither the supply-flow log nor the oracle day buckets answered. An empty chart would be a claim about the sector that the reads did not earn."

// ─── readers ────────────────────────────────────────────────────────

// RWAOracleHistoryReader reads daily oracle observation buckets. Backs
// the price leg of GET /v1/rwa/history.
//
// Declared apart from [OracleReader] rather than added to it: that seam
// is the live-snapshot one and has many implementations, and a history
// method on it would be a nil-returning stub in nearly all of them.
//
// Production wiring is *timescale.Store directly. Nil disables the
// endpoint (503) — the endpoint has nothing to degrade to, because a
// series with the price leg missing is not a shorter series, it is no
// series.
type RWAOracleHistoryReader interface {
	DailyOraclePrices(ctx context.Context, assets []canonical.Asset, quote canonical.Asset, from, to time.Time) ([]timescale.OracleDayPoint, error)
}

// rwaSupplyFlowHistoryReader is the narrow bulk capability the supply
// leg needs. Type-asserted off [Server.tokenSupply] rather than added to
// [TokenSupplyReader], the same optional-seam idiom
// [classicLakeSupplyReader] uses next door, so every existing stub
// implementing that seam keeps compiling and simply opts out.
//
// Production wiring: *clickhouse.SupplyReader, via
// clickhouse.NewSupplyReaderAuth in cmd/stellarindex-api/main.go.
type rwaSupplyFlowHistoryReader interface {
	DailySupplyFlowsForContracts(ctx context.Context, contractIDs []string) ([]clickhouse.SupplyFlowDay, error)
}

// ─── assembled history ──────────────────────────────────────────────

// rwaHistoryDay is one member's value on one day.
type rwaHistoryDay struct {
	day time.Time
	// valueUSD is the exact 2-dp decimal string, built by the SAME
	// helper the point-in-time reference valuation uses so the two
	// surfaces cannot drift into two roundings of one quantity.
	valueUSD string
}

// rwaHistoryMember is one constituent's assembled series.
type rwaHistoryMember struct {
	assetID string
	code    string
	issuer  string
	label   string
	// issuerLabel is the group label an issuer decomposition uses.
	issuerLabel string
	feed        string
	source      string
	days        []rwaHistoryDay
}

// rwaValueHistory is one assembly: every constituent's series, plus the
// account of who was left out.
type rwaValueHistory struct {
	available bool
	// setAssets and setIssuers size the whole RWA set the series is a
	// subset of.
	setAssets  int
	setIssuers int
	members    []rwaHistoryMember
	excluded   map[string]int
	sources    []string
	builtAt    time.Time
}

// ─── handler ────────────────────────────────────────────────────────

// rwaHistoryTimeframes is the accepted window vocabulary — the chart
// endpoint's, minus the two sub-daily tokens. `1h` and `24h` are refused
// rather than quietly served as one or two points: a daily series cannot
// answer an intraday question, and returning a two-point chart for it
// would look like an answer.
var rwaHistoryTimeframes = map[string]time.Duration{
	"1w":  7 * 24 * time.Hour,
	"1mo": 30 * 24 * time.Hour,
	"1y":  365 * 24 * time.Hour,
	"all": 0,
}

// handleRWAHistory serves GET /v1/rwa/history.
func (s *Server) handleRWAHistory(w http.ResponseWriter, r *http.Request) {
	tf, groupBy, ok := parseRWAHistoryParams(w, r)
	if !ok {
		return
	}
	view := RWAHistoryView{
		Basis:       rwaHistoryBasis,
		Granularity: "1d",
		Timeframe:   tf,
		Quote:       rwaHistoryQuote.String(),
		Points:      []RWAHistoryPoint{},
	}
	if s.assetsReader == nil || s.oracleHistory == nil {
		view.Basis = rwaHistoryBasisUnavailable
		writeEnvelope(w, Envelope{Data: view, Flags: Flags{}})
		return
	}

	hist := s.cachedRWAValueHistory(r.Context())
	if !hist.available {
		view.Basis = rwaHistoryBasisUnavailable
		writeEnvelope(w, Envelope{Data: view, Flags: Flags{}})
		return
	}

	from := rwaHistoryWindowStart(tf, hist.builtAt)
	view.Assets = hist.setAssets
	view.Issuers = hist.setIssuers
	view.Members = len(hist.members)
	view.MembershipAsOf = WireTime(hist.builtAt)
	view.Excluded = rwaHistoryExcludedRows(hist.excluded)
	view.Sources = hist.sources
	view.Points, view.Truncated = rwaHistoryTotal(hist.members, hist.setAssets, from)
	view.Groups = rwaHistoryGroups(hist, groupBy, from)
	writeEnvelope(w, Envelope{Data: view, Flags: Flags{}})
}

// parseRWAHistoryParams validates `timeframe` and `group_by`. ok=false
// after writing a problem response on any parse error.
func parseRWAHistoryParams(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	tf := r.URL.Query().Get("timeframe")
	if tf == "" {
		tf = "1y"
	}
	if _, known := rwaHistoryTimeframes[tf]; !known {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-timeframe",
			"Invalid timeframe", http.StatusBadRequest,
			"the real-world-asset value series is daily; timeframe must be one of: 1w, 1mo, 1y, all (got "+tf+")")
		return "", "", false
	}
	groupBy := r.URL.Query().Get("group_by")
	if groupBy == "" {
		groupBy = "none"
	}
	switch groupBy {
	case "none", "asset", "issuer":
	default:
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-group-by",
			"Invalid group_by", http.StatusBadRequest,
			"group_by must be one of: none, asset, issuer (got "+groupBy+")")
		return "", "", false
	}
	return tf, groupBy, true
}

// rwaHistoryWindowStart is the inclusive first day of the requested
// window, or the zero time for `all`.
func rwaHistoryWindowStart(tf string, now time.Time) time.Time {
	d := rwaHistoryTimeframes[tf]
	if d == 0 {
		return time.Time{}
	}
	return now.UTC().Add(-d).Truncate(24 * time.Hour)
}

// ─── total + groups ─────────────────────────────────────────────────

// rwaHistoryTotal merges the constituents into the set series.
//
// Every day any member could be valued on becomes a point; a day none
// could is ABSENT. `setAssets` — the size of the WHOLE set, not of the
// contributing subset — is what the unvalued count is taken against, so
// a point states its coverage of the sector rather than of whatever
// happened to be readable.
func rwaHistoryTotal(members []rwaHistoryMember, setAssets int, from time.Time) ([]RWAHistoryPoint, bool) {
	type acc struct {
		sum    *big.Rat
		valued int
	}
	byDay := map[time.Time]*acc{}
	for i := range members {
		for _, d := range members[i].days {
			if !from.IsZero() && d.day.Before(from) {
				continue
			}
			v, ok := new(big.Rat).SetString(d.valueUSD)
			if !ok {
				continue
			}
			a := byDay[d.day]
			if a == nil {
				a = &acc{sum: new(big.Rat)}
				byDay[d.day] = a
			}
			a.sum.Add(a.sum, v)
			a.valued++
		}
	}
	days := make([]time.Time, 0, len(byDay))
	for d := range byDay {
		days = append(days, d)
	}
	sort.Slice(days, func(i, j int) bool { return days[i].Before(days[j]) })

	truncated := false
	if len(days) > rwaHistoryMaxPoints {
		days = days[len(days)-rwaHistoryMaxPoints:]
		truncated = true
	}
	out := make([]RWAHistoryPoint, 0, len(days))
	for _, d := range days {
		a := byDay[d]
		unvalued := setAssets - a.valued
		if unvalued < 0 {
			unvalued = 0
		}
		out = append(out, RWAHistoryPoint{
			T:              WireTime(d),
			ValueUSD:       a.sum.FloatString(2),
			AssetsValued:   a.valued,
			AssetsUnvalued: unvalued,
			LowerBound:     unvalued > 0,
		})
	}
	return out, truncated
}

// rwaHistoryGroups decomposes the series, or returns nil when none was
// asked for.
func rwaHistoryGroups(hist rwaValueHistory, groupBy string, from time.Time) []RWAHistoryGroup {
	switch groupBy {
	case "asset":
		return rwaHistoryAssetGroups(hist, from)
	case "issuer":
		return rwaHistoryIssuerGroups(hist, from)
	default:
		return nil
	}
}

func rwaHistoryAssetGroups(hist rwaValueHistory, from time.Time) []RWAHistoryGroup {
	out := make([]RWAHistoryGroup, 0, len(hist.members))
	for i := range hist.members {
		m := &hist.members[i]
		pts, _ := rwaHistoryTotal(hist.members[i:i+1], 1, from)
		if len(pts) == 0 {
			continue
		}
		out = append(out, RWAHistoryGroup{
			Key:     m.assetID,
			Label:   m.label,
			Code:    m.code,
			Issuer:  m.issuer,
			Feed:    "rwa:" + m.feed,
			Source:  m.source,
			Members: 1,
			Points:  pts,
		})
	}
	rwaHistorySortGroups(out)
	return out
}

func rwaHistoryIssuerGroups(hist rwaValueHistory, from time.Time) []RWAHistoryGroup {
	byIssuer := map[string][]rwaHistoryMember{}
	label := map[string]string{}
	for _, m := range hist.members {
		byIssuer[m.issuer] = append(byIssuer[m.issuer], m)
		if label[m.issuer] == "" {
			label[m.issuer] = m.issuerLabel
		}
	}
	out := make([]RWAHistoryGroup, 0, len(byIssuer))
	for issuer, ms := range byIssuer {
		pts, _ := rwaHistoryTotal(ms, len(ms), from)
		if len(pts) == 0 {
			continue
		}
		out = append(out, RWAHistoryGroup{
			Key:     issuer,
			Label:   label[issuer],
			Issuer:  issuer,
			Members: len(ms),
			Points:  pts,
		})
	}
	rwaHistorySortGroups(out)
	return out
}

// rwaHistorySortGroups orders by the group's LAST value descending, so
// the largest constituent leads — and on an exact tie by key, so the
// order does not depend on map iteration.
func rwaHistorySortGroups(g []RWAHistoryGroup) {
	last := func(x RWAHistoryGroup) *big.Rat {
		if len(x.Points) == 0 {
			return new(big.Rat)
		}
		v, ok := new(big.Rat).SetString(x.Points[len(x.Points)-1].ValueUSD)
		if !ok {
			return new(big.Rat)
		}
		return v
	}
	sort.Slice(g, func(i, j int) bool {
		li, lj := last(g[i]), last(g[j])
		if c := li.Cmp(lj); c != 0 {
			return c > 0
		}
		return g[i].Key < g[j].Key
	})
}

func rwaHistoryExcludedRows(tally map[string]int) []RWAHistoryExcluded {
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
			Detail: rwaHistoryExcludedDetail[reason],
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

// cachedRWAValueHistory returns the assembled history, rebuilt at most
// once per TTL window and shared across concurrent requests by a
// single-flight gate — the same shape [Server.cachedRWAMembership] uses,
// and for a heavier reason: both legs are scans.
//
// On a failed assembly the last good history is served when there is
// one, and an UNAVAILABLE one when there is not. Unavailable is not
// empty: an empty series would say the sector has never been worth
// anything, which is a finding a read that did not answer may not make.
func (s *Server) cachedRWAValueHistory(ctx context.Context) rwaValueHistory {
	s.rwaHistMu.Lock()
	if s.rwaHistCache != nil && time.Since(s.rwaHistAt) < rwaHistoryTTL {
		h := *s.rwaHistCache
		s.rwaHistMu.Unlock()
		return h
	}
	if ch := s.rwaHistFlight; ch != nil {
		s.rwaHistMu.Unlock()
		select {
		case <-ch:
			s.rwaHistMu.Lock()
			var h rwaValueHistory
			if s.rwaHistCache != nil {
				h = *s.rwaHistCache
			}
			s.rwaHistMu.Unlock()
			return h
		case <-ctx.Done():
			return rwaValueHistory{}
		}
	}
	done := make(chan struct{})
	s.rwaHistFlight = done
	s.rwaHistMu.Unlock()

	built := s.buildRWAValueHistory(ctx)

	s.rwaHistMu.Lock()
	if built.available {
		s.rwaHistCache = &built
		s.rwaHistAt = time.Now()
	} else if s.rwaHistCache != nil {
		built = *s.rwaHistCache
	}
	s.rwaHistFlight = nil
	s.rwaHistMu.Unlock()
	close(done)
	return built
}

// buildRWAValueHistory assembles one history: today's membership, the
// members' supply levels reconstructed from the flow log, and the day
// buckets the oracles published for their bound instruments.
func (s *Server) buildRWAValueHistory(ctx context.Context) rwaValueHistory {
	ctx, cancel := context.WithTimeout(ctx, rwaHistoryBudget)
	defer cancel()

	m := s.cachedRWAMembership(ctx)
	if !m.available && len(m.members) == 0 {
		return rwaValueHistory{}
	}
	out := rwaValueHistory{
		setAssets:  len(m.members) + len(m.contracts),
		setIssuers: rwaHistoryIssuerCount(m),
		excluded:   map[string]int{},
		builtAt:    time.Now().UTC(),
	}
	out.excluded[RWAHistoryExcludedContract] = len(m.contracts)

	cands := rwaHistoryCandidates(m, out.excluded)
	if len(cands) == 0 {
		// Nothing to value, but the membership read DID answer — so the
		// account of who was excluded is itself the finding, and it is
		// worth caching and serving.
		out.available = true
		return out
	}

	supply, ok := s.rwaHistorySupplyLevels(ctx, cands, out.excluded)
	if !ok {
		return rwaValueHistory{}
	}
	prices, ok := s.rwaHistoryPrices(ctx, cands)
	if !ok {
		return rwaValueHistory{}
	}

	for _, c := range cands {
		levels, hasSupply := supply[c.sac]
		if !hasSupply {
			continue // already tallied by rwaHistorySupplyLevels
		}
		series := prices[c.feed]
		if series == nil {
			out.excluded[RWAHistoryExcludedNoReferenceHistory]++
			continue
		}
		days := rwaHistoryMemberDays(levels, series.days, c.decimals)
		if len(days) == 0 {
			out.excluded[RWAHistoryExcludedNoReferenceHistory]++
			continue
		}
		out.members = append(out.members, rwaHistoryMember{
			assetID:     c.assetID,
			code:        c.code,
			issuer:      c.issuer,
			label:       c.label,
			issuerLabel: c.issuerLabel,
			feed:        c.feed,
			source:      series.source,
			days:        days,
		})
	}
	out.sources = rwaHistorySources(out.members)
	out.available = true
	return out
}

func rwaHistoryIssuerCount(m rwaMembership) int {
	seen := map[string]struct{}{}
	for _, mem := range m.members {
		seen[mem.issuer] = struct{}{}
	}
	return len(seen) + len(m.contracts)
}

// rwaHistoryCandidate is one member that cleared every gate a value
// series can check BEFORE either leg is read.
type rwaHistoryCandidate struct {
	assetID     string
	code        string
	issuer      string
	label       string
	issuerLabel string
	feed        string
	sac         string
	decimals    int
}

// rwaHistoryCandidates applies the gates that do not need a read: the
// scam-flag suppression, the curated (code, issuer) → feed binding, and
// the existence of a deterministic SAC address to key the flow log by.
// Every refusal is tallied rather than dropped.
func rwaHistoryCandidates(m rwaMembership, excluded map[string]int) []rwaHistoryCandidate {
	out := make([]rwaHistoryCandidate, 0, len(m.members))
	for _, mem := range m.members {
		// First, and covering everything below it: the same suppression
		// /v1/assets applies. Handing a flagged issuer a real
		// instrument's NAV history would publish a larger claim through
		// the gap than the price we already withhold.
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
		sac, err := asset.SacContractID()
		if err != nil {
			excluded[RWAHistoryExcludedNoSupplyHistory]++
			continue
		}
		out = append(out, rwaHistoryCandidate{
			assetID:     asset.String(),
			code:        mem.code,
			issuer:      mem.issuer,
			label:       rwaHistoryLabel(mem.name, mem.code),
			issuerLabel: rwaHistoryLabel(mem.dirName, mem.homeDomain),
			feed:        feed,
			sac:         sac,
			// Every classic asset is 7 decimals. Stated as the constant
			// it is rather than read from a catalogue row: this arm
			// admits classic assets only (R1), so there is no other
			// value it could take, and a lookup would only introduce a
			// way for it to be wrong.
			decimals: 7,
		})
	}
	return out
}

func rwaHistoryLabel(preferred, fallback string) string {
	if p := strings.TrimSpace(preferred); p != "" {
		return p
	}
	return strings.TrimSpace(fallback)
}

// ─── supply leg ─────────────────────────────────────────────────────

// rwaSupplyLevel is the circulating supply, in the asset's smallest
// unit, from a given day onward.
type rwaSupplyLevel struct {
	from  time.Time
	level *big.Int
}

// rwaHistorySupplyLevels reconstructs each candidate's supply level
// history from the lake's flow log.
//
// Returns ok=false only when the READ itself failed — a distinction the
// caller needs, because an empty result from a working read is a finding
// about the lake's coverage and an empty result from a failed one is
// not.
func (s *Server) rwaHistorySupplyLevels(
	ctx context.Context, cands []rwaHistoryCandidate, excluded map[string]int,
) (map[string][]rwaSupplyLevel, bool) {
	if s.tokenSupply == nil {
		return nil, false
	}
	rd, ok := s.tokenSupply.(rwaSupplyFlowHistoryReader)
	if !ok {
		return nil, false
	}
	contracts := make([]string, 0, len(cands))
	for _, c := range cands {
		contracts = append(contracts, c.sac)
	}
	flows, err := rd.DailySupplyFlowsForContracts(ctx, contracts)
	if err != nil {
		s.logger.Warn("rwa history: supply flow read failed", "err", err)
		return nil, false
	}
	byContract := map[string][]clickhouse.SupplyFlowDay{}
	for _, f := range flows {
		byContract[f.ContractID] = append(byContract[f.ContractID], f)
	}
	out := make(map[string][]rwaSupplyLevel, len(cands))
	for _, c := range cands {
		days := byContract[c.sac]
		if len(days) == 0 {
			excluded[RWAHistoryExcludedNoSupplyHistory]++
			continue
		}
		levels, complete := rwaCumulateSupply(days)
		if !complete {
			excluded[RWAHistoryExcludedSupplyIncomplete]++
			continue
		}
		out[c.sac] = levels
	}
	return out, true
}

// rwaCumulateSupply turns a contract's daily net flows into its supply
// level from each flow day onward.
//
// Reports complete=false the moment the running total goes NEGATIVE. A
// token cannot have burned more than was ever minted, so a negative
// level is proof that the contract's flows are incompletely seeded in
// the lake — the same reading [clickhouse.TokenSupply.Incomplete] makes
// point-in-time. The whole series is refused rather than the offending
// days, because an incompletely seeded log understates every level after
// the missing mint, not only the ones that went below zero.
//
// Input must be ascending by day; the query orders it so.
func rwaCumulateSupply(days []clickhouse.SupplyFlowDay) ([]rwaSupplyLevel, bool) {
	out := make([]rwaSupplyLevel, 0, len(days))
	running := new(big.Int)
	for _, d := range days {
		if d.Net != nil {
			running.Add(running, d.Net)
		}
		if running.Sign() < 0 {
			return nil, false
		}
		out = append(out, rwaSupplyLevel{from: d.Day, level: new(big.Int).Set(running)})
	}
	return out, true
}

// ─── price leg ──────────────────────────────────────────────────────

// rwaFeedSeries is one instrument feed's day-bucket history, from the
// single oracle chosen to represent it.
type rwaFeedSeries struct {
	source string
	// days holds the day's closing value as an exact rational, keyed by
	// the UTC day.
	days map[time.Time]*big.Rat
}

// rwaHistoryPrices reads the oracle day buckets for the candidates'
// bound feeds and reduces each feed to ONE source's series.
//
// Returns ok=false only on a failed read, for
// [Server.rwaHistorySupplyLevels]'s reason.
func (s *Server) rwaHistoryPrices(
	ctx context.Context, cands []rwaHistoryCandidate,
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
	// Open-ended at the start: the series reaches as far back as the
	// oracle has ever published, and oracle_prices_1d carries no
	// retention policy, so there is nothing to bound it to.
	to := time.Now().UTC().Truncate(24 * time.Hour)
	rows, err := s.oracleHistory.DailyOraclePrices(ctx, assets, rwaHistoryQuote, time.Time{}, to)
	if err != nil {
		s.logger.Warn("rwa history: oracle day-bucket read failed", "err", err)
		return nil, false
	}
	return rwaReduceFeedSeries(rows), true
}

// rwaReduceFeedSeries keeps only oracle-class publishers and reduces
// each feed to ONE source's series.
//
// WHICH SOURCE, AND WHY IT IS CHOSEN ONCE. Several oracles may price the
// same instrument. The live snapshot picks the most recent observation,
// which a day bucket cannot reproduce — `last(price, ts)` keeps the
// value but not its timestamp, so two sources' closings within one day
// are unordered. Picking per day would let the line switch publisher
// between adjacent points and render the switch as a price move. So the
// choice is made ONCE for the whole series — the source with the most
// day buckets, ties broken on the source name so the pick never depends
// on scan order — and the chosen source is named on the wire.
//
// The oracle-class gate is R-C from rwa_reference.go: an aggregator
// writing into the same table for divergence comparison is not an
// independent valuation of the instrument.
func rwaReduceFeedSeries(rows []timescale.OracleDayPoint) map[string]*rwaFeedSeries {
	feeds := rwaFeedDaysBySource(rows)
	out := make(map[string]*rwaFeedSeries, len(feeds))
	for feed, sources := range feeds {
		if best := rwaPickFeedSource(sources); best != "" {
			out[feed] = &rwaFeedSeries{source: best, days: sources[best]}
		}
	}
	return out
}

// rwaFeedDayIndex is one feed's day buckets, still split by publisher.
type rwaFeedDayIndex map[string]map[time.Time]*big.Rat

// rwaFeedDaysBySource indexes the admissible day buckets by feed and
// then by publisher, applying the two row-level refusals: the
// oracle-class gate, and the non-positive value.
func rwaFeedDaysBySource(rows []timescale.OracleDayPoint) map[string]rwaFeedDayIndex {
	feeds := map[string]rwaFeedDayIndex{}
	for _, row := range rows {
		if !rwaAdmissibleFeedRow(row) {
			continue
		}
		src := feeds[row.Asset.Code]
		if src == nil {
			src = rwaFeedDayIndex{}
			feeds[row.Asset.Code] = src
		}
		if src[row.Source] == nil {
			src[row.Source] = map[time.Time]*big.Rat{}
		}
		src[row.Source][row.Bucket] = ratFromScaledInt(row.Price, row.Decimals)
	}
	return feeds
}

// rwaAdmissibleFeedRow reports whether one day bucket may enter a
// reference series.
//
// The oracle-class gate is R-C from rwa_reference.go: an aggregator
// writing into the same table for divergence comparison is not an
// independent valuation of the instrument. A non-positive value is bad
// data, not a valuation of zero — the same refusal
// rwaReferenceValuationOf makes. Dropping the bucket leaves the day a
// gap for this feed, which is correct: the oracle published, but not a
// number anything may be multiplied by.
func rwaAdmissibleFeedRow(row timescale.OracleDayPoint) bool {
	switch {
	case row.Asset.Type != canonical.AssetRWA:
		return false
	case external.Lookup(row.Source).Class != external.ClassOracle:
		return false
	case row.Price == nil || row.Price.Sign() <= 0:
		return false
	}
	return true
}

// rwaPickFeedSource chooses the one publisher a feed's series is drawn
// from: the one covering the most day buckets, ties broken on the source
// name so the pick never depends on map iteration order. Empty when the
// feed has no admissible publisher.
func rwaPickFeedSource(sources rwaFeedDayIndex) string {
	best, bestDays := "", -1
	for name, days := range sources {
		if len(days) > bestDays || (len(days) == bestDays && name < best) {
			best, bestDays = name, len(days)
		}
	}
	return best
}

func rwaHistorySources(members []rwaHistoryMember) []string {
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

// ─── the join ───────────────────────────────────────────────────────

// rwaHistoryMemberDays multiplies one member's supply levels by its
// feed's day buckets.
//
// The PRICE drives the series: a day is produced only where the oracle
// published. The supply level is then the running total as of that day —
// looked up in the level list, which changes only on a flow, so a day
// between two flows takes the earlier one's level. That is the
// asymmetry the file comment argues for, and it is the whole honesty of
// this function: the leg that is a complete log may be carried, the leg
// that is a sampled observation may not.
//
// A price day BEFORE the member's first flow yields nothing: the token
// did not exist yet, and a supply of zero times a real NAV is a
// perfectly computable dollar figure that means nothing at all.
func rwaHistoryMemberDays(levels []rwaSupplyLevel, prices map[time.Time]*big.Rat, decimals int) []rwaHistoryDay {
	if len(levels) == 0 || len(prices) == 0 {
		return nil
	}
	days := make([]time.Time, 0, len(prices))
	for d := range prices {
		days = append(days, d)
	}
	sort.Slice(days, func(i, j int) bool { return days[i].Before(days[j]) })

	out := make([]rwaHistoryDay, 0, len(days))
	li := 0
	var level *big.Int
	for _, d := range days {
		for li < len(levels) && !levels[li].from.After(d) {
			level = levels[li].level
			li++
		}
		if level == nil || level.Sign() == 0 {
			continue
		}
		v := rwaReferenceValueUSD(level.String(), decimals, prices[d])
		if v == "" {
			continue
		}
		out = append(out, rwaHistoryDay{day: d, valueUSD: v})
	}
	return out
}
