package v1

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// protocolDetailTTL is the freshness horizon of a cached
// /v1/protocols/{name} detail. Past it the entry is NOT dropped: it is
// served STALE (flags.stale + analytics.status="stale") while a detached
// single-flight rebuild runs — a previously-built page must never blank
// back to a cold 503 (2026-07-31: under replay load every on-demand
// build died at the request ceiling and pages lost their visual suites).
// 20 minutes pairs with the prewarm sweep in cmd/stellarindex-api (a
// full registry × windows sweep measured ~3–6 min of build work; the
// worker re-sweeps 10 min after each sweep ends, so entries are
// normally refreshed every ~13–16 min and requests almost always see a
// fresh copy). The analytics are 90d/windowed aggregates — minutes of
// staleness is immaterial.
const protocolDetailTTL = 20 * time.Minute

// protocolDetailRefreshTimeout bounds one detached detail rebuild.
// Measured on r1 UNDER replay load (2026-07-31, upper bounds): the
// bespoke tier is ~1.9s for soroswap 90d (raw-KPI 0.96s + window-KPI
// 0.48s dominant) and ~0.4s for cctp; the lake-analytics fills are
// seconds-class. 90s is generous headroom for contention spikes while
// still bounding a wedged query so it can't pin the single-flight.
const protocolDetailRefreshTimeout = 90 * time.Second

type protoDetailEntry struct {
	view ProtocolDetailView
	at   time.Time
}

// protoDetailInitLocked lazy-creates the cache maps. Caller holds
// protoDetailMu.
func (s *Server) protoDetailInitLocked() {
	if s.protoDetailCache == nil {
		s.protoDetailCache = map[string]protoDetailEntry{}
		s.protoDetailFlight = map[string]chan struct{}{}
	}
}

// cachedProtocolDetail returns the detail view for `key` with
// stale-while-revalidate semantics:
//
//   - fresh entry → served as-is (stale=false).
//   - entry past protocolDetailTTL → served immediately (stale=true)
//     while ONE detached rebuild runs on its own budget — a
//     previously-built page is never blanked by a slow/failing rebuild.
//   - never built → wait for the detached single-flight build up to the
//     caller's deadline. The build itself is NOT bound to that deadline
//     (the 2026-07-31 failure: builds bound to request contexts died
//     under replay load and the cache never filled), so a request that
//     times out 503s but the build completes and the retry lands warm.
//
// The key is the full request-shape key from protocolDetailCacheKey —
// protocol name AND bespoke window — NOT the bare name: the bespoke
// block's content varies with ?days=, so a name-only key would serve one
// window's numbers to another window's request. Per-server (the maps
// live on Server, lazy-init'd) so it never leaks across test instances.
// ok=false only when the caller's context is cancelled (or the build
// produced no cacheable entry) on the cold path.
func (s *Server) cachedProtocolDetail(ctx context.Context, key string, build func(context.Context) ProtocolDetailView) (view ProtocolDetailView, stale, ok bool) {
	s.protoDetailMu.Lock()
	s.protoDetailInitLocked()
	if e, has := s.protoDetailCache[key]; has {
		if time.Since(e.at) < protocolDetailTTL {
			s.protoDetailMu.Unlock()
			return e.view, false, true
		}
		s.protoDetailRefreshLocked(key, build) //nolint:contextcheck // intentional detach — the rebuild must outlive this request (see protoDetailRefreshLocked)
		s.protoDetailMu.Unlock()
		return e.view, true, true
	}
	done := s.protoDetailRefreshLocked(key, build) //nolint:contextcheck // intentional detach — a request that times out must not kill the fill
	s.protoDetailMu.Unlock()
	select {
	case <-done:
		s.protoDetailMu.Lock()
		e, has := s.protoDetailCache[key]
		s.protoDetailMu.Unlock()
		return e.view, false, has
	case <-ctx.Done():
		return ProtocolDetailView{}, false, false
	}
}

// protoDetailRefreshLocked kicks ONE detached rebuild for key (returning
// the existing flight's done channel when one is already up). Caller
// holds protoDetailMu. The rebuild runs on its own background context +
// protocolDetailRefreshTimeout — NEVER a request deadline — and on
// completion:
//
//   - a build that finished HEALTHY (analytics status "ok") replaces the
//     entry;
//   - a non-ok build — timed out OR degraded, including a FAST failure
//     (e.g. ClickHouse down makes every enrich error in milliseconds,
//     with rctx.Err() still nil) — is cached only when no HEALTHY entry
//     exists (registry-only beats 503 for a stone-cold key, and a
//     degraded entry may be refreshed by another degraded build). A
//     previously-good entry is NEVER displaced by a degraded build:
//     before this guard covered fast failures, one prewarm sweep during
//     a ClickHouse outage blanked every protocol page's analytics while
//     stamping the entries fresh.
//
// Every rebuild (request-kicked or prewarm) observes the paired
// stellarindex_protocol_detail_refresh_{total,duration_seconds} metrics.
func (s *Server) protoDetailRefreshLocked(key string, build func(context.Context) ProtocolDetailView) chan struct{} {
	if ch, inflight := s.protoDetailFlight[key]; inflight {
		return ch
	}
	done := make(chan struct{})
	s.protoDetailFlight[key] = done
	go func() {
		start := time.Now()
		rctx, cancel := context.WithTimeout(context.Background(), protocolDetailRefreshTimeout)
		defer cancel()
		view := build(rctx)

		outcome := "ok"
		switch {
		case rctx.Err() != nil:
			outcome = "timeout"
		case view.Analytics != nil && view.Analytics.Status == protocolAnalyticsUnavailable:
			outcome = "degraded"
		case view.Analytics != nil && view.Analytics.Status == protocolAnalyticsStale:
			// COMPLETE page, older bespoke block (served from the last-good
			// cache past its staleness horizon). Distinct from "degraded":
			// nothing is missing, so it still displaces the previous entry.
			outcome = "stale"
		}

		s.protoDetailMu.Lock()
		existing, exists := s.protoDetailCache[key]
		// Only a build that produced a COMPLETE page may displace an
		// existing HEALTHY entry. Checking rctx.Err() alone is not enough:
		// a fast-failing build (store down → enrich errors in ms) has
		// rctx.Err()==nil but an analytics-empty view — caching it over
		// good data would blank the page and stamp the blank fresh.
		if outcome == "ok" || outcome == "stale" || !exists || !protoDetailEntryHealthy(existing) {
			s.protoDetailCache[key] = protoDetailEntry{view: view, at: time.Now()}
		}
		delete(s.protoDetailFlight, key)
		s.protoDetailMu.Unlock()

		if outcome != "ok" && s.logger != nil {
			s.logger.Warn("protocol detail refresh degraded", "key", key, "outcome", outcome)
		}
		// Observe BEFORE closing done: waiters (incl. the prewarm sweep)
		// use the close as the completion edge, so observing after it
		// would let a sweep finish with its last observation unrecorded.
		obs.ProtocolDetailRefreshTotal.WithLabelValues(outcome).Inc()
		obs.ProtocolDetailRefreshDurationSeconds.WithLabelValues(outcome).Observe(time.Since(start).Seconds())
		close(done)
	}()
	return done
}

// protoDetailEntryHealthy reports whether a cached detail entry carries a
// COMPLETE page — status "ok", or "stale" (every panel present, the
// bespoke block served from the last-good cache). Those are the entries a
// DEGRADED rebuild must never displace; an "unavailable" entry is missing
// panels and any later build may replace it.
func protoDetailEntryHealthy(e protoDetailEntry) bool {
	if e.view.Analytics == nil {
		return false
	}
	return e.view.Analytics.Status == protocolAnalyticsOK ||
		e.view.Analytics.Status == protocolAnalyticsStale
}

// protocolDetailPrewarmPause is the gap between consecutive prewarm
// builds — one protocol-window at a time with a breather, so the sweep
// never stampedes the served tier / lake (it must be safe to run DURING
// replays; the whole point is that replay load is when cold builds die).
// A var only so the sweep regression test can shrink it; production
// never mutates it.
var protocolDetailPrewarmPause = 2 * time.Second

// protocolBespokeWindows is every window the ?days= whitelist admits
// (protocolBespokeWindowDays), default-first so the most-requested key
// (the no-param 90d page) warms before the drill-down windows.
var protocolBespokeWindows = []int{protocolActivityWindowDays, 1, 7, 30}

// PrewarmProtocolDetails refreshes EVERY (protocol, window) detail cache
// entry — all of protocolRegistry × protocolBespokeWindows — one build at
// a time with protocolDetailPrewarmPause between builds. Driven by
// cmd/stellarindex-api's dedicated prewarm goroutine (initial sweep at
// boot, then re-swept on a fixed sleep after each sweep completes), so
// every protocol page + window is warm BEFORE anyone asks and no user
// request ever pays for a cold analytics build. Refreshes
// unconditionally (no freshness check): the sweep cadence is chosen
// against protocolDetailTTL so entries are always fresh, and the load is
// deterministic. Shares the request path's single-flight, so an
// overlapping request-kicked refresh collapses into the same build.
func (s *Server) PrewarmProtocolDetails(ctx context.Context) {
	for _, meta := range protocolRegistry {
		for _, w := range protocolBespokeWindows {
			if ctx.Err() != nil {
				return
			}
			s.protoDetailMu.Lock()
			s.protoDetailInitLocked()
			done := s.protoDetailRefreshLocked(protocolDetailCacheKey(meta.Name, w), s.protocolDetailBuilder(meta, w)) //nolint:contextcheck // intentional detach — prewarm builds run on the refresh budget, not the sweep ctx
			s.protoDetailMu.Unlock()
			select {
			case <-done:
			case <-ctx.Done():
				return
			}
			select {
			case <-time.After(protocolDetailPrewarmPause):
			case <-ctx.Done():
				return
			}
		}
	}
}

// protocolDetailCacheKey is the single cache-key grammar for the protocol
// detail TTL cache — every dimension that changes the response (today: the
// protocol name and the bespoke ?days= window) goes through here, so a
// handler and any future prewarm path are physically incapable of keying
// the same request differently (the feedback_prewarm_handler_drift class).
func protocolDetailCacheKey(name string, windowDays int) string {
	return newCacheKey("protocolDetail").str(name).int(windowDays).build()
}

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

// protocolFastActivityReader is the OPTIONAL capability the daily
// pre-aggregation adds (BACKLOG #43). The handler type-asserts for it
// and probes availability (caching only definitive answers — see
// fastActivity); deployments without the contract_events_daily table
// stay on the raw scans transparently.
type protocolFastActivityReader interface {
	// DailyActivityAvailable reports whether the pre-aggregation is
	// usable. definitive=false means the probe got no authoritative
	// answer (transient store error) and the result must not be cached.
	DailyActivityAvailable(ctx context.Context) (available, definitive bool)
	ProtocolDailyActivityFast(ctx context.Context, contractIDs []string, sinceDay time.Time) ([]clickhouse.ProtocolDailyPoint, error)
	ProtocolEventBreakdownFast(ctx context.Context, contractIDs []string, sinceDay time.Time) ([]clickhouse.ProtocolEventTypeCount, error)
}

// ProtocolBespokeReader builds the per-category bespoke analytics block from the
// served-tier projected tables (TVL/volume/AUM/flows/feeds). Production wiring
// is timescale.Store. Nil → the bespoke block is absent (the rest of the page
// still serves).
type ProtocolBespokeReader interface {
	BuildProtocolBespoke(ctx context.Context, source, category string, windowDays int) (*timescale.BespokeBlock, error)
}

// bespokeFromStore maps the timescale-side block to the wire view (the two
// shapes are intentionally identical; timescale can't import v1).
func bespokeFromStore(b *timescale.BespokeBlock) *ProtocolBespoke {
	if b == nil {
		return nil
	}
	out := &ProtocolBespoke{Category: b.Category, Notes: b.Notes}
	for _, k := range b.KPIs {
		out.KPIs = append(out.KPIs, BespokeKPI{Label: k.Label, Value: k.Value, Unit: k.Unit, Hint: k.Hint})
	}
	for _, s := range b.Series {
		pts := make([]BespokeSeriesPt, 0, len(s.Points))
		for _, p := range s.Points {
			pts = append(pts, BespokeSeriesPt{Date: p.Date, Value: p.Value})
		}
		out.Series = append(out.Series, BespokeSeries{Name: s.Name, Unit: s.Unit, Points: pts})
	}
	for _, bd := range b.Breakdowns {
		rows := make([]BespokeBreakdownRow, 0, len(bd.Rows))
		for _, r := range bd.Rows {
			rows = append(rows, BespokeBreakdownRow{Label: r.Label, Value: r.Value, Count: r.Count})
		}
		out.Breakdowns = append(out.Breakdowns, BespokeBreakdown{Title: bd.Title, Unit: bd.Unit, Rows: rows})
	}
	for _, t := range b.Tables {
		out.Tables = append(out.Tables, BespokeTable{Title: t.Title, Columns: t.Columns, Rows: t.Rows})
	}
	return out
}

// ProtocolContractsReader is the read seam for the protocol_contracts
// registry (ADR-0035 factory-anchored gating). Production wiring is
// timescale.Store.ListProtocolContracts. Nil reader → contract lists
// and counts serve empty; never an error.
type ProtocolContractsReader interface {
	ListProtocolContracts(ctx context.Context, source string) ([]timescale.ProtocolContract, error)
	// ListSourceContractsFromProjection is the fallback roster for protocols
	// the protocol_contracts registry doesn't carry yet (only blend is seeded
	// today): the distinct contract ids from the source's projected table
	// (defindex_flows / phoenix_liquidity / comet_liquidity / aquarius_liquidity
	// / cctp_events / rozo_events). Returns nil for sources without one — the
	// page then keeps its registry/pairs path. Lets defindex/phoenix/comet/
	// aquarius/cctp/rozo show a full roster + the lake analytics scoped to it,
	// without waiting on the factory-enumeration team answer.
	ListSourceContractsFromProjection(ctx context.Context, source string) ([]string, error)
	// ProtocolContractIndex returns a contract_id → source map over every
	// registered protocol contract — the explorer's contract-attribution
	// overlay (the contracts directory + contract detail tag each contract
	// with its owning protocol). Returns an empty map (never an error) when
	// nothing is seeded.
	ProtocolContractIndex(ctx context.Context) (map[string]string, error)
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
	// TVL is the protocol's current pooled-liquidity value in USD
	// (background-refreshed snapshot — see [DEXTVLCache]). Absent for
	// protocols without an absolute reserve source, before the first
	// snapshot refresh, and when the cache isn't wired.
	TVL *ProtocolTVLView `json:"tvl,omitempty"`
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
	// Tokens is the ordered raw token contract C-strkeys the pool holds —
	// 2 for a pair, 3/4 for an Aquarius stableswap, N for a Comet weighted
	// pool, or the reserve-asset set for a lending market (blend). Absent for
	// non-pool contracts (factories, oracles). Parallel to TokenSymbols.
	Tokens []string `json:"tokens,omitempty"`
	// TokenSymbols is the human display symbols for Tokens, in the same
	// order ("XLM", "USDC", "AQUA", …). An unresolvable token degrades to a
	// short truncated contract ("CAS3…OWMA") rather than dropping — so this
	// stays parallel to Tokens.
	TokenSymbols []string `json:"token_symbols,omitempty"`
	// Pair is the roster's human label: TokenSymbols joined with "/" —
	// "XLM/USDC" for a pair, "XLM/USDC/USDT" for a 3-token stableswap, or the
	// reserve-asset list for a lending market. Absent when no tokens resolve.
	Pair string `json:"pair,omitempty"`
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

// ─── Bespoke per-category analytics (the Dune-surpassing block) ──────
//
// ProtocolBespoke is a generic rendering container — KPIs + named time-series
// + named top-N tables — filled with content BESPOKE to each protocol's
// category (lending shows TVL/borrows/utilization; a DEX shows swap volume +
// top pairs; a vault shows AUM + flows; a bridge shows transfer volume by
// domain; an oracle shows feeds + update cadence). The UI renders the three
// shapes generically, so adding/retuning a category's metrics is a server-side
// data change, not a new UI layout.

// ProtocolBespoke is the category-specific analytics block on
// GET /v1/protocols/{name}. Absent when no bespoke reader is wired or the
// category has none yet.
type ProtocolBespoke struct {
	// Category is the metric family: dex | amm | lending | vault | bridge | oracle.
	Category string `json:"category"`
	// KPIs are the headline numbers (pre-formatted) for the metric cards.
	KPIs []BespokeKPI `json:"kpis,omitempty"`
	// Series are named time-series for the charts (e.g. "USD volume", "TVL").
	Series []BespokeSeries `json:"series,omitempty"`
	// Breakdowns are named composition datasets for donut/pie rendering
	// (e.g. CCTP's "Inflows by source chain"), value-sorted descending.
	Breakdowns []BespokeBreakdown `json:"breakdowns,omitempty"`
	// Tables are named top-N tables (e.g. "Top pairs", "Supplied by asset").
	Tables []BespokeTable `json:"tables,omitempty"`
	// Notes are caveats/provenance lines rendered under the block.
	Notes []string `json:"notes,omitempty"`
}

// BespokeKPI is one headline metric card. Value is PRE-FORMATTED (the server
// owns formatting so the number is correct + ADR-0003-safe); Unit is advisory.
type BespokeKPI struct {
	Label string `json:"label"`
	Value string `json:"value"`
	Unit  string `json:"unit,omitempty"`
	Hint  string `json:"hint,omitempty"`
}

// BespokeSeries is a named time-series for a chart.
type BespokeSeries struct {
	Name   string            `json:"name"`
	Unit   string            `json:"unit,omitempty"`
	Points []BespokeSeriesPt `json:"points"`
}

// BespokeSeriesPt is one (date, value) point. Value is a numeric STRING
// (ADR-0003: amounts can exceed 2^53).
type BespokeSeriesPt struct {
	Date  string `json:"date"`
	Value string `json:"value"`
}

// BespokeBreakdown is a named composition dataset (the donut/pie complement
// to Series): value-weighted rows, window-scoped, sorted descending by the
// server. Generic — any category can adopt it.
type BespokeBreakdown struct {
	Title string                `json:"title"`
	Unit  string                `json:"unit,omitempty"`
	Rows  []BespokeBreakdownRow `json:"rows"`
}

// BespokeBreakdownRow is one composition slice. Value is a numeric STRING
// (ADR-0003); Count is the number of contributing transfers/events.
type BespokeBreakdownRow struct {
	Label string `json:"label"`
	Value string `json:"value"`
	Count int64  `json:"count"`
}

// BespokeTable is a named top-N table — columns + string rows (the server
// formats every cell).
type BespokeTable struct {
	Title   string     `json:"title"`
	Columns []string   `json:"columns"`
	Rows    [][]string `json:"rows"`
}

// ProtocolActivityPointView is one day of a protocol's event-activity series.
type ProtocolActivityPointView struct {
	Date   string `json:"date"`
	Events int64  `json:"events"`
}

// Analytics-status vocabulary for ProtocolAnalyticsStatus.Status.
const (
	// protocolAnalyticsOK: every analytics component (lake analytics +
	// bespoke) built successfully in this view's build.
	protocolAnalyticsOK = "ok"
	// protocolAnalyticsStale: the view was built healthy but is being
	// served past protocolDetailTTL while a background rebuild runs.
	protocolAnalyticsStale = "stale"
	// protocolAnalyticsUnavailable: at least one analytics component
	// failed or was skipped in this build — some analytics fields are
	// absent/zero because of DEGRADATION, not because the data is zero.
	protocolAnalyticsUnavailable = "unavailable"
)

// ProtocolAnalyticsStatus makes analytics degradation explicit on the
// wire. Before it, a failed bespoke build or lake fill silently omitted
// the block / zeroed events fields — indistinguishable client-side from
// a protocol with genuinely no activity (the 2026-07-31 replay-load
// failure). Clients: "ok" → trust the analytics; "stale" → real data, a
// refresh is in flight (AsOf says how old); "unavailable" → absence
// means degraded, render a hint, not a zero.
type ProtocolAnalyticsStatus struct {
	// Status is ok | stale | unavailable.
	Status string `json:"status"`
	// AsOf is when this view's analytics were built (RFC3339).
	AsOf string `json:"as_of,omitempty"`
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
	// and fast. Descending by count. Includes a synthetic "untyped" bucket
	// for events whose topic[0] isn't a denormalized Symbol in the lake
	// (predominantly AMM swap/sync events), so sum(EventBreakdown) reconciles
	// to EventsTotal.
	EventBreakdown []ProtocolEventTypeView `json:"event_breakdown,omitempty"`
	// ActivitySeries is the daily decoded-event count over the trailing
	// ActivityWindowDays — the protocol's on-chain activity chart.
	ActivitySeries []ProtocolActivityPointView `json:"activity_series,omitempty"`
	// ActivityWindowDays is the lookback all the analytics fields cover.
	ActivityWindowDays int `json:"activity_window_days,omitempty"`
	// EventsTotal is the contract-event count across every contract in the
	// protocol over ActivityWindowDays — the unfiltered total (= sum of
	// ActivitySeries = sum of EventBreakdown incl. the untyped bucket). NOT
	// the typed-breakdown sum, which excludes non-Symbol-topic'd events.
	EventsTotal int64 `json:"events_total,omitempty"`

	// Bespoke is the category-specific analytics block (TVL/volume/AUM/…) —
	// the Dune-surpassing, tailored-per-protocol content. Absent when no
	// bespoke reader is wired or the category has no bespoke metrics yet.
	// Check Analytics.Status to distinguish "legitimately none" (ok) from
	// "build degraded" (unavailable).
	Bespoke *ProtocolBespoke `json:"bespoke,omitempty"`

	// Analytics reports whether the analytics halves of this view (the
	// lake-derived fields + Bespoke) are fresh, stale-but-served, or
	// degraded — see ProtocolAnalyticsStatus.
	Analytics *ProtocolAnalyticsStatus `json:"analytics,omitempty"`
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
	tvls := s.protocolTVLs()

	view := ProtocolsView{Protocols: make([]ProtocolView, 0, len(protocolRegistry))}
	for _, meta := range protocolRegistry {
		contracts := s.protocolRoster(ctx, meta)
		row := buildProtocolView(meta, len(contracts), events, verdicts)
		attachProtocolTVL(&row, tvls)
		view.Protocols = append(view.Protocols, row)
	}
	view.TotalProtocols = len(view.Protocols)

	w.Header().Set("Cache-Control", "public, max-age=60")
	writeJSON(w, view, Flags{})
}

// protocolBespokeWindowDays parses the optional ?days= query param that
// windows the bespoke analytics block (the lake-analytics fields keep the
// fixed protocolActivityWindowDays lookback regardless). The whitelist is
// deliberate — each admitted window is a distinct cached scan over the
// projected tables on an unauthenticated endpoint, so arbitrary values
// would be an amplification surface (the same reasoning as the explorer
// contracts ladder, C3-009). ok=false ⇒ a problem+json 400 was written.
func protocolBespokeWindowDays(w http.ResponseWriter, r *http.Request) (int, bool) {
	raw := r.URL.Query().Get("days")
	switch raw {
	case "":
		return protocolActivityWindowDays, true
	case "1":
		return 1, true
	case "7":
		return 7, true
	case "30":
		return 30, true
	case "90":
		return 90, true
	}
	writeProblem(w, r,
		"https://api.stellarindex.io/errors/invalid-window",
		"Invalid days", http.StatusBadRequest,
		"days must be one of 1, 7, 30, 90")
	return 0, false
}

// handleProtocolDetail serves GET /v1/protocols/{name} — everything
// the directory row carries plus the contract registry, event-kind
// vocabulary and verification page. Unknown names 404. The optional
// ?days= param (1/7/30/90, default 90) windows the bespoke analytics
// block; anything else 400s.
func (s *Server) handleProtocolDetail(w http.ResponseWriter, r *http.Request) {
	meta, ok := protocolByName(r.PathValue("name"))
	if !ok {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/protocol-not-found",
			"Protocol not found", http.StatusNotFound,
			"unknown protocol name; GET /v1/protocols lists every known protocol")
		return
	}
	windowDays, ok := protocolBespokeWindowDays(w, r)
	if !ok {
		return
	}

	// This ceiling now only bounds how long a STONE-COLD request waits
	// for the detached build (built entries stale-serve instantly and
	// the build itself runs on protocolDetailRefreshTimeout, detached).
	// The prewarm sweep keeps every key built, so hitting this is the
	// exception (boot instant / brand-new deployment), and even then the
	// detached build survives the 503 so the retry lands warm.
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	// The cache key carries the bespoke window alongside the name — a
	// name-only key would let a ?days=7 hit serve the cached 90d view (or
	// vice versa). The no-param default builds the same key as an explicit
	// days=90, so the common path stays a single cached entry.
	view, stale, ok := s.cachedProtocolDetail(ctx, protocolDetailCacheKey(meta.Name, windowDays), s.protocolDetailBuilder(meta, windowDays))
	if !ok {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/protocol-detail-timeout",
			"Protocol detail timed out", http.StatusServiceUnavailable,
			"the protocol analytics are being recomputed; retry in a few seconds")
		return
	}
	if stale {
		view = staleProtocolDetail(view)
	}
	// The envelope flag tracks the ANALYTICS status, not just the cache
	// entry's age: a freshly-built view whose bespoke block came from the
	// last-good cache past its horizon is still stale data, and the wire
	// contract is that analytics.status="stale" always travels with
	// flags.stale (openapi: "flags.stale is set on the envelope too").
	staleFlag := stale || (view.Analytics != nil && view.Analytics.Status == protocolAnalyticsStale)

	w.Header().Set("Cache-Control", "public, max-age=60")
	writeJSON(w, view, Flags{Stale: staleFlag})
}

// protocolDetailBuilder returns the one build closure both the request
// path and the prewarm sweep use for (meta, windowDays) — a single
// construction site so the two paths are physically incapable of
// building different views for the same cache key (the
// feedback_prewarm_handler_drift class).
func (s *Server) protocolDetailBuilder(meta ProtocolMeta, windowDays int) func(context.Context) ProtocolDetailView {
	return func(c context.Context) ProtocolDetailView {
		return s.buildProtocolDetail(c, meta, windowDays)
	}
}

// buildProtocolDetail assembles the full detail view and stamps its
// analytics status: "ok" only when BOTH analytics halves (the lake
// analytics and the bespoke block) built healthy under a live context;
// "stale" when both are present but the bespoke block came from the
// last-good cache past its staleness horizon; otherwise "unavailable" —
// so a degraded build is explicit on the wire instead of masquerading as
// present zeros / silent absence.
func (s *Server) buildProtocolDetail(ctx context.Context, meta ProtocolMeta, windowDays int) ProtocolDetailView {
	contracts := s.protocolRoster(ctx, meta)
	classifyContractKinds(contracts, meta.Factories)
	s.enrichContractTokens(ctx, meta, contracts)
	v := ProtocolDetailView{
		ProtocolView:     buildProtocolView(meta, len(contracts), s.protocolEvents24h(ctx), s.protocolVerdicts(ctx)),
		Contracts:        contracts,
		EventKinds:       append([]string{}, meta.EventKinds...),
		VerificationPage: meta.VerificationPage,
	}
	attachProtocolTVL(&v.ProtocolView, s.protocolTVLs())
	lakeOK := s.enrichProtocolAnalytics(ctx, meta, &v)
	bespokeOK, bespokeStale := s.enrichBespoke(ctx, meta, &v, windowDays)
	status := protocolAnalyticsOK
	switch {
	case !lakeOK || !bespokeOK || ctx.Err() != nil:
		status = protocolAnalyticsUnavailable
	case bespokeStale:
		status = protocolAnalyticsStale
	}
	v.Analytics = &ProtocolAnalyticsStatus{Status: status, AsOf: time.Now().UTC().Format(time.RFC3339)}
	return v
}

// staleProtocolDetail is the serve-time overlay for an entry past its
// TTL: a shallow copy whose analytics status is downgraded ok→stale
// (an unavailable build stays unavailable — worst state wins). The
// cached entry itself is never mutated.
func staleProtocolDetail(v ProtocolDetailView) ProtocolDetailView {
	if v.Analytics == nil {
		return v
	}
	a := *v.Analytics
	if a.Status == protocolAnalyticsOK {
		a.Status = protocolAnalyticsStale
	}
	v.Analytics = &a
	return v
}

// enrichBespoke attaches the category-specific bespoke analytics block over
// the request's ?days= window (default protocolActivityWindowDays), degrading
// to absent only when NO block has ever been built for this key.
//
// The block comes from the last-good cache (protocol_bespoke_cache.go), so a
// build whose own battery is slow, failing, or starved keeps serving the
// previous block instead of dropping the page's visual suite — the §2.6b
// failure. Returns:
//
//   - ok: a block (or a legitimate "this category has none") was attached.
//     false ⇒ no reader wired, or the FIRST-EVER build for this key failed /
//     was starved / outran ctx; absence then means degradation and the caller
//     marks the view's analytics status accordingly.
//   - stale: the attached block is older than bespokeStaleAfter — real data,
//     but the refresh behind it has not landed for several sweeps, which the
//     caller surfaces as analytics.status "stale".
func (s *Server) enrichBespoke(ctx context.Context, meta ProtocolMeta, view *ProtocolDetailView, windowDays int) (ok, stale bool) {
	blk, stale, ok := s.cachedBespoke(ctx, meta.Name, meta.Category, windowDays)
	if !ok {
		return false, false
	}
	view.Bespoke = bespokeFromStore(blk)
	return true, stale
}

// classifyContractKinds tags each roster contract as "factory" (it is one of
// the protocol's verified trust-roots) or "instance" (a factory-deployed
// pool/vault/market). A contract already tagged (e.g. "module" for a folded-in
// sub-module contract) keeps its tag — only untagged rows are set to "instance".
func classifyContractKinds(contracts []ProtocolContractView, factories []string) {
	fset := make(map[string]struct{}, len(factories))
	for _, f := range factories {
		fset[f] = struct{}{}
	}
	for i := range contracts {
		if _, ok := fset[contracts[i].ContractID]; ok {
			contracts[i].Kind = "factory"
			continue
		}
		// Preserve a pre-set role (e.g. "module" for a folded-in sub-module
		// contract); only untagged rows default to "instance".
		if contracts[i].Kind == "" {
			contracts[i].Kind = "instance"
		}
	}
}

// protocolRoster returns meta's full contract roster: the registry / soroswap-
// pairs / projection contracts from protocolContracts, plus any ExtraContracts
// folded in from a sub-module source (the Blend Backstop's contracts belong to
// the Blend protocol page but live under the separate blend_backstop source).
// Extras are tagged Kind="module"; because protocolContractIDs scopes the lake
// analytics to every roster contract id, folding them here also pulls their
// events into the breakdown / activity / per-contract counts. Deduped against
// the base roster so a contract already present isn't doubled.
func (s *Server) protocolRoster(ctx context.Context, meta ProtocolMeta) []ProtocolContractView {
	rows := s.protocolContracts(ctx, meta.Name)
	if len(meta.ExtraContracts) == 0 {
		return rows
	}
	seen := make(map[string]struct{}, len(rows))
	for _, c := range rows {
		seen[c.ContractID] = struct{}{}
	}
	for _, id := range meta.ExtraContracts {
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		rows = append(rows, ProtocolContractView{ContractID: id, Kind: "module"})
	}
	return rows
}

// enrichProtocolAnalytics populates the lake-derived analytics on the detail
// view: the event-type breakdown, the daily activity series, and per-contract
// event counts merged onto the roster. Degrades to leaving the fields empty
// when the activity reader is nil or any query errors (same contract as the
// other optional joins — the directory + registry still serve). Returns
// whether the lake half is HEALTHY: true only when every fill completed
// (an empty roster is healthy — there is genuinely nothing to scope to);
// false on nil reader / unreadable tip / any failed fill, so the caller
// can mark the view's analytics status "unavailable" instead of letting
// the absent fields read as real zeros.
func (s *Server) enrichProtocolAnalytics(ctx context.Context, meta ProtocolMeta, view *ProtocolDetailView) bool {
	if s.protocolActivity == nil {
		return false
	}
	ids := protocolContractIDs(view.Contracts, meta.Factories)
	if len(ids) == 0 {
		return true
	}
	// All three analytics are bounded to the recent window: bounding by
	// ledger_seq prunes partitions, keeping each query well under the lake
	// reader's 30s budget even for the busiest protocols (an all-time scan ran
	// ~33s for blend / would be far worse for soroswap). An unreadable tip
	// skips the analytics entirely (degrade honestly, don't serve a
	// mislabeled window — a zero tip would make the fast path's day cutoff
	// collapse to yesterday while still claiming ActivityWindowDays).
	tip, err := s.protocolActivity.LakeTipLedger(ctx)
	if err != nil {
		s.logger.Warn("protocol activity tip read failed", "err", err)
		return false
	}
	plan := s.protocolActivityPlanFor(ctx, tip)
	view.ActivityWindowDays = protocolActivityWindowDays
	// The three lake reads are independent (~5s each on a cold cache) and
	// write disjoint fields of the view (ActivitySeries+EventsTotal /
	// EventBreakdown / Contracts[].Events), so run them concurrently rather
	// than serially — cutting the cold-path from ~15s to ~5s (audit
	// 2026-06-19 item 8; the cache + prewarm make repeat hits instant, this
	// keeps the detached rebuild fast). All three share ONE plan (one tip
	// read, one fast-vs-raw decision) so the series and breakdown can't
	// split across the fast/raw sources and de-reconcile. The breakdown's
	// "untyped" reconciling bucket needs EventsTotal (from the series), so
	// it's appended single-threaded after the barrier.
	var wg sync.WaitGroup
	var seriesOK, breakdownOK, contractsOK bool
	wg.Add(3)
	go func() { defer wg.Done(); seriesOK = s.fillProtocolSeries(ctx, meta.Name, ids, plan, view) }()
	go func() { defer wg.Done(); breakdownOK = s.fillProtocolBreakdown(ctx, meta.Name, ids, plan, view) }()
	go func() {
		defer wg.Done()
		contractsOK = s.fillProtocolContractActivity(ctx, meta.Name, ids, plan.sinceLedger, view)
	}()
	wg.Wait()
	reconcileProtocolBreakdown(view)
	return seriesOK && breakdownOK && contractsOK
}

// reconcileProtocolBreakdown appends the synthetic "untyped" bucket so the
// event breakdown sums to EventsTotal (the unfiltered window total set by
// fillProtocolSeries). The gap is events whose topic[0] symbol the lake
// didn't denormalize — predominantly AMM swap/sync — see fillProtocolBreakdown.
// Run after the parallel reads so EventsTotal is known.
func reconcileProtocolBreakdown(view *ProtocolDetailView) {
	var typedSum int64
	for _, b := range view.EventBreakdown {
		typedSum += b.Count
	}
	if untyped := view.EventsTotal - typedSum; untyped > 0 {
		view.EventBreakdown = append(view.EventBreakdown, ProtocolEventTypeView{EventType: "untyped", Count: untyped})
	}
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

// fillProtocolBreakdown populates EventBreakdown (degrades on error).
//
// The breakdown groups by topic[0]'s denormalized symbol (topic_0_sym),
// which the lake only populates when topic[0] is a plain Symbol SCVal.
// Many Soroban DEX events carry a non-Symbol topic[0] — Soroswap's
// swap/sync events are the dominant case (190k+ over a 90d window with an
// empty topic_0_sym) — so the typed breakdown alone under-counts the true
// event total by a wide margin, and is empty entirely for protocols whose
// every event is non-Symbol-topic'd (the phoenix "empty breakdown but the
// chart has data" case). To keep the breakdown reconciled with EventsTotal
// (which fillProtocolSeries sets from the unfiltered count), append a
// synthetic "untyped" bucket carrying the remainder. EventsTotal must
// already be set (series is filled first in enrichProtocolAnalytics).
func (s *Server) fillProtocolBreakdown(ctx context.Context, name string, ids []string, plan protocolActivityPlan, view *ProtocolDetailView) bool {
	breakdown, err := s.protocolBreakdown(ctx, ids, plan)
	if err != nil {
		s.logger.Warn("protocol event breakdown failed", "source", name, "err", err)
		return false
	}
	view.EventBreakdown = make([]ProtocolEventTypeView, 0, len(breakdown)+1)
	for _, b := range breakdown {
		view.EventBreakdown = append(view.EventBreakdown, ProtocolEventTypeView{EventType: b.EventType, Count: int64(b.Count)})
	}
	// The reconciling "untyped" remainder bucket is appended by
	// reconcileProtocolBreakdown after the parallel reads complete (it needs
	// EventsTotal, which fillProtocolSeries sets concurrently).
	return true
}

// fillProtocolSeries populates the daily ActivitySeries + EventsTotal
// (degrades on error). EventsTotal is the unfiltered contract-event count
// over the window (the sum of the daily points), which is the
// authoritative total the breakdown reconciles against — NOT the typed
// breakdown sum, which excludes non-Symbol-topic'd events.
func (s *Server) fillProtocolSeries(ctx context.Context, name string, ids []string, plan protocolActivityPlan, view *ProtocolDetailView) bool {
	series, err := s.protocolSeries(ctx, ids, plan)
	if err != nil {
		s.logger.Warn("protocol daily activity failed", "source", name, "err", err)
		return false
	}
	view.ActivitySeries = make([]ProtocolActivityPointView, 0, len(series))
	var total int64
	for _, p := range series {
		view.ActivitySeries = append(view.ActivitySeries, ProtocolActivityPointView{Date: p.Date, Events: int64(p.Events)})
		total += int64(p.Events)
	}
	view.EventsTotal = total
	return true
}

// fillProtocolContractActivity merges per-contract event counts + last-seen onto
// the roster (degrades on error).
func (s *Server) fillProtocolContractActivity(ctx context.Context, name string, ids []string, since uint32, view *ProtocolDetailView) bool {
	act, err := s.protocolActivity.ProtocolContractActivity(ctx, ids, since)
	if err != nil {
		s.logger.Warn("protocol contract activity failed", "source", name, "err", err)
		return false
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
	return true
}

// protocolActivityPlanFor derives the shared analytics plan from ONE lake
// tip read: the ledger cutoff (tip − window) plus, when the daily
// pre-aggregation is usable, the fast reader and its day-grain cutoff.
// The caller has already verified tip was read successfully.
func (s *Server) protocolActivityPlanFor(ctx context.Context, tip uint32) protocolActivityPlan {
	since := uint32(1) // whole chain inside the window
	if tip > protocolActivityWindowLedgers {
		since = tip - protocolActivityWindowLedgers
	}
	plan := protocolActivityPlan{sinceLedger: since}
	if fast := s.fastActivity(ctx); fast != nil {
		plan.fast = fast
		plan.sinceDay = protocolSinceDay(since, tip)
	}
	return plan
}

// buildProtocolView projects one registry entry + the dynamic joins
// into the directory wire shape.
func buildProtocolView(meta ProtocolMeta, contractCount int, events map[string]int64, verdicts map[string]timescale.CompletenessSnapshot) ProtocolView {
	events24h := events[meta.Name]
	// Fold in any sub-module source's 24h count (e.g. blend_backstop into
	// blend) so the protocol's headline event count reflects the whole
	// surface the page now shows.
	for _, src := range meta.ExtraEventSources {
		events24h += events[src]
	}
	v := ProtocolView{
		Name:          meta.Name,
		Category:      meta.Category,
		Description:   meta.Description,
		GenesisLedger: meta.GenesisLedger,
		Factories:     append([]string{}, meta.Factories...),
		ContractCount: contractCount,
		Events24h:     events24h,
	}
	if sn, ok := verdicts[meta.Name]; ok {
		v.Completeness = &ProtocolCompletenessView{
			Complete:        sn.Complete,
			WatermarkLedger: sn.Watermark,
		}
	}
	return v
}

// protocolTVLs reads the per-protocol TVL snapshot, degrading to nil
// (the tvl field stays absent everywhere) when the cache isn't wired
// or hasn't completed its first background refresh.
func (s *Server) protocolTVLs() map[string]ProtocolTVLView {
	if s.dexTVL == nil {
		return nil
	}
	snap, _ := s.dexTVL.Snapshot()
	return snap
}

// attachProtocolTVL joins the snapshot's TVL entry (if any) onto a
// directory row.
func attachProtocolTVL(view *ProtocolView, tvls map[string]ProtocolTVLView) {
	if t, ok := tvls[view.Name]; ok {
		view.TVL = &t
	}
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
	if len(rows) == 0 {
		// The protocol_contracts registry is empty for this source (only blend
		// is seeded today). Fall back to the contracts the decoder has actually
		// captured into the projected table, so defindex/phoenix/comet/cctp/rozo
		// get a real roster + the lake analytics scoped to it.
		return s.protocolContractsFromProjection(ctx, name)
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

// protocolContractsFromProjection is the registry-empty fallback: the distinct
// contracts from name's projected table (nil/empty when the source has no
// per-contract table). aquarius now populates here from aquarius_liquidity
// (2026-07-07, #91 — previously read 0 pools despite being the busiest AMM);
// the oracles (band/reflector-*/redstone) populate from their pinned contracts
// in oracle_updates via a source-scoped query (#91 — they read 0 before). Only
// sdex is op-keyed (no contract) and truly has no roster here.
func (s *Server) protocolContractsFromProjection(ctx context.Context, name string) []ProtocolContractView {
	ids, err := s.protocolContractsReader.ListSourceContractsFromProjection(ctx, name)
	if err != nil {
		s.logger.Warn("protocols projection roster read failed", "source", name, "err", err)
		return []ProtocolContractView{}
	}
	out := make([]ProtocolContractView, 0, len(ids))
	for _, id := range ids {
		out = append(out, ProtocolContractView{ContractID: id})
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

// protocolSinceDay converts the ledger-window cutoff into the daily
// table's day grain: the activity window is expressed in ledgers from
// tip (protocolActivityWindowLedgers); days = ledgers / ~17280.
func protocolSinceDay(sinceLedger, tip uint32) time.Time {
	if tip == 0 || sinceLedger >= tip {
		return time.Now().UTC().AddDate(0, 0, -1)
	}
	days := int((tip-sinceLedger)/17280) + 1
	return time.Now().UTC().AddDate(0, 0, -days)
}

// fastActivity returns the fast reader when the pre-aggregation is
// available on this deployment. The probe answer is cached only when it
// is DEFINITIVE (table missing, or rows found) — a transient ClickHouse
// blip on the FIRST probe must not latch the raw 12B-row scans for the
// process lifetime (the C1-048 class; the schemaProbe in
// internal/storage/clickhouse is the founding precedent for why a
// sync.Once is the wrong primitive here). A non-definitive answer
// degrades THIS call to the raw readers and re-probes next time.
func (s *Server) fastActivity(ctx context.Context) protocolFastActivityReader {
	fast, ok := s.protocolActivity.(protocolFastActivityReader)
	if !ok {
		return nil
	}
	s.protocolFastMu.Lock()
	defer s.protocolFastMu.Unlock()
	if !s.protocolFastSettled {
		avail, definitive := fast.DailyActivityAvailable(ctx)
		if definitive {
			s.protocolFastSettled = true
			s.protocolFastOK = avail
		}
		if !avail {
			return nil
		}
		return fast
	}
	if !s.protocolFastOK {
		return nil
	}
	return fast
}

// protocolActivityPlan is the ONE shared fast-vs-raw decision + window
// derivation for a detail build's three concurrent analytics fills.
// Before it, each fill probed fastActivity and re-read the lake tip
// independently — discarding the tip error, so a failed read (tip==0)
// made protocolSinceDay silently serve a 1-day window labeled 90d, and
// two fills could take DIFFERENT fast/raw paths, breaking the
// sum(EventBreakdown)==EventsTotal reconcile.
type protocolActivityPlan struct {
	// sinceLedger is the raw readers' cutoff (tip − window), always set.
	sinceLedger uint32
	// fast is non-nil when the daily pre-aggregation serves this build;
	// sinceDay is then its day-grain cutoff (derived from the same tip).
	fast     protocolFastActivityReader
	sinceDay time.Time
}

func (s *Server) protocolBreakdown(ctx context.Context, ids []string, plan protocolActivityPlan) ([]clickhouse.ProtocolEventTypeCount, error) {
	if plan.fast != nil {
		out, err := plan.fast.ProtocolEventBreakdownFast(ctx, ids, plan.sinceDay)
		if err == nil {
			return out, nil
		}
		s.logger.Warn("fast breakdown failed; raw fallback", "err", err)
	}
	return s.protocolActivity.ProtocolEventBreakdown(ctx, ids, plan.sinceLedger)
}

func (s *Server) protocolSeries(ctx context.Context, ids []string, plan protocolActivityPlan) ([]clickhouse.ProtocolDailyPoint, error) {
	if plan.fast != nil {
		out, err := plan.fast.ProtocolDailyActivityFast(ctx, ids, plan.sinceDay)
		if err == nil {
			return out, nil
		}
		s.logger.Warn("fast series failed; raw fallback", "err", err)
	}
	return s.protocolActivity.ProtocolDailyActivity(ctx, ids, plan.sinceLedger)
}
