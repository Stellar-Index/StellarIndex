package v1

import (
	"context"
	"net/http"

	"github.com/StellarIndex/stellar-index/internal/storage/timescale"
)

// ProtocolContractsReader is the read seam for the protocol_contracts
// registry (ADR-0035 factory-anchored gating). Production wiring is
// timescale.Store.ListProtocolContracts. Nil reader → contract lists
// and counts serve empty; never an error.
type ProtocolContractsReader interface {
	ListProtocolContracts(ctx context.Context, source string) ([]timescale.ProtocolContract, error)
}

// ProtocolStatsReader supplies the trailing-24h event count per source
// (one grouped UNION ALL over the per-protocol tables). Production
// wiring is timescale.Store.CountRecentEventsBySource. Nil reader →
// events_24h serves 0 for every protocol.
type ProtocolStatsReader interface {
	CountRecentEventsBySource(ctx context.Context) (map[string]int64, error)
}

// SoroswapPairsReader exposes the soroswap_pairs registry — Soroswap's
// equivalent of protocol_contracts (its pair set predates the unified
// registry and carries token identities the decoder needs). Production
// wiring is timescale.Store.LoadSoroswapPairRegistry. Nil reader →
// the soroswap contract list/count serves empty.
type SoroswapPairsReader interface {
	LoadSoroswapPairRegistry(ctx context.Context) ([]timescale.SoroswapPair, error)
}

// ProtocolCompletenessView is the verdict summary joined onto a
// protocol row — the headline slice of /v1/coverage's full verdict
// (same completeness_snapshots row, keyed by source name).
type ProtocolCompletenessView struct {
	// Complete is the headline ADR-0033 verdict.
	Complete bool `json:"complete"`
	// WatermarkLedger is the highest ledger the verdict covers.
	WatermarkLedger uint32 `json:"watermark_ledger"`
}

// ProtocolView is the wire shape of one directory row on
// GET /v1/protocols.
type ProtocolView struct {
	// Name is the canonical source name — the same identifier
	// /v1/coverage and /v1/sources use.
	Name string `json:"name"`
	// Category is one of: dex | amm | lending | yield | bridge | oracle | token.
	Category string `json:"category"`
	// Description is a one-sentence summary for the directory card.
	Description string `json:"description"`
	// GenesisLedger is the first ledger this protocol could have data at.
	GenesisLedger uint32 `json:"genesis_ledger"`
	// Factories lists the verified factory / trust-root contract IDs
	// the decoder anchors on (ADR-0035); empty for factory-less sources.
	Factories []string `json:"factories"`
	// ContractCount is the number of registered contract instances
	// (protocol_contracts rows; soroswap_pairs rows for soroswap).
	ContractCount int `json:"contract_count"`
	// Events24h is the trailing-24h decoded-event count across the
	// protocol's served tables.
	Events24h int64 `json:"events_24h"`
	// Completeness is the latest ADR-0033 verdict summary, absent when
	// no completeness snapshot exists for this source.
	Completeness *ProtocolCompletenessView `json:"completeness,omitempty"`
}

// ProtocolsView is the envelope data field of GET /v1/protocols.
type ProtocolsView struct {
	// Protocols lists every indexed protocol in registry order.
	Protocols []ProtocolView `json:"protocols"`
	// TotalProtocols is len(protocols), for symmetric pagination-free
	// consumers.
	TotalProtocols int `json:"total_protocols"`
}

// ProtocolContractView is one registered contract instance on
// GET /v1/protocols/{name} — a unified projection over
// protocol_contracts (factory-gated sources) and soroswap_pairs
// (token0/token1 populated, factory absent).
type ProtocolContractView struct {
	// ContractID is the instance's C-strkey.
	ContractID string `json:"contract_id"`
	// FactoryID is the deploying factory's C-strkey (gated sources;
	// empty for soroswap pairs, which are keyed by token identities).
	FactoryID string `json:"factory_id,omitempty"`
	// FirstLedger is the ledger the instance was first observed at
	// (0/absent when the seed source didn't carry it).
	FirstLedger uint32 `json:"first_ledger,omitempty"`
	// Token0 / Token1 are the pair's token C-strkeys (soroswap only).
	Token0 string `json:"token0,omitempty"`
	Token1 string `json:"token1,omitempty"`
}

// ProtocolDetailView is the envelope data field of
// GET /v1/protocols/{name}: the directory row plus the contract
// registry, decoded event vocabulary and verification write-up path.
type ProtocolDetailView struct {
	ProtocolView
	// Contracts lists every registered instance; empty for sources
	// without a contract registry (oracles, sdex, bridges).
	Contracts []ProtocolContractView `json:"contracts"`
	// EventKinds lists the EventKind() discriminators the source's
	// decoder emits.
	EventKinds []string `json:"event_kinds"`
	// VerificationPage is the repo-relative path of the protocol's
	// verification write-up, absent when none exists yet.
	VerificationPage string `json:"verification_page,omitempty"`
}

// handleProtocolsList serves GET /v1/protocols — the protocol
// directory backing the explorer's Protocols pillar. The static
// registry (protocols_registry.go) always serves; the dynamic joins
// (contract counts, 24h events, completeness verdicts) degrade to
// zeros/absent when their reader is nil or errors, so a deployment
// without the optional readers still gets a useful directory.
func (s *Server) handleProtocolsList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	events := s.protocolEvents24h(ctx)
	verdicts := s.protocolVerdicts(ctx)

	view := ProtocolsView{Protocols: make([]ProtocolView, 0, len(protocolRegistry))}
	for _, meta := range protocolRegistry {
		contracts := s.protocolContracts(ctx, meta.Name)
		view.Protocols = append(view.Protocols,
			buildProtocolView(meta, len(contracts), events, verdicts))
	}
	view.TotalProtocols = len(view.Protocols)

	w.Header().Set("Cache-Control", "public, max-age=60")
	writeJSON(w, view, Flags{})
}

// handleProtocolDetail serves GET /v1/protocols/{name} — everything
// the directory row carries plus the contract registry, event-kind
// vocabulary and verification page. Unknown names 404.
func (s *Server) handleProtocolDetail(w http.ResponseWriter, r *http.Request) {
	meta, ok := protocolByName(r.PathValue("name"))
	if !ok {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/protocol-not-found",
			"Protocol not found", http.StatusNotFound,
			"unknown protocol name; GET /v1/protocols lists every known protocol")
		return
	}

	ctx := r.Context()
	contracts := s.protocolContracts(ctx, meta.Name)
	view := ProtocolDetailView{
		ProtocolView:     buildProtocolView(meta, len(contracts), s.protocolEvents24h(ctx), s.protocolVerdicts(ctx)),
		Contracts:        contracts,
		EventKinds:       append([]string{}, meta.EventKinds...),
		VerificationPage: meta.VerificationPage,
	}

	w.Header().Set("Cache-Control", "public, max-age=60")
	writeJSON(w, view, Flags{})
}

// buildProtocolView projects one registry entry + the dynamic joins
// into the directory wire shape.
func buildProtocolView(meta ProtocolMeta, contractCount int, events map[string]int64, verdicts map[string]timescale.CompletenessSnapshot) ProtocolView {
	v := ProtocolView{
		Name:          meta.Name,
		Category:      meta.Category,
		Description:   meta.Description,
		GenesisLedger: meta.GenesisLedger,
		Factories:     append([]string{}, meta.Factories...),
		ContractCount: contractCount,
		Events24h:     events[meta.Name],
	}
	if sn, ok := verdicts[meta.Name]; ok {
		v.Completeness = &ProtocolCompletenessView{
			Complete:        sn.Complete,
			WatermarkLedger: sn.Watermark,
		}
	}
	return v
}

// protocolEvents24h reads the per-source trailing-24h event counts,
// degrading to an empty map (every protocol reads 0) when the reader
// is nil or errors.
func (s *Server) protocolEvents24h(ctx context.Context) map[string]int64 {
	if s.protocolStats == nil {
		return nil
	}
	counts, err := s.protocolStats.CountRecentEventsBySource(ctx)
	if err != nil {
		s.logger.Warn("protocols events_24h read failed", "err", err)
		return nil
	}
	return counts
}

// protocolVerdicts reads the latest completeness verdict per source,
// degrading to an empty map (verdict summaries absent) when the reader
// is nil or errors.
func (s *Server) protocolVerdicts(ctx context.Context) map[string]timescale.CompletenessSnapshot {
	if s.completenessReader == nil {
		return nil
	}
	snaps, err := s.completenessReader.ListCompletenessSnapshots(ctx)
	if err != nil {
		s.logger.Warn("protocols completeness read failed", "err", err)
		return nil
	}
	out := make(map[string]timescale.CompletenessSnapshot, len(snaps))
	for _, sn := range snaps {
		out[sn.Source] = sn
	}
	return out
}

// protocolContracts returns name's registered instances in the unified
// wire shape: soroswap_pairs for soroswap, protocol_contracts for the
// factory-gated sources, empty for everything else (and on nil reader
// or read error — same degradation contract as the other joins).
func (s *Server) protocolContracts(ctx context.Context, name string) []ProtocolContractView {
	if name == "soroswap" {
		return s.soroswapContracts(ctx)
	}
	if s.protocolContractsReader == nil {
		return []ProtocolContractView{}
	}
	rows, err := s.protocolContractsReader.ListProtocolContracts(ctx, name)
	if err != nil {
		s.logger.Warn("protocols contract registry read failed", "source", name, "err", err)
		return []ProtocolContractView{}
	}
	out := make([]ProtocolContractView, 0, len(rows))
	for _, row := range rows {
		out = append(out, ProtocolContractView{
			ContractID:  row.ContractID,
			FactoryID:   row.FactoryID,
			FirstLedger: row.FirstLedger,
		})
	}
	return out
}

// soroswapContracts projects the soroswap_pairs registry into the
// unified contract shape (pair strkey as the instance, token pair
// attached, no factory column — the pairs table predates ADR-0035).
func (s *Server) soroswapContracts(ctx context.Context) []ProtocolContractView {
	if s.soroswapPairs == nil {
		return []ProtocolContractView{}
	}
	pairs, err := s.soroswapPairs.LoadSoroswapPairRegistry(ctx)
	if err != nil {
		s.logger.Warn("protocols soroswap pair registry read failed", "err", err)
		return []ProtocolContractView{}
	}
	out := make([]ProtocolContractView, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, ProtocolContractView{
			ContractID: p.PairStrkey,
			Token0:     p.Token0Strkey,
			Token1:     p.Token1Strkey,
		})
	}
	return out
}
