package v1

import (
	"context"
	"math/big"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/rwa"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// GET /v1/rwa/assets — tokenized real-world assets on Stellar.
//
// The membership rule is `internal/rwa` and is documented for readers
// at docs/methodology/rwa-definition.md; this file is the read path and
// the wire shape. Two properties of the design are load-bearing and
// easy to erode:
//
//  1. MEMBERSHIP IS DECIDED BEFORE VALUATION. The set is built from
//     identity and attestation only. No number can move an asset in or
//     out, so a withheld price cannot silently shrink the set and a
//     large market cap cannot buy a place in it.
//
//  2. VALUATION COMES FROM THE EXISTING LISTING PIPELINE, UNCHANGED.
//     The same store query, the same substance gate, the same
//     supply-derived market cap, the same dust guard and the same
//     scam-issuer payload suppression that /v1/assets runs. This
//     surface adds no price path of its own, so it cannot publish a
//     figure /v1/assets would have withheld.
//
// Where a valuation is unavailable the row says so in
// `valuation.status` and carries no number. Nothing here renders a
// withheld or missing figure as zero.

// rwaMembershipTTL bounds how long the membership set is reused before
// a rebuild. Its inputs move on daily cadences — the SEP-1 refresh cron
// and the directory sync — so ten minutes keeps the scan off the
// request path while a newly-recognised issuer still appears the same
// hour. Matches the SEP-1 logo map beside it.
const rwaMembershipTTL = 10 * time.Minute

// rwaMaxIssuers bounds the per-issuer listing reads one rebuild will
// make. The qualifying issuer set is small by construction (four on the
// production network, 2026-09-05) because R3 requires an independent
// party to have named the account; the cap is a guard against a
// directory sync that suddenly tags thousands of accounts, not an
// expected condition. When it binds, the surface says so rather than
// serving a silently truncated set.
const rwaMaxIssuers = 64

// rwaAssetsPerIssuer bounds one issuer's listing page. An issuer with
// more classic assets than this has its long tail unread; the cap is
// reported the same way the issuer cap is.
const rwaAssetsPerIssuer = 500

// Sep1BoundCurrencyReader is the storage seam the membership build
// reads its attestations through. *timescale.Store satisfies it.
// Optional: a deployment without it serves an empty set with a stated
// reason rather than an error, exactly as the logo overlay degrades.
type Sep1BoundCurrencyReader interface {
	BoundSep1Currencies(ctx context.Context, keep timescale.Sep1CurrencyFilter) ([]timescale.Sep1BoundCurrency, int, error)
}

// ─── wire shape ─────────────────────────────────────────────────────

// RWAAssetsView is the /v1/rwa/assets payload: the set, its aggregates
// and the rule that produced it.
type RWAAssetsView struct {
	// Definition restates the membership rule in the response so a
	// consumer reads it from the same document as the rows, rather than
	// inferring it from whichever assets happen to qualify today.
	Definition RWADefinition `json:"definition"`
	// Summary is the aggregate over the assets below.
	Summary RWASummary `json:"summary"`
	// Assets is the set, ordered by published market cap descending,
	// then by observation count. Rows with no published valuation sort
	// after every valued row — an unvalued asset is never ranked above
	// a valued one on a number it does not have.
	Assets []RWAAsset `json:"assets"`
	// ByClass totals the set per declared real-world class. Assets
	// admitted on the oracle basis declare no class and are grouped
	// under `unclassified`.
	ByClass []RWAGroupTotal `json:"by_class"`
	// ByIssuer totals the set per issuer G-address.
	ByIssuer []RWAIssuerTotal `json:"by_issuer"`
	// Refused counts the candidates each requirement turned away, so
	// the served set is never mistaken for the whole population of
	// assets that CLAIM to be real-world assets.
	Refused []RWARefusal `json:"refused"`
}

// RWADefinition is the machine-readable membership rule.
type RWADefinition struct {
	// Requirements names the four conjunctive requirements in order.
	Requirements []string `json:"requirements"`
	// AnchorClasses is the closed SEP-1 anchor_asset_type vocabulary
	// that admits an asset on the declaration basis.
	AnchorClasses []string `json:"anchor_classes"`
	// RecognitionTags is the curated-directory vocabulary that counts
	// as independent recognition of the issuer account.
	RecognitionTags []string `json:"recognition_tags"`
	// ScamFlagTags is the vocabulary that excludes an issuer outright.
	ScamFlagTags []string `json:"scam_flag_tags"`
	// DocumentationURL points at the prose statement of the rule.
	DocumentationURL string `json:"documentation_url"`
}

// RWASummary aggregates the served set.
type RWASummary struct {
	Assets  int `json:"assets"`
	Issuers int `json:"issuers"`
	// MarketCapUSD is the exact sum of the PUBLISHED per-asset market
	// caps, as a decimal string. ABSENT — not "0.00" — when no asset in
	// the set publishes one: a zero there reads as a real total of zero
	// dollars, which is the one reading that is certainly wrong.
	MarketCapUSD *string `json:"market_cap_usd,omitempty"`
	// AssetsValued and AssetsUnvalued split the set by whether it
	// contributed to the total.
	AssetsValued   int `json:"assets_valued"`
	AssetsUnvalued int `json:"assets_unvalued"`
	// LowerBound is true whenever any member asset is unvalued, i.e.
	// whenever the total is less than the value of the set.
	LowerBound bool `json:"lower_bound"`
	// EarliestFirstSeenLedger is the lowest ledger at which any member
	// asset was first observed. The index holds every ledger from
	// genesis, so this is the true first appearance of the set, not the
	// start of a sampling window. 0 (omitted) when no member carried a
	// first-seen ledger.
	EarliestFirstSeenLedger uint32 `json:"earliest_first_seen_ledger,omitempty"`
	// Basis is a one-line statement of what was measured and how it was
	// valued, in the same posture the DEX TVL headline takes.
	Basis string `json:"basis"`
	// Truncated reports that a cap bound the rebuild, so the set is
	// known to be incomplete.
	Truncated bool `json:"truncated,omitempty"`
}

// RWAValuationStatus values. Every row carries exactly one, and a row
// whose status is not published carries no market cap.
const (
	// RWAValuationPublished — a price and a supply were both available
	// and neither gate withheld them.
	RWAValuationPublished = "published"
	// RWAValuationIssuerFlagged — the issuer acquired a scam-class
	// directory tag after the membership set was built. The row stays
	// (this surface hides nothing it admitted) with its valuation
	// withheld by the same suppression /v1/assets applies.
	RWAValuationIssuerFlagged = "withheld_issuer_flagged"
	// RWAValuationUnpriced — no USD price. Either the market never
	// produced one or the substance gate withheld it as too thin to
	// aggregate.
	RWAValuationUnpriced = "unpriced"
	// RWAValuationLowLiquidity — a price exists but the dust guard
	// refused to turn it into a market cap.
	RWAValuationLowLiquidity = "withheld_low_liquidity"
	// RWAValuationNoSupply — a price exists but no circulating-supply
	// reading does, so no market cap can be computed.
	RWAValuationNoSupply = "supply_unavailable"
)

// RWAValuation carries a row valuation and the reason when there is
// none. Both money fields are absent unless Status is published.
type RWAValuation struct {
	Status   string  `json:"status"`
	PriceUSD *string `json:"price_usd,omitempty"`
	// PriceBasis names a price that is NOT a direct market observation,
	// carried through verbatim from the listing row (declared_peg or
	// transitive). Absent means the price came from a market. It is on
	// the wire because a valuation surface that showed the figure and
	// hid how it was derived would be the same claim with the caveat
	// removed.
	PriceBasis   string  `json:"price_basis,omitempty"`
	MarketCapUSD *string `json:"market_cap_usd,omitempty"`
}

// RWAAsset is one member of the set.
type RWAAsset struct {
	AssetID string `json:"asset_id"`
	Code    string `json:"code"`
	Issuer  string `json:"issuer"`
	Slug    string `json:"slug,omitempty"`
	// Name is the [[CURRENCIES]] name from the issuer-bound SEP-1
	// entry. Issuer-authored display text.
	Name string `json:"name,omitempty"`
	// HomeDomain is the domain the issuer account set ON CHAIN, from
	// which the attestation was fetched.
	HomeDomain string `json:"home_domain,omitempty"`
	// IssuerDirectoryName and IssuerDirectoryTags are the independent
	// third-party label on the issuer G-address — the evidence for R3.
	IssuerDirectoryName string   `json:"issuer_directory_name,omitempty"`
	IssuerDirectoryTags []string `json:"issuer_directory_tags,omitempty"`
	// Basis names which requirement-4 arm admitted this asset.
	Basis string `json:"basis"`
	// AnchorClass is the closed-vocabulary class, present only under
	// the declaration basis.
	AnchorClass string `json:"anchor_class,omitempty"`
	// AnchorAsset is the off-chain instrument the issuer declared this
	// token anchors to, verbatim.
	AnchorAsset string `json:"anchor_asset,omitempty"`
	// Valuation is the money, or the reason there is none.
	Valuation RWAValuation `json:"valuation"`
	// CirculatingSupply is a raw chain fact and is served even when the
	// valuation is withheld, in the smallest integer unit.
	CirculatingSupply *string `json:"circulating_supply,omitempty"`
	// Volume24hUSD is the trailing-24h USD trade volume as served on
	// /v1/assets.
	Volume24hUSD *string `json:"volume_24h_usd,omitempty"`
	// FirstSeenLedger is the ledger this asset was first observed at,
	// from an index complete since genesis.
	FirstSeenLedger  uint32 `json:"first_seen_ledger,omitempty"`
	ObservationCount int64  `json:"observation_count"`
}

// RWAGroupTotal is one row of the per-class breakdown.
type RWAGroupTotal struct {
	Class  string `json:"class"`
	Assets int    `json:"assets"`
	// MarketCapUSD is absent when no asset in the group publishes one,
	// for the same reason the summary total is.
	MarketCapUSD   *string `json:"market_cap_usd,omitempty"`
	AssetsUnvalued int     `json:"assets_unvalued"`
}

// RWAIssuerTotal is one row of the per-issuer breakdown.
type RWAIssuerTotal struct {
	Issuer         string  `json:"issuer"`
	Name           string  `json:"name,omitempty"`
	HomeDomain     string  `json:"home_domain,omitempty"`
	Assets         int     `json:"assets"`
	MarketCapUSD   *string `json:"market_cap_usd,omitempty"`
	AssetsUnvalued int     `json:"assets_unvalued"`
}

// RWARefusal counts the candidates one requirement turned away.
type RWARefusal struct {
	Reason string `json:"reason"`
	Assets int    `json:"assets"`
}

// ─── membership ─────────────────────────────────────────────────────

// rwaMember is one admitted (code, issuer) with the evidence that
// admitted it. Built without any valuation input.
type rwaMember struct {
	code        string
	issuer      string
	name        string
	homeDomain  string
	anchorAsset string
	basis       string
	anchorClass string
	dirName     string
	dirTags     []string
}

// rwaMembership is one rebuild: the admitted set plus the refusal
// tally over every candidate considered.
type rwaMembership struct {
	members  []rwaMember
	refusals map[string]int
	// truncated records that rwaMaxIssuers bound the set.
	truncated bool
	// available is false when no attestation reader is wired, which is
	// a configuration statement rather than an empty population.
	available bool
}

// rwaCandidateFilter keeps only the bound SEP-1 entries that could
// possibly satisfy requirement 4, so the scan never materialises the
// tens of thousands of bound entries that declare an NFT, a crypto
// token or nothing at all.
func rwaCandidateFilter(c timescale.Sep1BoundCurrency) bool {
	return rwa.CouldQualify(c.Code, c.AnchorAssetType)
}

// buildRWAMembership applies the definition to every issuer-bound
// SEP-1 attestation and returns the admitted set.
//
// Order of work: attestations first (one indexed scan), then ONE batch
// directory lookup over the candidate issuers (no N+1), then the
// per-candidate verdict. Nothing here reads a price.
func (s *Server) buildRWAMembership(ctx context.Context) rwaMembership {
	out := rwaMembership{refusals: map[string]int{}}
	reader, ok := s.sep1Cache.(Sep1BoundCurrencyReader)
	if !ok {
		return out
	}
	out.available = true

	bound, dropped, err := reader.BoundSep1Currencies(ctx, rwaCandidateFilter)
	if err != nil {
		s.logger.Warn("rwa membership: bound sep1 scan failed", "err", err)
		out.available = false
		return out
	}
	// The pre-filter drops the bound entries that name no real-world
	// instrument at all — the NFT, crypto and undeclared majority. They
	// are refusals under requirement 4 and are counted as such; a
	// refusal tally that reported only what the scan happened to
	// materialise would understate the population it narrowed from.
	if dropped > 0 {
		out.refusals[rwa.RejectNoInstrumentClaim] += dropped
	}

	addrs := make([]string, 0, len(bound))
	seen := make(map[string]struct{}, len(bound))
	for _, c := range bound {
		if _, dup := seen[c.Issuer]; dup {
			continue
		}
		seen[c.Issuer] = struct{}{}
		addrs = append(addrs, c.Issuer)
	}

	var entries map[string]timescale.DirectoryEntry
	if s.directory != nil && len(addrs) > 0 {
		entries, err = s.directory.DirectoryEntriesByAddresses(ctx, addrs)
		if err != nil {
			// Requirement 3 cannot be evaluated without the directory,
			// and it is the requirement that keeps impersonators out.
			// Fail CLOSED here — unlike the display overlay, which fails
			// open because it only omits a label. Serving the set
			// without R3 would publish the lookalike-domain population
			// as real-world assets.
			s.logger.Warn("rwa membership: directory batch lookup failed", "n", len(addrs), "err", err)
			out.available = false
			return out
		}
	}

	issuers := map[string]struct{}{}
	for _, c := range bound {
		e := entries[c.Issuer]
		v := rwa.Qualify(rwa.Candidate{
			Code:               c.Code,
			Issuer:             c.Issuer,
			BoundSep1:          true,
			DeclaredAnchorType: c.AnchorAssetType,
			DirectoryTags:      e.Tags,
		})
		if !v.InSet {
			out.refusals[v.Reject]++
			continue
		}
		if _, known := issuers[c.Issuer]; !known {
			if len(issuers) >= rwaMaxIssuers {
				out.truncated = true
				continue
			}
			issuers[c.Issuer] = struct{}{}
		}
		out.members = append(out.members, rwaMember{
			code:        c.Code,
			issuer:      c.Issuer,
			name:        c.Name,
			homeDomain:  c.HomeDomain,
			anchorAsset: c.AnchorAsset,
			basis:       v.Basis,
			anchorClass: v.AnchorClass,
			dirName:     e.Name,
			dirTags:     e.Tags,
		})
	}
	return out
}

// cachedRWAMembership returns the membership set, rebuilt at most once
// per TTL window and shared across concurrent requests by a
// single-flight gate. The last good set is served on a rebuild error —
// a directory or SEP-1 read that fails must not empty a surface whose
// inputs move on a daily cadence.
func (s *Server) cachedRWAMembership(ctx context.Context) rwaMembership {
	s.rwaMu.Lock()
	if s.rwaCache != nil && time.Since(s.rwaAt) < rwaMembershipTTL {
		m := *s.rwaCache
		s.rwaMu.Unlock()
		return m
	}
	if ch := s.rwaFlight; ch != nil {
		s.rwaMu.Unlock()
		select {
		case <-ch:
			s.rwaMu.Lock()
			var m rwaMembership
			if s.rwaCache != nil {
				m = *s.rwaCache
			}
			s.rwaMu.Unlock()
			return m
		case <-ctx.Done():
			return rwaMembership{refusals: map[string]int{}}
		}
	}
	done := make(chan struct{})
	s.rwaFlight = done
	s.rwaMu.Unlock()

	built := s.buildRWAMembership(ctx)

	s.rwaMu.Lock()
	if built.available {
		s.rwaCache = &built
		s.rwaAt = time.Now()
	} else if s.rwaCache != nil {
		built = *s.rwaCache
	}
	s.rwaFlight = nil
	s.rwaMu.Unlock()
	close(done)
	return built
}

// ─── handler ────────────────────────────────────────────────────────

// handleRWAAssets serves GET /v1/rwa/assets.
//
// No query parameters: the set is small and complete by construction,
// so there is nothing to page and no filter that would not be better
// applied by the caller over a whole document it already holds.
func (s *Server) handleRWAAssets(w http.ResponseWriter, r *http.Request) {
	view := RWAAssetsView{
		Definition: rwaDefinition(),
		Assets:     []RWAAsset{},
		ByClass:    []RWAGroupTotal{},
		ByIssuer:   []RWAIssuerTotal{},
		Refused:    []RWARefusal{},
	}
	if s.assetsReader == nil {
		view.Summary.Basis = rwaBasisUnavailable
		writeEnvelope(w, Envelope{Data: view, Flags: Flags{}})
		return
	}

	m := s.cachedRWAMembership(r.Context())
	view.Refused = rwaRefusalRows(m.refusals)
	if !m.available && len(m.members) == 0 {
		view.Summary.Basis = rwaBasisUnavailable
		writeEnvelope(w, Envelope{Data: view, Flags: Flags{}})
		return
	}

	rows, readErr := s.rwaListingRows(r.Context(), m)
	if readErr != nil {
		if clientAborted(r, readErr) {
			return
		}
		s.logger.Error("rwa listing read failed", "err", readErr)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}

	view.Assets = s.rwaAssetRows(m, rows)
	view.Summary = rwaSummarise(view.Assets, m.truncated)
	view.ByClass = rwaByClass(view.Assets)
	view.ByIssuer = rwaByIssuer(view.Assets)
	writeEnvelope(w, Envelope{Data: view, Flags: Flags{}})
}

// rwaBasisUnavailable is the summary basis when the attestation or
// directory reads that decide membership are not answering. It states
// the absence rather than serving an empty set as a finding.
const rwaBasisUnavailable = "Membership could not be established: the issuer-bound SEP-1 attestations or the curated account directory did not answer. No set is published rather than an unverified one."

func rwaDefinition() RWADefinition {
	return RWADefinition{
		Requirements: []string{
			"classic asset identified by (code, issuer)",
			"issuer-bound SEP-1 [[CURRENCIES]] entry served from the on-chain home_domain",
			"issuer independently recognised in the curated account directory and not scam-flagged",
			"real-world instrument by SEP-1 anchor_asset_type or by an ADR-0028 oracle feed",
		},
		AnchorClasses:    rwa.AnchorClasses(),
		RecognitionTags:  rwa.RecognitionTags(),
		ScamFlagTags:     append([]string(nil), timescale.DirectoryScamFlagTags...),
		DocumentationURL: "https://stellarindex.io/docs/methodology/rwa-definition",
	}
}

// rwaListingRows reads the member issuers through the SAME listing
// query /v1/assets uses, one indexed page per issuer, and returns the
// projected rows keyed by asset_id. Running the real listing path is
// what guarantees this surface cannot publish a valuation /v1/assets
// would have refused.
func (s *Server) rwaListingRows(ctx context.Context, m rwaMembership) (map[string]AssetDetail, error) {
	issuers := make([]string, 0, rwaMaxIssuers)
	seen := map[string]struct{}{}
	for _, mem := range m.members {
		if _, dup := seen[mem.issuer]; dup {
			continue
		}
		seen[mem.issuer] = struct{}{}
		issuers = append(issuers, mem.issuer)
	}
	sort.Strings(issuers)

	wanted := make(map[string]struct{}, len(m.members))
	for _, mem := range m.members {
		wanted[rwaKey(mem.code, mem.issuer)] = struct{}{}
	}

	out := make(map[string]AssetDetail, len(m.members))
	for _, issuer := range issuers {
		rows, err := s.assetsReader.ListAssetsExt(ctx, timescale.ListAssetsOptions{
			Limit:  rwaAssetsPerIssuer,
			Issuer: issuer,
			Type:   "classic",
		})
		if err != nil {
			return nil, err
		}
		keep := make([]timescale.AssetRow, 0, 8)
		for _, row := range rows {
			if _, ok := wanted[rwaKey(row.Code, row.IssuerGStrkey)]; ok {
				keep = append(keep, row)
			}
		}
		if len(keep) == 0 {
			continue
		}
		details := make([]AssetDetail, 0, len(keep))
		for _, row := range keep {
			details = append(details, assetDetailFromAssetRow(row))
		}
		// The /v1/assets post-query pipeline, in its order. Every step
		// is a price producer or a gate, and the order between them is
		// load-bearing there for reasons that apply identically here:
		// the collision stamp must precede the market-cap fill (which
		// reads it), the peg fill must follow both the gate that would
		// strip it and the fill that must not derive a valuation from
		// it, and the directory stamp brings the scam suppression.
		//
		// Running the whole pipeline rather than a chosen subset is the
		// point. A surface that ran only the steps it believed it
		// needed would drift from /v1/assets one omission at a time,
		// and each omission would show up as this page publishing a
		// figure that page withholds, or withholding one it publishes.
		s.stampListingCollisions(details)
		s.applySubstanceGateToListing(ctx, details)
		s.fillMarketCapsFromSupply(ctx, details, assetRowSourceCounts(keep))
		s.fillDeclaredPegPricesInListing(ctx, details)
		s.fillIssuerDirectoryTags(ctx, details)
		for _, d := range details {
			out[rwaKey(d.Code, issuer)] = d
		}
	}
	return out, nil
}

// rwaKey is the membership join key: the code case-folded (the SEP-1
// overlay matches codes case-insensitively, and so must the join that
// reads its output) and the issuer G-address exact.
func rwaKey(code, issuer string) string {
	return strings.ToUpper(strings.TrimSpace(code)) + "-" + issuer
}

// rwaAssetRows joins the membership evidence to the valued listing rows
// and orders the result. A member with no listing row is dropped: the
// asset was attested but has never been observed on chain, and this
// surface reports what the index holds.
func (s *Server) rwaAssetRows(m rwaMembership, rows map[string]AssetDetail) []RWAAsset {
	out := make([]RWAAsset, 0, len(m.members))
	for _, mem := range m.members {
		d, ok := rows[rwaKey(mem.code, mem.issuer)]
		if !ok {
			continue
		}
		a := RWAAsset{
			AssetID:             d.AssetID,
			Code:                d.Code,
			Issuer:              mem.issuer,
			Slug:                d.Slug,
			Name:                strings.TrimSpace(mem.name),
			HomeDomain:          mem.homeDomain,
			IssuerDirectoryName: mem.dirName,
			IssuerDirectoryTags: d.IssuerDirectoryTags,
			Basis:               mem.basis,
			AnchorClass:         mem.anchorClass,
			AnchorAsset:         strings.TrimSpace(mem.anchorAsset),
			Valuation:           rwaValuationOf(d),
			CirculatingSupply:   d.CirculatingSupply,
			Volume24hUSD:        d.VolumeUSD24h,
		}
		if len(a.IssuerDirectoryTags) == 0 {
			a.IssuerDirectoryTags = mem.dirTags
		}
		if d.FirstSeenLedger != nil {
			a.FirstSeenLedger = *d.FirstSeenLedger
		}
		if d.ObservationCount != nil {
			a.ObservationCount = *d.ObservationCount
		}
		out = append(out, a)
	}
	sort.SliceStable(out, func(i, j int) bool {
		li, lj := ratFromOptionalString(out[i].Valuation.MarketCapUSD), ratFromOptionalString(out[j].Valuation.MarketCapUSD)
		switch {
		case li != nil && lj == nil:
			return true
		case li == nil && lj != nil:
			return false
		case li != nil && lj != nil && li.Cmp(lj) != 0:
			return li.Cmp(lj) > 0
		}
		if out[i].ObservationCount != out[j].ObservationCount {
			return out[i].ObservationCount > out[j].ObservationCount
		}
		return out[i].AssetID < out[j].AssetID
	})
	return out
}

// rwaValuationOf reads the valuation OFF the already-gated listing row.
// It never recomputes a price or a market cap: every withholding
// decision was made upstream by the gates, and this function only
// reports which one applied. That is why a withheld figure cannot
// reappear here as a number.
func rwaValuationOf(d AssetDetail) RWAValuation {
	if pricingguard.IsDirectoryScamFlagged(d.IssuerDirectoryTags) {
		return RWAValuation{Status: RWAValuationIssuerFlagged}
	}
	if d.PriceUSD == nil {
		return RWAValuation{Status: RWAValuationUnpriced}
	}
	if d.MarketCapUSD == nil {
		if d.MarketCapLowLiquidity {
			return RWAValuation{Status: RWAValuationLowLiquidity, PriceUSD: d.PriceUSD, PriceBasis: d.PriceBasis}
		}
		return RWAValuation{Status: RWAValuationNoSupply, PriceUSD: d.PriceUSD, PriceBasis: d.PriceBasis}
	}
	return RWAValuation{
		Status:       RWAValuationPublished,
		PriceUSD:     d.PriceUSD,
		PriceBasis:   d.PriceBasis,
		MarketCapUSD: d.MarketCapUSD,
	}
}

// ─── aggregates ─────────────────────────────────────────────────────

// rwaSumMarketCaps returns the exact sum of the published market caps
// and how many rows contributed. Exact big.Rat arithmetic over
// already-rounded 2-dp per-asset figures (ADR-0003), so each level is
// the exact sum of the level below. Returns nil when nothing was
// published.
func rwaSumMarketCaps(assets []RWAAsset) (*string, int) {
	sum := new(big.Rat)
	valued := 0
	for _, a := range assets {
		r := ratFromOptionalString(a.Valuation.MarketCapUSD)
		if r == nil {
			continue
		}
		sum.Add(sum, r)
		valued++
	}
	if valued == 0 {
		return nil, 0
	}
	s := sum.FloatString(2)
	return &s, valued
}

func rwaSummarise(assets []RWAAsset, truncated bool) RWASummary {
	total, valued := rwaSumMarketCaps(assets)
	issuers := map[string]struct{}{}
	var earliest uint32
	for _, a := range assets {
		issuers[a.Issuer] = struct{}{}
		if a.FirstSeenLedger != 0 && (earliest == 0 || a.FirstSeenLedger < earliest) {
			earliest = a.FirstSeenLedger
		}
	}
	return RWASummary{
		Assets:                  len(assets),
		Issuers:                 len(issuers),
		MarketCapUSD:            total,
		AssetsValued:            valued,
		AssetsUnvalued:          len(assets) - valued,
		LowerBound:              len(assets)-valued > 0,
		EarliestFirstSeenLedger: earliest,
		Basis:                   rwaBasis(len(assets), valued, truncated),
		Truncated:               truncated,
	}
}

// rwaBasis states what the total measured, in prose, so a reader of the
// figure gets its scope without having to reconstruct it from counts.
func rwaBasis(total, valued int, truncated bool) string {
	var b strings.Builder
	b.WriteString("Sum of the published market caps of the assets meeting the four-requirement definition. ")
	b.WriteString("Market cap is circulating supply times the served USD price, both as /v1/assets serves them, ")
	b.WriteString("under the same substance, dust-liquidity and scam-issuer gates. ")
	switch {
	case total == 0:
		b.WriteString("No asset currently meets the definition.")
	case valued == total:
		b.WriteString("Every asset in the set publishes a valuation.")
	default:
		b.WriteString("Assets whose valuation is withheld or unavailable contribute nothing and are counted separately, so the total is a LOWER BOUND on the value of the set.")
	}
	if truncated {
		b.WriteString(" The issuer cap bound this rebuild, so the set is known to be incomplete.")
	}
	return b.String()
}

// rwaUnclassified groups the assets admitted on the oracle basis, which
// declare no anchor class. Naming the group is honest; inventing a
// class for it would not be.
const rwaUnclassified = "unclassified"

func rwaByClass(assets []RWAAsset) []RWAGroupTotal {
	byClass := map[string][]RWAAsset{}
	for _, a := range assets {
		c := a.AnchorClass
		if c == "" {
			c = rwaUnclassified
		}
		byClass[c] = append(byClass[c], a)
	}
	out := make([]RWAGroupTotal, 0, len(byClass))
	for c, group := range byClass {
		total, valued := rwaSumMarketCaps(group)
		out = append(out, RWAGroupTotal{
			Class:          c,
			Assets:         len(group),
			MarketCapUSD:   total,
			AssetsUnvalued: len(group) - valued,
		})
	}
	sortRWAGroups(out)
	return out
}

func sortRWAGroups(out []RWAGroupTotal) {
	sort.SliceStable(out, func(i, j int) bool {
		li, lj := ratFromOptionalString(out[i].MarketCapUSD), ratFromOptionalString(out[j].MarketCapUSD)
		switch {
		case li != nil && lj == nil:
			return true
		case li == nil && lj != nil:
			return false
		case li != nil && lj != nil && li.Cmp(lj) != 0:
			return li.Cmp(lj) > 0
		}
		return out[i].Class < out[j].Class
	})
}

func rwaByIssuer(assets []RWAAsset) []RWAIssuerTotal {
	order := make([]string, 0, 8)
	byIssuer := map[string][]RWAAsset{}
	for _, a := range assets {
		if _, ok := byIssuer[a.Issuer]; !ok {
			order = append(order, a.Issuer)
		}
		byIssuer[a.Issuer] = append(byIssuer[a.Issuer], a)
	}
	out := make([]RWAIssuerTotal, 0, len(order))
	for _, issuer := range order {
		group := byIssuer[issuer]
		total, valued := rwaSumMarketCaps(group)
		out = append(out, RWAIssuerTotal{
			Issuer:         issuer,
			Name:           group[0].IssuerDirectoryName,
			HomeDomain:     group[0].HomeDomain,
			Assets:         len(group),
			MarketCapUSD:   total,
			AssetsUnvalued: len(group) - valued,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		li, lj := ratFromOptionalString(out[i].MarketCapUSD), ratFromOptionalString(out[j].MarketCapUSD)
		switch {
		case li != nil && lj == nil:
			return true
		case li == nil && lj != nil:
			return false
		case li != nil && lj != nil && li.Cmp(lj) != 0:
			return li.Cmp(lj) > 0
		}
		return out[i].Issuer < out[j].Issuer
	})
	return out
}

func rwaRefusalRows(refusals map[string]int) []RWARefusal {
	out := make([]RWARefusal, 0, len(refusals))
	for reason, n := range refusals {
		out = append(out, RWARefusal{Reason: reason, Assets: n})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Assets != out[j].Assets {
			return out[i].Assets > out[j].Assets
		}
		return out[i].Reason < out[j].Reason
	})
	return out
}
