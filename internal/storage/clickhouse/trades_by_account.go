package clickhouse

import (
	"context"
	"fmt"
	"math/big"
	"sort"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// stellar.trades_by_account is Postgres `trades` re-keyed account-first (DDL: deploy/clickhouse/trades_by_account.sql),
// written by `ch-rebuild -trades-by-account` and read by GET /v1/accounts/{g}/trades below the PG compression horizon.

// Trade roles, matching the Postgres read's role column.
const (
	TradeRoleTaker = "taker"
	TradeRoleMaker = "maker"
)

// TradeByAccountRow is one (account, trade) row; amounts are full Int128 and USDVolume is "" when unvalued, never "0".
type TradeByAccountRow struct {
	Account      string
	Ts           time.Time
	Ledger       uint32
	TxHash       string
	OpIndex      uint32
	Source       string
	Role         string
	Counterparty string
	BaseAsset    string
	QuoteAsset   string
	BaseAmount   *big.Int
	QuoteAmount  *big.Int
	USDVolume    string
	RoutedVia    string
}

// FanOutTradeByAccount yields the taker row and, when the maker is a different account, the maker row (a self-trade appears once, as in PG).
func FanOutTradeByAccount(t canonical.Trade, usdVolume string) []TradeByAccountRow {
	base := TradeByAccountRow{
		Ts:          t.Timestamp.UTC(),
		Ledger:      t.Ledger,
		TxHash:      t.TxHash,
		OpIndex:     t.OpIndex,
		Source:      t.Source,
		BaseAsset:   t.Pair.Base.String(),
		QuoteAsset:  t.Pair.Quote.String(),
		BaseAmount:  t.BaseAmount.BigInt(),
		QuoteAmount: t.QuoteAmount.BigInt(),
		USDVolume:   usdVolume,
		RoutedVia:   t.RoutedVia,
	}
	var out []TradeByAccountRow
	if t.Taker != "" {
		r := base
		r.Account, r.Role, r.Counterparty = t.Taker, TradeRoleTaker, t.Maker
		out = append(out, r)
	}
	if t.Maker != "" && t.Maker != t.Taker {
		r := base
		r.Account, r.Role, r.Counterparty = t.Maker, TradeRoleMaker, t.Taker
		out = append(out, r)
	}
	return out
}

const tradesByAccountInsertChunk = 20_000

// InsertTradesByAccount batch-inserts rows in ledger order (retry-safe: ReplacingMergeTree absorbs duplicates); returns rows sent.
func InsertTradesByAccount(ctx context.Context, addr string, rows []TradeByAccountRow) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Ledger < rows[j].Ledger })
	conn, err := openAccountMovementsWrite(ctx, addr)
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Close() }()
	var written int64
	for i := 0; i < len(rows); i += tradesByAccountInsertChunk {
		end := min(i+tradesByAccountInsertChunk, len(rows))
		if err := insertTradesByAccountChunk(ctx, conn, rows[i:end]); err != nil {
			return written, fmt.Errorf("clickhouse: InsertTradesByAccount: chunk [%d,%d): %w", i, end, err)
		}
		written += int64(end - i)
	}
	return written, nil
}

func insertTradesByAccountChunk(ctx context.Context, conn driver.Conn, rows []TradeByAccountRow) error {
	batch, err := conn.PrepareBatch(ctx, `INSERT INTO stellar.trades_by_account
		(account, ts, ledger, tx_hash, op_index, source, role, counterparty,
		 base_asset, quote_asset, base_amount, quote_amount, usd_volume, routed_via)`)
	if err != nil {
		return fmt.Errorf("prepare trades_by_account batch: %w", err)
	}
	for _, r := range rows {
		if err := batch.Append(r.Account, r.Ts, r.Ledger, r.TxHash, r.OpIndex, r.Source, r.Role, r.Counterparty,
			r.BaseAsset, r.QuoteAsset, nonNilBig(r.BaseAmount), nonNilBig(r.QuoteAmount), r.USDVolume, r.RoutedVia); err != nil {
			return fmt.Errorf("append %s/%s/%d/%s: %w", r.Account, r.TxHash, r.OpIndex, r.Role, err)
		}
	}
	return wrapSend(batch.Send(), "trades_by_account")
}

func nonNilBig(v *big.Int) *big.Int {
	if v == nil {
		return big.NewInt(0)
	}
	return v
}

// RecordTradesByAccountWindow marks [lo,hi] as fully written. Call it only after every row of the window was sent: the API serves
// archive rows only inside a contiguous run of recorded windows.
func RecordTradesByAccountWindow(ctx context.Context, addr string, lo, hi uint32, rows int64) error {
	conn, err := openAccountMovementsWrite(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	var (
		found        uint64
		fromTs, toTs time.Time
	)
	const q = `SELECT uniqExact(ledger_seq), minIf(close_time, ledger_seq = ?), maxIf(close_time, ledger_seq = ?)
		FROM stellar.ledgers WHERE ledger_seq IN (?, ?)`
	if err := conn.QueryRow(ctx, q, lo, hi, lo, hi).Scan(&found, &fromTs, &toTs); err != nil {
		return fmt.Errorf("clickhouse: trades_by_account window close times: %w", err)
	}
	want := uint64(2)
	if lo == hi {
		want = 1
	}
	if found != want {
		return fmt.Errorf("clickhouse: trades_by_account window [%d,%d]: %d of %d end ledgers in stellar.ledgers; refusing to record coverage", lo, hi, found, want)
	}
	if err := conn.Exec(ctx, `INSERT INTO stellar.trades_by_account_windows (from_ledger, to_ledger, from_ts, to_ts, rows) VALUES (?, ?, ?, ?, ?)`,
		lo, hi, fromTs, toTs, uint64(max(rows, 0))); err != nil {
		return fmt.Errorf("clickhouse: record trades_by_account window [%d,%d]: %w", lo, hi, err)
	}
	return nil
}

// TradesByAccountRun is a contiguous ledger range of recorded windows and the close times of its end ledgers.
type TradesByAccountRun struct {
	FromLedger, ToLedger uint32
	FromTs, ToTs         time.Time
}

// MergeTradesByAccountWindows folds windows into contiguous runs, ordered by FromLedger.
func MergeTradesByAccountWindows(ws []TradesByAccountRun) []TradesByAccountRun {
	sorted := append([]TradesByAccountRun(nil), ws...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].FromLedger < sorted[j].FromLedger })
	var runs []TradesByAccountRun
	for _, w := range sorted {
		if n := len(runs); n > 0 && uint64(w.FromLedger) <= uint64(runs[n-1].ToLedger)+1 {
			if w.ToLedger > runs[n-1].ToLedger {
				runs[n-1].ToLedger, runs[n-1].ToTs = w.ToLedger, w.ToTs
			}
			continue
		}
		runs = append(runs, w)
	}
	return runs
}

// CoveringTradesByAccountRun returns the run whose time span contains at, i.e. the archive range that joins the PG horizon seamlessly.
func CoveringTradesByAccountRun(runs []TradesByAccountRun, at time.Time) (TradesByAccountRun, bool) {
	for _, r := range runs {
		if !r.FromTs.After(at) && !r.ToTs.Before(at) {
			return r, true
		}
	}
	return TradesByAccountRun{}, false
}

// TradesByAccountCoverage returns the merged coverage runs (nil when the table does not exist yet).
func (r *ExplorerReader) TradesByAccountCoverage(ctx context.Context) ([]TradesByAccountRun, error) {
	rows, err := r.conn.Query(ctx, `SELECT from_ledger, to_ledger, from_ts, to_ts
		FROM stellar.trades_by_account_windows FINAL`)
	if err != nil {
		if isSchemaAbsent(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("clickhouse: trades_by_account coverage: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ws []TradesByAccountRun
	for rows.Next() {
		var w TradesByAccountRun
		if err := rows.Scan(&w.FromLedger, &w.ToLedger, &w.FromTs, &w.ToTs); err != nil {
			return nil, fmt.Errorf("clickhouse: trades_by_account coverage scan: %w", err)
		}
		ws = append(ws, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("clickhouse: trades_by_account coverage rows: %w", err)
	}
	return MergeTradesByAccountWindows(ws), nil
}

// TradesByAccountCursor is the keyset position of the last served row (ts, ledger, tx_hash, op_index), as in the Postgres read.
type TradesByAccountCursor struct {
	Ts      time.Time
	Ledger  uint32
	TxHash  string
	OpIndex uint32
}

// archivedAccountTradesQuery: LIMIT 1 BY the identity collapses unmerged ReplacingMergeTree duplicates, newest ingest first.
func archivedAccountTradesQuery(hasCursor bool) string {
	cursor := ""
	if hasCursor {
		cursor = ` AND (ts, ledger, tx_hash, op_index) < (fromUnixTimestamp64Milli(?, 'UTC'), ?, ?, ?)`
	}
	return `SELECT ts, ledger, tx_hash, op_index, source, role, counterparty,
		       base_asset, quote_asset, base_amount, quote_amount, usd_volume, routed_via
		FROM stellar.trades_by_account
		WHERE account = ? AND ts >= fromUnixTimestamp64Milli(?, 'UTC') AND ts < fromUnixTimestamp64Milli(?, 'UTC')` + cursor + `
		ORDER BY ts DESC, ledger DESC, tx_hash DESC, op_index DESC, source DESC, ingested_at DESC
		LIMIT 1 BY ts, ledger, tx_hash, op_index, source
		LIMIT ?` + explorerScanSettings
}

// ListArchivedAccountTrades returns the account's trades with floor <= ts < below, newest first, keyset-paged.
func (r *ExplorerReader) ListArchivedAccountTrades(ctx context.Context, account string, limit int, floor, below time.Time, cur TradesByAccountCursor) ([]TradeByAccountRow, error) {
	hasCursor := !cur.Ts.IsZero()
	args := []any{account, floor.UnixMilli(), below.UnixMilli()}
	if hasCursor {
		args = append(args, cur.Ts.UnixMilli(), cur.Ledger, cur.TxHash, cur.OpIndex)
	}
	args = append(args, limit)
	rows, err := r.conn.Query(ctx, archivedAccountTradesQuery(hasCursor), args...)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: ListArchivedAccountTrades: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []TradeByAccountRow
	for rows.Next() {
		t := TradeByAccountRow{Account: account}
		if err := rows.Scan(&t.Ts, &t.Ledger, &t.TxHash, &t.OpIndex, &t.Source, &t.Role, &t.Counterparty,
			&t.BaseAsset, &t.QuoteAsset, &t.BaseAmount, &t.QuoteAmount, &t.USDVolume, &t.RoutedVia); err != nil {
			return nil, fmt.Errorf("clickhouse: ListArchivedAccountTrades scan: %w", err)
		}
		t.Ts = t.Ts.UTC()
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("clickhouse: ListArchivedAccountTrades rows: %w", err)
	}
	return out, nil
}
