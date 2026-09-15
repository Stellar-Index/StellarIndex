package v1

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/rwa"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// The contract arm of GET /v1/rwa/assets — membership, valuation and the
// coverage signal, for tokens that have no (code, issuer) pair.
//
// The definition itself is internal/rwa/contract.go, where the argument
// for what replaces the issuer-bound SEP-1 attestation lives. This file
// is the read path: where the candidate population comes from, how a
// contract row is valued, and what the response says about the entities
// it still cannot reach.
//
// # Why this arm exists at all
//
// The classic arm walks issuers, and that table is populated from ONE
// call site — registerIssuerSeen, on classic-asset registration. An
// entity issuing only contract tokens gets no row there, so it gets no
// SEP-1 fetch and never becomes a candidate. Measured on r1 2026-09-10,
// `sep1-refresh -issuer` for Franklin Templeton and for Spiko both
// return `sql: no rows in result set`, while both accounts sit in the
// curated directory tagged `issuer` with their real domains.
//
// That is not a requirement refusing them. It is a population they were
// never in, which is the silent-discard shape the funnel exists to make
// impossible. So this arm draws its population from the one table that
// records the tie between a real-world entity and a Stellar address
// without passing through classic issuance.
//
// # Two things this arm reports that nothing else could
//
//  1. Contracts an independent party named, evaluated and counted, with
//     each refusal attributed.
//  2. Entities that are recognised, unflagged, real — and for which this
//     index holds no Stellar token at all. Those are not refused by any
//     requirement; there is nothing to refuse. Before this they were
//     invisible, and a reader could not tell such an entity from one
//     that does not exist. They are named on the response.

// rwaContractScanCap bounds the contracts one rebuild evaluates. The
// recognised-contract set is small by construction — a third party has
// to have named the exact address with an issuing tag — so this guards
// against a directory sync that suddenly tags thousands of contracts,
// not an expected condition. When it binds the funnel says so.
const rwaContractScanCap = 256

// rwaUnreachedSampleCap bounds how many recognised-but-tokenless
// entities are NAMED on the response. The count is exact and always
// served; the names are a bounded sample, ordered by address so the same
// rebuild always names the same entities. A page that dumped several
// hundred names would bury the number that matters.
const rwaUnreachedSampleCap = 50

// rwaContractMetadataBudget bounds the per-contract metadata reads a
// rebuild will make (symbol, decimals, supply). It is a whole-phase
// budget rather than a per-read timeout: the rebuild runs off the
// request path behind the membership TTL, but an unbounded fan-out over
// a suddenly-large candidate set would still hold the single-flight gate
// open for every concurrent reader.
const rwaContractMetadataBudget = 20 * time.Second

// ─── storage seams ──────────────────────────────────────────────────

// RWADirectoryContractReader is the seam the contract arm draws its
// candidate population through. *timescale.Store satisfies it.
//
// Optional, like every other reader on this surface: a deployment
// without it serves the classic arm alone and says so in the funnel,
// rather than erroring. The distinction matters — an absent reader is a
// configuration statement, and a funnel that reported it as a measured
// population of zero contracts would be asserting something about the
// network that nobody looked at.
type RWADirectoryContractReader interface {
	DirectoryRecognisedContracts(ctx context.Context, issuingTags []string) (
		[]timescale.DirectoryEntry, timescale.DirectoryRWACensus, error)
	DirectoryRecognisedIssuersWithoutAsset(ctx context.Context, issuingTags []string, limit int) (
		[]timescale.DirectoryEntry, error)
}

// ContractCatalogueReader reads catalogue rows for an explicit contract
// set — the volume-gate-free read the RWA surface needs and the listing
// spine cannot express. *timescale.Store satisfies it.
type ContractCatalogueReader interface {
	ContractCatalogueRows(ctx context.Context, contractIDs []string) (map[string]timescale.AssetRow, error)
}

// TokenSymbolReader resolves a token contract's on-chain SEP-41 symbol.
// The lake ExplorerReader satisfies it.
//
// Read ONLY after an independent party has named the contract. The value
// is contract-authored and answers which instrument, never whether — see
// the reader's own doc comment for why that ordering is load-bearing.
type TokenSymbolReader interface {
	TokenSymbol(ctx context.Context, contractID string) (string, bool, error)
}

// ─── membership ─────────────────────────────────────────────────────

// rwaContractMember is one admitted contract with the evidence that
// admitted it. Built without any valuation input, exactly as
// [rwaMember] is.
type rwaContractMember struct {
	contractID string
	symbol     string
	basis      string
	class      string
	dirName    string
	dirDomain  string
	dirTags    []string
	// recognition names WHICH independent party's naming satisfied C2.
	// Served on the row because the two arms are not the same strength
	// of evidence — see [rwa.Verdict].Recognition.
	recognition string
	// listing is the independent listing row that corroborated this
	// address, when one did. It carries the price this arm values the
	// row at; an empty ListingID means no listing named the address,
	// which is the normal case for a directory-recognised member.
	listing timescale.ListingEntry
}

// rwaUnreachedEntity is a curated-directory entity recognised as an
// issuing party for which this index holds no Stellar token.
type rwaUnreachedEntity struct {
	address string
	name    string
	domain  string
	tags    []string
}

// rwaContractCensus is the contract arm's accounting: the directory
// census the storage layer measured, plus what this layer then did with
// the candidates.
type rwaContractCensus struct {
	dir timescale.DirectoryRWACensus
	// evaluated is the number of recognised contracts actually put to
	// the definition — fewer than dir.ContractsRecognised when a cap
	// bound the scan.
	evaluated int
	// overCap is the difference, reported so the funnel closes rather
	// than losing candidates between two counts.
	overCap int
	// duplicates counts a contract named twice by the directory. The
	// address is the primary key upstream so this should be zero; it is
	// counted rather than assumed, because "should be zero" is how a
	// double-counted market cap gets shipped.
	duplicates int
	// available is false when no directory contract reader is wired.
	available bool
}

// buildRWAContractMembership applies the contract requirements to every
// recognised contract the curated directory names.
//
// Order of work: the directory census and candidate rows (one scan, one
// count), then ONE metadata read per candidate under a shared budget,
// then the per-candidate verdict. Nothing here reads a price — the
// contract arm decides membership before valuation for exactly the same
// reason the classic arm does.
func (s *Server) buildRWAContractMembership(ctx context.Context) rwaContractBuild {
	var out rwaContractBuild
	out.refusals = map[string]int{}
	out.listingRefusals = map[string]int{}

	// ONE listing read per rebuild, shared by both arms. The first arm
	// reads it only to record which of its members a second party also
	// names; the second arm's whole recognition rests on it.
	listing := s.rwaListingSnapshot(ctx)

	if s.rwaContracts == nil {
		// The directory arm is unwired, which says nothing about the
		// listing arm — they draw on different readers. The second arm
		// still runs, over a population that is a table in this
		// repository rather than anything the directory supplies.
		out.members, out.listingCensus = s.buildRWAListingMembership(
			ctx, listing, map[string]struct{}{}, out.listingRefusals)
		return out
	}
	census, members, evaluated := s.buildRWADirectoryMembership(ctx, listing, out.refusals)
	out.census = census
	out.members = members
	out.unreached = s.rwaUnreachedEntities(ctx, rwa.ContractRecognitionTags())

	listingMembers, listingCensus := s.buildRWAListingMembership(
		ctx, listing, evaluated, out.listingRefusals)
	out.members = append(out.members, listingMembers...)
	out.listingCensus = listingCensus
	return out
}

// rwaContractBuild is one contract-side rebuild: the admitted set from
// BOTH C2 arms, and each arm's own census and refusal tally.
//
// The refusal tallies are kept APART rather than merged here. Both arms
// run the same rwa.QualifyContract and can therefore produce the same
// reason string, so one tally would make the funnel attribute an arm-2
// refusal to arm 1's narrowing and stop the stage arithmetic closing.
// They are merged once, at the very top, into the single `refused[]`
// the response publishes — which is a statement about the whole
// surface and correctly does not care which arm turned a candidate away.
type rwaContractBuild struct {
	members         []rwaContractMember
	unreached       []rwaUnreachedEntity
	census          rwaContractCensus
	listingCensus   rwaListingCensus
	refusals        map[string]int
	listingRefusals map[string]int
}

// buildRWADirectoryMembership is C2's first arm: the contract addresses
// the curated account directory names with an issuing tag.
//
// Returns the set of addresses it EVALUATED alongside its members, so
// the second arm can remove them from its own population. Evaluated, not
// admitted: an address this arm looked at and refused must still not be
// re-evaluated by the other arm under a different rule, or the funnel
// would report one candidate twice and the refusal that turned it away
// would be contradicted by an admission.
func (s *Server) buildRWADirectoryMembership(
	ctx context.Context, listing rwaListing, refusals map[string]int,
) (rwaContractCensus, []rwaContractMember, map[string]struct{}) {
	var census rwaContractCensus
	tags := rwa.ContractRecognitionTags()
	entries, dirCensus, err := s.rwaContracts.DirectoryRecognisedContracts(ctx, tags)
	if err != nil {
		// Fail CLOSED for this arm, the same way the classic arm fails
		// closed when the directory cannot answer. The directory IS the
		// provenance requirement here — there is no second attestation to
		// fall back on — so serving contract rows without it would
		// publish exactly the unvouched-for population the definition
		// refuses.
		s.logger.Warn("rwa contract membership: directory scan failed", "err", err)
		return census, nil, map[string]struct{}{}
	}
	census.available = true
	census.dir = dirCensus

	if len(entries) > rwaContractScanCap {
		census.overCap = len(entries) - rwaContractScanCap
		entries = entries[:rwaContractScanCap]
	}
	// The storage cap can bind before this one does. Both differences
	// land in the same bucket: a candidate the scan did not evaluate,
	// whoever stopped it.
	census.overCap += max(dirCensus.ContractsRecognised-len(entries)-census.overCap, 0)
	census.evaluated = len(entries)

	mctx, cancel := context.WithTimeout(ctx, rwaContractMetadataBudget)
	defer cancel()

	members := make([]rwaContractMember, 0, len(entries))
	admitted := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		if _, dup := admitted[e.Address]; dup {
			census.duplicates++
			continue
		}
		admitted[e.Address] = struct{}{}
		// The symbol read happens AFTER the directory named the address
		// and BEFORE the verdict, because C4 is the only requirement that
		// reads it. A contract the directory did not name never reaches
		// this line, so its self-declared metadata is never consulted at
		// all.
		symbol := s.rwaContractSymbol(mctx, e.Address)
		v := rwa.QualifyContract(rwa.ContractCandidate{
			ContractID:     e.Address,
			DirectoryNamed: true,
			DirectoryTags:  e.Tags,
			Symbol:         symbol,
			// Passed even though this arm does not need it. A member
			// the listing ALSO names is a stronger row than one only
			// the directory names, and the verdict is where that is
			// decided rather than here — this layer supplies inputs
			// and never reasons about which requirement uses them.
			ListingNamed:     listing.names(e.Address),
			ListingAvailable: listing.available,
		})
		if !v.InSet {
			refusals[v.Reject]++
			continue
		}
		members = append(members, rwaContractMember{
			contractID:  e.Address,
			symbol:      symbol,
			basis:       v.Basis,
			class:       v.AnchorClass,
			dirName:     e.Name,
			dirDomain:   e.Domain,
			dirTags:     e.Tags,
			recognition: v.Recognition,
			listing:     listing.byAddress[e.Address],
		})
	}

	return census, members, admitted
}

// buildRWAListingMembership is C2's second arm: every in-repo curated
// binding the first arm is not already evaluating, put to the
// definition with an independent listing as its recognition.
//
// Structurally smaller than the first arm, and for a reason worth
// stating: its population is a hand-reviewed table in this repository,
// so it needs no scan cap, no duplicate guard and no truncation
// accounting. It cannot grow without a code change, and
// [rwa.ContractInstrumentBindings] returns it deduplicated and ordered.
func (s *Server) buildRWAListingMembership(
	ctx context.Context, listing rwaListing,
	directoryEvaluated map[string]struct{}, refusals map[string]int,
) ([]rwaContractMember, rwaListingCensus) {
	candidates, census := s.rwaListingCandidates(ctx, listing, directoryEvaluated)
	if !census.available {
		return nil, census
	}
	// The refusals the candidate build already decided, folded into the
	// tally under the SAME reason strings rwa.QualifyContract would
	// have produced. They are decided upstream because they are
	// properties of the population rather than of a candidate — but a
	// reader of `refused[]` must not have to know that, and the funnel
	// reconciles against one vocabulary.
	refusals[rwa.RejectContractListingUnavailable] += census.listingUnavailable
	refusals[rwa.RejectContractCuratedNotListed] += census.notListed
	if len(candidates) == 0 {
		return nil, census
	}

	mctx, cancel := context.WithTimeout(ctx, rwaContractMetadataBudget)
	defer cancel()

	members := make([]rwaContractMember, 0, len(candidates))
	for _, c := range candidates {
		symbol := s.rwaContractSymbol(mctx, c.contractID)
		v := rwa.QualifyContract(rwa.ContractCandidate{
			ContractID: c.contractID,
			// The directory does not name this address as an issuer —
			// that is the whole reason the candidate is on this arm.
			// Its TAGS are still supplied, because C3 reads them and a
			// scam flag beats recognition from any source.
			DirectoryNamed:   false,
			DirectoryTags:    c.dirTags,
			Symbol:           symbol,
			ListingNamed:     true,
			ListingAvailable: true,
		})
		if !v.InSet {
			refusals[v.Reject]++
			continue
		}
		entry := listing.byAddress[c.contractID]
		members = append(members, rwaContractMember{
			contractID:  c.contractID,
			symbol:      symbol,
			basis:       v.Basis,
			class:       v.AnchorClass,
			recognition: v.Recognition,
			listing:     entry,
			// No curated-directory label: the directory does not name
			// this address. The row's name comes from the curated
			// binding's instrument, filled at projection, so the
			// surface never presents a listing platform's display text
			// as an identity attestation.
			dirTags: c.dirTags,
		})
	}
	return members, census
}

// rwaContractSymbol reads one contract's on-chain symbol, best-effort.
// An unavailable reader or a token with no usable metadata yields "",
// which fails C4's oracle arm — never an error, and never a guess.
func (s *Server) rwaContractSymbol(ctx context.Context, contractID string) string {
	if s.tokenSymbol == nil {
		return ""
	}
	sym, ok, err := s.tokenSymbol.TokenSymbol(ctx, contractID)
	if err != nil || !ok {
		return ""
	}
	return sym
}

// rwaUnreachedEntities names the recognised issuing entities this index
// holds no token for. Best-effort: a failed read costs the names, never
// the response, and the exact count still comes from the census.
func (s *Server) rwaUnreachedEntities(ctx context.Context, tags []string) []rwaUnreachedEntity {
	rows, err := s.rwaContracts.DirectoryRecognisedIssuersWithoutAsset(ctx, tags, rwaUnreachedSampleCap)
	if err != nil {
		s.logger.Warn("rwa contract membership: unreached entity scan failed", "err", err)
		return nil
	}
	out := make([]rwaUnreachedEntity, 0, len(rows))
	for _, e := range rows {
		out = append(out, rwaUnreachedEntity{
			address: e.Address, name: e.Name, domain: e.Domain, tags: e.Tags,
		})
	}
	return out
}

// ─── valuation ──────────────────────────────────────────────────────

// rwaContractListingRows reads the admitted contracts through the same
// /v1/assets post-query pipeline the classic arm runs, then fills the
// two things that pipeline structurally cannot fill for a contract.
//
// # What the shared pipeline does and does not do here
//
// Every step runs, in the listing's order, for the same reason
// [Server.rwaListingRows] runs them: a surface that ran only the steps
// it believed it needed would drift from /v1/assets one omission at a
// time. Three of them are no-ops on a contract row and that is fine —
// running a no-op costs nothing and keeps the sequence provably equal to
// the listing's (TestRWAContractPipelineMatchesTheAssetsListing).
//
// The substance gate is NOT a no-op: it explicitly covers
// canonical.AssetSoroban, so a contract token whose price came from too
// thin a market has that price withheld here exactly as it would be on
// /v1/assets.
//
// # The two fills the pipeline cannot do
//
//  1. SUPPLY AND MARKET CAP. fillMarketCapsFromSupply reads two maps,
//     both keyed by classic identity — the supply_1d rollup and a
//     trustline-balance sum. A SEP-41 contract has no trustlines, so
//     both miss and every contract row would be `supply_unavailable`
//     forever. The supply for these tokens lives in the certified lake
//     (stellar.supply_flows), which is where /v1/assets/{id} and
//     /v1/assets/{id}/supply already read it from. This reuses that
//     reader rather than adding a second supply path.
//  2. THE SCAM SUPPRESSION. fillIssuerDirectoryTags keys on the issuer
//     G-address and skips every row without one, so a contract asset has
//     never been subject to the directory scam gate on any surface. On
//     this surface the directory is the PROVENANCE requirement, so
//     leaving its flag unread would mean admitting a contract on a
//     directory entry and then declining to read the same entry when it
//     turns hostile. The tags are re-read at valuation time, not reused
//     from the membership build, so a flag acquired inside the ten
//     minute membership TTL still suppresses.
func (s *Server) rwaContractListingRows(
	ctx context.Context, members []rwaContractMember,
) (map[string]AssetDetail, int, error) {
	if len(members) == 0 || s.contractCatalogue == nil {
		return map[string]AssetDetail{}, 0, nil
	}
	ids := make([]string, 0, len(members))
	for _, m := range members {
		ids = append(ids, m.contractID)
	}
	sort.Strings(ids)

	rows, err := s.contractCatalogue.ContractCatalogueRows(ctx, ids)
	if err != nil {
		return nil, 0, err
	}

	details := make([]AssetDetail, 0, len(rows))
	for _, id := range ids {
		row, ok := rows[id]
		if !ok {
			continue
		}
		details = append(details, assetDetailFromAssetRow(row))
	}
	notObserved := len(ids) - len(details)
	if len(details) == 0 {
		return map[string]AssetDetail{}, notObserved, nil
	}

	// The /v1/assets post-query pipeline, in its order — see
	// rwaListingRows for why the whole sequence runs rather than a
	// chosen subset.
	s.stampListingCollisions(details)
	s.applySubstanceGateToListing(ctx, details)
	s.fillMarketCapsFromSupply(ctx, details, map[string]int{})
	s.fillDeclaredPegPricesInListing(ctx, details)
	s.fillIssuerDirectoryTags(ctx, details)

	// Contract-specific, in an order that is load-bearing: decimals
	// BEFORE the market cap that divides by them (the same ordering
	// contract handleAssetGet states between applyTokenDecimals and
	// applyF2Fields), and the directory suppression LAST so it can empty
	// a figure the fill just produced rather than racing it.
	mctx, cancel := context.WithTimeout(ctx, rwaContractMetadataBudget)
	defer cancel()
	s.fillContractDecimals(mctx, details, rows)
	s.fillContractMarketCaps(mctx, details, rows)
	s.fillContractDirectoryTags(ctx, details)

	out := make(map[string]AssetDetail, len(details))
	for _, d := range details {
		out[d.AssetID] = d
	}
	return out, notObserved, nil
}

// fillContractDecimals overlays each contract's real on-chain decimals.
//
// assetDetailFromAssetRow hardcodes 7 for every row, which is right for
// classic assets and for SACs and wrong for any SEP-41 token that
// declares otherwise. It matters here more than anywhere else on the
// surface: market cap divides supply by 10^decimals, so a 6-decimal
// token valued at 7 publishes a tenth of its real capitalisation and a
// 18-decimal one publishes a hundred billion times it. A wrong decimals
// reading is not a display defect on this page, it is the number.
//
// A token with no readable metadata keeps the default, which is the same
// thing /v1/assets/{id} does.
func (s *Server) fillContractDecimals(ctx context.Context, rows []AssetDetail, src map[string]timescale.AssetRow) {
	if s.tokenDecimals == nil {
		return
	}
	for i := range rows {
		if _, ok := src[rows[i].AssetID]; !ok {
			continue
		}
		d, ok, err := s.tokenDecimals.TokenDecimals(ctx, rows[i].AssetID)
		if err != nil || !ok {
			continue
		}
		rows[i].Decimals = int(d)
	}
}

// fillContractMarketCaps fills circulating supply and market cap for
// contract rows from the certified lake.
//
// Supply is the lake sum of mints less burns and clawbacks, in the
// token's smallest unit — the same figure GET /v1/assets/{id}/supply
// serves and the same reader. An INCOMPLETE reading (the net is
// negative, meaning the flows are incompletely seeded rather than that
// supply is negative) is treated as UNAVAILABLE and never clamped to
// zero: a clamped zero reads as a fully-burned token and understates it,
// which is the refusal SEP41Computer.Compute already makes and
// /v1/assets/{id} already honours.
//
// The dust-liquidity guard runs here on the same inputs the listing
// applies it to. A single-venue price under the volume floor suppresses
// the CAP and leaves the price and the supply — both of which are facts
// the surface is willing to state — exactly as fillRowMarketCap does.
func (s *Server) fillContractMarketCaps(ctx context.Context, rows []AssetDetail, src map[string]timescale.AssetRow) {
	if s.tokenSupply == nil {
		return
	}
	for i := range rows {
		row := &rows[i]
		sup, err := s.tokenSupply.TokenSupply(ctx, row.AssetID)
		if err != nil || sup.Total == nil || sup.Incomplete {
			continue
		}
		circ := sup.Total.String()
		// Circulating supply is a raw chain fact, not a valuation, so it
		// is served even when no cap can be. Same split the listing and
		// the detail page both make.
		row.CirculatingSupply = &circ
		if row.PriceUSD == nil {
			continue
		}
		var sources int
		if sc := src[row.AssetID].SourceCount; sc != nil {
			sources = *sc
		}
		if dustLiquiditySuppressed(sources, row.VolumeUSD24h, s.minMarketCapVolumeUSD) {
			row.MarketCapLowLiquidity = true
			continue
		}
		if mc := computeMarketCapUSD(circ, *row.PriceUSD, row.Decimals); mc != "" {
			row.MarketCapUSD = &mc
		}
	}
}

// fillContractDirectoryTags stamps the curated label from the CONTRACT
// address and applies the scam suppression to it.
//
// The classic twin keys on the issuer G-address and skips every row
// without one. That has always meant a contract asset escapes the
// directory scam gate entirely, on every surface — a gap that costs
// nothing on a page that publishes no valuation for contracts and costs
// everything on one that does.
func (s *Server) fillContractDirectoryTags(ctx context.Context, rows []AssetDetail) {
	if s.directory == nil || len(rows) == 0 {
		return
	}
	addrs := make([]string, 0, len(rows))
	for i := range rows {
		addrs = append(addrs, rows[i].AssetID)
	}
	found, err := s.directory.DirectoryEntriesByAddresses(ctx, addrs)
	if err != nil {
		s.logger.Warn("rwa contract directory batch lookup failed", "n", len(addrs), "err", err)
		return
	}
	for i := range rows {
		e, ok := found[rows[i].AssetID]
		if !ok {
			continue
		}
		stampIssuerDirectory(&rows[i], e)
		suppressScamIssuerPricing(&rows[i])
	}
}

// ─── projection ─────────────────────────────────────────────────────

// rwaContractAssetRows joins the membership evidence to the valued rows
// and returns the wire rows. Ordering is applied once over the combined
// set by the caller, so a contract row and a classic row are ranked by
// the same rule.
func rwaContractAssetRows(members []rwaContractMember, rows map[string]AssetDetail) []RWAAsset {
	out := make([]RWAAsset, 0, len(members))
	for _, m := range members {
		d, ok := rows[m.contractID]
		if !ok {
			continue
		}
		a := RWAAsset{
			AssetID:    d.AssetID,
			ContractID: m.contractID,
			// Code and Issuer stay empty. A contract token has neither,
			// and filling Code from the on-chain symbol would put a
			// self-declared ticker in the field the whole definition
			// refuses to identify anything by. The symbol is served under
			// its own name so a reader can see it is metadata, not
			// identity.
			Symbol:              m.symbol,
			Slug:                d.Slug,
			Name:                rwaContractName(m),
			HomeDomain:          m.dirDomain,
			IssuerDirectoryName: m.dirName,
			IssuerDirectoryTags: d.IssuerDirectoryTags,
			Basis:               m.basis,
			Recognition:         m.recognition,
			AnchorClass:         m.class,
			Valuation:           rwaValuationOf(d),
			CirculatingSupply:   d.CirculatingSupply,
			// The contract's REAL declared scale, overlaid by
			// fillContractDecimals — not the 7 assetDetailFromAssetRow
			// starts every row at. It is on the wire because a
			// contract-issued row is the case where assuming 7 is
			// actually wrong.
			Decimals:     d.Decimals,
			Volume24hUSD: d.VolumeUSD24h,
		}
		if len(a.IssuerDirectoryTags) == 0 {
			a.IssuerDirectoryTags = m.dirTags
		}
		if d.FirstSeenLedger != nil {
			a.FirstSeenLedger = *d.FirstSeenLedger
		}
		if d.ObservationCount != nil {
			a.ObservationCount = *d.ObservationCount
		}
		out = append(out, a)
	}
	return out
}

// rwaContractArmCounts is one C2 arm's share of the catalogue join.
type rwaContractArmCounts struct {
	served      int
	notObserved int
}

// rwaContractArmSplit attributes each admitted contract's catalogue-join
// outcome to the arm that admitted it.
//
// Derived from the SAME two values the projection uses — the member list
// and the valued-row map — rather than counted alongside it, so the
// funnel's per-arm stages cannot disagree with the rows actually served.
// A member whose address is absent from the map was admitted and never
// observed on chain, which is the structural drop both arms report under
// rwaDropNotInCatalogue.
func rwaContractArmSplit(
	members []rwaContractMember, rows map[string]AssetDetail,
) (directory, listing rwaContractArmCounts) {
	for _, m := range members {
		arm := &directory
		if m.recognition == rwa.RecognitionListingCorroborated {
			arm = &listing
		}
		if _, ok := rows[m.contractID]; ok {
			arm.served++
			continue
		}
		arm.notObserved++
	}
	return directory, listing
}

// rwaContractName is the human-readable name for a contract row.
//
// The curated directory's label first, which is what the first C2 arm
// admitted on. A row admitted on the SECOND arm has no directory entry
// by definition, so it falls back to the in-repo curated binding's
// instrument — the same string [rwa.ContractInstrumentBindings]
// publishes on the wire, so a reader can trace the name to the entry it
// came from.
//
// What it never falls back to is the listing platform's own display
// text. That source is this surface's CORROBORATION, not its identity:
// letting it name a row would put a price aggregator's label where the
// definition says an independent identity attestation goes, and a
// reader could not tell the two apart. An unnamed row is the correct
// outcome if neither source names it.
func rwaContractName(m rwaContractMember) string {
	if n := strings.TrimSpace(m.dirName); n != "" {
		return n
	}
	for _, b := range rwa.ContractInstrumentBindings() {
		if b.ContractID == m.contractID {
			return b.Instrument
		}
	}
	return ""
}

// rwaUnreachedRows projects the coverage sample onto the wire.
func rwaUnreachedRows(entities []rwaUnreachedEntity) []RWAUnreachedEntity {
	out := make([]RWAUnreachedEntity, 0, len(entities))
	for _, e := range entities {
		out = append(out, RWAUnreachedEntity{
			Address: e.address,
			Name:    e.name,
			Domain:  e.domain,
			Tags:    e.tags,
		})
	}
	return out
}
