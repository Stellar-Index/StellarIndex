package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// FXCoverage is the storage-layer projection of how much FX-history
// the fx_quotes hypertable currently holds. Powers the
// /v1/diagnostics/ingestion surface so operators can see at a glance
// whether the Frankfurter / Massive backfill ran to completion +
// whether the daily live-write tick is keeping up.
//
// Empty / zero-valued when fx_quotes has no rows yet — handlers
// project that as "—" rather than an error.
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

// FXCoverageStats returns the current coverage state of the
// fx_quotes hypertable. A single GROUPING() aggregate over the
// hypertable; cheap (~1ms on a populated table because the
// hypertable's index sits on bucket).
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

// SupplyCoverage is the storage-layer projection of how many assets
// have a supply snapshot today + how recently the most recent
// snapshot was written. Powers the ingestion-diagnostics surface so
// operators can spot a stalled supply observer
// (LastSnapshotAt > a few minutes ago) without paging through
// asset_supply_history by hand.
//
// "Classic" vs "SEP-41" splits asset_key by prefix — SEP-41 contract
// IDs start with "C", classic assets are "native" or
// "CODE:G-strkey". The split mirrors the three-domain supply
// algorithm split (XLM / classic / SEP-41).
type SupplyCoverage struct {
	ClassicAssets  int
	SEP41Assets    int
	LastSnapshotAt time.Time
	LatestLedger   int64
}

// LedgerRangeToTimeRange returns the MIN/MAX(ts) of trades whose
// ledger falls in [fromLedger, toLedger]. Used by the backfill
// tool to translate a ledger-range chunk into the timestamp range
// needed for CAGG materialisation. Returns ErrNotFound when no
// trades exist in the range — caller treats that as "nothing to
// refresh" rather than an error.
//
// O(log N) via the trades_source_ledger_idx index plus a per-chunk
// scan; sub-second on a populated hypertable.
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

// LedgerRangeToOracleTimeRange is [Store.LedgerRangeToTimeRange] over
// `oracle_updates`, the root of [OracleCAGGs]. ts is the publication
// time those views bucket on. Off-chain rows carry ledger 0, so they
// never fall in a backfill range.
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

// TradesCAGGs is the ORDERED set of every continuous aggregate rooted
// on `trades`, with each one's minimum refresh window. It is the ONE
// list the refresh allow-list, the backfill's price set
// ([CAGGsLiveForever]) and the usd_volume restamp follow-up derive
// from; two hand-copied lists had drifted to seven and twelve entries.
//
// Membership and order come from `_timescaledb_catalog.continuous_agg`:
// exactly these twelve have `trades` as their root hypertable (oracle_prices_*
// hang off oracle_updates, supply_1d off asset_supply_history). Ten read
// `trades` directly and are mutually independent; twap_1h / twap_1d are
// HIERARCHICAL — built on prices_1m's materialisation — so prices_1m leads and
// the twaps trail, or they re-materialise from stale input. The other prices_*
// rungs are NOT built on prices_1m (each reads `trades` itself) and do not
// inherit its refresh. TestTradesCAGGsMatchCatalog holds the list against the
// migrated schema.
//
// Per-entry MinWindow is the Timescale-imposed minimum refresh
// window: refresh_continuous_aggregate rejects (`SQLSTATE 22023:
// refresh window too small`) any window narrower than 2× bucket
// width. Callers pad the actual ts range up to MinWindow before
// invoking refresh ([PadRefreshWindow]); the padded area beyond the
// range has no new trades, so the no-op buckets are nearly free.
var TradesCAGGs = []CAGGSpec{
	// Must lead: twap_1h / twap_1d are materialised FROM this one.
	{Name: "prices_1m", MinWindow: 2 * time.Minute, Bucket: time.Minute},
	{Name: "prices_15m", MinWindow: 30 * time.Minute, Bucket: 15 * time.Minute},
	{Name: "prices_1h", MinWindow: 3 * time.Hour, Bucket: time.Hour},
	{Name: "prices_4h", MinWindow: 12 * time.Hour, Bucket: 4 * time.Hour},
	{Name: "prices_1d", MinWindow: 3 * 24 * time.Hour, Bucket: 24 * time.Hour},
	{Name: "prices_1w", MinWindow: 3 * 7 * 24 * time.Hour, Bucket: 7 * 24 * time.Hour},
	// 1mo CAGG uses calendar months, not 30-day windows. Padding
	// to ~93 days (3 calendar months) trivially clears the
	// "must span >= 2 buckets" minimum without depending on month
	// arithmetic at the storage seam.
	{Name: "prices_1mo", MinWindow: 93 * 24 * time.Hour, Bucket: MonthBucket},
	{Name: "dex_volume_by_pair_1d", MinWindow: 3 * 24 * time.Hour, Bucket: 24 * time.Hour},
	{Name: "source_volume_1h", MinWindow: 3 * time.Hour, Bucket: time.Hour},
	{Name: "pools_per_source_1h", MinWindow: 3 * time.Hour, Bucket: time.Hour},
	// Must trail prices_1m.
	{Name: "twap_1h", MinWindow: 3 * time.Hour, Bucket: time.Hour},
	{Name: "twap_1d", MinWindow: 3 * 24 * time.Hour, Bucket: 24 * time.Hour},
}

// OracleCAGGs is every continuous aggregate rooted on `oracle_updates`
// (migration 0034), each MinWindow matching its prices_* rung. All seven
// read `oracle_updates` directly, so order is free. They were created
// WITH NO DATA and their policies reach back 5 minutes to 3 months, so a
// backfilled range reaches them only through an explicit refresh.
// TestTradesCAGGsMatchCatalog holds the list to the schema.
var OracleCAGGs = []CAGGSpec{
	{Name: "oracle_prices_1m", MinWindow: 2 * time.Minute, Bucket: time.Minute},
	{Name: "oracle_prices_15m", MinWindow: 30 * time.Minute, Bucket: 15 * time.Minute},
	{Name: "oracle_prices_1h", MinWindow: 3 * time.Hour, Bucket: time.Hour},
	{Name: "oracle_prices_4h", MinWindow: 12 * time.Hour, Bucket: 4 * time.Hour},
	{Name: "oracle_prices_1d", MinWindow: 3 * 24 * time.Hour, Bucket: 24 * time.Hour},
	{Name: "oracle_prices_1w", MinWindow: 3 * 7 * 24 * time.Hour, Bucket: 7 * 24 * time.Hour},
	{Name: "oracle_prices_1mo", MinWindow: 93 * 24 * time.Hour, Bucket: MonthBucket},
}

// CAGGsOnPrices1m are the [TradesCAGGs] views materialised FROM
// prices_1m rather than from `trades`. Migration 0156's retention drops
// prices_1m chunks without an invalidation, so refreshing one of these
// over minute rows that were dropped and not force-rebuilt deletes its
// history. TestTradesCAGGsMatchCatalog holds the list to the schema.
var CAGGsOnPrices1m = []string{"twap_1h", "twap_1d"}

// SupplyCAGG is the continuous aggregate over asset_supply_history
// (migration 0066). Its policy reaches back only 7 days, so a snapshot
// re-derived further back reaches it only through an explicit refresh.
var SupplyCAGG = CAGGSpec{Name: "supply_1d", MinWindow: 3 * 24 * time.Hour}

// allowedCAGGViews is the strict allow-list of view names accepted by
// RefreshContinuousAggregate, derived from [TradesCAGGs],
// [OracleCAGGs] and [SupplyCAGG]. Required
// because we string-format the view name into the SQL — the
// procedure's first arg is REGCLASS and pgx doesn't placeholder it.
// Allow-list keeps SQL injection off the table even though callers
// are internal.
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

// CAGGsLiveForever is the ORDERED set of served price rungs, one per
// [HistoryGranularity]: the prices_* subset of [TradesCAGGs] that the
// backfill tool refreshes after each chunk. Every SERVED rung has to be here:
// the policy refresher only rolls forward, so a rung left out is a permanent
// hole in every backfilled range. The minute grains are served
// over caller-chosen windows (/v1/ohlc 1m-30m, /v1/chart 1m|15m up to
// `all`, /v1/history/since-inception 1m|15m, which returns any gap first).
//
// migration 0002 gave prices_1m and prices_15m a 30-day retention and
// migration 0031 removed it; none carries retention now except prices_1m,
// whose 90-day policy (migration 0156) ships disabled. Where armed, refreshing an older chunk is wasted
// work, not a fault; the repair for an old range is the migration header's
// forced refresh with the policy disarmed.
//
// twap_1h and twap_1d are built FROM prices_1m, so they are not rungs here,
// but [PlanCAGGRefresh] refreshes them after forcing prices_1m over their
// window. prices_1m leads because it is the one view another aggregate is
// defined over, in [TradesCAGGs]' order.
//
// The two fine rungs pad the scanned range by only 2 and 30 minutes against
// the coarse set's ~117 days; their cost is rows written, bounded by the
// chunk's trade count, and those rows are exactly what the surfaces read.
//
// CAGGSpec names a continuous aggregate and its minimum refresh window.
type CAGGSpec struct {
	Name      string
	MinWindow time.Duration
	// Bucket is the view's time_bucket width, or [MonthBucket]. Zero
	// keeps a refresh of the view in one CALL ([RefreshPieces]).
	Bucket time.Duration
}

// MonthBucket is the [CAGGSpec.Bucket] of a view bucketed by
// time_bucket('1 month', ts, 'UTC'), which has no fixed width.
const MonthBucket time.Duration = -1

var CAGGsLiveForever = func() []CAGGSpec {
	out := make([]CAGGSpec, 0, len(TradesCAGGs))
	for _, c := range TradesCAGGs {
		if strings.HasPrefix(c.Name, "prices_") {
			out = append(out, c)
		}
	}
	return out
}()

// PadRefreshWindow expands [from, to] to span at least minWindow
// while staying centered on the original midpoint. Used by the
// backfill tool's per-chunk CAGG-refresh helper to satisfy the
// 2-buckets-minimum invariant. Padded area beyond the chunk's
// actual data is materialized as empty buckets (cheap).
func PadRefreshWindow(from, to time.Time, minWindow time.Duration) (time.Time, time.Time) {
	span := to.Sub(from)
	if span >= minWindow {
		return from, to
	}
	pad := (minWindow - span) / 2
	return from.Add(-pad), to.Add(pad)
}

// RefreshContinuousAggregate force-materialises a continuous
// aggregate over the given time window. Calls Timescale's
// `refresh_continuous_aggregate(view, from, to)` procedure, which
// blocks until the materialisation completes.
//
// Required after backfill runs because the policy refresher only
// rolls FORWARD: a policy materialises buckets inside its own
// look-back window, and the widest of the seven price views
// (prices_1mo) looks back three months, so a historical insert older
// than that is never materialised on its own cadence however long you
// wait. The backfill tool calls this at the end of each chunk to make
// CAGG materialisation atomic with the trade insert.
//
// The roll-forward policy is the WHOLE reason: raw trades are never
// pruned (migration 0031). The rows stay; only the
// materialisation is missing. That is why repairing an
// already-backfilled range needs no re-decode and no archive read,
// just a bounded refresh (docs/operations/backfill-procedure.md).
//
// Idempotent: refreshing an already-materialised range is a no-op.
// Fail-loud on unknown view name (defends against typo-driven
// SQL injection through the view-name string format).
//
// Every CALL is bounded (W8-19): a statement_timeout derived from the
// window's length by [CAGGRefreshTimeout] is applied on the pinned
// connection that runs it, so a wedged refresh fails THIS call with a
// [*CAGGRefreshTimeoutError] instead of holding the ops backfill pool
// — and its `-parallel` siblings behind ingest.caggRefreshMu — until
// SIGINT. See cagg_refresh_timeout.go for the sizing and the
// connection hygiene. A caller with a reason to bound differently
// uses [Store.RefreshContinuousAggregateWithTimeout].
func (s *Store) RefreshContinuousAggregate(ctx context.Context, viewName string, from, to time.Time) error {
	return s.RefreshContinuousAggregateWithTimeout(ctx, viewName, from, to, CAGGRefreshTimeout(to.Sub(from)))
}

// RefreshContinuousAggregateWithTimeout is [Store.RefreshContinuousAggregate]
// under an explicit per-CALL statement_timeout. timeout must be positive:
// an unbounded refresh is the defect this bound exists to remove, so
// there is deliberately no "0 disables it" arm.
func (s *Store) RefreshContinuousAggregateWithTimeout(ctx context.Context, viewName string, from, to time.Time, timeout time.Duration) error {
	return s.refreshCAGG(ctx, viewName, from, to, timeout, false)
}

// RefreshContinuousAggregateForced is [Store.RefreshContinuousAggregate]
// with `force => true`: the whole window is recomputed from the source,
// not only the ranges the invalidation log names. A retention drop logs
// no invalidation (migration 0156), so over a dropped range only the
// forced form restores rows.
func (s *Store) RefreshContinuousAggregateForced(ctx context.Context, viewName string, from, to time.Time) error {
	return s.refreshCAGG(ctx, viewName, from, to, CAGGRefreshTimeout(to.Sub(from)), true)
}

// Prices1mEarliestBucket returns prices_1m's earliest materialised bucket
// (the view is materialized_only, migration 0172), [ErrNotFound] when it
// holds none. Below it, minute rows were dropped or never materialised.
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

// Prices1mRetentionArmed reports whether migration 0156's retention
// policy on prices_1m is scheduled. It matches on the view name, as that
// migration requires; no policy reads as not armed.
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
	// CALL refresh_continuous_aggregate(view, $1::timestamptz, $2::timestamptz).
	// The first arg is REGCLASS in Timescale's signature, which pgx
	// can't placeholder; concatenating from the allow-list is safe.
	// Time params need explicit ::timestamptz casts: the database/sql
	// stored-procedure CALL path doesn't propagate the declared
	// parameter types from the procedure signature, so an
	// untyped placeholder fails with `42P18: could not determine
	// data type of parameter $1`.
	forceArg := ""
	if force {
		forceArg = ", force => true"
	}
	q := fmt.Sprintf(`CALL refresh_continuous_aggregate('%s', $1::timestamptz, $2::timestamptz%s)`, viewName, forceArg)
	// Retry on 55P03 (concurrent refresh) — Timescale serializes
	// refresh of the same CAGG, but it does so by REJECTING the
	// loser immediately rather than blocking it, so two callers
	// racing one view need a retry on this side. A `-parallel 4` SDEX
	// backfill races every chunk's prices_1mo refresh.
	//
	// The budget below is ~3.0s total, and it is NOT sized to
	// outlast a contending refresh. That reading was true only for
	// prices_1mo, whose padded window materialises a handful of
	// calendar buckets; prices_1m over the same chunk runs as long
	// as the chunk has trades, which is minutes for a `-parallel 4`
	// weekly slice. The budget is sized for the SHORT collisions —
	// the policy refresher's own tick, an operator's slice — and
	// callers that fan out in-process must not rely on it: the
	// backfill tool holds a process-wide mutex over its refresh loop
	// (ingest.caggRefreshMu) so its own workers never contend here.
	// A caller that skips that and loses a race to a long refresh
	// exhausts these five attempts and gets a hard error.
	//
	// The timeout is per ATTEMPT, not per call: a 55P03 loser did not
	// start materialising, so its attempt cost nothing against the
	// bound, and the retry that wins gets the full budget.
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

// isConcurrentRefreshErr reports whether err is the
// SQLSTATE 55P03 ("could not refresh continuous aggregate due to
// a concurrent refresh") emitted by Timescale when two callers
// race on the same CAGG. Matched by message substring rather
// than driver-typed code so we don't take a hard pq dep here.
func isConcurrentRefreshErr(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "55P03") ||
		strings.Contains(err.Error(), "concurrent refresh"))
}

// CAGGCoverage describes the time range and row count of one
// continuous aggregate — prices_1h, which is canonical for this
// answer only because it is the coarsest rung every deployment has
// always materialised, not because the finer ones are transient.
// Nothing in prices_1h is pruned: migration 0031 removed the 90-day
// retention on raw `trades` and the 30-day retention on prices_1m /
// prices_15m. `trades`, prices_1h and this stat still
// span the same history. prices_1m need not, on a deployment that has
// armed migration 0156's 90-day policy on that one view — one more
// reason this answer is about prices_1h alone. A stale comment would describe
// migration 0002's world instead — trades kept for a rolling 90 days,
// only the hourly-and-coarser aggregates kept indefinitely — which
// migration 0031 retired.
//
// A wide CAGGCoverage therefore means what it says — a healthy
// since-genesis backfill — but it says it about prices_1h alone. It
// is NOT evidence that the finer grains are materialised over the
// same span: that was exactly the gap this file's [CAGGsLiveForever]
// comment describes, and it was invisible on this stat for four
// months.
type CAGGCoverage struct {
	EarliestBucket time.Time
	LatestBucket   time.Time
	BucketCount    int64
}

// CAGGCoverageStats returns the earliest + latest buckets in
// prices_1h. Sub-second under the (base_asset, quote_asset, bucket)
// index. Empty when the CAGG has not yet been materialised at all
// (cold-start before any aggregator tick).
func (s *Store) CAGGCoverageStats(ctx context.Context) (CAGGCoverage, error) {
	// MIN/MAX use the bucket index. The exact COUNT(*) over prices_1h was a
	// full scan that grew to ~36s as the CAGG accrued ~175M rows (1h OHLC back
	// to 2015) — and /v1/diagnostics/ingestion polls this every ~15s, so it
	// hammered Postgres and tripped parallel-worker churn (the recurring
	// "terminating parallel worker" log flood). BucketCount is a coverage
	// stat, so TimescaleDB's chunk-metadata approximate_row_count is more than
	// precise enough and removes the scan.
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

// BackfillCoverage is one row of the per-source coverage summary —
// the earliest + latest ledgers we have any trade for, plus the
// total trade count. Lets the diagnostics surface answer the
// operator's first question: "do we have data from genesis to
// tip?" — yes if EarliestLedger ≤ source's known genesis and
// LatestLedger ≈ network tip; gaps inside that range aren't
// detected by this projection (would need a much heavier
// distinct-ledger scan).
//
// CEX/FX sources report (0, 0) because their trades carry no
// Stellar ledger context — we record TradeCount but the
// EarliestLedger / LatestLedger columns are meaningless.
type BackfillCoverage struct {
	Source         string
	EarliestLedger int64
	LatestLedger   int64
	TradeCount     int64
}

// BackfillCoverageStats is intentionally a no-op (returns no rows); it
// exists only to satisfy the CoverageCache reader interface.
//
// Coverage is cursor-first: every mapped source's density, earliest and
// latest ledger come from the backfill-cursor union and the
// source_entry_counts tally, so a per-source scan of `trades` would be
// discarded. It is also unsafe: oracle sources have no `trades` rows, so
// their scan cannot chunk-exclude and walks the whole hypertable to the
// statement timeout.
func (s *Store) BackfillCoverageStats(_ context.Context) ([]BackfillCoverage, error) {
	return nil, nil
}

// supplyCoverageStatsQuery backs [Store.SupplyCoverageStats]. The `newest`
// CTE orders by `time` alone: close time is monotonic in ledger sequence
// (no two ledgers share a close time), so the newest row already carries
// the highest ledger and a ledger_sequence tie-break can never fire — it
// would only cost the plan an extra sort key over the index/compress-orderby,
// defeating the single ordered-append seek.
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

// SupplyCoverageStats returns the current coverage state of the
// asset_supply_history hypertable: how many assets have ever been given a
// supply (SEP-41 vs classic) and the newest snapshot's time and ledger.
//
// No time floor: an asset whose last snapshot predates any window still has
// a supply, so a floor would under-count. The asset set is walked as a loose
// index scan over asset_supply_history_asset_time_idx, one seek per distinct
// asset_key per chunk, instead of reading and sorting every history row.
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

// SourceEntryCounts returns the per-source running entry tally from
// `source_entry_counts` (migration 0035) — every decoded-event
// hypertable (see [Store.SeedSourceEntryCounts]), keyed by source. This is a ~20-row PK scan of a tiny tally table,
// so it is O(1)-ish and ALWAYS fast — unlike BackfillCoverageStats
// it does not touch the trades/oracle_updates hypertables, so it
// stays responsive even during an all-time backfill (the whole
// reason the counter exists). Powers the `entries` column on
// /v1/diagnostics/ingestion.
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

// SeedSourceEntryCounts authoritatively recomputes source_entry_counts
// from a full GROUP BY over every decoded-event hypertable and
// overwrites the tally (SET, not ADD — so re-running converges).
// Returns the number of source rows reconciled.
//
// The incremental bump cannot know pre-counter history, so this is the
// operator one-shot `stellarindex-ops seed-entry-counts`, run after a
// backfill: it scans every relevant chunk in one transaction.
//
// The tables folded are those in seedSourceEntryCountsSQL, kept in
// lockstep with DefaultGapDetectorTargets by
// [TestSeedSourceEntryCountsFoldsEveryPerSourceHypertable]; a watched
// table left out would have its tally overwritten by a re-seed.
//
// Unlike the trades/oracle_updates bump, which fires inside the idempotent
// INSERT, pipeline/sink.go's bumpEntryCount adds +1 unconditionally, so a
// replay double-counts. Each folded table holds exactly one idempotent row
// per bumped event, so its COUNT equals the steady-state bumps, and a
// re-seed after a replay corrects the drift. Sources spread over several
// tables (blend's four; comet, soroswap and phoenix non-swap streams plus
// their swaps in trades) are disjoint event sets, so the outer GROUP BY's
// sum is the honest total.
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

// BumpSourceEntryCount increments the running entry tally for one
// source by n. Under ON CONFLICT the first writer for a source creates
// the row, subsequent writers ADD. Two callers, with different
// replay-safety:
//
//   - the trades + oracle_updates + fx_quotes insert paths bump INLINE
//     inside the idempotent INSERT (`HAVING count(*) > 0`) — the bump
//     fires only when a row is actually inserted, so a backfill re-walk
//     over already-stored rows is a no-op. Replay-safe.
//   - pipeline/sink.go::bumpEntryCount — n = 1 per decoded event in the
//     dispatcher → sink hand-off, for every non-trades/oracle sink
//     (blend*, comet, cctp, rozo, soroswap-router, defindex, the SAC /
//     account / trustline / claimable / LP observers, sep41_*, phoenix
//     liquidity/stake, soroswap skim). This ADD is UNCONDITIONAL — NOT
//     replay-safe: re-driving the sink over an already-ingested range
//     double-counts.
//
// That drift is corrected by [Store.SeedSourceEntryCounts], which
// SET-resets every source from its now-existing countable table — so a
// replay's over-count is transient, reconciled on the next seed.
//
// The bump is a single UPSERT — cheap enough for per-event use on
// the low-volume log-only sinks (router + defindex emit handfuls
// per minute at steady state).
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
