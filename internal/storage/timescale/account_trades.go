package timescale

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// This file is the read side for GET /v1/accounts/{g_strkey}/trades —
// per-address historic trades out of the `trades` hypertable.
//
// ACCOUNT ATTRIBUTION — what the table actually holds: `trades` has no
// source_account column (verified against migrations/ and the live r1
// schema). The per-row account attribution is:
//
//   taker — the acting account: the tx-level source for sdex, the
//           user/sender address for aquarius, phoenix, and comet, and
//           the SwapEvent `to` recipient for soroswap (verified 100%
//           taker coverage on new rows; soroswap rows ingested before
//           the decoder captured `to` carry NULL unless re-derived).
//   maker — the resting-offer account (sdex only).
//
// Both are NULL for off-chain CEX/FX rows (ledger=0 — no Stellar
// account exists to attribute). The read therefore serves "trades
// where this address is recorded as taker or maker".
//
// QUERY SHAPE: two index-friendly arms UNION'd, not `taker = $1 OR
// maker = $1` — an OR across two columns can't ride a single
// account-leading index in output order, so the planner degrades to a
// bitmap-or + sort over the account's whole history before the LIMIT.
// Each arm carries its own ORDER BY + LIMIT so it walks its partial
// index (migration 0123) in output order and stops after one page; the
// outer merge-sort + LIMIT then picks the page. The union of two
// individually-top-N arms provably contains the union's top N (same
// invariant clickhouse.accountTransactionsQuery documents). The maker
// arm excludes rows where the address is ALSO the taker so a
// self-crossed sdex trade appears once (as the taker leg).
//
// KEYSET: (ts DESC, ledger DESC, tx_hash DESC, op_index DESC). ts alone
// is not unique (every trade in a ledger shares its close time) and
// (ts, ledger, op_index) still collides for two same-account txs in one
// ledger, so tx_hash rides in the tuple — never re-emits a served row,
// never skips an unserved one.

// accountTradesMaxLimit clamps one page. Mirrors the sibling explorer
// listings' 200-row ceiling.
const (
	accountTradesDefaultLimit = 50
	accountTradesMaxLimit     = 200
)

// AccountTradeRow is one per-address trade row. Amounts are decimal
// strings straight from the NUMERIC columns (ADR-0003 — i128 amounts
// exceed IEEE 754 double precision; nothing in this path may pass
// through a float). USDVolume is "" when the stored column is NULL
// (unknown — the aggregator could not value the trade), never "0".
type AccountTradeRow struct {
	Source      string
	Ledger      uint32
	TxHash      string
	OpIndex     uint32
	Ts          time.Time
	BaseAsset   string
	QuoteAsset  string
	BaseAmount  string
	QuoteAmount string
	USDVolume   string
	// Role is which side of the trade this address was recorded on:
	// "taker" (acting account) or "maker" (resting sdex offer).
	Role string
	// Counterparty is the OTHER recorded account, when the venue
	// recorded one ("" otherwise — most venues record only the taker).
	Counterparty string
	RoutedVia    string
	// Signer is the transaction source account (fee-payer / initiator)
	// behind an AMM/Soroban swap, back-tagged from the lake (migration
	// 0150). "" for a non-AMM trade or one not yet swept.
	Signer string
}

// AccountTradesCursor is the keyset position for ListAccountTrades —
// the (ts, ledger, tx_hash, op_index) of the last served row. Zero
// value (Ts.IsZero()) means first page.
type AccountTradesCursor struct {
	Ts      time.Time
	Ledger  uint32
	TxHash  string
	OpIndex uint32
}

// IsSet reports whether the cursor points past a previously served row.
func (c AccountTradesCursor) IsSet() bool { return !c.Ts.IsZero() }

// accountTradesInnerCols is the per-arm column list. base/quote amounts
// and usd_volume are cast to text server-side so the driver never sees a
// float (ADR-0003).
//
// The two COALESCE expressions MUST carry explicit aliases. Without them
// PostgreSQL names both output columns `coalesce`, so the outer SELECT's
// reference to `usd_volume` resolves against nothing and the statement
// fails at PLAN time (`pq: column "usd_volume" does not exist ...
// (42703)`) — GET /v1/accounts/{id}/trades would return 500 for every
// account. A test that only asserts substrings of the query STRING cannot
// see this; it needs real SQL execution.
const accountTradesInnerCols = `source, ledger, tx_hash, op_index, ts,
	       base_asset, quote_asset,
	       base_amount::text AS base_amount, quote_amount::text AS quote_amount,
	       COALESCE(usd_volume::text, '') AS usd_volume,
	       COALESCE(routed_via, '') AS routed_via,
	       COALESCE(signer, '') AS signer`

// accountTradesOuterCols re-projects the UNION's already-normalised
// columns by name — no re-casting, no re-COALESCE.
const accountTradesOuterCols = `source, ledger, tx_hash, op_index, ts,
	       base_asset, quote_asset,
	       base_amount, quote_amount, usd_volume, routed_via, signer`

// accountTradesQuery builds the two-arm UNION described in the file
// header. hasCursor appends the keyset tuple comparison to both arms.
//
// Placeholder layout (hasCursor=true):
//
//	$1 address · $2 ts floor · $3..$6 cursor · $7 limit (arm 1)
//	$1 address · $2 ts floor · $3..$6 cursor · $7 limit (arm 2, same params)
//
// Without a cursor the limit moves to $3.
//
// Both arms reuse the same numbered placeholders, so the caller passes
// each value once regardless of arm count.
func accountTradesQuery(hasCursor bool) string {
	// $2 is always the compression-horizon ts floor computed by
	// tradesUncompressedHorizon: the per-account partial indexes
	// exist only on UNCOMPRESSED chunks — a compressed chunk has no
	// btree, so an arm that descends into one decompress-scans it (~46k
	// buffers/chunk; 16.4M buffers ≈ 8s measured proving a ZERO-trade
	// account empty). The floor is the END of the newest COMPRESSED
	// chunk, so ts >= floor can never resolve to a compressed chunk and
	// ChunkAppend excludes them outright. The caller surfaces the floor
	// as an explicit coverage note rather than serving a silently-partial
	// "all time" answer.
	cursorClause := ""
	limitPh := "$3"
	if hasCursor {
		cursorClause = ` AND (ts, ledger, tx_hash, op_index) < ($3, $4, $5, $6)`
		limitPh = "$7"
	}
	orderBy := ` ORDER BY ts DESC, ledger DESC, tx_hash DESC, op_index DESC LIMIT ` + limitPh
	return `SELECT ` + accountTradesOuterCols + `, role, counterparty FROM (
		(SELECT ` + accountTradesInnerCols + `, 'taker' AS role, COALESCE(maker, '') AS counterparty
		   FROM trades WHERE taker = $1 AND ts >= $2` + cursorClause + orderBy + `)
		UNION ALL
		(SELECT ` + accountTradesInnerCols + `, 'maker' AS role, COALESCE(taker, '') AS counterparty
		   FROM trades WHERE maker = $1 AND ts >= $2 AND (taker IS NULL OR taker <> $1)` + cursorClause + orderBy + `)
	) u` + orderBy
}

// tradesHorizonFailClosedWindow is the fail-CLOSED floor served on a
// catalog lookup error: short enough that it is always inside the
// compression policy's compress_after window (15 days on `trades`, see
// TradesCompressionPolicy) with margin, so it can never claim a chunk
// that might already be compressed as index-safe.
const tradesHorizonFailClosedWindow = 24 * time.Hour

// tradesUncompressedHorizon returns the floor below which per-account
// trades reads have no usable index (see accountTradesQuery): the END
// of the NEWEST COMPRESSED chunk — not the start of the oldest
// uncompressed one.
//
// The two are not interchangeable: compression is not a time
// prefix. An old chunk can sit uncompressed (a stuck compression job, a
// late-arriving backfill) while everything after it has already
// compressed, and min(range_start) over uncompressed chunks then picks
// that ancient straggler as the "floor" — observed on r1 publishing
// 2021 as trades_total_since while 313 of 472 trades chunks were
// already compressed, i.e. not a floor at all. max(range_end) over
// COMPRESSED chunks is monotone-safe instead: by definition of max, no
// compressed chunk's range extends past it, so ts >= floor can never
// need a compressed-chunk scan regardless of what an out-of-order
// straggler below it is doing.
//
// A stranded uncompressed chunk (one whose range starts before that
// floor) doesn't make the floor unsafe — it's excluded either way — but
// it is an anomaly worth an operator's attention (genuinely-indexed
// history is being excluded from the fast path), so it's logged.
//
// Cached for 10 minutes; the horizon only moves when the compression
// policy compresses another chunk. Fails CLOSED — a short recent window
// (tradesHorizonFailClosedWindow), NOT epoch — on a catalog lookup
// error: epoch means "no floor", which sends the read straight into the
// unindexed full-history scan, which on r1 hit the 8s timeout and
// returned 503. AccountTrades and
// computeAccountActivity already render any non-zero, post-1971 horizon
// as an honest "showing trades since <date>" / trades_total_since
// coverage note, so failing closed degrades into that same channel
// instead of silently serving as if all history were indexed.
func (s *Store) tradesUncompressedHorizon(ctx context.Context) time.Time {
	tradesHorizonMu.Lock()
	defer tradesHorizonMu.Unlock()
	if time.Since(tradesHorizonAt) < 10*time.Minute && !tradesHorizon.IsZero() {
		return tradesHorizon
	}
	const q = `WITH compressed_floor AS (
	             SELECT coalesce(max(range_end), 'epoch'::timestamptz) AS floor
	               FROM timescaledb_information.chunks
	              WHERE hypertable_name = 'trades' AND is_compressed
	           )
	           SELECT compressed_floor.floor,
	                  EXISTS (
	                      SELECT 1 FROM timescaledb_information.chunks c, compressed_floor
	                       WHERE c.hypertable_name = 'trades' AND NOT c.is_compressed
	                         AND c.range_start < compressed_floor.floor
	                  ) AS stranded_uncompressed_chunk
	             FROM compressed_floor`
	var (
		t        time.Time
		stranded bool
	)
	if err := s.db.QueryRowContext(ctx, q).Scan(&t, &stranded); err != nil {
		// Fail CLOSED: a short recent window, not epoch — see doc comment.
		return time.Now().Add(-tradesHorizonFailClosedWindow)
	}
	if stranded {
		slog.Warn("timescale: tradesUncompressedHorizon: an uncompressed trades chunk predates the newest compressed chunk",
			"floor", t)
	}
	tradesHorizon, tradesHorizonAt = t, time.Now()
	return t
}

var (
	tradesHorizonMu sync.Mutex
	tradesHorizon   time.Time
	tradesHorizonAt time.Time
)

// clampAccountTradesLimit normalizes a caller limit onto
// [1, accountTradesMaxLimit], defaulting out-of-range values.
func clampAccountTradesLimit(limit int) int {
	if limit <= 0 || limit > accountTradesMaxLimit {
		return accountTradesDefaultLimit
	}
	return limit
}

// ListAccountTrades returns the address's trades (taker or maker side),
// newest first, keyset-paged. Empty slice + nil error when the address
// has no attributed trades.
func (s *Store) ListAccountTrades(ctx context.Context, address string, limit int, cur AccountTradesCursor) ([]AccountTradeRow, time.Time, error) {
	limit = clampAccountTradesLimit(limit)
	horizon := s.tradesUncompressedHorizon(ctx)
	args := []any{address, horizon}
	if cur.IsSet() {
		args = append(args, cur.Ts.UTC(), cur.Ledger, cur.TxHash, cur.OpIndex)
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, accountTradesQuery(cur.IsSet()), args...)
	if err != nil {
		return nil, horizon, fmt.Errorf("timescale: ListAccountTrades: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []AccountTradeRow
	for rows.Next() {
		var (
			r      AccountTradeRow
			ledger int64
			opIdx  int64
		)
		if err := rows.Scan(&r.Source, &ledger, &r.TxHash, &opIdx, &r.Ts,
			&r.BaseAsset, &r.QuoteAsset, &r.BaseAmount, &r.QuoteAmount,
			&r.USDVolume, &r.RoutedVia, &r.Signer, &r.Role, &r.Counterparty); err != nil {
			return nil, horizon, fmt.Errorf("timescale: ListAccountTrades scan: %w", err)
		}
		r.Ledger = uint32(ledger) //nolint:gosec // ledger seq fits uint32
		r.OpIndex = uint32(opIdx) //nolint:gosec // op_index fits uint32
		r.Ts = r.Ts.UTC()
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, horizon, fmt.Errorf("timescale: ListAccountTrades rows: %w", err)
	}
	return out, horizon, nil
}

// CountAccountTrades returns the address's attributed trade count
// (taker or maker side, each row once) SINCE the returned horizon —
// the compression boundary below which the per-account partial indexes
// don't exist (an all-time OR count decompress-scanned all 248
// compressed chunks in ~8s when measured, burning the activity
// endpoint's trades_total budget). The OR here
// stays deliberate: with the ts floor the scan is confined to indexed
// uncompressed chunks, where a bitmap-or counts each row once.
func (s *Store) CountAccountTrades(ctx context.Context, address string) (int64, time.Time, error) {
	horizon := s.tradesUncompressedHorizon(ctx)
	const q = `SELECT count(*) FROM trades WHERE (taker = $1 OR maker = $1) AND ts >= $2`
	var n int64
	if err := s.db.QueryRowContext(ctx, q, address, horizon).Scan(&n); err != nil {
		return 0, horizon, fmt.Errorf("timescale: CountAccountTrades: %w", err)
	}
	return n, horizon, nil
}
