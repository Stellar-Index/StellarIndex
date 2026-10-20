package v1

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/rwa"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

// The contract arm of GET /v1/rwa/assets: membership, valuation and coverage for tokens with no
// (code, issuer) pair. The definition is internal/rwa/contract.go; this file is the read path.
//
// The classic arm walks issuers, so an entity issuing only contract tokens would be silently
// discarded. This arm draws from the table tying an entity to a Stellar address without classic
// issuance, and names recognised entities holding no token here.

// rwaContractScanCap bounds the contracts one rebuild evaluates; the recognised set is small, so it guards a directory sync that suddenly
// tags thousands. When it binds the funnel says so.
const rwaContractScanCap = 256

// rwaUnreachedSampleCap bounds how many recognised-but-tokenless entities are NAMED; the count is exact and always served, and names are
// ordered by address so a rebuild names the same entities.
const rwaUnreachedSampleCap = 50

// rwaContractMetadataBudget is a whole-phase budget (not per-read) for a rebuild's contract metadata reads (symbol, decimals,
// supply): an unbounded fan-out would hold the single-flight gate open for every concurrent reader. Rebuild only; requests run
// under rwaContractValuationBudget.
const rwaContractMetadataBudget = 20 * time.Second

// rwaContractValuationBudget bounds the contract valuation phase of one /v1/rwa/assets request (both arms), inside
// maxHandlerBudget with room for the classic arm, so the handler decides what a cut valuation looks like: the rows it reached,
// and flags.stale.
const rwaContractValuationBudget = 8 * time.Second

// ─── storage seams ──────────────────────────────────────────────────

// RWADirectoryContractReader is the optional seam for the contract arm's candidate population; without it the funnel says the arm is unmeasured rather than reporting zero contracts.
type RWADirectoryContractReader interface {
	DirectoryRecognisedContracts(ctx context.Context, issuingTags []string) (
		[]timescale.DirectoryEntry, timescale.DirectoryRWACensus, error)
	DirectoryRecognisedIssuersWithoutAsset(ctx context.Context, issuingTags []string, limit int) (
		[]timescale.DirectoryEntry, error)
}

// ContractCatalogueReader reads catalogue rows for an explicit contract set, without the volume gate the listing spine applies.
type ContractCatalogueReader interface {
	ContractCatalogueRows(ctx context.Context, contractIDs []string) (map[string]timescale.AssetRow, error)
}

// TokenSymbolReader resolves a token contract's on-chain SEP-41 symbol; read ONLY after an independent party named the contract, since the value is contract-authored and says which instrument, never whether.
type TokenSymbolReader interface {
	TokenSymbol(ctx context.Context, contractID string) (string, bool, error)
}

// ─── membership ─────────────────────────────────────────────────────

// rwaContractMember is one admitted contract with its admitting evidence, built without valuation
// input as [rwaMember] is.
type rwaContractMember struct {
	contractID string
	symbol     string
	basis      string
	class      string
	dirName    string
	dirDomain  string
	dirTags    []string
	// recognition names WHICH independent party satisfied C2; served because the two arms are not
	// equal-strength evidence ([rwa.Verdict].Recognition).
	recognition string
	// listing is the independent listing row that corroborated this address, if any; it carries
	// the price this arm values the row at. An empty ListingID is the normal directory case.
	listing timescale.ListingEntry
}

// rwaUnreachedEntity is a recognised issuing entity for which this index holds no Stellar token.
type rwaUnreachedEntity struct {
	address string
	name    string
	domain  string
	tags    []string
}

// rwaContractCensus is the contract arm's accounting: the directory census the storage layer
// measured, plus what this layer then did with the candidates.
type rwaContractCensus struct {
	dir timescale.DirectoryRWACensus
	// evaluated is the number of recognised contracts put to the definition; fewer than
	// dir.ContractsRecognised when a cap bound the scan.
	evaluated int
	// overCap is the difference, reported so the funnel closes rather than losing candidates.
	overCap int
	// duplicates counts a contract named twice by the directory. It should be zero (the address is
	// the upstream key) but is counted, since "should be zero" is how a double-counted cap ships.
	duplicates int
	// available is false when no directory contract reader is wired.
	available bool
}

// buildRWAContractMembership applies the contract requirements to every recognised contract the curated directory names: ONE
// metadata read per candidate under a shared budget, then the verdict. Nothing here reads a price; membership is decided before
// valuation.
func (s *Server) buildRWAContractMembership(ctx context.Context) rwaContractBuild {
	var out rwaContractBuild
	out.refusals = map[string]int{}
	out.listingRefusals = map[string]int{}

	// ONE listing read per rebuild, shared by both arms: the first uses it only to record which
	// members a second party also names; the second's whole recognition rests on it.
	listing := s.rwaListingSnapshot(ctx)

	if s.RWAContracts == nil {
		// An unwired directory says nothing about the listing arm; they draw on different readers.
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

// rwaContractBuild is one contract-side rebuild: the admitted set from BOTH C2 arms plus each arm's census and refusal tally. The
// tallies stay APART because both arms can produce the same reason string and one tally would misattribute refusals; they merge
// once, at the top, into `refused[]`.
type rwaContractBuild struct {
	members         []rwaContractMember
	unreached       []rwaUnreachedEntity
	census          rwaContractCensus
	listingCensus   rwaListingCensus
	refusals        map[string]int
	listingRefusals map[string]int
}

// buildRWADirectoryMembership is C2's first arm: contract addresses the curated account directory names with an issuing tag. It
// returns the addresses it EVALUATED (not just admitted) so the second arm can remove them, else the funnel counts a candidate
// twice and a refusal here is contradicted by an admission there.
func (s *Server) buildRWADirectoryMembership(
	ctx context.Context, listing rwaListing, refusals map[string]int,
) (rwaContractCensus, []rwaContractMember, map[string]struct{}) {
	var census rwaContractCensus
	tags := rwa.ContractRecognitionTags()
	entries, dirCensus, err := s.RWAContracts.DirectoryRecognisedContracts(ctx, tags)
	if err != nil {
		// Fail CLOSED, as the classic arm does. The directory IS the provenance requirement here, so
		// serving contract rows without it would publish the unvouched-for population the
		// definition refuses.
		s.logger.Warn("rwa contract membership: directory scan failed", "err", err)
		return census, nil, map[string]struct{}{}
	}
	census.available = true
	census.dir = dirCensus

	if len(entries) > rwaContractScanCap {
		census.overCap = len(entries) - rwaContractScanCap
		entries = entries[:rwaContractScanCap]
	}
	// The storage cap can bind first; both differences land in one bucket, a candidate the scan
	// did not evaluate.
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
		// The symbol is read AFTER the directory named the address and BEFORE the verdict, since
		// only C4 reads it; a contract the directory did not name never has its self-declared
		// metadata consulted.
		symbol := s.rwaContractSymbol(mctx, e.Address)
		v := rwa.QualifyContract(rwa.ContractCandidate{
			ContractID:     e.Address,
			DirectoryNamed: true,
			DirectoryTags:  e.Tags,
			Symbol:         symbol,
			// Passed though this arm does not need it: a member the listing ALSO names is a
			// stronger row, and the verdict decides that. This layer only supplies inputs.
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

// buildRWAListingMembership is C2's second arm: every in-repo curated binding the first arm is not evaluating, with an independent
// listing as its recognition.
// Its population is a hand-reviewed table arriving deduplicated and ordered ([rwa.ContractInstrumentBindings]), so it needs no scan cap
// or truncation accounting.
func (s *Server) buildRWAListingMembership(
	ctx context.Context, listing rwaListing,
	directoryEvaluated map[string]struct{}, refusals map[string]int,
) ([]rwaContractMember, rwaListingCensus) {
	candidates, census := s.rwaListingCandidates(ctx, listing, directoryEvaluated)
	if !census.available {
		return nil, census
	}
	// Refusals the candidate build decided (properties of the population, not of a candidate) are
	// folded in under the SAME reason strings rwa.QualifyContract would produce, so `refused[]` and
	// the funnel reconcile against one vocabulary.
	refusals[rwa.RejectContractListingUnavailable] += census.listingUnavailable
	refusals[rwa.RejectContractCuratedTagsUnavailable] += census.tagsUnavailable
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
			// The directory does not name this address as an issuer, which is why it is on this arm.
			// Its TAGS are still supplied because C3 reads them and a scam flag beats any recognition.
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
			// No directory label: the name comes from the curated binding's instrument at projection,
			// so a listing platform's display text is never presented as an identity attestation.
			dirTags: c.dirTags,
		})
	}
	return members, census
}

// rwaContractSymbol reads one contract's on-chain symbol, best-effort: no reader or no usable
// metadata yields "", which fails C4's oracle arm, never an error or a guess.
func (s *Server) rwaContractSymbol(ctx context.Context, contractID string) string {
	if s.TokenSymbol == nil {
		return ""
	}
	sym, ok, err := s.TokenSymbol.TokenSymbol(ctx, contractID)
	if err != nil || !ok {
		return ""
	}
	return sym
}

// rwaUnreachedEntities names recognised issuing entities with no token here. Best-effort: a failed
// read costs the names, not the response; the exact count still comes from the census.
func (s *Server) rwaUnreachedEntities(ctx context.Context, tags []string) []rwaUnreachedEntity {
	rows, err := s.RWAContracts.DirectoryRecognisedIssuersWithoutAsset(ctx, tags, rwaUnreachedSampleCap)
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

// rwaContractListingRows reads the admitted contracts through the /v1/assets post-query pipeline,
// in the listing's order so the surface cannot drift from it
// (TestSourceShapeRules/rwa_contract_pipeline_matches_assets_listing). The substance gate covers
// canonical.AssetSoroban, so a thin-market price is withheld here. It then fills:
//  1. supply and market cap: only the precise supply map reaches a contract row;
//     fillContractMarketCaps has the last word (drops the generic cap, and every cap on a row whose
//     scale was not read).
//  2. scam suppression: tags are re-read at valuation time, not reused from the membership build.
//     valuationCut reports ctx ended mid-fill: rows lacking a valuation were never read, not
//
// refused.
func (s *Server) rwaContractListingRows(
	ctx context.Context, members []rwaContractMember,
) (byID map[string]AssetDetail, notObserved int, valuationCut bool, err error) {
	if len(members) == 0 || s.ContractCatalogue == nil {
		return map[string]AssetDetail{}, 0, false, nil
	}
	ids := make([]string, 0, len(members))
	for _, m := range members {
		ids = append(ids, m.contractID)
	}
	sort.Strings(ids)

	rows, err := s.ContractCatalogue.ContractCatalogueRows(ctx, ids)
	if err != nil {
		return nil, 0, false, err
	}

	details := make([]AssetDetail, 0, len(rows))
	for _, id := range ids {
		row, ok := rows[id]
		if !ok {
			continue
		}
		details = append(details, assetDetailFromAssetRow(row))
	}
	notObserved = len(ids) - len(details)
	if len(details) == 0 {
		return map[string]AssetDetail{}, notObserved, false, nil
	}

	// The /v1/assets post-query pipeline, in its order (see rwaListingRows). Its market-cap fill can
	// still divide by the default 7 when its decimals read misses; fillContractMarketCaps below
	// discards any such cap.
	s.stampListingCollisions(details)
	s.applySubstanceGateToListing(ctx, details, false)
	s.fillMarketCapsFromSupply(ctx, details, map[string]int{})
	s.fillDeclaredPegPricesInListing(ctx, details)
	s.fillIssuerDirectoryTags(ctx, details)

	// Order is load-bearing: decimals BEFORE the market cap that divides by them, and the directory
	// suppression LAST so it can empty a figure the fill just produced. The fills run on the
	// caller's budget; request paths bound them with rwaContractValuationBudget.
	scaled, decimalsCut := s.fillContractDecimals(ctx, details, rows)
	capsCut := s.fillContractMarketCaps(ctx, details, rows, scaled)
	s.fillContractDirectoryTags(ctx, details)

	out := make(map[string]AssetDetail, len(details))
	for _, d := range details {
		out[d.AssetID] = d
	}
	return out, notObserved, decimalsCut || capsCut, nil
}

// fillContractDecimals overlays each contract's real on-chain decimals; assetDetailFromAssetRow hardcodes 7, and market cap
// divides supply by 10^decimals, so a 5-decimal fund read at 7 publishes one hundredth of its capitalisation. A token with no
// readable metadata keeps 7 but may not be multiplied by it: the returned set holds addresses whose scale was READ, and a row
// absent from it carries no valuation (unwired reader, instance not in the lake, missing or conflicting scale all mean unknown).
// ctx ending mid-walk is not a refusal: cut reports it.
func (s *Server) fillContractDecimals(
	ctx context.Context, rows []AssetDetail, src map[string]timescale.AssetRow,
) (resolved map[string]struct{}, cut bool) {
	resolved = make(map[string]struct{}, len(rows))
	if s.TokenDecimals == nil {
		return resolved, false
	}
	for i := range rows {
		if _, ok := src[rows[i].AssetID]; !ok {
			continue
		}
		d, ok, err := s.TokenDecimals.TokenDecimals(ctx, rows[i].AssetID)
		if err != nil && ctx.Err() != nil {
			return resolved, true
		}
		if err != nil || !ok {
			continue
		}
		rows[i].Decimals = int(d)
		resolved[rows[i].AssetID] = struct{}{}
	}
	return resolved, false
}

// fillContractMarketCaps fills circulating supply and market cap for contract rows from the certified lake: mints less burns and
// clawbacks in the smallest unit, as /v1/assets/{id}/supply serves.
// An INCOMPLETE reading (negative net: under-seeded flows) is UNAVAILABLE, never clamped to zero, which would read as fully burned. The
// dust-liquidity guard suppresses the CAP only.
// cut reports ctx ended mid-walk: unread, not refused.
func (s *Server) fillContractMarketCaps(
	ctx context.Context, rows []AssetDetail, src map[string]timescale.AssetRow,
	scaled map[string]struct{},
) (cut bool) {
	refuseUnscaledContractCaps(rows, scaled)
	if s.TokenSupply == nil {
		return false
	}
	for i := range rows {
		row := &rows[i]
		circ, basis, ok := s.contractSupplyReading(ctx, row.AssetID)
		if !ok && ctx.Err() != nil {
			return true
		}
		if !ok {
			continue
		}
		_, isScaled := scaled[row.AssetID]
		var sources int
		if sc := src[row.AssetID].SourceCount; sc != nil {
			sources = *sc
		}
		s.applyContractMarketCap(row, circ, basis, isScaled, sources)
	}
	return false
}

// applyContractMarketCap fills one contract row's supply/cap from a successful supply reading, encoding the refusal rules: unscaled
// decimals, dust liquidity, scale disagreement and turnover.
func (s *Server) applyContractMarketCap(
	row *AssetDetail, circ string, basis supply.Basis, isScaled bool, sources int,
) {
	// This reading owns the cap: a figure the generic fill derived from a different supply must not
	// survive a refusal below.
	row.MarketCapUSD = nil
	b := basis.String()
	row.SupplyBasis = &b
	// Supply is a raw chain fact and is served either way. The CAP divides by 10^decimals, and an
	// unread scale is a convention, so multiplying by it would be off by a power of ten.
	if !isScaled {
		row.CirculatingSupply = &circ
		row.DecimalsUnresolved = true
		return
	}
	row.CirculatingSupply = &circ
	if row.PriceUSD == nil {
		return
	}
	if dustLiquiditySuppressed(sources, row.VolumeUSD24h, s.MinMarketCapVolumeUSD) {
		row.MarketCapLowLiquidity = true
		return
	}
	if s.contractPriceScaleDisagrees(row) {
		row.MarketCapDecimalsMismatch = true
		return
	}
	mc := computeMarketCapUSD(circ, *row.PriceUSD, row.Decimals)
	if mc == "" {
		return
	}
	if capExceedsObservedTurnover(mc, row.VolumeUSD24h, s.MaxMarketCapVolumeRatio) {
		row.MarketCapLowLiquidity = true
		return
	}
	row.MarketCapUSD = &mc
}

// contractPriceScaleDisagrees reports that a contract row's DEX price (normalised through nonstandard_decimals_assets, else
// [aggregate.StandardDecimals]) and its lake decimals are on different scales, so their product is no cap. Same refusal as
// [Server.applyTokenDecimals]: an unseeded 6-decimal token carries a raw price and would publish ten times its capitalisation.
// The oracle valuation is untouched.
func (s *Server) contractPriceScaleDisagrees(row *AssetDetail) bool {
	confirmed, hasConfirmed := s.NonstandardDecimals.Lookup(row.AssetID)
	if (hasConfirmed && confirmed == row.Decimals) ||
		(!hasConfirmed && row.Decimals == aggregate.StandardDecimals) {
		return false
	}
	obs.NonstandardDecimalsLockstepMismatchTotal.WithLabelValues("rwa_contract", "").Inc()
	if s.logger != nil {
		s.logger.Warn("rwa contract decimals resolvers disagree; refusing market cap",
			"contract_id", row.AssetID, "lake_decimals", row.Decimals,
			"projection_decimals", confirmed, "projection_row", hasConfirmed)
	}
	return true
}

// refuseUnscaledContractCaps drops the cap from every row whose scale was not read, whichever supply
// produced it: the generic fill divides by the default 7 without a reading.
func refuseUnscaledContractCaps(rows []AssetDetail, scaled map[string]struct{}) {
	for i := range rows {
		if _, ok := scaled[rows[i].AssetID]; ok {
			continue
		}
		rows[i].MarketCapUSD = nil
		if rows[i].CirculatingSupply != nil {
			rows[i].DecimalsUnresolved = true
		}
	}
}

// contractSupplyReading resolves one contract token's circulating supply and basis. Counterpart to [classicSupplyReading]: the
// preference order lives in ONE place, or two surfaces publish different bases under one field name. Readings measure ISSUANCE
// against DISTRIBUTION and are never summed. ok=false means no defensible figure, including an INCOMPLETE flow sum (negative net,
// never clamped to zero).
func (s *Server) contractSupplyReading(
	ctx context.Context, assetID string,
) (string, supply.Basis, bool) {
	sup, err := s.TokenSupply.TokenSupply(ctx, assetID)
	if err != nil || sup.Total == nil || sup.Incomplete {
		return "", "", false
	}
	// A token emitting no SEP-41 events is ABSENT from the event reader and sums to a confident zero.
	// With no log at all, read held balances, as GET /v1/assets/{id}/supply does.
	if sup.FlowCount == 0 {
		if st, ok := s.contractStorageCirculating(ctx, assetID); ok {
			return st, supply.BasisContractStorageBalances, true
		}
	}
	// The lake's event-flow sum over the contract's own mint/burn/clawback log.
	return sup.Total.String(), supply.BasisSEP41LakeFlows, true
}

// contractStorageCirculating reads supply from per-holder balance entries in contract storage, for a contract that emits no
// SEP-41 events. It is a DIFFERENT BASIS, used only where the event reading has no history and never summed with it. ok=false on
// no reader, a read error, a SAC (balances live in trustlines) or a holder set past the cap; a zero-entry read is the ABSENCE of
// a reading, not zero supply.
func (s *Server) contractStorageCirculating(ctx context.Context, contractID string) (string, bool) {
	if s.ContractStorageSupply == nil {
		return "", false
	}
	st, err := s.ContractStorageSupply.ContractStorageSupply(ctx, contractID)
	if err != nil {
		// A SAC is the expected structural refusal here, so this is debug volume, not a warning.
		s.logger.Debug("rwa: contract storage supply declined",
			"contract_id", contractID, "err", err)
		return "", false
	}
	if st.BalanceEntries == 0 || st.Total == nil || st.Total.Sign() == 0 {
		return "", false
	}
	return st.Total.String(), true
}

// fillContractDirectoryTags stamps the curated label from the CONTRACT address and applies scam suppression to it: the classic
// twin keys on the issuer G-address, so a contract asset would otherwise escape the directory scam gate on a surface that values
// contracts.
func (s *Server) fillContractDirectoryTags(ctx context.Context, rows []AssetDetail) {
	if s.Directory == nil || len(rows) == 0 {
		return
	}
	addrs := make([]string, 0, len(rows))
	for i := range rows {
		addrs = append(addrs, rows[i].AssetID)
	}
	found, err := s.Directory.DirectoryEntriesByAddresses(ctx, addrs)
	if err != nil {
		s.logger.Warn("rwa contract directory batch lookup failed — withholding pricing",
			"n", len(addrs), "err", err)
		for i := range rows {
			withholdUncheckedIssuerPricing(&rows[i])
		}
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

// rwaContractAssetRows joins membership evidence to the valued rows. The caller orders the combined
// set once, so contract and classic rows rank by the same rule.
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
			// Code and Issuer stay empty: filling Code from the on-chain symbol would put a
			// self-declared ticker in the field the definition refuses to identify anything by. The
			// symbol is served under its own name so it reads as metadata, not identity.
			Symbol:                m.symbol,
			Slug:                  d.Slug,
			Name:                  rwaContractName(m),
			IssuerDirectoryName:   m.dirName,
			IssuerDirectoryTags:   d.IssuerDirectoryTags,
			IssuerDirectoryDomain: m.dirDomain,
			Basis:                 m.basis,
			Recognition:           m.recognition,
			AnchorClass:           m.class,
			Valuation:             rwaValuationOf(d),
			CirculatingSupply:     d.CirculatingSupply,
			// The REAL scale from fillContractDecimals, not the default 7 (served as null instead).
			Decimals:     rwaDecimalsOf(d),
			Volume24hUSD: d.VolumeUSD24h,
		}
		a.SupplyBasis, a.CirculatingSupplyLowerBound = rwaSupplyProvenance(d)
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

// rwaContractArmSplit attributes each admitted contract's catalogue-join outcome to its arm, from the SAME member list and valued-row map
// the projection uses so per-arm stages cannot disagree with the rows served.
// A member absent from the map was admitted and never observed on chain: the drop both arms report under rwaDropNotInCatalogue.
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

// rwaContractName is the directory label, else (second arm) the in-repo binding's instrument; never the listing platform's display text,
// which is CORROBORATION, not identity. An unnamed row is correct.
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
