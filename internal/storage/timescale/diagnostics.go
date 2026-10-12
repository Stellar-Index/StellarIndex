package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// FXCoverage is how much FX history fx_quotes holds, for /v1/diagnostics/ingestion; zero-valued when the table is empty and rendered as "—", not an error.
type FXCoverage struct {
	// EarliestQuote / LatestQuote are MIN/MAX(bucket). Zero
	// values when fx_quotes is empty.
	EarliestQuote time.Time
	LatestQuote   time.Time
	// TotalQuotes is the total row count across all tickers +
	// dates. Useful for sanity-checking against an expected
	// "tickers × days" multiplier.
	TotalQuotes int64
	// CurrenciesCount is COUNT(DISTINCT ticker) — i.e. how many
	// distinct fiat currencies have at least one quote.
	CurrenciesCount int
}

// FXCoverageStats returns the current coverage state of the fx_quotes hypertable.
func (s *Store) FXCoverageStats(ctx context.Context) (FXCoverage, error) {
	const q = `
		SELECT
		    MIN(bucket),
		    MAX(bucket),
		    COUNT(*),
		    COUNT(DISTINCT ticker)
		FROM fx_quotes
	`
	var (
		minB, maxB sql.NullTime
		total      int64
		currencies int
	)
	if err := s.db.QueryRowContext(ctx, q).Scan(&minB, &maxB, &total, &currencies); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return FXCoverage{}, nil
		}
		return FXCoverage{}, err
	}
	out := FXCoverage{TotalQuotes: total, CurrenciesCount: currencies}
	if minB.Valid {
		out.EarliestQuote = minB.Time
	}
	if maxB.Valid {
		out.LatestQuote = maxB.Time
	}
	return out, nil
}

// SupplyCoverage counts assets with a supply snapshot and when the latest was written, so a stalled supply observer shows in LastSnapshotAt.
type SupplyCoverage struct {
	ClassicAssets  int
	SEP41Assets    int
	LastSnapshotAt time.Time
	LatestLedger   int64
}

// LedgerRangeToTimeRange returns MIN/MAX(ts) of trades in [fromLedger, toLedger] to turn a backfill chunk into a CAGG refresh range; ErrNotFound means nothing to refresh.
func (s *Store) LedgerRangeToTimeRange(ctx context.Context, fromLedger, toLedger uint32) (time.Time, time.Time, error) {
	const q = `SELECT MIN(ts), MAX(ts) FROM trades WHERE ledger BETWEEN $1 AND $2`
	var minTs, maxTs sql.NullTime
	if err := s.db.QueryRowContext(ctx, q, fromLedger, toLedger).Scan(&minTs, &maxTs); err != nil {
		return time.Time{}, time.Time{}, err
	}
	if !minTs.Valid || !maxTs.Valid {
		return time.Time{}, time.Time{}, ErrNotFound
	}
	return minTs.Time, maxTs.Time, nil
}

// LedgerRangeToOracleTimeRange is [Store.LedgerRangeToTimeRange] over `oracle_updates`, the root of [OracleCAGGs]; off-chain rows carry ledger 0 so they never fall in a backfill range.
func (s *Store) LedgerRangeToOracleTimeRange(ctx context.Context, fromLedger, toLedger uint32) (time.Time, time.Time, error) {
	const q = `SELECT MIN(ts), MAX(ts) FROM oracle_updates WHERE ledger BETWEEN $1 AND $2`
	var minTs, maxTs sql.NullTime
	if err := s.db.QueryRowContext(ctx, q, fromLedger, toLedger).Scan(&minTs, &maxTs); err != nil {
		return time.Time{}, time.Time{}, err
	}
	if !minTs.Valid || !maxTs.Valid {
		return time.Time{}, time.Time{}, ErrNotFound
	}
	return minTs.Time, maxTs.Time, nil
}

// TradesCAGGs is the ORDERED set of every continuous aggregate rooted on `trades`, the single list the refresh allow-list, [CAGGsLiveForever] and the usd_volume restamp derive from; twap_* build on prices_1m so it must come first.
var TradesCAGGs = []CAGGSpec{
	// Must lead: twap_1h / twap_1d are materialised FROM this one.
	{Name: "prices_1m", MinWindow: 2 * time.Minute, Bucket: time.Minute},
	{Name: "prices_15m", MinWindow: 30 * time.Minute, Bucket: 15 * time.Minute},
	{Name: "prices_1h", MinWindow: 3 * time.Hour, Bucket: time.Hour},
	{Name: "prices_4h", MinWindow: 12 * time.Hour, Bucket: 4 * time.Hour},
	{Name: "prices_1d", MinWindow: 3 * 24 * time.Hour, Bucket: 24 * time.Hour},
	{Name: "prices_1w", MinWindow: 3 * 7 * 24 * time.Hour, Bucket: 7 * 24 * time.Hour},
	// 1mo buckets are calendar months, not 30 days; padding to ~93 days clears the two-bucket
	// minimum without month arithmetic at the storage seam.
	{Name: "prices_1mo", MinWindow: 93 * 24 * time.Hour, Bucket: MonthBucket},
	{Name: "dex_volume_by_pair_1d", MinWindow: 3 * 24 * time.Hour, Bucket: 24 * time.Hour},
	{Name: "source_volume_1h", MinWindow: 3 * time.Hour, Bucket: time.Hour},
	{Name: "pools_per_source_1h", MinWindow: 3 * time.Hour, Bucket: time.Hour},
	// Must trail prices_1m.
	{Name: "twap_1h", MinWindow: 3 * time.Hour, Bucket: time.Hour},
	{Name: "twap_1d", MinWindow: 3 * 24 * time.Hour, Bucket: 24 * time.Hour},
}

// OracleCAGGs is every continuous aggregate rooted on `oracle_updates` (order-free); their policies reach back only 5 minutes to 3 months, so a backfilled range needs an explicit refresh.
var OracleCAGGs = []CAGGSpec{
	{Name: "oracle_prices_1m", MinWindow: 2 * time.Minute, Bucket: time.Minute},
	{Name: "oracle_prices_15m", MinWindow: 30 * time.Minute, Bucket: 15 * time.Minute},
	{Name: "oracle_prices_1h", MinWindow: 3 * time.Hour, Bucket: time.Hour},
	{Name: "oracle_prices_4h", MinWindow: 12 * time.Hour, Bucket: 4 * time.Hour},
	{Name: "oracle_prices_1d", MinWindow: 3 * 24 * time.Hour, Bucket: 24 * time.Hour},
	{Name: "oracle_prices_1w", MinWindow: 3 * 7 * 24 * time.Hour, Bucket: 7 * 24 * time.Hour},
	{Name: "oracle_prices_1mo", MinWindow: 93 * 24 * time.Hour, Bucket: MonthBucket},
}

// CAGGsOnPrices1m are the [TradesCAGGs] views materialised FROM prices_1m, not `trades`.
var CAGGsOnPrices1m = []string{"twap_1h", "twap_1d"}

// SupplyCAGG is the continuous aggregate over asset_supply_history; its policy reaches back only 7 days, so older re-derived snapshots need an explicit refresh.
var SupplyCAGG = CAGGSpec{Name: "supply_1d", MinWindow: 3 * 24 * time.Hour}

// allowedCAGGViews is the allow-list of view names RefreshContinuousAggregate accepts. The name is
// string-formatted into the SQL (the procedure's first arg is REGCLASS, which pgx cannot
// placeholder), so the list is what keeps injection out.
var allowedCAGGViews = func() map[string]bool {
	m := make(map[string]bool, len(TradesCAGGs)+len(OracleCAGGs)+1)
	for _, c := range TradesCAGGs {
		m[c.Name] = true
	}
	for _, c := range OracleCAGGs {
		m[c.Name] = true
	}
	m[SupplyCAGG.Name] = true
	return m
}()

// IsRefreshableCAGG reports whether RefreshContinuousAggregate accepts viewName.
func IsRefreshableCAGG(viewName string) bool { return allowedCAGGViews[viewName] }

// CAGGSpec names a continuous aggregate and its minimum refresh window.
type CAGGSpec struct {
	Name      string
	MinWindow time.Duration
	// Bucket is the view's time_bucket width, or [MonthBucket]. Zero
	// keeps a refresh of the view in one CALL ([RefreshPieces]).
	Bucket time.Duration
}

// MonthBucket is the [CAGGSpec.Bucket] of a view bucketed by time_bucket('1 month', ts, 'UTC'), which has no fixed width.
const MonthBucket time.Duration = -1

// CAGGsLiveForever is the ordered set of served prices_* rungs the backfill refreshes after each chunk; an omitted rung is a permanent hole in every backfilled range.
var CAGGsLiveForever = func() []CAGGSpec {
	out := make([]CAGGSpec, 0, len(TradesCAGGs))
	for _, c := range TradesCAGGs {
		if strings.HasPrefix(c.Name, "prices_") {
			out = append(out, c)
		}
	}
	return out
}()

// PadRefreshWindow expands [from, to] to span at least minWindow, centered on the original midpoint, to satisfy the two-bucket minimum.
func PadRefreshWindow(from, to time.Time, minWindow time.Duration) (time.Time, time.Time) {
	span := to.Sub(from)
	if span >= minWindow {
		return from, to
	}
	pad := (minWindow - span) / 2
	return from.Add(-pad), to.Add(pad)
}

// RefreshContinuousAggregate materialises a continuous aggregate over [from, to] and blocks until done; required after backfill because the policy refresher only rolls FORWARD, idempotent, and an unknown view fails loudly.
func (s *Store) RefreshContinuousAggregate(ctx context.Context, viewName string, from, to time.Time) error {
	return s.RefreshContinuousAggregateWithTimeout(ctx, viewName, from, to, CAGGRefreshTimeout(to.Sub(from)))
}

// RefreshContinuousAggregateWithTimeout is [Store.RefreshContinuousAggregate] under a per-CALL statement_timeout; timeout must be positive, with deliberately no "0 disables" arm.
func (s *Store) RefreshContinuousAggregateWithTimeout(ctx context.Context, viewName string, from, to time.Time, timeout time.Duration) error {
	return s.refreshCAGG(ctx, viewName, from, to, timeout, false)
}

// RefreshContinuousAggregateForced is [Store.RefreshContinuousAggregate] with `force => true`, recomputing the whole window; a retention drop logs no invalidation, so only the forced form restores dropped ranges.
func (s *Store) RefreshContinuousAggregateForced(ctx context.Context, viewName string, from, to time.Time) error {
	return s.refreshCAGG(ctx, viewName, from, to, CAGGRefreshTimeout(to.Sub(from)), true)
}

// Prices1mEarliestBucket returns prices_1m's earliest materialised bucket (the view is materialized_only), [ErrNotFound] when empty; below it minute rows were dropped or never materialised.
func (s *Store) Prices1mEarliestBucket(ctx context.Context) (time.Time, error) {
	var b time.Time
	err := s.db.QueryRowContext(ctx, `SELECT bucket FROM prices_1m ORDER BY bucket LIMIT 1`).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, ErrNotFound
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("timescale: Prices1mEarliestBucket: %w", err)
	}
	return b, nil
}

// Prices1mRetentionArmed reports whether the retention policy on prices_1m is scheduled, matched on the view name; no policy reads as not armed.
func (s *Store) Prices1mRetentionArmed(ctx context.Context) (bool, error) {
	var armed bool
	err := s.db.QueryRowContext(ctx, `
		SELECT coalesce(bool_or(scheduled), false)
		  FROM timescaledb_information.jobs
		 WHERE proc_name = 'policy_retention'
		   AND hypertable_name = 'prices_1m'`).Scan(&armed)
	if err != nil {
		return false, fmt.Errorf("timescale: Prices1mRetentionArmed: %w", err)
	}
	return armed, nil
}

func (s *Store) refreshCAGG(ctx context.Context, viewName string, from, to time.Time, timeout time.Duration, force bool) error {
	if !allowedCAGGViews[viewName] {
		return fmt.Errorf("timescale: RefreshContinuousAggregate: unknown view %q", viewName)
	}
	if timeout <= 0 {
		return fmt.Errorf("timescale: RefreshContinuousAggregate(%s): non-positive timeout %s", viewName, timeout)
	}
	// The REGCLASS first arg cannot be a pgx placeholder, so the allow-listed view name is
	// concatenated. The time params need explicit ::timestamptz casts: the CALL path does not
	// propagate the procedure's declared types, so an untyped placeholder fails with 42P18.
	forceArg := ""
	if force {
		forceArg = ", force => true"
	}
	q := fmt.Sprintf(`CALL refresh_continuous_aggregate('%s', $1::timestamptz, $2::timestamptz%s)`, viewName, forceArg)
	// Retry on 55P03: Timescale rejects the loser of two concurrent refreshes of one CAGG
	// immediately instead of blocking it.
	//
	// The ~3.0s budget covers only SHORT collisions (the policy refresher's tick, an operator's
	// slice), not a long refresh: prices_1m over a weekly chunk runs for minutes. In-process
	// fan-out must hold ingest.caggRefreshMu instead, or it exhausts these attempts with a hard
	// error.
	//
	// The timeout is per ATTEMPT: a 55P03 loser never started materialising, so the winning
	// retry gets the full budget.
	const maxAttempts = 5
	for attempt := 0; attempt < maxAttempts; attempt++ {
		err := s.refreshCAGGBounded(ctx, q, from, to, timeout)
		if err == nil {
			return nil
		}
		if isStatementTimeoutErr(err) || errors.Is(err, context.DeadlineExceeded) {
			return &CAGGRefreshTimeoutError{View: viewName, From: from, To: to, Timeout: timeout, Err: err}
		}
		if !isConcurrentRefreshErr(err) || attempt == maxAttempts-1 {
			return fmt.Errorf("timescale: RefreshContinuousAggregate(%s): %w", viewName, err)
		}
		// Exponential backoff with jitter: 200ms, 400ms, 800ms, 1.6s.
		// Sleep capped via select so ctx-cancel exits promptly.
		backoff := time.Duration(200*(1<<attempt)) * time.Millisecond
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
	}
	return nil // unreachable
}

// isConcurrentRefreshErr reports whether err is SQLSTATE 55P03 (concurrent refresh of one CAGG).
// It matches the message substring to avoid a hard pq dependency.
func isConcurrentRefreshErr(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "55P03") ||
		strings.Contains(err.Error(), "concurrent refresh"))
}

// CAGGCoverage is the time range and row count of prices_1h, the coarsest rung every deployment has materialised; nothing there is pruned, whereas prices_1m may be where retention is armed.
type CAGGCoverage struct {
	EarliestBucket time.Time
	LatestBucket   time.Time
	BucketCount    int64
}

// CAGGCoverageStats returns the earliest and latest prices_1h buckets, empty before the CAGG is first materialised.
func (s *Store) CAGGCoverageStats(ctx context.Context) (CAGGCoverage, error) {
	// MIN/MAX use the bucket index. An exact COUNT(*) over prices_1h is a full scan of ~175M rows,
	// and /v1/diagnostics/ingestion polls this every ~15s. BucketCount is only a coverage stat, so
	// the chunk-metadata approximate_row_count is precise enough and avoids the scan.
	const minMaxQ = `SELECT MIN(bucket), MAX(bucket) FROM prices_1h`
	const countQ = `SELECT approximate_row_count('prices_1h')`
	var (
		minB, maxB sql.NullTime
		count      int64
	)
	if err := s.db.QueryRowContext(ctx, minMaxQ).Scan(&minB, &maxB); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return CAGGCoverage{}, nil
		}
		return CAGGCoverage{}, err
	}
	// A failed/unsupported approximate_row_count is non-fatal: the
	// earliest/latest buckets still answer the coverage question.
	if err := s.db.QueryRowContext(ctx, countQ).Scan(&count); err != nil {
		count = 0
	}
	out := CAGGCoverage{BucketCount: count}
	if minB.Valid {
		out.EarliestBucket = minB.Time
	}
	if maxB.Valid {
		out.LatestBucket = maxB.Time
	}
	return out, nil
}

// BackfillCoverage is one per-source row of earliest and latest trade ledgers plus trade count; gaps inside the range are not detected, and CEX/FX sources report ledgers (0, 0) so only TradeCount is meaningful.
type BackfillCoverage struct {
	Source         string
	EarliestLedger int64
	LatestLedger   int64
	TradeCount     int64
}

// BackfillCoverageStats is intentionally a no-op that exists only to satisfy the CoverageCache reader interface.
func (s *Store) BackfillCoverageStats(_ context.Context) ([]BackfillCoverage, error) {
	return nil, nil
}

// supplyCoverageStatsQuery backs [Store.SupplyCoverageStats]. The `newest` CTE orders by `time`
// alone: close time is monotonic in ledger sequence, so a ledger_sequence tie-break could never
// fire and would only add a sort key that defeats the single ordered-append seek.
const supplyCoverageStatsQuery = `
	WITH RECURSIVE assets AS (
	    (SELECT asset_key FROM asset_supply_history ORDER BY asset_key LIMIT 1)
	    UNION ALL
	    SELECT (SELECT h.asset_key
	              FROM asset_supply_history h
	             WHERE h.asset_key > a.asset_key
	             ORDER BY h.asset_key
	             LIMIT 1)
	      FROM assets a
	     WHERE a.asset_key IS NOT NULL
	), newest AS (
	    SELECT time, ledger_sequence
	      FROM asset_supply_history
	     ORDER BY time DESC
	     LIMIT 1
	)
	SELECT
	    COUNT(*) FILTER (WHERE asset_key LIKE 'C%' AND LENGTH(asset_key) = 56) AS sep41,
	    COUNT(*) FILTER (WHERE NOT (asset_key LIKE 'C%' AND LENGTH(asset_key) = 56)) AS classic,
	    (SELECT time FROM newest)            AS last_at,
	    (SELECT ledger_sequence FROM newest) AS last_ledger
	FROM assets
	WHERE asset_key IS NOT NULL
`

// SupplyCoverageStats counts assets ever given a supply (SEP-41 vs classic) and the newest snapshot's time and ledger; deliberately no time floor, since an asset with an old snapshot still has a supply.
func (s *Store) SupplyCoverageStats(ctx context.Context) (SupplyCoverage, error) {
	var (
		sep41, classic int
		lastAt         sql.NullTime
		lastLedger     sql.NullInt64
	)
	if err := s.db.QueryRowContext(ctx, supplyCoverageStatsQuery).Scan(&sep41, &classic, &lastAt, &lastLedger); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SupplyCoverage{}, nil
		}
		return SupplyCoverage{}, err
	}
	out := SupplyCoverage{
		ClassicAssets: classic,
		SEP41Assets:   sep41,
	}
	if lastAt.Valid {
		out.LastSnapshotAt = lastAt.Time
	}
	if lastLedger.Valid {
		out.LatestLedger = lastLedger.Int64
	}
	return out, nil
}

// SourceEntryCounts returns the per-source running tally from `source_entry_counts`, keyed by source; it never touches the trades/oracle_updates hypertables so it stays fast during an all-time backfill.
func (s *Store) SourceEntryCounts(ctx context.Context) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT source, entry_count FROM source_entry_counts`)
	if err != nil {
		return nil, fmt.Errorf("timescale: SourceEntryCounts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]int64, 32)
	for rows.Next() {
		var src string
		var n int64
		if err := rows.Scan(&src, &n); err != nil {
			return nil, fmt.Errorf("timescale: SourceEntryCounts scan: %w", err)
		}
		out[src] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: SourceEntryCounts rows: %w", err)
	}
	return out, nil
}

// seedSourceEntryCountsSQL recomputes source_entry_counts from a full GROUP BY over every
// decoded-event hypertable and overwrites the tally (SET, not ADD, so re-running converges). The
// incremental bump cannot know pre-counter history, hence the operator one-shot
// `stellarindex-ops seed-entry-counts` after a backfill, which scans every chunk in one
// transaction.
//
// Its tables stay in lockstep with DefaultGapDetectorTargets via
// [TestSeedSourceEntryCountsFoldsEveryPerSourceHypertable]; a watched table left out would have
// its tally overwritten by a re-seed.
//
// pipeline/sink.go's bumpEntryCount adds +1 unconditionally, so a replay double-counts; each
// folded table holds one idempotent row per bumped event, so a re-seed corrects the drift. A
// source spread over several tables has disjoint event sets, so the outer GROUP BY sum is the
// honest total. SeedSourceEntryCounts returns the number of source rows reconciled.
const seedSourceEntryCountsSQL = `
        INSERT INTO source_entry_counts AS sec (source, entry_count, updated_at)
        SELECT source, sum(c)::bigint, now()
        FROM (
            SELECT source, count(*) AS c FROM trades         GROUP BY source
            UNION ALL
            SELECT source, count(*) AS c FROM oracle_updates GROUP BY source -- totality: includes unmapped
            UNION ALL
            -- fx_quotes.source is nullable; coalesce to 'unknown-fx'
            -- so rows that landed without a source label still get
            -- accounted for and surface the gap rather than vanishing.
            SELECT COALESCE(source, 'unknown-fx') AS source, count(*) AS c
              FROM fx_quotes GROUP BY 1
            UNION ALL
            -- Single-source observer tables — the table is
            -- single-source by construction. Literals here match the
            -- registry SourceName for each observer.
            SELECT 'blend'              AS source, count(*) AS c FROM blend_auctions
            UNION ALL
            SELECT 'accounts'           AS source, count(*) AS c FROM account_observations
            UNION ALL
            SELECT 'trustlines'         AS source, count(*) AS c FROM trustline_observations
            UNION ALL
            SELECT 'claimable_balances' AS source, count(*) AS c FROM claimable_observations
            UNION ALL
            SELECT 'liquidity_pools'    AS source, count(*) AS c FROM lp_reserve_observations
            UNION ALL
            SELECT 'sac_balances'       AS source, count(*) AS c FROM sac_balance_observations
            UNION ALL
            SELECT 'sep41_supply'       AS source, count(*) AS c FROM sep41_supply_events
            UNION ALL
            -- Log-only sinks (bumped 1/event, non-idempotently) that now
            -- ALSO persist one idempotent row/event to a countable table,
            -- so the COUNT is replay-stable and safe to SET-reset from.
            SELECT 'soroswap-router'    AS source, count(*) AS c FROM soroswap_router_swaps
            UNION ALL
            SELECT 'defindex'           AS source, count(*) AS c FROM defindex_flows
            UNION ALL
            -- defindex ALSO lands dfees fee-distribution entries in their own
            -- table (migration 0146, W5.2) — one row per bumped DFeesEvent, a
            -- DISJOINT event set from defindex_flows (one decoded event -> one
            -- handler -> one table), so folding it keeps the seed's 'defindex'
            -- total equal to the full bump total (the outer GROUP BY sums both).
            SELECT 'defindex'           AS source, count(*) AS c FROM defindex_fees
            UNION ALL
            -- One row per bumped defindex AdminEvent (migration 0192), disjoint
            -- from the flow and fee tables like defindex_fees above.
            SELECT 'defindex'           AS source, count(*) AS c FROM defindex_admin_events
            UNION ALL
            -- Per-source non-'trades' sinks whose 'entries' tally is bumped
            -- 1/event via pipeline/sink.go::bumpEntryCount — a NON-idempotent
            -- +1 (unlike the trades/oracle_updates bump, which is inlined in
            -- the idempotent INSERT and so is replay-safe). Each row below is
            -- an idempotent (ON CONFLICT DO NOTHING) hypertable holding exactly
            -- one row per bumped event, so its COUNT is replay-stable and equals
            -- the bump total — folding it gives the source the same self-healing
            -- SET-reset the trades-backed sources already have.
            --
            -- comet / soroswap / phoenix ALSO write to 'trades' (their swap
            -- events), which the trades GROUP BY source above already counts.
            -- The tables below hold their NON-swap events (liquidity / skim /
            -- stake) — a DISJOINT event set (one decoded event -> exactly one
            -- handler -> one table), so the outer GROUP-BY sum is the honest
            -- total-activity count, NOT a double-count of any trade row.
            SELECT 'comet'              AS source, count(*) AS c FROM comet_liquidity
            UNION ALL
            SELECT 'soroswap'           AS source, count(*) AS c FROM soroswap_skim_events
            UNION ALL
            SELECT 'phoenix'            AS source, count(*) AS c FROM phoenix_liquidity
            UNION ALL
            SELECT 'phoenix'            AS source, count(*) AS c FROM phoenix_stake_events
            UNION ALL
            -- Blend already contributes blend_auctions above; its position /
            -- emission / admin event streams bump the SAME 'blend' source but
            -- land in separate tables. Fold each so the seed's 'blend' total =
            -- the full bump total (the outer GROUP BY sums all four).
            SELECT 'blend'              AS source, count(*) AS c FROM blend_positions
            UNION ALL
            SELECT 'blend'              AS source, count(*) AS c FROM blend_emissions
            UNION ALL
            SELECT 'blend'              AS source, count(*) AS c FROM blend_admin
            UNION ALL
            -- Pure log-only sinks (no 'trades' side at all): the whole
            -- 'entries' tally is the bumpEntryCount total, so the table COUNT
            -- is authoritative on its own.
            SELECT 'blend_backstop'     AS source, count(*) AS c FROM blend_backstop_events
            UNION ALL
            SELECT 'cctp'               AS source, count(*) AS c FROM cctp_events
            UNION ALL
            SELECT 'rozo'               AS source, count(*) AS c FROM rozo_events
            UNION ALL
            SELECT 'sep41_transfers'    AS source, count(*) AS c FROM sep41_transfers
            UNION ALL
            -- Aquarius swaps are 'trades' rows (counted above); every
            -- other Aquarius event lands in exactly one of these seven
            -- tables. soroswap / phoenix likewise gain their remaining
            -- non-swap streams (liquidity; initialize + admin).
            SELECT 'aquarius'           AS source, count(*) AS c FROM aquarius_reserves
            UNION ALL
            SELECT 'aquarius'           AS source, count(*) AS c FROM aquarius_reserves_sync
            UNION ALL
            SELECT 'aquarius'           AS source, count(*) AS c FROM aquarius_protocol_fee
            UNION ALL
            SELECT 'aquarius'           AS source, count(*) AS c FROM aquarius_kill_switches
            UNION ALL
            SELECT 'aquarius'           AS source, count(*) AS c FROM aquarius_liquidity
            UNION ALL
            SELECT 'aquarius'           AS source, count(*) AS c FROM aquarius_rewards_events
            UNION ALL
            SELECT 'aquarius'           AS source, count(*) AS c FROM aquarius_admin
            UNION ALL
            SELECT 'soroswap'           AS source, count(*) AS c FROM soroswap_liquidity
            UNION ALL
            SELECT 'sushiswap_v3'       AS source, count(*) AS c FROM sushiswap_v3_position_events
            UNION ALL
            SELECT 'phoenix'            AS source, count(*) AS c FROM phoenix_initialize
            UNION ALL
            SELECT 'phoenix'            AS source, count(*) AS c FROM phoenix_admin_events
            UNION ALL
            -- Pure log-only sinks with one table per decoded event
            -- (sorocredit routes each event type to exactly one of its
            -- four tables, so the four are a DISJOINT partition).
            SELECT 'blend_emitter'      AS source, count(*) AS c FROM blend_emitter_events
            UNION ALL
            SELECT 'sorocredit'         AS source, count(*) AS c FROM credit_positions
            UNION ALL
            SELECT 'sorocredit'         AS source, count(*) AS c FROM credit_statements
            UNION ALL
            SELECT 'sorocredit'         AS source, count(*) AS c FROM credit_settlements
            UNION ALL
            SELECT 'sorocredit'         AS source, count(*) AS c FROM credit_events
            UNION ALL
            SELECT 'upshift'            AS source, count(*) AS c FROM upshift_vault_events
            UNION ALL
            SELECT 'spectra'            AS source, count(*) AS c FROM spectra_events
        ) u
        GROUP BY source
        ON CONFLICT (source) DO UPDATE
          SET entry_count = EXCLUDED.entry_count,
              updated_at  = EXCLUDED.updated_at
    `

func (s *Store) SeedSourceEntryCounts(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, seedSourceEntryCountsSQL)
	if err != nil {
		return 0, fmt.Errorf("timescale: SeedSourceEntryCounts: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// BumpSourceEntryCount adds n to one source's running tally (the first writer creates the row).
func (s *Store) BumpSourceEntryCount(ctx context.Context, source string, n int64) error {
	if n <= 0 {
		return nil
	}
	const q = `
        INSERT INTO source_entry_counts (source, entry_count, updated_at)
        VALUES ($1, $2, now())
        ON CONFLICT (source) DO UPDATE
          SET entry_count = source_entry_counts.entry_count + EXCLUDED.entry_count,
              updated_at  = EXCLUDED.updated_at
    `
	if _, err := s.db.ExecContext(ctx, q, source, n); err != nil {
		return fmt.Errorf("timescale: BumpSourceEntryCount(%s, %d): %w", source, n, err)
	}
	return nil
}
