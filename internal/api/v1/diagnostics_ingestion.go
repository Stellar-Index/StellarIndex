package v1

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/sources/defindex"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/version"
	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// FXCoverageReader is the seam for fx_quotes coverage stats; timescale.Store satisfies it.
type FXCoverageReader interface {
	FXCoverageStats(ctx context.Context) (timescale.FXCoverage, error)
}

// SupplyCoverageReader is the seam for asset_supply_history coverage stats; timescale.Store
// satisfies it.
type SupplyCoverageReader interface {
	SupplyCoverageStats(ctx context.Context) (timescale.SupplyCoverage, error)
}

// CAGGCoverageReader is the seam for prices_1h coverage stats; timescale.Store satisfies it.
type CAGGCoverageReader interface {
	CAGGCoverageStats(ctx context.Context) (timescale.CAGGCoverage, error)
}

// SourceEntryCountReader is the seam for the per-source entry tally; timescale.Store satisfies
// it. The ~20-row read stays cheap during an all-time backfill.
type SourceEntryCountReader interface {
	SourceEntryCounts(ctx context.Context) (map[string]int64, error)
}

// IngestionDiagnostics is the wire shape for /v1/diagnostics/ingestion: one snapshot of the
// region's ingest state, so the status page renders its whole ingestion panel from a single call.
type IngestionDiagnostics struct {
	Region   RegionInfo             `json:"region"`
	Version  IngestionVersionInfo   `json:"version"`
	Ledger   LedgerTip              `json:"ledger"`
	Backfill []BackfillDecoderState `json:"backfill"`
	// BackfillCoverage is the per-source genesis-to-tip processed fraction. It is data-derived: the
	// gap detector persists source_coverage_snapshots and the handler reads one row, with no
	// trades scan. Cursors are an operational journal and do not feed coverage; trade_count is
	// best-effort enrichment from the background trades-scan cache.
	BackfillCoverage   []BackfillCoverageRow `json:"backfill_coverage"`
	BackfillCoverageAt string                `json:"backfill_coverage_as_of,omitempty"`
	// entryCounts backs the `entries` column (source_entry_counts); never on the wire.
	entryCounts map[string]int64 `json:"-"`
	// degraded is set when a critical reader failed or was not wired. The handler maps it to
	// Flags.Stale: zero-valued fields under stale:false would read as real zero counters. Fillers
	// never write it directly; an atomic.Bool sidecar is copied here after wg.Wait() to avoid a
	// data race.
	degraded bool `json:"-"`
	// CAGGCoverage is the time range of prices_1h, which powers /v1/chart and the since-inception
	// history endpoint. The Postgres trades hypertable is only the served tier, so its MIN(ledger)
	// understates history; prices_1h is the indefinite OHLC spine.
	CAGGCoverage CAGGCoverageView  `json:"cagg_coverage"`
	FXBackfill   FXBackfillState   `json:"fx_backfill"`
	Supply       SupplyStateView   `json:"supply"`
	Sources      []SourceHealthRow `json:"sources"`
}

// CAGGCoverageView is the prices_1h coverage summary; buckets are RFC3339, empty until the CAGG
// is materialised.
type CAGGCoverageView struct {
	EarliestBucket string `json:"earliest_bucket,omitempty"`
	LatestBucket   string `json:"latest_bucket,omitempty"`
	BucketCount    int64  `json:"bucket_count"`
}

// SourceCoverageReader is the ADR-0031 read seam for the data-derived coverage projection
// (source_coverage_snapshots, written by the gap detector each 30-minute cycle).
// An empty slice with nil error means "detector has not run yet, fall back to v1", never a
// coverage regression.
type SourceCoverageReader interface {
	ListSourceCoverage(ctx context.Context) ([]timescale.SourceCoverage, error)
}

// CompletenessReader is the read seam for the per-source watermark verdict, wired to
// timescale.Store.ListCompletenessSnapshots (filled by `stellarindex-ops compute-completeness`).
// A nil reader or empty slice leaves the completeness_* fields absent and the UI falls back to
// the gap_free coverage signal.
type CompletenessReader interface {
	ListCompletenessSnapshots(ctx context.Context) ([]timescale.CompletenessSnapshot, error)
}

// BackfillCoverageRow is the per-source coverage projection.
//
// DensityPct is distinct_ledger_count / (tip - genesis + 1) from source_coverage_snapshots,
// because cursors can say "fine" while the data says "missing". GenesisLedger comes from
// sourceGenesisLedger; EarliestLedger / LatestLedger are display only. GapFreePct is
// 1 - max_gap / expected: a sparse source running cleanly hits 1.0 despite low DensityPct, so
// the alert reads it.
type BackfillCoverageRow struct {
	Source         string `json:"source"`
	Applies        bool   `json:"applies"`
	GenesisLedger  int64  `json:"genesis_ledger,omitempty"`
	EarliestLedger int64  `json:"earliest_ledger,omitempty"`
	LatestLedger   int64  `json:"latest_ledger,omitempty"`
	// Entries is the per-source tally of ingested rows from the tiny source_entry_counts table; it
	// stays exact during an all-time backfill, when a scan of the contended trades table would
	// collapse to 0.
	Entries int64 `json:"entries"`
	// CoveragePct is the customer-facing "are we walking this source completely" signal,
	// sourced from gap_free_pct (see overlaySourceCoverageV2). Sparse sources still hit 1.0,
	// since 100% means the indexer skipped no ledger, not that the contract emits constantly.
	// It is not DensityPct, which reads near 0% for any legitimately sparse source.
	CoveragePct float64 `json:"coverage_pct,omitempty"`
	// DensityPct is the fraction of ledgers processed between genesis and tip; 1.0 = fully
	// backfilled.
	DensityPct float64 `json:"density_pct,omitempty"`
	// CoveredLedgers is the numerator of DensityPct.
	CoveredLedgers int64 `json:"covered_ledgers,omitempty"`
	// ExpectedLedgers is the denominator of DensityPct. It is seeded as tip - genesis + 1, then
	// overwritten by overlaySourceCoverageV2 with the snapshot's window size, because the gap
	// detector scans a trailing window and covered/expected must stay coherent with DensityPct.
	ExpectedLedgers int64 `json:"expected_ledgers,omitempty"`

	// GapFreePct is 1 - max_gap_ledgers / expected_ledger from the source_coverage_snapshots row.
	GapFreePct float64 `json:"gap_free_pct,omitempty"`

	// CoverageSnapshotAt is when the gap detector last refreshed this row, so the status page
	// can mark stale reads. Nil until the detector's first cycle after a deploy.
	CoverageSnapshotAt *WireTime `json:"coverage_snapshot_at,omitempty"`

	// CoverageScanCadenceS is the longest gap-detector scan cadence (seconds) among this row's
	// tables, so a reader ages CoverageSnapshotAt against the cadence that refreshes it.
	CoverageScanCadenceS int64 `json:"coverage_scan_cadence_s,omitempty"`

	// CompletenessPct is (watermark - genesis + 1) / (tip - genesis + 1), where the watermark is
	// the highest ledger with substrate continuity, hash chain and recognition verified from
	// genesis. The served-tier reconcile gates only CompletenessComplete, so this can read 100%
	// while the served tier is short. No sparsity threshold: one proven gap pins it. A pointer so
	// a computed 0 is still emitted; presence means the audit ran.
	CompletenessPct *float64 `json:"completeness_pct,omitempty"`
	// CompletenessWatermark is the highest fully-verified ledger.
	CompletenessWatermark int64 `json:"completeness_watermark,omitempty"`
	// CompletenessComplete is true when the watermark reached tip: the SERVED/combined axis,
	// substrate ∧ recognition ∧ the retention-scoped projection reconcile.
	CompletenessComplete bool `json:"completeness_complete,omitempty"`
	// CompletenessLakeComplete is the LAKE (archive) axis of the two-axis verdict: substrate and
	// recognition only, genesis-to-tip, without the projection reconcile. A source is routinely
	// lake_complete:true, complete:false while the served tier catches up; with only `complete`,
	// "data does not exist" would be indistinguishable from "projection is behind".
	CompletenessLakeComplete bool `json:"completeness_lake_complete,omitempty"`
	// CompletenessComputedAt is when compute-completeness last ran.
	CompletenessComputedAt *WireTime `json:"completeness_computed_at,omitempty"`
}

// sourceGenesisLedger maps each source to the earliest ledger it can have data for: 2 for SDEX
// (ledger 1 has no operations), the audited minimum create_contract ledger for a protocol, or 0
// when not applicable. It is the DensityPct denominator: too early makes 100% unreachable, too
// late hides early-history gaps, so never use a rounded estimate.
var sourceGenesisLedger = map[string]int64{
	"sdex": 2,
	// Soroban: MIN first-deploy ledger across the source's contracts, from the per-source WASM
	// audits (docs/operations/wasm-audits/<src>.md).
	"soroswap":        50_746_266, // factory first-deploy (soroswap.md:242)
	"soroswap-router": 50_746_272, // router, +6 ledgers after factory (soroswap.md:236)
	"aquarius":        52_728_375, // MIN across 313 pools + router
	"phoenix":         51_572_016, // factory first-deploy
	"sushiswap_v3":    61_487_379, // pool factory first pool_created (sushiswap_v3.md)
	"spectra":         63_778_088, // registry's first role_granted (spectra.md)
	"upshift":         62_623_313, // earnUSDC vault `admin_set`, 6 ledgers ahead of earnXLM (upshift.md)
	// comet and blend share a genesis on purpose: the only mainnet Comet is Blend's backstop
	// pool, instantiated in the same ledger as Blend's Pool Factory V2. 51_499_545 is the
	// ContractCode-upload boundary, one before the ContractInstance create.
	"comet":         51_499_546, // Blend backstop pool instantiation (comet.md:157)
	"blend":         51_499_546, // Pool Factory V2 deploy, same ledger (blend.md:90)
	"reflector-dex": 50_644_229, // v2 WASM deploy (reflector.md:186)
	"reflector-cex": 50_644_239, // v2 WASM deploy, +10 after DEX (reflector.md:186)
	"reflector-fx":  56_733_481, // deployed fresh on v3, no prior history (reflector.md:195)
	"band":          50_842_736, // single stable WASM since first deploy (band.md:198)
	"redstone":      58_758_722, // first-deploy hotfix, replaced +420 ledgers (redstone.md:179)
	// defindex: MIN across every contract the source routes, including the earliest of its four
	// factories, not only the current one.
	"defindex": int64(defindex.GenesisLedger),

	// cctp and rozo: MIN deploy ledger across each source's contracts, resolved through the
	// lake's ledgers table from the audited deploy close_time.
	"cctp": 62_147_265,
	// rozo predates the contract-storage capture window, so density showing a gap below ~62M is
	// the honest "pre-capture history not backfilled" signal, not a decoder fault.
	"rozo": 60_829_370,
}

// RegionInfo identifies the deployment that generated this snapshot; same shape as Region on
// /v1/status so the two endpoints cannot drift.
type RegionInfo struct {
	Name       string `json:"name"`
	Deployment string `json:"deployment"`
}

// IngestionVersionInfo is the same five fields /v1/version returns, duplicated onto the
// ingestion response so an operator screen needs no second fetch when comparing regions.
type IngestionVersionInfo struct {
	Version   string `json:"version"`
	BuildDate string `json:"build_date"`
	Commit    string `json:"commit"`
	Dirty     string `json:"dirty"`
	GoVersion string `json:"go_version"`
}

// LedgerTip summarises live-network ingest progress. LatestLedger is prices_1m's
// MAX(ledger_sequence); LagSeconds is the age of the latest ledgerstream cursor update. Volume,
// markets and assets share /v1/network/stats's source so the two endpoints agree.
type LedgerTip struct {
	LatestLedger    int64  `json:"latest_ledger"`
	LagSeconds      int64  `json:"lag_seconds"`
	Volume24hUSD    string `json:"volume_24h_usd,omitempty"`
	MarketsCount24h int64  `json:"markets_count_24h"`
	AssetsIndexed   int64  `json:"assets_indexed"`
}

// BackfillDecoderState is one row of the per-decoder backfill summary, collapsing the many
// concurrent backfill ranges into running/stalled/oldest per decoder set. Decoder is the
// comma-separated set after the range prefix of the cursor's sub_source (e.g.
// "50534290-51275895:sdex,soroswap" gives "sdex,soroswap").
type BackfillDecoderState struct {
	Decoder string `json:"decoder"`
	// RangesTotal is the total cursor rows for this decoder set; Complete, Running and Stalled
	// decompose it.
	RangesTotal int `json:"ranges_total"`
	// RangesComplete: cursor's last_ledger == range_end.
	RangesComplete int `json:"ranges_complete"`
	// RangesRunning: last_ledger < range_end and last_updated within stalledThreshold.
	RangesRunning int `json:"ranges_running"`
	// RangesStalled: last_ledger < range_end and last_updated older than stalledThreshold.
	// Usually an operator killed the backfill mid-run; it needs a -resume restart.
	RangesStalled int `json:"ranges_stalled"`
	// RangesActive is RangesRunning + RangesStalled, the "incomplete count".
	RangesActive    int    `json:"ranges_active"`
	OldestUpdatedAt string `json:"oldest_updated_at,omitempty"`
	OldestLagSecs   int64  `json:"oldest_lag_seconds"`
	NewestLedger    int64  `json:"newest_ledger"`
}

// stalledThreshold is the age above which an in-progress backfill cursor counts as stalled.
// It matches statusActiveMaxAge in diagnostics_cursors.go so both endpoints agree.
const stalledThreshold = 10 * time.Minute

// FXBackfillState describes the fiat-rate history in fx_quotes. Earliest/Latest are RFC3339
// dates at day granularity (daily upstream snapshots); empty when the table is empty.
type FXBackfillState struct {
	EarliestQuote   string `json:"earliest_quote,omitempty"`
	LatestQuote     string `json:"latest_quote,omitempty"`
	TotalQuotes     int64  `json:"total_quotes"`
	CurrenciesCount int    `json:"currencies_count"`
}

// SupplyStateView projects timescale.SupplyCoverage onto the wire. Classic and SEP-41 assets
// are split because their supply observers run independently and can wedge separately.
type SupplyStateView struct {
	ClassicAssets  int    `json:"classic_assets_with_supply"`
	SEP41Assets    int    `json:"sep41_assets_with_supply"`
	LastSnapshotAt string `json:"last_snapshot_at,omitempty"`
	LatestLedger   int64  `json:"latest_ledger,omitempty"`
}

// SourceHealthRow joins static external.Registry metadata with the live 24h stats from
// sourcesStats so operators can see which sources are silent. Sorted by name for deterministic
// responses.
type SourceHealthRow struct {
	Name          string `json:"name"`
	Class         string `json:"class"`
	Subclass      string `json:"subclass,omitempty"`
	IncludeInVWAP bool   `json:"include_in_vwap"`
	BackfillSafe  bool   `json:"backfill_safe"`
	TradeCount24h int64  `json:"trade_count_24h"`
	// Entries24h is the trailing-24h event count from stellarindex_source_events_total. Unlike
	// trade_count_24h it is non-zero for every active source, since trade_count_24h is
	// trades-table-only (0 for oracles, bridges, FX) and a scan timeout can silently zero it.
	Entries24h int64 `json:"entries_24h"`
	// Enabled distinguishes "switched on and quiet" from "never switched on". The table lists
	// every external.Registry entry, so an unwired source would otherwise read as broken, not off.
	Enabled         bool   `json:"enabled"`
	VolumeUSD24h    string `json:"volume_24h_usd,omitempty"`
	MarketsCount24h int64  `json:"markets_count_24h"`
}

// handleDiagnosticsIngestion serves GET /v1/diagnostics/ingestion.
//
// Readers fan out under one deadline and soft-fail per reader, so one stuck dependency empties
// its section instead of failing the response. Cache is public max-age=15s: the data moves at
// most every few seconds, so 15s smooths status-page refresh load without hiding degradation.
func (s *Server) handleDiagnosticsIngestion(w http.ResponseWriter, r *http.Request) {
	// Prefer the background-refreshed snapshot (sub-millisecond versus a 200-500ms inline build).
	// Fall back to an inline build when the refresher has not fired yet or has gone stale, so a
	// dead refresher never leaves requests on frozen data.
	if entry := s.freshIngestionSnapshot(); entry != nil {
		w.Header().Set("Cache-Control", "public, max-age=15, s-maxage=15")
		writeEnvelope(w, Envelope{Data: entry.snap, AsOf: WireTime(entry.builtAt.UTC()), Flags: ingestionFlags(entry.snap)})
		return
	}

	// Per-handler ceiling. Each filler has its own sub-context so one slow reader cannot abort
	// every later filler with a deadline error and show 0% coverage on every source. An inline
	// build that outruns the budget returns a degraded shape that ingestionFlags discloses via
	// flags.stale; it is only reachable on a cold process.
	ctx, cancel := context.WithTimeout(r.Context(), maxHandlerBudget)
	defer cancel()

	out := s.buildIngestionSnapshot(ctx)
	w.Header().Set("Cache-Control", "public, max-age=15, s-maxage=15")
	writeJSON(w, out, ingestionFlags(out))
}

// ingestionFlags maps a built IngestionDiagnostics onto the wire Flags. flags.stale fires when a
// critical filler soft-failed or LatestLedger == 0, so an all-zeros response from a failing
// reader cascade is never mistaken for a network with zero ledgers and trades.
func ingestionFlags(snap IngestionDiagnostics) Flags {
	stale := snap.degraded || snap.Ledger.LatestLedger == 0
	return Flags{Stale: stale, Degraded: snap.degraded}
}

// ingestionSnapshotEntry wraps a snapshot for atomic storage. computedAt lets the handler detect
// a stopped refresher; builtAt is the served as_of, which a kept last-good snapshot preserves
// while computedAt moves on.
type ingestionSnapshotEntry struct {
	snap       IngestionDiagnostics
	computedAt time.Time
	builtAt    time.Time
}

// ingestionSnapshotCadence is the refresh interval. ingestionSnapshotMaxAge tolerates a few slow
// cycles but stops a dead refresher from serving a frozen snapshot forever.
const (
	ingestionSnapshotCadence = 15 * time.Second
	ingestionSnapshotMaxAge  = 4 * ingestionSnapshotCadence
)

// freshIngestionSnapshot returns the refreshed snapshot if within ingestionSnapshotMaxAge, else
// nil. Readers must use it rather than Load() so a dead refresher is never served.
func (s *Server) freshIngestionSnapshot() *ingestionSnapshotEntry {
	entry := s.ingestionSnapshot.Load()
	if entry == nil || time.Since(entry.computedAt) > ingestionSnapshotMaxAge {
		return nil
	}
	return entry
}

// StartIngestionSnapshotRefresh rebuilds [Server.ingestionSnapshot] every 15s, matching the
// Cache-Control max-age, until ctx is cancelled. Call it once. A slow build never blocks reads,
// which keep serving the previous snapshot until the store.
func (s *Server) StartIngestionSnapshotRefresh(ctx context.Context) {
	const cadence = ingestionSnapshotCadence
	// Fire once immediately so the first request hits the warm path. Each build uses its own
	// context, detached from any request, like the SWR detached fill in CachedHistoryReader.
	doRefresh := func() { //nolint:gosec,contextcheck // G118/contextcheck — intentional detached build; the parent ctx is the LIFETIME of the api process, not any individual request, so the closure deliberately doesn't accept the caller's ctx + uses context.Background+timeout instead.
		buildCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		out := s.buildIngestionSnapshot(buildCtx)
		// Keep the last good snapshot when the new build is degraded and empty, since all zeros
		// would hide the failure; it is marked degraded so ingestionFlags still reports it.
		if out.degraded && out.Ledger.LatestLedger == 0 {
			if prev := s.ingestionSnapshot.Load(); prev != nil && prev.snap.Ledger.LatestLedger > 0 {
				keep := prev.snap
				keep.degraded = true
				s.ingestionSnapshot.Store(&ingestionSnapshotEntry{snap: keep, computedAt: time.Now(), builtAt: prev.builtAt})
				return
			}
		}
		now := time.Now()
		s.ingestionSnapshot.Store(&ingestionSnapshotEntry{snap: out, computedAt: now, builtAt: now})
	}
	doRefresh()
	t := time.NewTicker(cadence)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			doRefresh()
		}
	}
}

// buildIngestionSnapshot runs the parallel-fillers pipeline for one IngestionDiagnostics. Both
// the inline fallback and the background refresher call it; it writes no response.
func (s *Server) buildIngestionSnapshot(ctx context.Context) IngestionDiagnostics { //nolint:funlen,gocognit,gocyclo // linear filler-orchestration; splitting would obscure the parallel pattern.
	out := IngestionDiagnostics{
		Region: RegionInfo{
			Name:       s.regionName,
			Deployment: s.regionDeployment,
		},
		Version: IngestionVersionInfo{
			Version:   version.Version,
			BuildDate: version.BuildDate,
			Commit:    version.Commit,
			Dirty:     version.Dirty,
			GoVersion: version.GoVersion,
		},
	}
	// Defensive empty-slice init: Go marshals nil slices as `null`,
	// which crashes naïve clients that do `rows.length`.
	out.Backfill = []BackfillDecoderState{}
	out.BackfillCoverage = []BackfillCoverageRow{}
	out.Sources = []SourceHealthRow{}

	// Fillers run concurrently, each with its own timeout so a slow reader cannot block the others.
	// The trades-scan cache is best-effort; overlaySourceCoverageV2 supplies authoritative coverage,
	// so an empty or stale cache cannot blank the snapshot.
	var cacheRows []timescale.BackfillCoverage
	if s.BackfillCoverage != nil {
		cacheRows, _ = s.BackfillCoverage.Snapshot()
	}

	// degraded is shared across fillers, so it is an atomic.Bool here (a field on out would race)
	// and is copied onto out.degraded after wg.Wait().
	var degraded atomic.Bool
	type filler struct {
		name    string
		fn      func(context.Context)
		timeout time.Duration
	}
	fillers := []filler{
		{"sources", func(c context.Context) { out.Sources = buildSourceHealth(c, s) }, 8 * time.Second},
		{"ledger", func(c context.Context) { s.fillIngestionLedger(c, &out, &degraded) }, 6 * time.Second},
		{"backfill", func(c context.Context) { s.fillIngestionBackfill(c, &out, &degraded) }, 5 * time.Second},
		{"fx_coverage", func(c context.Context) { s.fillIngestionFXCoverage(c, &out) }, 5 * time.Second},
		{"supply_coverage", func(c context.Context) { s.fillIngestionSupplyCoverage(c, &out) }, 5 * time.Second},
		{"cagg_coverage", func(c context.Context) { s.fillIngestionCAGGCoverage(c, &out) }, 5 * time.Second},
		{"entry_counts", func(c context.Context) { s.fillIngestionEntryCounts(c, &out, &degraded) }, 5 * time.Second},
	}
	var wg sync.WaitGroup
	for _, f := range fillers {
		wg.Add(1)
		go func(f filler) {
			defer wg.Done()
			// middleware.Recoverer does not cover this goroutine, so an unrecovered filler panic
			// would crash the API. Recovery leaves that filler's fields zero, the same shape as
			// a timed-out filler.
			defer worker.Recover(s.logger, "api-ingestion-snapshot-filler")
			subCtx, subCancel := context.WithTimeout(ctx, f.timeout)
			defer subCancel()
			f.fn(subCtx)
		}(f)
	}
	wg.Wait()
	if degraded.Load() {
		out.degraded = true
	}

	// The row skeleton needs the tip that fillIngestionLedger just populated.
	tip := out.Ledger.LatestLedger
	out.BackfillCoverage = buildBackfillCoverage(cacheRows, out.entryCounts, tip)
	// The data-derived snapshot is the headline signal. With an empty snapshot table the fields
	// stay zero and the status page shows "Pending" rather than a misleading 100%.
	s.overlaySourceCoverageV2(ctx, &out.BackfillCoverage)
	s.overlayCompleteness(ctx, &out.BackfillCoverage)
	if len(out.BackfillCoverage) > 0 {
		// Stamp request time, not the possibly stale trades-scan cache time.
		out.BackfillCoverageAt = time.Now().UTC().Format(time.RFC3339)
	}
	return out
}

// overlaySourceCoverageV2 fills the authoritative DensityPct, CoveredLedgers, GapFreePct and
// CoverageSnapshotAt from source_coverage_snapshots.
//
// Multi-table sources take the MIN of density, gap_free and covered_ledgers across tables (one
// empty target means not fully covered); snapshot_at is the OLDEST last_updated. expected_ledgers
// is the snapshot's trailing-window size so covered/expected stays coherent with density_pct.
// Soft-fail: a reader error or empty table leaves zeros, which the UI renders as "Pending".
//
//nolint:gocognit // linear pipeline; multi-table aggregation reads better inline than as a separate helper.
func (s *Server) overlaySourceCoverageV2(ctx context.Context, rows *[]BackfillCoverageRow) {
	if s.CoverageReader == nil {
		return
	}
	snaps, err := s.CoverageReader.ListSourceCoverage(ctx)
	if err != nil {
		s.logger.Warn("diagnostics/ingestion: source_coverage_snapshots read failed (rows show Pending)", "err", err)
		return
	}
	bySource := make(map[string][]timescale.SourceCoverage, len(snaps))
	cadence := make(map[string]time.Duration, len(snaps))
	for _, sn := range snaps {
		key := sourceFromTargetSource(sn.Source)
		bySource[key] = append(bySource[key], sn)
		if c := targetScanCadence(sn.Source); c > cadence[key] {
			cadence[key] = c
		}
	}
	for i := range *rows {
		src := (*rows)[i].Source
		ss, ok := bySource[src]
		if !ok || len(ss) == 0 {
			continue
		}
		density, gapFree := ss[0].DensityPct, ss[0].GapFreePct
		covered := ss[0].DistinctLedgers
		expected := ss[0].ExpectedLedgers
		oldest := ss[0].LastUpdated
		for _, s := range ss[1:] {
			if s.DensityPct < density {
				density = s.DensityPct
			}
			if s.GapFreePct < gapFree {
				gapFree = s.GapFreePct
			}
			if s.DistinctLedgers < covered {
				covered = s.DistinctLedgers
			}
			if s.ExpectedLedgers < expected {
				expected = s.ExpectedLedgers
			}
			if s.LastUpdated.Before(oldest) {
				oldest = s.LastUpdated
			}
		}
		(*rows)[i].DensityPct = density
		(*rows)[i].CoveredLedgers = covered
		(*rows)[i].GapFreePct = gapFree
		(*rows)[i].CoverageSnapshotAt = wireTimePtr(&oldest)
		(*rows)[i].CoverageScanCadenceS = int64(cadence[src] / time.Second)
		// Guarded so a snapshot carrying expected=0 never zeroes a populated row.
		if expected > 0 {
			(*rows)[i].ExpectedLedgers = expected
		}
		// coverage_pct must answer "did the indexer skip any ledger", not "how dense is the
		// event stream": a sparse oracle has near-zero density but should read 100% covered.
		// gap_free_pct (ADR-0030) is exactly that signal.
		(*rows)[i].CoveragePct = gapFree
	}
}

// overlayCompleteness fills the ADR-0033 watermark fields from completeness_snapshots. It only
// touches completeness_* fields (and display ledgers when unset) for sources with a snapshot;
// the system "recognition" snapshot has no per-source row and is not overlaid.
func (s *Server) overlayCompleteness(ctx context.Context, rows *[]BackfillCoverageRow) {
	if s.CompletenessReader == nil {
		return
	}
	snaps, err := s.CompletenessReader.ListCompletenessSnapshots(ctx)
	if err != nil {
		s.logger.Warn("diagnostics/ingestion: completeness_snapshots read failed (completeness_* absent)", "err", err)
		return
	}
	bySource := make(map[string]timescale.CompletenessSnapshot, len(snaps))
	for _, sn := range snaps {
		bySource[sn.Source] = sn
	}
	for i := range *rows {
		sn, ok := bySource[(*rows)[i].Source]
		if !ok {
			continue
		}
		computedAt := sn.ComputedAt
		pct := sn.CoveragePct
		(*rows)[i].CompletenessPct = &pct
		(*rows)[i].CompletenessWatermark = int64(sn.Watermark)
		(*rows)[i].CompletenessComplete = sn.Complete
		(*rows)[i].CompletenessLakeComplete = sn.LakeComplete
		(*rows)[i].CompletenessComputedAt = wireTimePtr(&computedAt)
		if (*rows)[i].EarliestLedger == 0 && sn.Watermark > 0 {
			from := sn.Genesis
			if sn.ProjectionVerifiedFrom > 0 {
				from = sn.ProjectionVerifiedFrom
			}
			(*rows)[i].EarliestLedger = int64(from)
			(*rows)[i].LatestLedger = int64(sn.Watermark)
		}
	}
}

// targetScanCadence returns the named gap-detector target's scan cadence, or the default interval
// for an unregistered name.
func targetScanCadence(targetSource string) time.Duration {
	for _, t := range timescale.DefaultGapDetectorTargets {
		if t.Source == targetSource {
			return t.EffectiveScanCadence()
		}
	}
	return timescale.GapDetectorInterval
}

// sourceFromTargetSource maps a per-table gap-detector target name back to the per-protocol
// source name ("blend-positions" → "blend"). Only the multi-table protocols (blend, phoenix,
// sep41) need explicit mapping.
func sourceFromTargetSource(targetSource string) string {
	switch targetSource {
	case "blend-positions", "blend-emissions", "blend-admin", "blend-auctions":
		return "blend"
	case "phoenix-liquidity", "phoenix-stake":
		return "phoenix"
	case "sep41-transfers", "sep41-supply":
		// SEP-41 is not a protocol-level source here; per-table targets surface directly.
		return targetSource
	case "comet-liquidity":
		return "comet"
	case "soroswap-skim":
		return "soroswap"
	}
	// Default 1:1 (sdex, soroban-events, cctp, rozo, ...)
	return targetSource
}

// fillIngestionLedger copies network-stats onto out.Ledger. Soft-fail: a stuck reader leaves
// zero defaults but sets the shared degraded flag, so the response is marked flags.stale rather
// than passing zeros off as a fresh read.
func (s *Server) fillIngestionLedger(ctx context.Context, out *IngestionDiagnostics, degraded *atomic.Bool) {
	if s.NetworkStats == nil {
		degraded.Store(true)
		return
	}
	ns, err := s.NetworkStats.GetNetworkStats(ctx)
	if err != nil {
		s.logger.Warn("diagnostics/ingestion: network_stats", "err", err)
		degraded.Store(true)
		return
	}
	out.Ledger.LatestLedger = ns.LatestLedger
	out.Ledger.MarketsCount24h = ns.MarketsCount24h
	out.Ledger.AssetsIndexed = ns.AssetsIndexed
	if ns.Volume24hUSD != nil {
		out.Ledger.Volume24hUSD = *ns.Volume24hUSD
	}
}

// fillIngestionBackfill derives out.Backfill and out.Ledger.LagSeconds from one cursors fetch.
func (s *Server) fillIngestionBackfill(ctx context.Context, out *IngestionDiagnostics, degraded *atomic.Bool) {
	if s.Cursors == nil {
		degraded.Store(true)
		return
	}
	rows, err := s.Cursors.ListCursors(ctx)
	if err != nil {
		s.logger.Warn("diagnostics/ingestion: cursors", "err", err)
		degraded.Store(true)
		return
	}
	out.Backfill = aggregateBackfill(rows)
	out.Ledger.LagSeconds = ledgerStreamLagSeconds(rows)
}

// fillIngestionFXCoverage leaves the section empty when the wired FX reader (a test fake, say)
// does not implement FXCoverageReader.
func (s *Server) fillIngestionFXCoverage(ctx context.Context, out *IngestionDiagnostics) {
	reader, ok := s.FXHistory.(FXCoverageReader)
	if !ok || reader == nil {
		return
	}
	cov, err := reader.FXCoverageStats(ctx)
	if err != nil {
		s.logger.Warn("diagnostics/ingestion: fx_coverage", "err", err)
		return
	}
	out.FXBackfill = FXBackfillState{
		TotalQuotes:     cov.TotalQuotes,
		CurrenciesCount: cov.CurrenciesCount,
	}
	if !cov.EarliestQuote.IsZero() {
		out.FXBackfill.EarliestQuote = cov.EarliestQuote.Format("2006-01-02")
	}
	if !cov.LatestQuote.IsZero() {
		out.FXBackfill.LatestQuote = cov.LatestQuote.Format("2006-01-02")
	}
}

// fillIngestionCAGGCoverage reads prices_1h's MIN/MAX bucket, the real historical-span answer
// under ADR-0034 (the trades table is only the served tier). The type assertion no-ops on test
// fakes that do not implement CAGGCoverageReader.
func (s *Server) fillIngestionCAGGCoverage(ctx context.Context, out *IngestionDiagnostics) {
	reader, ok := s.FXHistory.(CAGGCoverageReader)
	if !ok || reader == nil {
		return
	}
	cov, err := reader.CAGGCoverageStats(ctx)
	if err != nil {
		s.logger.Warn("diagnostics/ingestion: cagg_coverage", "err", err)
		return
	}
	out.CAGGCoverage = CAGGCoverageView{BucketCount: cov.BucketCount}
	if !cov.EarliestBucket.IsZero() {
		out.CAGGCoverage.EarliestBucket = cov.EarliestBucket.UTC().Format(time.RFC3339)
	}
	if !cov.LatestBucket.IsZero() {
		out.CAGGCoverage.LatestBucket = cov.LatestBucket.UTC().Format(time.RFC3339)
	}
}

// fillIngestionEntryCounts stashes the source_entry_counts tally onto out for
// buildBackfillCoverage. Soft-fail: a reader error leaves entryCounts nil, so `entries` reads 0.
func (s *Server) fillIngestionEntryCounts(ctx context.Context, out *IngestionDiagnostics, degraded *atomic.Bool) {
	reader, ok := s.FXHistory.(SourceEntryCountReader)
	if !ok || reader == nil {
		// In production this means the fxHistory adapter lacks its SourceEntryCounts delegate,
		// which silently zeroes `entries` for every source; warn so that wiring bug is visible.
		s.logger.Warn("diagnostics/ingestion: entry_counts reader unavailable — entries will read 0 for all sources")
		return
	}
	counts, err := reader.SourceEntryCounts(ctx)
	if err != nil {
		s.logger.Warn("diagnostics/ingestion: entry_counts", "err", err)
		degraded.Store(true)
		return
	}
	out.entryCounts = counts
}

// buildBackfillCoverage returns the per-source row skeleton (name, genesis, entries, expected
// ledgers); overlaySourceCoverageV2 fills the data-derived fields afterwards (ADR-0031).
// Cursors never enter coverage. entryCounts supplies `entries`, exact even mid-backfill;
// cacheRows only carries off-chain CEX/FX row presence, so an empty cache just drops those rows.
func buildBackfillCoverage(cacheRows []timescale.BackfillCoverage, entryCounts map[string]int64, tip int64) []BackfillCoverageRow {
	out := make([]BackfillCoverageRow, 0, len(sourceGenesisLedger)+len(cacheRows))

	sources := make([]string, 0, len(sourceGenesisLedger))
	for src := range sourceGenesisLedger {
		sources = append(sources, src)
	}
	sort.Strings(sources)
	for _, src := range sources {
		genesis := sourceGenesisLedger[src]
		row := BackfillCoverageRow{
			Source:        src,
			Applies:       true,
			GenesisLedger: genesis,
			Entries:       entryCounts[src],
		}
		if genesis > 0 && tip > 0 {
			row.ExpectedLedgers = tip - genesis + 1
		}
		// The coverage fields are filled later by overlaySourceCoverageV2.
		out = append(out, row)
	}

	// Cache-only rows: off-chain CEX/FX (no genesis, no cursors) and unmapped on-chain sources.
	// Best-effort; absent when the cache is cold.
	for _, r := range cacheRows {
		if _, mapped := sourceGenesisLedger[r.Source]; mapped {
			continue
		}
		row := BackfillCoverageRow{Source: r.Source, Entries: entryCounts[r.Source]}
		if r.EarliestLedger > 0 && r.LatestLedger > 0 {
			row.Applies = true
			row.EarliestLedger = r.EarliestLedger
			row.LatestLedger = r.LatestLedger
		}
		out = append(out, row)
	}
	return out
}

// fillIngestionSupplyCoverage leaves the section empty when the wired supply reader does not
// implement SupplyCoverageReader.
func (s *Server) fillIngestionSupplyCoverage(ctx context.Context, out *IngestionDiagnostics) {
	reader, ok := s.Supply.(SupplyCoverageReader)
	if !ok || reader == nil {
		return
	}
	cov, err := reader.SupplyCoverageStats(ctx)
	if err != nil {
		s.logger.Warn("diagnostics/ingestion: supply_coverage", "err", err)
		return
	}
	out.Supply = SupplyStateView{
		ClassicAssets: cov.ClassicAssets,
		SEP41Assets:   cov.SEP41Assets,
		LatestLedger:  cov.LatestLedger,
	}
	if !cov.LastSnapshotAt.IsZero() {
		out.Supply.LastSnapshotAt = cov.LastSnapshotAt.UTC().Format(time.RFC3339)
	}
}

// aggregateBackfill collapses per-range backfill cursor rows into one BackfillDecoderState per
// decoder set. The cursor sub_source format is "<start>-<end>:<decoder-set>" (e.g.
// "50534290-51275895:sdex,soroswap"). RangesActive counts ranges whose last_ledger < range_end,
// i.e. still catching up.
func aggregateBackfill(rows []timescale.Cursor) []BackfillDecoderState {
	type group struct {
		total       int
		complete    int // last_ledger == range_end
		running     int // incomplete AND updated_at within stalledThreshold
		stalled     int // incomplete AND updated_at older than stalledThreshold
		newestLedge int64
		oldestAt    time.Time
	}
	groups := map[string]*group{}
	now := time.Now().UTC()
	for _, c := range rows {
		if c.Source != "backfill" {
			continue
		}
		decoder, rangeEnd := parseBackfillSub(c.Sub)
		if decoder == "" {
			continue
		}
		g := groups[decoder]
		if g == nil {
			g = &group{}
			groups[decoder] = g
		}
		g.total++
		incomplete := rangeEnd > 0 && int64(c.LastLedger) < rangeEnd
		if !incomplete {
			g.complete++
		} else if now.Sub(c.UpdatedAt) <= stalledThreshold {
			g.running++
		} else {
			g.stalled++
		}
		if int64(c.LastLedger) > g.newestLedge {
			g.newestLedge = int64(c.LastLedger)
		}
		if g.oldestAt.IsZero() || c.UpdatedAt.Before(g.oldestAt) {
			g.oldestAt = c.UpdatedAt
		}
	}
	out := make([]BackfillDecoderState, 0, len(groups))
	for decoder, g := range groups {
		state := BackfillDecoderState{
			Decoder:        decoder,
			RangesTotal:    g.total,
			RangesComplete: g.complete,
			RangesRunning:  g.running,
			RangesStalled:  g.stalled,
			RangesActive:   g.running + g.stalled,
			NewestLedger:   g.newestLedge,
		}
		if !g.oldestAt.IsZero() {
			state.OldestUpdatedAt = g.oldestAt.UTC().Format(time.RFC3339)
			state.OldestLagSecs = int64(now.Sub(g.oldestAt).Seconds())
		}
		out = append(out, state)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Decoder < out[j].Decoder })
	return out
}

// parseBackfillSub splits a "<start>-<end>:<decoder-set>" sub_source into the decoder set and
// range end. A malformed cursor returns ("", 0) rather than breaking aggregation.
func parseBackfillSub(sub string) (decoder string, rangeEnd int64) {
	colonIdx := strings.IndexByte(sub, ':')
	if colonIdx <= 0 || colonIdx == len(sub)-1 {
		return "", 0
	}
	rangePart := sub[:colonIdx]
	decoder = sub[colonIdx+1:]
	dashIdx := strings.IndexByte(rangePart, '-')
	if dashIdx <= 0 || dashIdx == len(rangePart)-1 {
		return decoder, 0
	}
	endStr := rangePart[dashIdx+1:]
	rangeEnd = parseInt64(endStr)
	return decoder, rangeEnd
}

// parseInt64 returns 0 on parse failure — defensive default.
func parseInt64(s string) int64 {
	var n int64
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int64(r-'0')
	}
	return n
}

// ledgerStreamLagSeconds reports the age of the "ledgerstream" cursor, or 0 when none exists.
func ledgerStreamLagSeconds(rows []timescale.Cursor) int64 {
	now := time.Now().UTC()
	for _, c := range rows {
		if c.Source == "ledgerstream" {
			return int64(now.Sub(c.UpdatedAt).Seconds())
		}
	}
	return 0
}

// buildSourceHealth projects the static external.Registry onto the wire shape, joined with the
// 24h sourcesStats. Sources with no recent trades render as 0/empty rather than absent: zero
// trades in 24h is a signal, not a hidden row.
func buildSourceHealth(ctx context.Context, s *Server) []SourceHealthRow {
	// Entries-24h from the per-source Prometheus event counter runs FIRST under its own short
	// deadline: the trades aggregation below is slow under backfill load and can consume the whole
	// filler budget, which would leave this query no deadline and read 0 for every source.
	// Soft-fail to empty.
	entries24h := map[string]int64{}
	if s.StatusBackend != nil {
		ectx, cancel := context.WithTimeout(ctx, 5*time.Second)
		m, err := s.StatusBackend.SourceEntries24h(ectx)
		cancel()
		if err == nil {
			entries24h = m
		} else {
			s.logger.Warn("diagnostics/ingestion: source entries_24h", "err", err)
		}
	}
	// Which sources are switched on. On error every row reads enabled=false: visibly wrong rather
	// than quietly implying a dead connector, and it self-corrects on the next request.
	enabledSources := map[string]bool{}
	if s.StatusBackend != nil {
		ectx, cancel := context.WithTimeout(ctx, 5*time.Second)
		m, err := s.StatusBackend.SourceEnabled(ectx)
		cancel()
		if err == nil {
			enabledSources = m
		} else {
			s.logger.Warn("diagnostics/ingestion: source enabled", "err", err)
		}
	}

	// Trades/volume/markets are best-effort: the error is swallowed because under backfill load this
	// scan can time out, leaving trade_count/volume empty while entries_24h still shows activity.
	statsBySource := map[string]timescale.SourceStats{}
	if s.SourcesStats != nil {
		if rows, err := s.SourcesStats.GetSourceStats(ctx); err == nil {
			for _, r := range rows {
				statsBySource[r.Source] = r
			}
		}
	}
	out := make([]SourceHealthRow, 0, len(external.Registry))
	for name, meta := range external.Registry {
		row := SourceHealthRow{
			Name:          name,
			Class:         string(meta.Class),
			Subclass:      string(meta.Subclass),
			IncludeInVWAP: meta.IncludeInVWAP,
			BackfillSafe:  meta.BackfillSafe(),
		}
		if st, ok := statsBySource[name]; ok {
			row.TradeCount24h = st.TradeCount24h
			row.MarketsCount24h = st.MarketsCount24h
			if st.VolumeUSD24h.Valid {
				row.VolumeUSD24h = st.VolumeUSD24h.String
			}
		}
		row.Entries24h = entries24h[name]
		row.Enabled = enabledSources[name]
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
