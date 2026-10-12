package v1

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/explorer"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// protocolDetailTTL is the freshness horizon of a cached /v1/protocols/{name} detail. Past it the
// entry is served STALE (flags.stale + analytics.status="stale") while a detached single-flight
// rebuild runs, so a built page never blanks back to a cold 503.
const protocolDetailTTL = 20 * time.Minute

// protocolDetailRefreshTimeout bounds one detached rebuild so a wedged query cannot pin the
// single-flight.
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

// cachedProtocolDetail returns the detail view for `key` with stale-while-revalidate semantics:
//
//   - fresh entry: served as-is.
//   - past protocolDetailTTL: served immediately (stale=true) while ONE detached rebuild runs.
//   - never built: wait for the detached single-flight build up to the caller's deadline. The build
//     is NOT bound to that deadline (a request-bound build can be cancelled forever and never fill
//     the cache), so a request that times out 503s but the retry lands warm.
//
// The key is protocolDetailCacheKey (name AND bespoke window): the bespoke block varies with
// ?days=, so a name-only key would serve one window's numbers to another's request. ok=false only
// on the cold path, when the caller's context is cancelled or the build left no cacheable entry.
func (s *Server) cachedProtocolDetail(ctx context.Context, key string, build func(context.Context) ProtocolDetailView) (view ProtocolDetailView, at time.Time, stale, ok bool) {
	s.protoDetailMu.Lock()
	s.protoDetailInitLocked()
	if e, has := s.protoDetailCache[key]; has {
		if time.Since(e.at) < protocolDetailTTL {
			s.protoDetailMu.Unlock()
			return e.view, e.at, false, true
		}
		s.protoDetailRefreshLocked(key, build) //nolint:contextcheck // intentional detach — the rebuild must outlive this request (see protoDetailRefreshLocked)
		s.protoDetailMu.Unlock()
		return e.view, e.at, true, true
	}
	done := s.protoDetailRefreshLocked(key, build) //nolint:contextcheck // intentional detach — a request that times out must not kill the fill
	s.protoDetailMu.Unlock()
	select {
	case <-done:
		s.protoDetailMu.Lock()
		e, has := s.protoDetailCache[key]
		s.protoDetailMu.Unlock()
		return e.view, e.at, false, has
	case <-ctx.Done():
		return ProtocolDetailView{}, time.Time{}, false, false
	}
}

// protoDetailRefreshLocked kicks ONE detached rebuild for key, returning the existing flight's done
// channel when one is already up. Caller holds protoDetailMu. The rebuild runs on its own
// background context + protocolDetailRefreshTimeout, NEVER a request deadline. A non-ok build
// (including a FAST failure such as ClickHouse down) is cached only when no HEALTHY entry exists:
// registry-only beats 503 for a cold key, but a prewarm sweep during an outage must not blank
// every page's analytics while stamping them fresh.
func (s *Server) protoDetailRefreshLocked(key string, build func(context.Context) ProtocolDetailView) chan struct{} {
	if ch, inflight := s.protoDetailFlight[key]; inflight {
		return ch
	}
	done := make(chan struct{})
	s.protoDetailFlight[key] = done
	go func() {
		// Deferred so a panic in build cannot skip deregistering the flight: the key would wedge
		// permanently, with waiters on a channel that never closes. A panic counts as "degraded"
		// so an already-HEALTHY cached page survives.
		defer func() {
			if rec := recover(); rec != nil {
				worker.Report(s.logger, "api-protocol-detail-refresh", rec)
				obs.ProtocolDetailRefreshTotal.WithLabelValues("degraded").Inc()
			}
			s.endProtoDetailFlight(key, done)
		}()
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
			// Complete page with an older bespoke block; unlike "degraded" it may displace the entry.
			outcome = "stale"
		}

		s.protoDetailMu.Lock()
		existing, exists := s.protoDetailCache[key]
		// rctx.Err() alone is not enough: a fast-failing build (store down) has it nil but an
		// analytics-empty view.
		if outcome == "ok" || outcome == "stale" || !exists || !protoDetailEntryHealthy(existing) {
			s.protoDetailCache[key] = protoDetailEntry{view: view, at: time.Now()}
		}
		s.protoDetailMu.Unlock()

		if outcome != "ok" && s.logger != nil {
			s.logger.Warn("protocol detail refresh degraded", "key", key, "outcome", outcome)
		}
		// Observe BEFORE closing done: waiters use the close as the completion edge.
		obs.ProtocolDetailRefreshTotal.WithLabelValues(outcome).Inc()
		obs.ProtocolDetailRefreshDurationSeconds.WithLabelValues(outcome).Observe(time.Since(start).Seconds())
	}()
	return done
}

// endProtoDetailFlight deregisters the in-flight rebuild for key and releases its waiters. It is
// deferred so it runs on EVERY exit including a panic.
func (s *Server) endProtoDetailFlight(key string, done chan struct{}) {
	s.protoDetailMu.Lock()
	delete(s.protoDetailFlight, key)
	s.protoDetailMu.Unlock()
	close(done)
}

// protoDetailEntryHealthy reports whether a cached entry carries a COMPLETE page: status "ok", or
// "stale" (every panel present, bespoke block from the last-good cache). A DEGRADED rebuild must
// never displace these; an "unavailable" entry is missing panels and any build may replace it.
func protoDetailEntryHealthy(e protoDetailEntry) bool {
	if e.view.Analytics == nil {
		return false
	}
	return e.view.Analytics.Status == protocolAnalyticsOK ||
		e.view.Analytics.Status == protocolAnalyticsStale
}

// protocolDetailPrewarmPause spaces prewarm builds so the sweep never stampedes the served tier or
// lake and stays safe DURING replays. A var only so the sweep test can shrink it.
var protocolDetailPrewarmPause = 2 * time.Second

// protocolBespokeWindows is every window the ?days= whitelist admits, default-first so the
// most-requested no-param page warms first.
var protocolBespokeWindows = []int{protocolActivityWindowDays, 1, 7, 30}

// PrewarmProtocolDetails refreshes EVERY (protocol, window) detail entry one build at a time, sharing the request path's single-flight.
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

// protocolDetailCacheKey is the single cache-key grammar for the detail cache, so the handler and
// the prewarm path cannot key the same request differently.
func protocolDetailCacheKey(name string, windowDays int) string {
	return newCacheKey("protocolDetail").str(name).int(windowDays).build()
}

// protocolActivityWindowDays is the lookback for the windowed per-protocol analytics, bounded so
// the lake query prunes partitions on the multi-billion-row table.
const protocolActivityWindowDays = 90

// protocolActivityWindowLedgers is protocolActivityWindowDays in ledgers (5s close, 17,280/day).
const protocolActivityWindowLedgers = protocolActivityWindowDays * 17280

// ProtocolActivityReader serves per-protocol analytics from the certified lake, scoped to a contract-id set; a nil reader serves analytics fields empty.
type ProtocolActivityReader interface {
	LakeTipLedger(ctx context.Context) (uint32, error)
	ProtocolEventBreakdown(ctx context.Context, contractIDs []string, sinceLedger uint32) ([]clickhouse.ProtocolEventTypeCount, error)
	ProtocolDailyActivity(ctx context.Context, contractIDs []string, sinceLedger uint32) ([]clickhouse.ProtocolDailyPoint, error)
	ProtocolContractActivity(ctx context.Context, contractIDs []string, sinceLedger uint32) ([]clickhouse.ProtocolContractActivity, error)
}

// protocolFastActivityReader is the OPTIONAL daily pre-aggregation capability. The handler
// type-asserts for it and caches only definitive availability answers (see fastActivity);
// deployments without contract_events_daily stay on the raw scans.
type protocolFastActivityReader interface {
	// DailyActivityAvailable reports whether the pre-aggregation is usable; definitive=false means
	// a transient store error, so the result must not be cached.
	DailyActivityAvailable(ctx context.Context) (available, definitive bool)
	ProtocolDailyActivityFast(ctx context.Context, contractIDs []string, sinceDay time.Time) ([]clickhouse.ProtocolDailyPoint, error)
	ProtocolEventBreakdownFast(ctx context.Context, contractIDs []string, sinceDay time.Time) ([]clickhouse.ProtocolEventTypeCount, error)
	ProtocolContractActivityFast(ctx context.Context, contractIDs []string, sinceDay time.Time) ([]clickhouse.ProtocolContractActivity, error)
}

// ProtocolBespokeReader builds the per-category bespoke analytics block from served-tier projected tables; nil omits the block, not the page.
type ProtocolBespokeReader interface {
	BuildProtocolBespoke(ctx context.Context, source, category string, windowDays int) (*timescale.BespokeBlock, error)
}

// bespokeFromStore maps the timescale-side block to the wire view; timescale cannot import v1.
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
		rows := t.Rows
		if rows == nil {
			// An empty table serves `rows: []`, never null — the spec types rows as an array.
			rows = [][]string{}
		}
		out.Tables = append(out.Tables, BespokeTable{Title: t.Title, Columns: t.Columns, Rows: rows})
	}
	return out
}

// ProtocolContractsReader is the read seam for the protocol_contracts registry (ADR-0035); a nil reader serves contract lists and counts empty, never an error.
type ProtocolContractsReader interface {
	ListProtocolContracts(ctx context.Context, source string) ([]timescale.ProtocolContract, error)
	// ListSourceContractsFromProjection is the fallback roster for protocols the registry does not
	// carry: distinct contract ids from the source's projected table, nil when it has none.
	ListSourceContractsFromProjection(ctx context.Context, source string) ([]string, error)
	// ProtocolContractIndex returns a contract_id -> source map over every registered protocol
	// contract, the explorer's attribution overlay. Empty map (never an error) when unseeded.
	ProtocolContractIndex(ctx context.Context) (map[string]string, error)
}

// protocolContractCounter is the OPTIONAL count-without-enumeration capability the contracts reader
// may add. It exists for a protocol whose contract set dwarfs the roster paths' LIMIT 5000: a
// capped roster would publish 5,000, an unenumerated one 0, both wrong where an exact COUNT is cheap.
type protocolContractCounter interface {
	// CountSourceContracts returns the exact total; ok=false with a nil error means no count-only
	// path, so the caller counts its roster.
	CountSourceContracts(ctx context.Context, source string) (int64, bool, error)
}

// ProtocolStatsReader supplies the trailing-24h event count per source; a nil reader serves events_24h as 0.
type ProtocolStatsReader interface {
	CountRecentEventsBySource(ctx context.Context) (map[string]int64, error)
}

// SoroswapPairsReader exposes the soroswap_pairs registry, Soroswap's equivalent of protocol_contracts; a nil reader serves its contract list and count empty.
type SoroswapPairsReader interface {
	LoadSoroswapPairRegistry(ctx context.Context) ([]timescale.SoroswapPair, error)
}

// ProtocolCompletenessView is the headline slice of /v1/coverage's verdict joined onto a protocol row.
type ProtocolCompletenessView struct {
	// Complete is the headline ADR-0033 verdict.
	Complete bool `json:"complete"`
	// WatermarkLedger is the highest ledger the verdict covers.
	WatermarkLedger uint32 `json:"watermark_ledger"`
	// ProjectionVerifiedFrom is the bottom of the range Complete is a claim about; without it
	// Complete reads as a claim back to genesis_ledger, wrong for a served tier starting
	// mid-history. See [CoverageVerdictView.ProjectionVerifiedFrom].
	ProjectionVerifiedFrom uint32 `json:"projection_verified_from,omitempty"`
}

// ProtocolView is one directory row on GET /v1/protocols.
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
	// ContractCount is the number of registered contract instances (protocol_contracts rows;
	// soroswap_pairs rows for soroswap). It is the TRUE total, which for a counted-not-enumerated
	// protocol exceeds len(Contracts) on the detail view; see protocolContractCounter.
	ContractCount int `json:"contract_count"`
	// Events24h is the trailing-24h decoded-event count across the
	// protocol's served tables.
	Events24h int64 `json:"events_24h"`
	// Completeness is the latest ADR-0033 verdict summary, absent when
	// no completeness snapshot exists for this source.
	Completeness *ProtocolCompletenessView `json:"completeness,omitempty"`
	// TVL is the current pooled-liquidity value in USD from the [DEXTVLCache] snapshot; absent
	// without an absolute reserve source, before the first refresh, or with no cache wired.
	TVL *ProtocolTVLView `json:"tvl,omitempty"`
}

// ProtocolsView is the envelope data field of GET /v1/protocols.
type ProtocolsView struct {
	// Protocols lists every indexed protocol in registry order.
	Protocols []ProtocolView `json:"protocols"`
	// TotalProtocols is len(protocols), a COUNT of directory rows, not the money figure TVLTotal.
	TotalProtocols int `json:"total_protocols"`
	// TVLTotal is the reconciled pooled-liquidity total across the protocols carrying a `tvl`
	// block on this response; absent on cold start or with no TVL derivation.
	TVLTotal *DEXTVLTotalView `json:"tvl_total,omitempty"`
	// CoverageNote is non-empty when a source's roster read failed with no last-good count, so the
	// source is OMITTED from `protocols` rather than published with a fabricated contract_count: 0.
	// Absent means the directory is complete.
	CoverageNote string `json:"coverage_note,omitempty"`
}

// ProtocolContractView is one registered contract on GET /v1/protocols/{name}, unifying protocol_contracts and soroswap_pairs.
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
	// Tokens is the ordered raw token contract C-strkeys the pool holds (2 for a pair, 3/4 for an
	// Aquarius stableswap, N for a Comet pool, the reserve set for a blend market); absent for
	// non-pool contracts. Parallel to TokenSymbols.
	Tokens []string `json:"tokens,omitempty"`
	// TokenSymbols are the display symbols for Tokens, in order. An unresolvable token degrades to
	// a truncated contract ("CAS3…OWMA") rather than dropping, to stay parallel to Tokens.
	TokenSymbols []string `json:"token_symbols,omitempty"`
	// Pair is TokenSymbols joined with "/"; absent when no tokens resolve.
	Pair string `json:"pair,omitempty"`
	// Kind is "factory" (a verified trust-root in meta.Factories) or "instance" (a pool, vault or
	// market the factory deployed).
	Kind string `json:"kind,omitempty"`
	// Events is the all-time decoded contract-event count from the lake; 0/absent without an
	// activity reader.
	Events int64 `json:"events,omitempty"`
	// LastSeen is the close time of the instance's most recent event (RFC3339); absent when unknown.
	LastSeen string `json:"last_seen,omitempty"`
}

// ProtocolEventTypeView is one slice of a protocol's all-time event-type distribution: a topic[0] symbol and its count.
type ProtocolEventTypeView struct {
	EventType string `json:"event_type"`
	Count     int64  `json:"count"`
}

// ─── Bespoke per-category analytics ──────
//
// ProtocolBespoke is a generic rendering container (KPIs + named time-series + named top-N tables)
// filled per protocol category. The UI renders the three shapes generically, so retuning a
// category's metrics is a server-side data change.

// ProtocolBespoke is the category-specific analytics block on GET /v1/protocols/{name}, absent when no bespoke reader is wired.
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

// BespokeKPI is one headline metric card; Value is PRE-FORMATTED server-side (ADR-0003-safe) and Unit is advisory.
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

// BespokeSeriesPt is one (date, value) point; Value is a numeric STRING (ADR-0003).
type BespokeSeriesPt struct {
	Date  string `json:"date"`
	Value string `json:"value"`
}

// BespokeBreakdown is a named, window-scoped composition dataset sorted descending by the server; any category can adopt it.
type BespokeBreakdown struct {
	Title string                `json:"title"`
	Unit  string                `json:"unit,omitempty"`
	Rows  []BespokeBreakdownRow `json:"rows"`
}

// BespokeBreakdownRow is one composition slice; Value is a numeric STRING (ADR-0003) and Count the contributing events.
type BespokeBreakdownRow struct {
	Label string `json:"label"`
	Value string `json:"value"`
	Count int64  `json:"count"`
}

// BespokeTable is a named top-N table of columns and server-formatted string rows.
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
	// protocolAnalyticsOK: every analytics component (lake + bespoke) built successfully.
	protocolAnalyticsOK = "ok"
	// protocolAnalyticsStale: built healthy, served past protocolDetailTTL during a rebuild.
	protocolAnalyticsStale = "stale"
	// protocolAnalyticsUnavailable: a component failed or was skipped, so absent/zero fields mean
	// DEGRADATION, not zero data.
	protocolAnalyticsUnavailable = "unavailable"
)

// ProtocolAnalyticsStatus makes analytics degradation explicit ("ok", "stale", "unavailable"); on "unavailable" render a hint, not a zero.
type ProtocolAnalyticsStatus struct {
	// Status is ok | stale | unavailable.
	Status string `json:"status"`
	// AsOf is when this view's analytics were built (RFC3339).
	AsOf string `json:"as_of,omitempty"`
}

// ProtocolDetailView is the GET /v1/protocols/{name} data: the directory row plus contract registry, decoded event vocabulary and verification write-up path.
type ProtocolDetailView struct {
	ProtocolView
	// Contracts lists every registered instance; empty for sources without a contract registry
	// AND for a source whose instances are counted rather than enumerated (see
	// protocolContractCounter). An empty list never contradicts a non-zero ContractCount.
	Contracts []ProtocolContractView `json:"contracts"`
	// EventKinds lists the EventKind() discriminators the source's
	// decoder emits.
	EventKinds []string `json:"event_kinds"`
	// VerificationPage is the repo-relative path of the protocol's
	// verification write-up, absent when none exists yet.
	VerificationPage string `json:"verification_page,omitempty"`

	// verdictsStale is the /v1/coverage freshness gate for the embedded verdict; it raises flags.stale.
	verdictsStale bool

	// ─── Lake analytics (populated when the activity reader is wired) ──

	// EventBreakdown is the event-type distribution (topic[0] symbol -> count) over the trailing
	// ActivityWindowDays, descending. A synthetic "untyped" bucket covers events whose topic[0] is
	// not a denormalized Symbol, so sum(EventBreakdown) reconciles to EventsTotal.
	EventBreakdown []ProtocolEventTypeView `json:"event_breakdown,omitempty"`
	// ActivitySeries is the daily decoded-event count over the trailing
	// ActivityWindowDays — the protocol's on-chain activity chart.
	ActivitySeries []ProtocolActivityPointView `json:"activity_series,omitempty"`
	// ActivityWindowDays is the lookback all the analytics fields cover.
	ActivityWindowDays int `json:"activity_window_days,omitempty"`
	// EventsTotal is the unfiltered contract-event count over ActivityWindowDays (= sum of
	// ActivitySeries = sum of EventBreakdown incl. untyped), NOT the typed-breakdown sum.
	EventsTotal int64 `json:"events_total,omitempty"`

	// Bespoke is the category-specific analytics block (TVL, volume, AUM and so on). Absent when
	// no bespoke reader is wired or the category has no bespoke metrics yet; Analytics.Status
	// distinguishes "legitimately none" (ok) from "build degraded" (unavailable).
	Bespoke *ProtocolBespoke `json:"bespoke,omitempty"`

	// Analytics reports whether the lake-derived fields and Bespoke are fresh, stale or degraded.
	Analytics *ProtocolAnalyticsStatus `json:"analytics,omitempty"`
}

// handleProtocolsList serves GET /v1/protocols, the protocol directory. The static registry always
// serves; the dynamic joins (24h events, completeness verdicts) degrade to zeros/absent when their
// reader is nil or errors.
//
// contract_count comes from the roster-count cache (protocol_roster_cache.go): a registry-empty
// source's roster is a `SELECT DISTINCT ... LIMIT 5000` scan on an unauthenticated route, so
// scanning per miss would be a DoS surface. When a read FAILS with no last-good count the source
// is OMITTED and named in coverage_note, since a failed read is not a zero.
func (s *Server) handleProtocolsList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	events, eventsOK := s.protocolEvents24h(ctx)
	verdicts, verdictsOK, verdictsStale := s.protocolVerdicts(ctx)
	tvls, tvlTotal := s.protocolTVLsAndTotal()

	view := ProtocolsView{Protocols: make([]ProtocolView, 0, len(protocolRegistry))}
	var degraded []string
	for _, meta := range protocolRegistry {
		count, ok := s.cachedRosterCount(ctx, meta)
		if !ok {
			// Omit rather than serialise a fake 0.
			degraded = append(degraded, meta.Name)
			continue
		}
		row := buildProtocolView(meta, count, events, verdicts)
		attachProtocolTVL(&row, tvls)
		view.Protocols = append(view.Protocols, row)
	}
	view.TotalProtocols = len(view.Protocols)
	view.TVLTotal = tvlTotal
	view.CoverageNote = protocolsCoverageNote(degraded)

	w.Header().Set("Cache-Control", "public, max-age=60")
	// Each row republishes a /v1/coverage verdict, so the envelope carries that freshness gate.
	writeJSON(w, view, Flags{Stale: verdictsStale, Degraded: !eventsOK || !verdictsOK || len(degraded) > 0})
}

// protocolsCoverageNote is non-empty when sources were omitted because their roster read failed
// with no last-good count, so an incomplete directory is distinguishable on the wire.
func protocolsCoverageNote(omitted []string) string {
	if len(omitted) == 0 {
		return ""
	}
	return "the contract roster for " + strings.Join(omitted, ", ") +
		" could not be read and no cached count was available; " +
		"these sources are omitted from this directory (not shown with a zero contract_count) and will reappear once the read recovers"
}

// protocolBespokeWindowDays parses the optional ?days= param that windows the bespoke block (the
// lake-analytics fields keep the fixed protocolActivityWindowDays). The whitelist is deliberate:
// each window is a distinct cached scan on an unauthenticated endpoint, so arbitrary values would
// be an amplification surface. ok=false means a problem+json 400 was written.
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

// handleProtocolDetail serves GET /v1/protocols/{name}: the directory row plus the contract
// registry, event-kind vocabulary and verification page. Unknown names 404. The optional ?days=
// param (1/7/30/90, default 90) windows the bespoke analytics block; anything else 400s.
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

	// Bounds only how long a STONE-COLD request waits for the detached build (the longest budget
	// that still beats the blanket request deadline); built entries stale-serve instantly.
	ctx, cancel := context.WithTimeout(r.Context(), maxHandlerBudget)
	defer cancel()
	// The key carries the bespoke window, or ?days=7 would serve the cached 90d view.
	view, builtAt, stale, ok := s.cachedProtocolDetail(ctx, protocolDetailCacheKey(meta.Name, windowDays), s.protocolDetailBuilder(meta, windowDays))
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
	// The flag tracks the ANALYTICS status, not just entry age: analytics.status="stale" always
	// travels with flags.stale (openapi contract).
	staleFlag := stale || view.verdictsStale ||
		(view.Analytics != nil && view.Analytics.Status == protocolAnalyticsStale)

	w.Header().Set("Cache-Control", "public, max-age=60")
	// A stale or partial view is replaced by the next rebuild; verdictsStale alone stays cacheable.
	degraded := stale || (view.Analytics != nil && view.Analytics.Status != protocolAnalyticsOK)
	// as_of is the build time, so a re-served entry replays byte-identical.
	writeEnvelope(w, Envelope{Data: view, AsOf: WireTime(builtAt.UTC()), Flags: Flags{Stale: staleFlag, Degraded: degraded}})
}

// protocolDetailBuilder returns the one build closure both the request path and the prewarm sweep
// use, so they cannot build different views for the same cache key.
func (s *Server) protocolDetailBuilder(meta ProtocolMeta, windowDays int) func(context.Context) ProtocolDetailView {
	return func(c context.Context) ProtocolDetailView {
		return s.buildProtocolDetail(c, meta, windowDays)
	}
}

// buildProtocolDetail assembles the detail view and stamps its analytics status: "ok" only when
// EVERY component built healthy under a live context; "stale" when the bespoke block came from the
// last-good cache past its horizon; otherwise "unavailable". The count, events and verdict reads
// fail independently of the lake/bespoke halves, so a verdict-read error stays distinguishable
// from "no snapshot".
func (s *Server) buildProtocolDetail(ctx context.Context, meta ProtocolMeta, windowDays int) ProtocolDetailView {
	contracts, rosterOK := s.protocolRoster(ctx, meta)
	classifyContractKinds(contracts, meta.Factories)
	tokensOK := s.enrichContractTokens(ctx, meta, contracts)
	contractCount, contractCountOK := s.detailContractCount(ctx, meta, contracts)
	events, eventsOK := s.protocolEvents24h(ctx)
	verdicts, verdictsOK, verdictsStale := s.protocolVerdicts(ctx)
	v := ProtocolDetailView{
		ProtocolView:     buildProtocolView(meta, contractCount, events, verdicts),
		verdictsStale:    verdictsStale,
		Contracts:        contracts,
		EventKinds:       append([]string{}, meta.EventKinds...),
		VerificationPage: meta.VerificationPage,
	}
	attachProtocolTVL(&v.ProtocolView, s.protocolTVLs())
	lakeOK := s.enrichProtocolAnalytics(ctx, meta, &v)
	bespokeOK, bespokeStale := s.enrichBespoke(ctx, meta, &v, windowDays)
	status := protocolAnalyticsOK
	switch {
	case !rosterOK || !tokensOK || !lakeOK || !bespokeOK || !contractCountOK || !eventsOK || !verdictsOK || ctx.Err() != nil:
		status = protocolAnalyticsUnavailable
	case bespokeStale:
		status = protocolAnalyticsStale
	}
	v.Analytics = &ProtocolAnalyticsStatus{Status: status, AsOf: time.Now().UTC().Format(time.RFC3339)}
	return v
}

// staleProtocolDetail returns a shallow copy for an entry past its TTL, downgrading analytics
// ok -> stale (worst state wins); the cached entry is never mutated.
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

// enrichBespoke attaches the bespoke block for the request's ?days= window from the last-good cache
// (protocol_bespoke_cache.go), so a failing build keeps serving the previous block.
//
//   - ok=false: no reader is wired or the FIRST-EVER build for this key failed, so absence means
//     degradation.
//   - stale: the block is older than bespokeStaleAfter, surfaced as analytics.status "stale".
func (s *Server) enrichBespoke(ctx context.Context, meta ProtocolMeta, view *ProtocolDetailView, windowDays int) (ok, stale bool) {
	blk, stale, ok := s.cachedBespoke(ctx, meta.Name, meta.Category, windowDays)
	if !ok {
		return false, false
	}
	view.Bespoke = bespokeFromStore(blk)
	return true, stale
}

// classifyContractKinds tags each roster contract "factory" (a verified trust-root) or "instance";
// a pre-set tag such as "module" is kept.
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
		if contracts[i].Kind == "" {
			contracts[i].Kind = "instance"
		}
	}
}

// protocolRoster returns meta's full contract roster plus any ExtraContracts folded in from a
// sub-module source (the Blend Backstop), tagged Kind="module" and deduped; since
// protocolContractIDs scopes the lake analytics to the roster, their events join the counts.
// ok=false means the read failed and the empty roster is not a true empty: the caller must fold it
// into analytics.status, or the SWR cache overwrites a healthy entry with contract_count 0.
func (s *Server) protocolRoster(ctx context.Context, meta ProtocolMeta) ([]ProtocolContractView, bool) {
	rows, err := s.rosterErr(ctx, meta)
	if err != nil {
		s.logger.Warn("protocol roster read failed", "source", meta.Name, "err", err)
		return []ProtocolContractView{}, false
	}
	if rows == nil {
		return []ProtocolContractView{}, true
	}
	return rows, true
}

// rosterErr returns meta's full contract roster, SURFACING a read error. protocolRoster wraps it
// for the detail path; the roster-count cache uses it directly so a failed read omits the source
// instead of publishing a fake contract_count: 0.
func (s *Server) rosterErr(ctx context.Context, meta ProtocolMeta) ([]ProtocolContractView, error) {
	rows, err := s.protocolContractsErr(ctx, meta.Name)
	if err != nil {
		return nil, err
	}
	if len(meta.ExtraContracts) == 0 {
		return rows, nil
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
	return rows, nil
}

// countedContractTotal resolves meta's contract_count WITHOUT enumerating a roster
// (protocolContractCounter). ok=false means no counter is wired for this source and the caller
// counts the roster. An error is surfaced, since an unreadable count is not a zero.
func (s *Server) countedContractTotal(ctx context.Context, meta ProtocolMeta) (int, bool, error) {
	counter, has := s.ProtocolContracts.(protocolContractCounter)
	if !has {
		return 0, false, nil
	}
	n, ok, err := counter.CountSourceContracts(ctx, meta.Name)
	if err != nil || !ok {
		return 0, false, err
	}
	// ExtraContracts live in the sub-module's own tables, never inside this source's count.
	return int(n) + len(meta.ExtraContracts), true, nil
}

// rosterCountErr returns the counted TRUE total where one exists, else the roster length. Errors
// surface so the directory names the source in coverage_note instead of publishing a zero.
func (s *Server) rosterCountErr(ctx context.Context, meta ProtocolMeta) (int, error) {
	n, ok, err := s.countedContractTotal(ctx, meta)
	if err != nil {
		return 0, err
	}
	if ok {
		return n, nil
	}
	rows, err := s.rosterErr(ctx, meta)
	if err != nil {
		return 0, err
	}
	return len(rows), nil
}

// detailContractCount is the detail view's contract_count: len(roster), or the counted TRUE total
// for a non-enumerable source. A failed count degrades to the roster length (the detail path cannot
// omit the protocol); ok=false lets the caller fold it into analytics.status.
func (s *Server) detailContractCount(ctx context.Context, meta ProtocolMeta, roster []ProtocolContractView) (int, bool) {
	n, ok, err := s.countedContractTotal(ctx, meta)
	if err != nil {
		s.logger.Warn("protocol contract count read failed", "source", meta.Name, "err", err)
		return len(roster), false
	}
	if !ok {
		return len(roster), true
	}
	return n, true
}

// enrichProtocolAnalytics fills the lake-derived analytics on the detail view. It returns true only
// when every fill completed (an empty roster is healthy), so absent fields are marked
// "unavailable" rather than read as real zeros.
func (s *Server) enrichProtocolAnalytics(ctx context.Context, meta ProtocolMeta, view *ProtocolDetailView) bool {
	if s.ProtocolActivity == nil {
		return false
	}
	ids := protocolContractIDs(view.Contracts, meta.Factories)
	if len(ids) == 0 {
		return true
	}
	// Bounding by ledger_seq prunes partitions, keeping each query under the lake reader's 30s
	// budget. An unreadable tip skips analytics: a zero tip would collapse the day cutoff to
	// yesterday while claiming ActivityWindowDays.
	tip, err := s.ProtocolActivity.LakeTipLedger(ctx)
	if err != nil {
		s.logger.Warn("protocol activity tip read failed", "err", err)
		return false
	}
	plan := s.protocolActivityPlanFor(ctx, tip)
	view.ActivityWindowDays = protocolActivityWindowDays
	// The reads write disjoint fields and run concurrently; they share ONE plan so series and
	// breakdown cannot split across sources. "untyped" needs EventsTotal, so it is appended after
	// the barrier.
	var wg sync.WaitGroup
	var seriesOK, breakdownOK, contractsOK bool
	wg.Add(2)
	// Detached from the handler, so middleware.Recoverer does not cover these; a recovered panic
	// leaves the OK flag false so it cannot displace a healthy cached entry. Series and breakdown
	// are ONE fill, or mixed fast/raw windows would break sum(EventBreakdown)==EventsTotal.
	go func() {
		defer wg.Done()
		defer worker.Recover(s.logger, "api-protocol-series-breakdown-fill")
		seriesOK, breakdownOK = s.fillProtocolSeriesAndBreakdown(ctx, meta.Name, ids, plan, view)
	}()
	go func() {
		defer wg.Done()
		defer worker.Recover(s.logger, "api-protocol-contract-activity-fill")
		contractsOK = s.fillProtocolContractActivity(ctx, meta.Name, ids, plan, view)
	}()
	wg.Wait()
	reconcileProtocolBreakdown(view)
	return seriesOK && breakdownOK && contractsOK
}

// reconcileProtocolBreakdown appends the "untyped" bucket (events whose topic[0] symbol the lake
// did not denormalize, mostly AMM swap/sync) so the breakdown sums to EventsTotal.
func reconcileProtocolBreakdown(view *ProtocolDetailView) {
	var typedSum int64
	for _, b := range view.EventBreakdown {
		typedSum += b.Count
	}
	if untyped := view.EventsTotal - typedSum; untyped > 0 {
		view.EventBreakdown = append(view.EventBreakdown, ProtocolEventTypeView{EventType: "untyped", Count: untyped})
	}
}

// protocolContractIDs is the dedup'd analytics scope: instances plus the factories, which emit
// events too (new_pair, deploy).
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

// fillProtocolSeriesAndBreakdown fills the daily series (+EventsTotal) and the breakdown from the
// SAME source, fast or raw: the windows differ (day-grain vs ledger-window) and
// sum(EventBreakdown)==EventsTotal assumes one, so EITHER fast query erroring forces BOTH onto raw.
// The breakdown groups by topic_0_sym, set only for plain Symbol topics, hence the "untyped" bucket.
func (s *Server) fillProtocolSeriesAndBreakdown(ctx context.Context, name string, ids []string, plan protocolActivityPlan, view *ProtocolDetailView) (seriesOK, breakdownOK bool) {
	if plan.fast != nil {
		series, sErr := plan.fast.ProtocolDailyActivityFast(ctx, ids, plan.sinceDay)
		breakdown, bErr := plan.fast.ProtocolEventBreakdownFast(ctx, ids, plan.sinceDay)
		if sErr == nil && bErr == nil {
			applyProtocolSeries(view, series)
			applyProtocolBreakdown(view, breakdown)
			return true, true
		}
		s.logger.Warn("fast series/breakdown would split across windows; raw fallback for both",
			"source", name, "series_err", sErr, "breakdown_err", bErr)
	}
	series, sErr := s.ProtocolActivity.ProtocolDailyActivity(ctx, ids, plan.sinceLedger)
	if sErr != nil {
		s.logger.Warn("protocol daily activity failed", "source", name, "err", sErr)
	} else {
		applyProtocolSeries(view, series)
	}
	breakdown, bErr := s.ProtocolActivity.ProtocolEventBreakdown(ctx, ids, plan.sinceLedger)
	if bErr != nil {
		s.logger.Warn("protocol event breakdown failed", "source", name, "err", bErr)
	} else {
		applyProtocolBreakdown(view, breakdown)
	}
	return sErr == nil, bErr == nil
}

// applyProtocolSeries projects the daily points onto the view and sets EventsTotal, the unfiltered
// window total (sum of the daily points) the breakdown reconciles against, NOT the typed sum.
func applyProtocolSeries(view *ProtocolDetailView, series []clickhouse.ProtocolDailyPoint) {
	view.ActivitySeries = make([]ProtocolActivityPointView, 0, len(series))
	var total int64
	for _, p := range series {
		view.ActivitySeries = append(view.ActivitySeries, ProtocolActivityPointView{Date: p.Date, Events: int64(p.Events)})
		total += int64(p.Events)
	}
	view.EventsTotal = total
}

// applyProtocolBreakdown projects the typed event-count rows onto the
// view. The reconciling "untyped" remainder bucket is appended by
// reconcileProtocolBreakdown after both fields are set (it needs
// EventsTotal).
func applyProtocolBreakdown(view *ProtocolDetailView, breakdown []clickhouse.ProtocolEventTypeCount) {
	view.EventBreakdown = make([]ProtocolEventTypeView, 0, len(breakdown)+1)
	for _, b := range breakdown {
		view.EventBreakdown = append(view.EventBreakdown, ProtocolEventTypeView{EventType: b.EventType, Count: int64(b.Count)})
	}
}

// fillProtocolContractActivity merges per-contract event counts + last-seen onto
// the roster (degrades on error).
func (s *Server) fillProtocolContractActivity(ctx context.Context, name string, ids []string, plan protocolActivityPlan, view *ProtocolDetailView) bool {
	if plan.fast != nil {
		// Fast path: per-contract counts over the daily pre-aggregation instead of the raw
		// `contract_events FINAL` scan, which can blow the lake memory limit. Falls back to raw
		// when the daily table is unavailable or the read errors.
		act, err := plan.fast.ProtocolContractActivityFast(ctx, ids, plan.sinceDay)
		if err == nil {
			applyProtocolContractActivity(view, act)
			return true
		}
		s.logger.Warn("fast contract activity failed; raw fallback", "source", name, "err", err)
	}
	act, err := s.ProtocolActivity.ProtocolContractActivity(ctx, ids, plan.sinceLedger)
	if err != nil {
		s.logger.Warn("protocol contract activity failed", "source", name, "err", err)
		return false
	}
	applyProtocolContractActivity(view, act)
	return true
}

// applyProtocolContractActivity merges per-contract counts + last-seen onto the roster.
func applyProtocolContractActivity(view *ProtocolDetailView, act []clickhouse.ProtocolContractActivity) {
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

// protocolActivityPlanFor derives the shared analytics plan from ONE lake tip read: the ledger
// cutoff plus, when the daily pre-aggregation is usable, the fast reader and its day cutoff.
func (s *Server) protocolActivityPlanFor(ctx context.Context, tip uint32) protocolActivityPlan {
	since, sinceDay := s.protocolWindowFloor(ctx, tip)
	plan := protocolActivityPlan{sinceLedger: since}
	if fast := s.fastActivity(ctx); fast != nil {
		plan.fast = fast
		plan.sinceDay = sinceDay
	}
	return plan
}

// protocolLedgerAtCloseTimeReader is the optional capability behind an accurate window boundary: a
// ProtocolActivityReader that can resolve a ledger's close_time. A reader without it degrades to
// the theoretical-cadence approximation below.
type protocolLedgerAtCloseTimeReader interface {
	explorer.LedgerPager
	LedgerBySeq(ctx context.Context, seq uint32) (clickhouse.LedgerHeader, bool, error)
}

// The capability is an optional type assertion, so a production reader that
// drifted out of it would silently serve the approximation instead.
var _ protocolLedgerAtCloseTimeReader = (*clickhouse.ExplorerReader)(nil)

// protocolWindowFloor derives the raw readers' ledger cutoff AND the fast reader's day-grain
// cutoff from ONE close_time boundary: tip's close_time minus protocolActivityWindowDays. A
// ledger-count multiple of a nominal cadence overshoots the published activity_window_days
// because pubnet runs below it. Falls back to that approximation only when close_time can't be
// resolved.
func (s *Server) protocolWindowFloor(ctx context.Context, tip uint32) (sinceLedger uint32, sinceDay time.Time) {
	fallback := uint32(1) // whole chain inside the window
	if tip > protocolActivityWindowLedgers {
		fallback = tip - protocolActivityWindowLedgers
	}
	closeTimeReader, ok := s.ProtocolActivity.(protocolLedgerAtCloseTimeReader)
	if !ok {
		return fallback, protocolSinceDay(fallback, tip)
	}
	tipHdr, found, err := closeTimeReader.LedgerBySeq(ctx, tip)
	if err != nil || !found {
		return fallback, protocolSinceDay(fallback, tip)
	}
	boundary := tipHdr.CloseTime.UTC().AddDate(0, 0, -protocolActivityWindowDays)
	// Close times are whole seconds, so "at or after boundary" is "after boundary-1s".
	since, err := explorer.FirstCapturedClosedAfter(ctx, closeTimeReader, tip, boundary.Add(-time.Second))
	if err != nil {
		return fallback, protocolSinceDay(fallback, tip)
	}
	if since == 0 {
		since = 1
	}
	return since, boundary
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
			Complete:               sn.Complete,
			WatermarkLedger:        sn.Watermark,
			ProjectionVerifiedFrom: sn.ProjectionVerifiedFrom,
		}
	}
	return v
}

// protocolTVLs reads the per-protocol TVL snapshot, degrading to nil
// (the tvl field stays absent everywhere) when the cache isn't wired
// or hasn't completed its first background refresh.
func (s *Server) protocolTVLs() map[string]ProtocolTVLView {
	if s.DEXTVL == nil {
		return nil
	}
	snap, _ := s.DEXTVL.Snapshot()
	return snap
}

// protocolTVLsAndTotal reads the per-protocol TVL snapshot together with the headline total from
// the SAME refresh cycle: two separate calls can straddle a concurrent Refresh() and pair one
// cycle's figures with another's total. Both are nil when the cache is unwired or not yet filled.
func (s *Server) protocolTVLsAndTotal() (map[string]ProtocolTVLView, *DEXTVLTotalView) {
	if s.DEXTVL == nil {
		return nil, nil
	}
	snap, total, _ := s.DEXTVL.SnapshotAndTotal()
	return snap, total
}

// attachProtocolTVL joins the snapshot's TVL entry (if any) onto a
// directory row.
func attachProtocolTVL(view *ProtocolView, tvls map[string]ProtocolTVLView) {
	if t, ok := tvls[view.Name]; ok {
		view.TVL = &t
	}
}

// protocolEvents24h reads the per-source trailing-24h event counts, degrading to an empty map
// (every protocol reads 0) when the reader is nil or errors. ok=false only on a read error: a
// nil reader is the documented "not wired" shape and not a degradation analytics.status should
// report, whereas a query error is, so the caller can fold it in.
func (s *Server) protocolEvents24h(ctx context.Context) (map[string]int64, bool) {
	if s.ProtocolStats == nil {
		return nil, true
	}
	counts, err := s.ProtocolStats.CountRecentEventsBySource(ctx)
	if err != nil {
		s.logger.Warn("protocols events_24h read failed", "err", err)
		return nil, false
	}
	return counts, true
}

// protocolVerdicts reads the latest completeness verdict per source plus the /v1/coverage
// freshness gate. ok=true covers both a nil reader (no snapshot exists) and a successful read;
// ok=false is a read ERROR, so "no snapshot" and "read failed" are distinguishable. stale is true
// only when a successful read's own verdicts are stale.
func (s *Server) protocolVerdicts(ctx context.Context) (verdicts map[string]timescale.CompletenessSnapshot, ok, stale bool) {
	if s.CompletenessReader == nil {
		return nil, true, false
	}
	snaps, verdictsStale, err := s.completenessVerdicts(ctx)
	if err != nil {
		s.logger.Warn("protocols completeness read failed", "err", err)
		return nil, false, false
	}
	out := make(map[string]timescale.CompletenessSnapshot, len(snaps))
	for _, sn := range snaps {
		out[sn.Source] = sn
	}
	return out, true, verdictsStale
}

// protocolContractsErr returns name's registered instances in the unified wire shape. A nil
// reader is NOT an error (unwired is a genuine empty roster); a store read that ERRORS is
// returned so the caller can degrade honestly instead of masking it as an empty roster.
// protocolContracts wraps this and swallows the error to empty.
func (s *Server) protocolContractsErr(ctx context.Context, name string) ([]ProtocolContractView, error) {
	if name == "soroswap" {
		return s.soroswapContractsErr(ctx)
	}
	if s.ProtocolContracts == nil {
		return []ProtocolContractView{}, nil
	}
	rows, err := s.ProtocolContracts.ListProtocolContracts(ctx, name)
	if err != nil {
		s.logger.Warn("protocols contract registry read failed", "source", name, "err", err)
		return nil, err
	}
	if len(rows) == 0 {
		// The protocol_contracts registry is empty for this source (only blend
		// is seeded today). Fall back to the contracts the decoder has actually
		// captured into the projected table, so defindex/phoenix/comet/cctp/rozo
		// get a real roster + the lake analytics scoped to it.
		return s.protocolContractsFromProjectionErr(ctx, name)
	}
	out := make([]ProtocolContractView, 0, len(rows))
	for _, row := range rows {
		out = append(out, ProtocolContractView{
			ContractID:  row.ContractID,
			FactoryID:   row.FactoryID,
			FirstLedger: row.FirstLedger,
		})
	}
	return out, nil
}

// protocolContractsFromProjectionErr is the registry-empty fallback: the distinct contracts from
// name's projected table (empty when the source has none), surfacing a read error. Only sdex is
// op-keyed with no contract roster. This is the `SELECT DISTINCT ... LIMIT 5000` served-tier scan
// the roster-count cache keeps off the unauthenticated request path.
func (s *Server) protocolContractsFromProjectionErr(ctx context.Context, name string) ([]ProtocolContractView, error) {
	ids, err := s.ProtocolContracts.ListSourceContractsFromProjection(ctx, name)
	if err != nil {
		s.logger.Warn("protocols projection roster read failed", "source", name, "err", err)
		return nil, err
	}
	out := make([]ProtocolContractView, 0, len(ids))
	for _, id := range ids {
		out = append(out, ProtocolContractView{ContractID: id})
	}
	return out, nil
}

// soroswapContractsErr projects the soroswap_pairs registry into the unified contract shape
// (pair strkey as the instance, token pair attached, no factory because the pairs table records
// none), surfacing a read error rather than swallowing it. A nil reader is a genuine empty
// roster, not an error.
func (s *Server) soroswapContractsErr(ctx context.Context) ([]ProtocolContractView, error) {
	if s.SoroswapPairs == nil {
		return []ProtocolContractView{}, nil
	}
	pairs, err := s.SoroswapPairs.LoadSoroswapPairRegistry(ctx)
	if err != nil {
		s.logger.Warn("protocols soroswap pair registry read failed", "err", err)
		return nil, err
	}
	out := make([]ProtocolContractView, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, ProtocolContractView{
			ContractID: p.PairStrkey,
			Token0:     p.Token0Strkey,
			Token1:     p.Token1Strkey,
		})
	}
	return out, nil
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

// fastActivity returns the fast reader when the pre-aggregation is available. The probe answer is
// cached only when DEFINITIVE (table missing, or rows found): a transient ClickHouse blip must not
// latch the raw multi-billion-row scans for the process lifetime, so sync.Once is the wrong
// primitive. A non-definitive answer degrades THIS call to raw and re-probes next time.
func (s *Server) fastActivity(ctx context.Context) protocolFastActivityReader {
	fast, ok := s.ProtocolActivity.(protocolFastActivityReader)
	if !ok {
		return nil
	}
	s.protocolFastMu.Lock()
	settled, settledOK := s.protocolFastSettled, s.protocolFastOK
	s.protocolFastMu.Unlock()
	if settled {
		if !settledOK {
			return nil
		}
		return fast
	}

	// Unsettled: probe WITHOUT holding protocolFastMu, or a wedged ClickHouse would re-probe under
	// the lock and serialise every detail build behind one stuck probe. Concurrent probes may race;
	// only a definitive answer is recorded, so the cost is redundant probes, never a wrong verdict.
	avail, definitive := fast.DailyActivityAvailable(ctx)
	if definitive {
		s.protocolFastMu.Lock()
		s.protocolFastSettled = true
		s.protocolFastOK = avail
		s.protocolFastMu.Unlock()
	}
	if !avail {
		return nil
	}
	return fast
}

// protocolActivityPlan is the ONE shared fast-vs-raw decision and window for a detail build's
// concurrent analytics fills. Independent probes could take different paths and break the
// sum(EventBreakdown)==EventsTotal reconcile, and a failed tip read (tip==0) would silently serve
// a 1-day window labeled 90d.
type protocolActivityPlan struct {
	// sinceLedger is the raw readers' cutoff (tip − window), always set.
	sinceLedger uint32
	// fast is non-nil when the daily pre-aggregation serves this build;
	// sinceDay is then its day-grain cutoff (derived from the same tip).
	fast     protocolFastActivityReader
	sinceDay time.Time
}
