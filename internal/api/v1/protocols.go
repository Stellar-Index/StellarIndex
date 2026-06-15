package v1

import (
	"context"
	"net/http"
	"time"

	"github.com/StellarIndex/stellar-index/internal/storage/clickhouse"
	"github.com/StellarIndex/stellar-index/internal/storage/timescale"
)

// protocolActivityWindowDays is the lookback for the windowed per-protocol
// analytics (the activity time-series). ~90 days ≈ 1.55M ledgers — bounded so
// the lake query prunes partitions and stays fast on the 12B-row table.
const protocolActivityWindowDays = 90

// protocolActivityWindowLedgers is protocolActivityWindowDays expressed in
// ledgers (~5s close time → 17,280/day).
const protocolActivityWindowLedgers = protocolActivityWindowDays * 17280

// ProtocolActivityReader serves per-protocol on-chain analytics from the
// certified lake (contract_events): event-type breakdown, daily activity
// series, and per-contract rollups, all scoped to a protocol's contract-id set.
// Production wiring is *clickhouse.ExplorerReader (the same lake reader the
// network explorer uses). Nil reader → the analytics fields serve empty; the
// directory + registry still work.
type ProtocolActivityReader interface {
	LakeTipLedger(ctx context.Context) (uint32, error)
	ProtocolEventBreakdown(ctx context.Context, contractIDs []string, sinceLedger uint32) ([]clickhouse.ProtocolEventTypeCount, error)
	ProtocolDailyActivity(ctx context.Context, contractIDs []string, sinceLedger uint32) ([]clickhouse.ProtocolDailyPoint, error)
	ProtocolContractActivity(ctx context.Context, contractIDs []string, sinceLedger uint32) ([]clickhouse.ProtocolContractActivity, error)
}

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
	// Kind classifies the instance within the protocol: "factory" (a verified
	// trust-root in meta.Factories) or "instance" (a pool/vault/market the
	// factory deployed). Lets the page group the roster by role.
	Kind string `json:"kind,omitempty"`
	// Events is the all-time decoded contract-event count emitted by this
	// instance (from the lake). 0/absent when the activity reader is nil.
	Events int64 `json:"events,omitempty"`
	// LastSeen is the close time of this instance's most recent event
	// (RFC3339); absent when unknown / no activity reader.
	LastSeen string `json:"last_seen,omitempty"`
}

// ProtocolEventTypeView is one slice of a protocol's event-type distribution:
// a topic[0] symbol and how many times it fired (all-time, from the lake).
type ProtocolEventTypeView struct {
	EventType string `json:"event_type"`
	Count     int64  `json:"count"`
}

// ProtocolActivityPointView is one day of a protocol's event-activity series.
type ProtocolActivityPointView struct {
	Date   string `json:"date"`
	Events int64  `json:"events"`
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

	// ─── Lake analytics (populated when the activity reader is wired) ──

	// EventBreakdown is the event-type distribution (topic[0] symbol →
	// count) across the protocol's contracts over the trailing
	// ActivityWindowDays — "which event types fired, and how often." All
	// analytics share this window so the lake queries stay partition-pruned
	// and fast. Descending by count.
	EventBreakdown []ProtocolEventTypeView `json:"event_breakdown,omitempty"`
	// ActivitySeries is the daily decoded-event count over the trailing
	// ActivityWindowDays — the protocol's on-chain activity chart.
	ActivitySeries []ProtocolActivityPointView `json:"activity_series,omitempty"`
	// ActivityWindowDays is the lookback all the analytics fields cover.
	ActivityWindowDays int `json:"activity_window_days,omitempty"`
	// EventsTotal is the decoded contract-event count across every contract
	// in the protocol over ActivityWindowDays (sum of EventBreakdown counts).
	EventsTotal int64 `json:"events_total,omitempty"`
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
	classifyContractKinds(contracts, meta.Factories)
	view := ProtocolDetailView{
		ProtocolView:     buildProtocolView(meta, len(contracts), s.protocolEvents24h(ctx), s.protocolVerdicts(ctx)),
		Contracts:        contracts,
		EventKinds:       append([]string{}, meta.EventKinds...),
		VerificationPage: meta.VerificationPage,
	}
	s.enrichProtocolAnalytics(ctx, meta, &view)

	w.Header().Set("Cache-Control", "public, max-age=60")
	writeJSON(w, view, Flags{})
}

// classifyContractKinds tags each roster contract as "factory" (it is one of
// the protocol's verified trust-roots) or "instance" (a factory-deployed
// pool/vault/market).
func classifyContractKinds(contracts []ProtocolContractView, factories []string) {
	fset := make(map[string]struct{}, len(factories))
	for _, f := range factories {
		fset[f] = struct{}{}
	}
	for i := range contracts {
		if _, ok := fset[contracts[i].ContractID]; ok {
			contracts[i].Kind = "factory"
		} else {
			contracts[i].Kind = "instance"
		}
	}
}

// enrichProtocolAnalytics populates the lake-derived analytics on the detail
// view: the event-type breakdown, the daily activity series, and per-contract
// event counts merged onto the roster. Degrades to leaving the fields empty
// when the activity reader is nil or any query errors (same contract as the
// other optional joins — the directory + registry still serve).
func (s *Server) enrichProtocolAnalytics(ctx context.Context, meta ProtocolMeta, view *ProtocolDetailView) {
	if s.protocolActivity == nil {
		return
	}
	ids := protocolContractIDs(view.Contracts, meta.Factories)
	if len(ids) == 0 {
		return
	}
	// All three analytics are bounded to the recent window: bounding by
	// ledger_seq prunes partitions, keeping each query well under the lake
	// reader's 30s budget even for the busiest protocols (an all-time scan ran
	// ~33s for blend / would be far worse for soroswap). 0 ⇒ tip unreadable ⇒
	// skip the analytics entirely (degrade, don't serve partial/timed-out).
	since := s.protocolActivitySince(ctx)
	if since == 0 {
		return
	}
	view.ActivityWindowDays = protocolActivityWindowDays
	s.fillProtocolBreakdown(ctx, meta.Name, ids, since, view)
	s.fillProtocolSeries(ctx, meta.Name, ids, since, view)
	s.fillProtocolContractActivity(ctx, meta.Name, ids, since, view)
}

// protocolContractIDs is the dedup'd analytics scope: every registered instance
// + the verified factories themselves (factories emit events too, e.g.
// new_pair / deploy).
func protocolContractIDs(contracts []ProtocolContractView, factories []string) []string {
	ids := make([]string, 0, len(contracts)+len(factories))
	seen := make(map[string]struct{}, cap(ids))
	add := func(id string) {
		if id == "" {
			return
		}
		if _, dup := seen[id]; dup {
			return
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	for _, c := range contracts {
		add(c.ContractID)
	}
	for _, f := range factories {
		add(f)
	}
	return ids
}

// fillProtocolBreakdown populates EventBreakdown + EventsTotal (degrades on error).
func (s *Server) fillProtocolBreakdown(ctx context.Context, name string, ids []string, since uint32, view *ProtocolDetailView) {
	breakdown, err := s.protocolActivity.ProtocolEventBreakdown(ctx, ids, since)
	if err != nil {
		s.logger.Warn("protocol event breakdown failed", "source", name, "err", err)
		return
	}
	view.EventBreakdown = make([]ProtocolEventTypeView, 0, len(breakdown))
	for _, b := range breakdown {
		view.EventBreakdown = append(view.EventBreakdown, ProtocolEventTypeView{EventType: b.EventType, Count: int64(b.Count)})
		view.EventsTotal += int64(b.Count)
	}
}

// fillProtocolSeries populates the daily ActivitySeries (degrades on error).
func (s *Server) fillProtocolSeries(ctx context.Context, name string, ids []string, since uint32, view *ProtocolDetailView) {
	series, err := s.protocolActivity.ProtocolDailyActivity(ctx, ids, since)
	if err != nil {
		s.logger.Warn("protocol daily activity failed", "source", name, "err", err)
		return
	}
	view.ActivitySeries = make([]ProtocolActivityPointView, 0, len(series))
	for _, p := range series {
		view.ActivitySeries = append(view.ActivitySeries, ProtocolActivityPointView{Date: p.Date, Events: int64(p.Events)})
	}
}

// fillProtocolContractActivity merges per-contract event counts + last-seen onto
// the roster (degrades on error).
func (s *Server) fillProtocolContractActivity(ctx context.Context, name string, ids []string, since uint32, view *ProtocolDetailView) {
	act, err := s.protocolActivity.ProtocolContractActivity(ctx, ids, since)
	if err != nil {
		s.logger.Warn("protocol contract activity failed", "source", name, "err", err)
		return
	}
	byID := make(map[string]clickhouse.ProtocolContractActivity, len(act))
	for _, a := range act {
		byID[a.ContractID] = a
	}
	for i := range view.Contracts {
		a, ok := byID[view.Contracts[i].ContractID]
		if !ok {
			continue
		}
		view.Contracts[i].Events = int64(a.Events)
		if !a.LastSeen.IsZero() {
			view.Contracts[i].LastSeen = a.LastSeen.UTC().Format(time.RFC3339)
		}
	}
}

// protocolActivitySince returns the ledger cutoff for the windowed activity
// series (tip − window). 0 when the tip can't be read (caller skips the series).
func (s *Server) protocolActivitySince(ctx context.Context) uint32 {
	tip, err := s.protocolActivity.LakeTipLedger(ctx)
	if err != nil {
		s.logger.Warn("protocol activity tip read failed", "err", err)
		return 0
	}
	if tip <= protocolActivityWindowLedgers {
		return 1 // whole chain is inside the window
	}
	return tip - protocolActivityWindowLedgers
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
