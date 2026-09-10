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
func (s *Server) buildRWAContractMembership(ctx context.Context) ([]rwaContractMember, []rwaUnreachedEntity, rwaContractCensus, map[string]int) {
	refusals := map[string]int{}
	var census rwaContractCensus
	if s.rwaContracts == nil {
		return nil, nil, census, refusals
	}
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
		return nil, nil, census, refusals
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
		})
		if !v.InSet {
			refusals[v.Reject]++
			continue
		}
		members = append(members, rwaContractMember{
			contractID: e.Address,
			symbol:     symbol,
			basis:      v.Basis,
			class:      v.AnchorClass,
			dirName:    e.Name,
			dirDomain:  e.Domain,
			dirTags:    e.Tags,
		})
	}

	return members, s.rwaUnreachedEntities(ctx, tags), census, refusals
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
			Name:                strings.TrimSpace(m.dirName),
			HomeDomain:          m.dirDomain,
			IssuerDirectoryName: m.dirName,
			IssuerDirectoryTags: d.IssuerDirectoryTags,
			Basis:               m.basis,
			AnchorClass:         m.class,
			Valuation:           rwaValuationOf(d),
			CirculatingSupply:   d.CirculatingSupply,
			Volume24hUSD:        d.VolumeUSD24h,
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
