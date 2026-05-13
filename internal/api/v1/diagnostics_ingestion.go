package v1

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/RatesEngine/rates-engine/internal/currency/marketcap"
	"github.com/RatesEngine/rates-engine/internal/sources/external"
	"github.com/RatesEngine/rates-engine/internal/storage/timescale"
	"github.com/RatesEngine/rates-engine/internal/version"
)

// FXCoverageReader is the seam the ingestion diagnostics reads
// fx_quotes coverage stats through. timescale.Store satisfies it
// via FXCoverageStats.
type FXCoverageReader interface {
	FXCoverageStats(ctx context.Context) (timescale.FXCoverage, error)
}

// SupplyCoverageReader is the seam the ingestion diagnostics reads
// asset_supply_history coverage stats through. timescale.Store
// satisfies it via SupplyCoverageStats.
type SupplyCoverageReader interface {
	SupplyCoverageStats(ctx context.Context) (timescale.SupplyCoverage, error)
}

// IngestionDiagnostics is the wire shape for
// /v1/diagnostics/ingestion. One snapshot of the region's ingest
// state — region label, version, ledger tip, per-decoder backfill,
// FX coverage, market-cap cache state, supply coverage, and the
// full source registry projected onto live stats. Designed to be
// the only call the public status page makes for the ingestion
// section so it can render the whole region panel without scraping
// five separate endpoints.
type IngestionDiagnostics struct {
	Region   RegionInfo             `json:"region"`
	Version  IngestionVersionInfo   `json:"version"`
	Ledger   LedgerTip              `json:"ledger"`
	Backfill []BackfillDecoderState `json:"backfill"`
	// BackfillCoverage answers "do we have data from genesis to
	// tip?" by reporting per-source MIN/MAX ledger + trade count
	// derived from the trades hypertable. SECONDARY to Backfill —
	// `Backfill` shows what backfill *is doing*, `BackfillCoverage`
	// shows what data we *actually have*. Both matter: a stalled
	// backfill range with last_ledger=18M against an SDEX coverage
	// MIN=61M means SDEX history pre-61M is missing AND the
	// backfill that would fill it isn't progressing.
	BackfillCoverage   []BackfillCoverageRow `json:"backfill_coverage"`
	BackfillCoverageAt string                `json:"backfill_coverage_as_of,omitempty"`
	FXBackfill         FXBackfillState       `json:"fx_backfill"`
	MarketCap          MarketCapState        `json:"market_cap"`
	Supply             SupplyStateView       `json:"supply"`
	Sources            []SourceHealthRow     `json:"sources"`
}

// BackfillCoverageRow is the per-source coverage projection.
//
// EarliestLedger / LatestLedger come straight from the trades
// hypertable's MIN/MAX(ledger) for that source. CEX/FX sources
// report 0 / 0 because their trades don't carry a Stellar ledger
// — `applies` distinguishes the two cases for the UI (no point
// drawing a "coverage bar" for binance).
//
// GenesisLedger is the source's known start point — 1 for SDEX
// (Stellar pubnet genesis), the contract deploy ledger for
// Soroban contracts, 0 ("not applicable") for CEX/FX. Hardcoded
// in `sourceGenesisLedger`; when an operator deploys a new source
// add a row there.
//
// CoveragePct is `(LatestLedger - max(EarliestLedger, GenesisLedger)
// + 1) / (LatestLedger - GenesisLedger + 1)` — i.e. fraction of the
// expected genesis-to-tip range we have any data for. Doesn't
// detect internal gaps (EarliestLedger=1, LatestLedger=tip with a
// hole at 30M-40M still scores 100%); for that we'd need a much
// heavier distinct-ledger scan, deferred.
type BackfillCoverageRow struct {
	Source         string  `json:"source"`
	Applies        bool    `json:"applies"`
	GenesisLedger  int64   `json:"genesis_ledger,omitempty"`
	EarliestLedger int64   `json:"earliest_ledger,omitempty"`
	LatestLedger   int64   `json:"latest_ledger,omitempty"`
	TradeCount     int64   `json:"trade_count"`
	CoveragePct    float64 `json:"coverage_pct,omitempty"`
}

// sourceGenesisLedger is the operator-curated map of "what's the
// earliest ledger this source can possibly have data for". Values:
//   - 1                : SDEX (Stellar pubnet genesis 2015-08-19).
//   - <contract deploy>: per-Soroban-contract first observable
//     ledger. For dispatcher-routed sources we set it slightly
//     before the on-chain deploy (gives the "we cover this fully"
//     check some slack against the exact deploy ledger). Approx
//     values are fine — the UI shows "X% of expected range" so
//     a few-thousand-ledger error is invisible.
//   - 0 (default)      : not applicable (CEX/FX/aggregator/oracle —
//     these sources don't have a Stellar-ledger genesis concept).
//
// When a new on-chain source ships, add its known deploy ledger
// here. The list intentionally sits next to the projection so a
// reviewer notices it during PR review.
var sourceGenesisLedger = map[string]int64{
	"sdex": 1,
	// Soroban contracts — approximate deploy-era ledgers from
	// the per-contract WASM audits (docs/operations/wasm-audits/).
	"soroswap":      54_000_000,
	"aquarius":      54_500_000,
	"phoenix":       53_700_000,
	"comet":         53_900_000,
	"blend":         54_000_000,
	"reflector-cex": 51_000_000,
	"reflector-dex": 51_000_000,
	"reflector-fx":  51_000_000,
	"band":          53_500_000,
	"redstone":      55_000_000,
}

// RegionInfo identifies which deployment generated this snapshot.
// Today r1/production only; r2/r3 will join when their playbooks
// land. Mirrors the Region shape on /v1/status — a clean rename
// here would mean cross-endpoint drift, so we lift the same one.
type RegionInfo struct {
	Name       string `json:"name"`
	Deployment string `json:"deployment"`
}

// IngestionVersionInfo is the same five fields /v1/version returns.
// Duplicated onto the ingestion response so an operator screen can
// render "what's running here" without a second fetch — matters
// during cross-region drift investigations when comparing r1 vs r2
// without a SSH window open.
type IngestionVersionInfo struct {
	Version   string `json:"version"`
	BuildDate string `json:"build_date"`
	Commit    string `json:"commit"`
	Dirty     string `json:"dirty"`
	GoVersion string `json:"go_version"`
}

// LedgerTip summarises live-network ingest progress. LatestLedger
// comes from prices_1m's MAX(ledger_sequence); LagSeconds is the
// wall-clock age of the most recent ledgerstream cursor update.
// Volume / markets / assets come from the same source as
// /v1/network/stats so the two endpoints agree.
type LedgerTip struct {
	LatestLedger    int64  `json:"latest_ledger"`
	LagSeconds      int64  `json:"lag_seconds"`
	Volume24hUSD    string `json:"volume_24h_usd,omitempty"`
	MarketsCount24h int64  `json:"markets_count_24h"`
	AssetsIndexed   int64  `json:"assets_indexed"`
}

// BackfillDecoderState is one row of the per-decoder backfill
// summary. The aggregator runs many backfill ranges concurrently
// (one per worker × decoder set); this struct collapses them into
// the per-decoder view operators actually want: how many ranges
// are still running, which is oldest, when did the slowest one
// last advance.
//
// Decoder = the comma-separated decoder set from the cursor's
// sub_source after the range prefix (e.g.
// "50534290-51275895:sdex,soroswap" → decoder "sdex,soroswap").
type BackfillDecoderState struct {
	Decoder         string `json:"decoder"`
	RangesTotal     int    `json:"ranges_total"`
	RangesActive    int    `json:"ranges_active"`
	OldestUpdatedAt string `json:"oldest_updated_at,omitempty"`
	OldestLagSecs   int64  `json:"oldest_lag_seconds"`
	NewestLedger    int64  `json:"newest_ledger"`
}

// FXBackfillState describes how much fiat-rate history we have in
// fx_quotes. Earliest/Latest are RFC3339 dates (truncated to day
// boundaries by the upstream daily snapshot cadence). Empty when
// the table is empty.
type FXBackfillState struct {
	EarliestQuote   string `json:"earliest_quote,omitempty"`
	LatestQuote     string `json:"latest_quote,omitempty"`
	TotalQuotes     int64  `json:"total_quotes"`
	CurrenciesCount int    `json:"currencies_count"`
}

// MarketCapState is a slim view of the marketcap.Cache. Populated
// at request time from cache.All() — small map (~22 catalogue
// entries today), so no incremental cost. OldestFetchedAt is the
// LRU age of the oldest entry (if any); a stale OldestFetchedAt
// signals the CG refresher is wedged.
type MarketCapState struct {
	EntriesCount    int    `json:"entries_count"`
	OldestFetchedAt string `json:"oldest_fetched_at,omitempty"`
	NewestFetchedAt string `json:"newest_fetched_at,omitempty"`
}

// SupplyStateView projects timescale.SupplyCoverage onto the wire.
// Splits "classic" (XLM + CODE:G…) from "SEP-41" (C-strkey
// contract addresses) so operators can spot a stalled observer
// per asset domain — the supply observers run independently and
// can wedge separately.
type SupplyStateView struct {
	ClassicAssets  int    `json:"classic_assets_with_supply"`
	SEP41Assets    int    `json:"sep41_assets_with_supply"`
	LastSnapshotAt string `json:"last_snapshot_at,omitempty"`
	LatestLedger   int64  `json:"latest_ledger,omitempty"`
}

// SourceHealthRow joins the static external.Registry metadata
// (class, subclass, VWAP-eligibility, backfill-safety) with the
// live 24h stats from sourcesStats (trades, volume, markets) so
// operators can see at a glance which sources are silent vs
// actively ingesting. Sorted by name for deterministic responses.
type SourceHealthRow struct {
	Name            string `json:"name"`
	Class           string `json:"class"`
	Subclass        string `json:"subclass,omitempty"`
	IncludeInVWAP   bool   `json:"include_in_vwap"`
	BackfillSafe    bool   `json:"backfill_safe"`
	TradeCount24h   int64  `json:"trade_count_24h"`
	VolumeUSD24h    string `json:"volume_24h_usd,omitempty"`
	MarketsCount24h int64  `json:"markets_count_24h"`
}

// handleDiagnosticsIngestion serves GET /v1/diagnostics/ingestion.
//
// Composes the ingestion snapshot from existing readers + the
// in-memory marketcap cache + the external.Registry. Every reader
// fans out under a single 6s deadline; per-reader soft-fail means
// one stuck dependency degrades that section to an empty / default
// value rather than failing the whole response.
//
// Cache: short, public, max-age=15s. The data underneath changes
// at most every few seconds (cursor updates, supply observer ticks)
// so 15s smooths the load from a refreshing status page without
// hiding live degradation.
func (s *Server) handleDiagnosticsIngestion(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
	defer cancel()

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
		Sources: buildSourceHealth(ctx, s),
	}
	s.fillIngestionLedger(ctx, &out)
	s.fillIngestionBackfill(ctx, &out)
	s.fillIngestionFXCoverage(ctx, &out)
	s.fillIngestionSupplyCoverage(ctx, &out)
	s.fillIngestionBackfillCoverage(&out)
	out.MarketCap = projectMarketCapState(s.marketCaps)

	w.Header().Set("Cache-Control", "public, max-age=15, s-maxage=15")
	writeJSON(w, out, Flags{})
}

// fillIngestionLedger reads network-stats and copies the four
// numeric fields onto out.Ledger. Soft-fail: a stuck reader leaves
// the section at zero-valued defaults rather than erroring the
// whole response.
func (s *Server) fillIngestionLedger(ctx context.Context, out *IngestionDiagnostics) {
	if s.networkStats == nil {
		return
	}
	ns, err := s.networkStats.GetNetworkStats(ctx)
	if err != nil {
		s.logger.Warn("diagnostics/ingestion: network_stats", "err", err)
		return
	}
	out.Ledger.LatestLedger = ns.LatestLedger
	out.Ledger.MarketsCount24h = ns.MarketsCount24h
	out.Ledger.AssetsIndexed = ns.AssetsIndexed
	if ns.Volume24hUSD != nil {
		out.Ledger.Volume24hUSD = *ns.Volume24hUSD
	}
}

// fillIngestionBackfill reads the cursors table and projects two
// outputs: the per-decoder backfill state on out.Backfill, and the
// live-stream cursor age on out.Ledger.LagSeconds. Done in one
// helper because both derive from the same fetch.
func (s *Server) fillIngestionBackfill(ctx context.Context, out *IngestionDiagnostics) {
	if s.cursors == nil {
		return
	}
	rows, err := s.cursors.ListCursors(ctx)
	if err != nil {
		s.logger.Warn("diagnostics/ingestion: cursors", "err", err)
		return
	}
	out.Backfill = aggregateBackfill(rows)
	out.Ledger.LagSeconds = ledgerStreamLagSeconds(rows)
}

// fillIngestionFXCoverage type-asserts that the wired FX reader
// also implements FXCoverageReader. Production wiring (Store) does;
// test fakes may not, in which case the section stays empty.
func (s *Server) fillIngestionFXCoverage(ctx context.Context, out *IngestionDiagnostics) {
	reader, ok := s.fxHistory.(FXCoverageReader)
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

// fillIngestionBackfillCoverage projects the cached coverage
// snapshot onto the wire shape. Reads from a process-local cache
// (background-refreshed every 5 min); zero-allocates on a normal
// hit. Empty section when the cache hasn't been wired (test
// builds) or hasn't completed its first refresh yet (cold start
// in the first 5 min).
func (s *Server) fillIngestionBackfillCoverage(out *IngestionDiagnostics) {
	if s.backfillCoverage == nil {
		return
	}
	rows, fetchedAt := s.backfillCoverage.Snapshot()
	if fetchedAt.IsZero() {
		return
	}
	out.BackfillCoverageAt = fetchedAt.UTC().Format(time.RFC3339)
	tip := out.Ledger.LatestLedger
	out.BackfillCoverage = projectBackfillCoverage(rows, tip)
}

// projectBackfillCoverage maps the storage-layer rows onto the
// wire shape, joining with sourceGenesisLedger to compute
// CoveragePct. CEX/FX sources (LatestLedger == 0) are reported
// with Applies=false so the UI can render them as "not applicable"
// rather than a misleading 0% bar.
//
// CoveragePct definition: of the (genesis → tip) range we want to
// cover, what fraction do we have *any* data for? Doesn't measure
// internal density — gaps inside the EarliestLedger..LatestLedger
// span aren't visible here. That's a separate, much heavier query
// (distinct-ledger scan) deferred to a follow-up.
func projectBackfillCoverage(rows []timescale.BackfillCoverage, tip int64) []BackfillCoverageRow {
	out := make([]BackfillCoverageRow, 0, len(rows))
	for _, r := range rows {
		row := BackfillCoverageRow{
			Source:     r.Source,
			TradeCount: r.TradeCount,
		}
		applies := r.LatestLedger > 0 && r.EarliestLedger > 0
		if applies {
			row.Applies = true
			row.EarliestLedger = r.EarliestLedger
			row.LatestLedger = r.LatestLedger
			if g, ok := sourceGenesisLedger[r.Source]; ok {
				row.GenesisLedger = g
			}
			row.CoveragePct = computeCoveragePct(row.GenesisLedger, r.EarliestLedger, r.LatestLedger, tip)
		}
		out = append(out, row)
	}
	return out
}

// computeCoveragePct returns the fraction of the
// (genesis → tip) interval we have any data for. Returns 0 if
// genesis or tip aren't usable (cold start, missing config).
// Capped at 1.0; any LatestLedger ≥ tip → 1.0 (covered to head).
func computeCoveragePct(genesis, earliest, latest, tip int64) float64 {
	if tip <= 0 || genesis <= 0 {
		return 0
	}
	expectedSpan := tip - genesis + 1
	if expectedSpan <= 0 {
		return 0
	}
	covStart := earliest
	if covStart < genesis {
		covStart = genesis
	}
	covEnd := latest
	if covEnd > tip {
		covEnd = tip
	}
	if covEnd < covStart {
		return 0
	}
	covered := covEnd - covStart + 1
	pct := float64(covered) / float64(expectedSpan)
	if pct > 1 {
		pct = 1
	}
	return pct
}

// fillIngestionSupplyCoverage type-asserts that the wired
// supply reader also implements SupplyCoverageReader.
func (s *Server) fillIngestionSupplyCoverage(ctx context.Context, out *IngestionDiagnostics) {
	reader, ok := s.supply.(SupplyCoverageReader)
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

// aggregateBackfill collapses the per-range backfill cursor rows
// into one row per decoder set. The cursor sub_source format is
// "<start>-<end>:<decoder-set>" (e.g.
// "50534290-51275895:sdex,soroswap"); a single decoder-set string
// becomes one BackfillDecoderState row. RangesActive counts ranges
// whose last_ledger < range_end — i.e. still catching up.
func aggregateBackfill(rows []timescale.Cursor) []BackfillDecoderState {
	type group struct {
		total       int
		active      int
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
		if rangeEnd > 0 && int64(c.LastLedger) < rangeEnd {
			g.active++
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
			Decoder:      decoder,
			RangesTotal:  g.total,
			RangesActive: g.active,
			NewestLedger: g.newestLedge,
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

// parseBackfillSub splits a cursor sub_source string of the form
// "<start>-<end>:<decoder-set>" into its decoder-set tail and the
// numeric range end. Returns ("", 0) when the format doesn't match
// (defensive — a malformed cursor doesn't crash aggregation).
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

// ledgerStreamLagSeconds finds the live-stream cursor (source =
// "ledgerstream") and reports its age in seconds. Returns 0 when
// no live cursor exists (cold start / catastrophic stall).
func ledgerStreamLagSeconds(rows []timescale.Cursor) int64 {
	now := time.Now().UTC()
	for _, c := range rows {
		if c.Source == "ledgerstream" {
			return int64(now.Sub(c.UpdatedAt).Seconds())
		}
	}
	return 0
}

// projectMarketCapState reads cache.All() once and reduces it to
// the EntriesCount / Oldest / Newest summary. Iterates the map at
// most once; no allocs beyond the output. Nil cache → empty state.
func projectMarketCapState(c *marketcap.Cache) MarketCapState {
	if c == nil {
		return MarketCapState{}
	}
	all := c.All()
	if len(all) == 0 {
		return MarketCapState{}
	}
	var oldest, newest time.Time
	for _, snap := range all {
		if snap.FetchedAt.IsZero() {
			continue
		}
		if oldest.IsZero() || snap.FetchedAt.Before(oldest) {
			oldest = snap.FetchedAt
		}
		if newest.IsZero() || snap.FetchedAt.After(newest) {
			newest = snap.FetchedAt
		}
	}
	out := MarketCapState{EntriesCount: len(all)}
	if !oldest.IsZero() {
		out.OldestFetchedAt = oldest.UTC().Format(time.RFC3339)
	}
	if !newest.IsZero() {
		out.NewestFetchedAt = newest.UTC().Format(time.RFC3339)
	}
	return out
}

// buildSourceHealth projects the static external.Registry onto
// the wire shape, joining each row with the live 24h stats from
// sourcesStats. Sources with no recent trades render as 0/empty
// rather than absent — operators want to see "binance had 0
// trades in 24h", which is a signal not a hidden row.
func buildSourceHealth(ctx context.Context, s *Server) []SourceHealthRow {
	statsBySource := map[string]timescale.SourceStats{}
	if s.sourcesStats != nil {
		if rows, err := s.sourcesStats.GetSourceStats(ctx); err == nil {
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
			BackfillSafe:  meta.BackfillSafe,
		}
		if st, ok := statsBySource[name]; ok {
			row.TradeCount24h = st.TradeCount24h
			row.MarketsCount24h = st.MarketsCount24h
			if st.VolumeUSD24h.Valid {
				row.VolumeUSD24h = st.VolumeUSD24h.String
			}
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
