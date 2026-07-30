package timescale

import (
	"context"
	"fmt"
	"time"
)

// This file is the read side for GET /v1/accounts/{g_strkey}/trades —
// per-address historic trades out of the `trades` hypertable.
//
// ACCOUNT ATTRIBUTION — what the table actually holds: `trades` has no
// source_account column (verified against migrations/ and the live r1
// schema, 2026-07-30). The per-row account attribution is:
//
//   taker — the acting account: the tx-level source for sdex, the
//           user/sender address for aquarius, phoenix, and comet.
//   maker — the resting-offer account (sdex only).
//
// Both are NULL for off-chain CEX/FX rows (ledger=0 — no Stellar
// account exists to attribute) and for soroswap (its decoder does not
// capture the SwapEvent `to` field — a documented coverage gap the
// endpoint's static note discloses, not something this reader can
// conjure). The read therefore serves "trades where this address is
// recorded as taker or maker".
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

// accountTradesCols is the per-arm column list. base/quote amounts and
// usd_volume are cast to text server-side so the driver never sees a
// float (ADR-0003).
const accountTradesCols = `source, ledger, tx_hash, op_index, ts,
	       base_asset, quote_asset,
	       base_amount::text, quote_amount::text,
	       COALESCE(usd_volume::text, ''), COALESCE(routed_via, '')`

// accountTradesQuery builds the two-arm UNION described in the file
// header. hasCursor appends the keyset tuple comparison to both arms.
//
// Placeholder layout (hasCursor=true):
//
//	$1 address · $2..$5 cursor · $6 limit (arm 1)
//	$1 address · $2..$5 cursor · $6 limit (arm 2, same params)
//
// Both arms reuse the same numbered placeholders, so the caller passes
// each value once regardless of arm count.
func accountTradesQuery(hasCursor bool) string {
	cursorClause := ""
	limitPh := "$2"
	if hasCursor {
		cursorClause = ` AND (ts, ledger, tx_hash, op_index) < ($2, $3, $4, $5)`
		limitPh = "$6"
	}
	orderBy := ` ORDER BY ts DESC, ledger DESC, tx_hash DESC, op_index DESC LIMIT ` + limitPh
	return `SELECT ` + accountTradesCols + `, role, counterparty FROM (
		(SELECT ` + accountTradesCols + `, 'taker' AS role, COALESCE(maker, '') AS counterparty
		   FROM trades WHERE taker = $1` + cursorClause + orderBy + `)
		UNION ALL
		(SELECT ` + accountTradesCols + `, 'maker' AS role, COALESCE(taker, '') AS counterparty
		   FROM trades WHERE maker = $1 AND (taker IS NULL OR taker <> $1)` + cursorClause + orderBy + `)
	) u` + orderBy
}

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
func (s *Store) ListAccountTrades(ctx context.Context, address string, limit int, cur AccountTradesCursor) ([]AccountTradeRow, error) {
	limit = clampAccountTradesLimit(limit)
	args := []any{address}
	if cur.IsSet() {
		args = append(args, cur.Ts.UTC(), cur.Ledger, cur.TxHash, cur.OpIndex)
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, accountTradesQuery(cur.IsSet()), args...)
	if err != nil {
		return nil, fmt.Errorf("timescale: ListAccountTrades: %w", err)
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
			&r.USDVolume, &r.RoutedVia, &r.Role, &r.Counterparty); err != nil {
			return nil, fmt.Errorf("timescale: ListAccountTrades scan: %w", err)
		}
		r.Ledger = uint32(ledger) //nolint:gosec // ledger seq fits uint32
		r.OpIndex = uint32(opIdx) //nolint:gosec // op_index fits uint32
		r.Ts = r.Ts.UTC()
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: ListAccountTrades rows: %w", err)
	}
	return out, nil
}

// CountAccountTrades returns the address's all-time attributed trade
// count (taker or maker side, each row once).
//
// The OR here is deliberate (unlike ListAccountTrades' UNION): a plain
// count has no ORDER BY + LIMIT for an index to preserve, so a
// bitmap-or over the two partial indexes (migration 0123) is exactly
// what the planner should do, and each matching row is counted once
// with no dedup step. Cost scales with the address's own trade count —
// callers run it under the activity endpoint's detached
// stale-while-revalidate budget, not a request deadline.
func (s *Store) CountAccountTrades(ctx context.Context, address string) (int64, error) {
	const q = `SELECT count(*) FROM trades WHERE taker = $1 OR maker = $1`
	var n int64
	if err := s.db.QueryRowContext(ctx, q, address).Scan(&n); err != nil {
		return 0, fmt.Errorf("timescale: CountAccountTrades: %w", err)
	}
	return n, nil
}
