package clickhouse

import (
	"context"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// TradesByAccountSources is the trade-source set a window must have decoded for the API to count it as coverage.
// Literals because storage sits below sources; ch-rebuild's catalogue test pins them to each package's SourceName.
var TradesByAccountSources = []string{"aquarius", "comet", "phoenix", "sdex", "soroswap", "sushiswap_v3"}

// TradesSourcesKey is the stable form of a source set stored on a window row: sorted, de-duplicated, comma-joined.
func TradesSourcesKey(sources []string) string {
	sorted := append([]string(nil), sources...)
	sort.Strings(sorted)
	out := sorted[:0]
	for i, s := range sorted {
		if i == 0 || s != sorted[i-1] {
			out = append(out, s)
		}
	}
	return strings.Join(out, ",")
}

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

var (
	int128Max = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 127), big.NewInt(1))
	int128Min = new(big.Int).Neg(new(big.Int).Lsh(big.NewInt(1), 127))
)

func fitsInt128(v *big.Int) bool {
	return v == nil || (v.Cmp(int128Min) >= 0 && v.Cmp(int128Max) <= 0)
}

// FanOutTradeByAccount yields the taker row and, when the maker is a different account, the maker row (a self-trade appears once, as in PG).
// An amount outside Int128 is an error: the column would wrap it into a wrong served number.
func FanOutTradeByAccount(t canonical.Trade, usdVolume string) ([]TradeByAccountRow, error) {
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
	if !fitsInt128(base.BaseAmount) || !fitsInt128(base.QuoteAmount) {
		return nil, fmt.Errorf("clickhouse: trade %s/%d (%s): amount outside Int128 (base %s, quote %s)",
			t.TxHash, t.OpIndex, t.Source, base.BaseAmount, base.QuoteAmount)
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
	return out, nil
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

// RecordTradesByAccountWindow marks [lo,hi] as fully written for the source set in sourcesKey. Call it only after every row of the window
// was sent: the API serves archive rows only inside a contiguous run of recorded windows.
func RecordTradesByAccountWindow(ctx context.Context, addr string, lo, hi uint32, rows int64, sourcesKey string) error {
	conn, err := openAccountMovementsWrite(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	substrate := func(ctx context.Context, from, to uint32) (uint32, bool, string, error) {
		return substrateProblemOn(ctx, conn, addr, from, to)
	}
	return recordTradesByAccountWindowOn(ctx, conn, substrate, lo, hi, rows, sourcesKey)
}

type tradesWindowSubstrateCheck func(ctx context.Context, from, to uint32) (problem uint32, hasProblem bool, detail string, err error)

// recordTradesByAccountWindowOn refuses a window the lake cannot certify (ADR-0033 Claim 1: every ledger present and hash-linked): a
// decode over a hole finds no trades there, and recording it would claim them absent.
func recordTradesByAccountWindowOn(ctx context.Context, conn driver.Conn, substrate tradesWindowSubstrateCheck, lo, hi uint32, rows int64, sourcesKey string) error {
	if sourcesKey == "" {
		return fmt.Errorf("clickhouse: trades_by_account window [%d,%d]: empty source set; refusing to record coverage", lo, hi)
	}
	problem, hasProblem, detail, err := substrate(ctx, lo, hi)
	if err != nil {
		return fmt.Errorf("clickhouse: trades_by_account window [%d,%d]: lake completeness check: %w", lo, hi, err)
	}
	if hasProblem {
		return fmt.Errorf("clickhouse: trades_by_account window [%d,%d]: lake not complete at ledger %d (%s); refusing to record coverage", lo, hi, problem, detail)
	}
	var fromTs, toTs time.Time
	const q = `SELECT minIf(close_time, ledger_seq = ?), maxIf(close_time, ledger_seq = ?)
		FROM stellar.ledgers WHERE ledger_seq IN (?, ?)`
	if err := conn.QueryRow(ctx, q, lo, hi, lo, hi).Scan(&fromTs, &toTs); err != nil {
		return fmt.Errorf("clickhouse: trades_by_account window close times: %w", err)
	}
	if err := conn.Exec(ctx, `INSERT INTO stellar.trades_by_account_windows (from_ledger, to_ledger, from_ts, to_ts, rows, sources) VALUES (?, ?, ?, ?, ?, ?)`,
		lo, hi, fromTs, toTs, uint64(max(rows, 0)), sourcesKey); err != nil {
		return fmt.Errorf("clickhouse: record trades_by_account window [%d,%d]: %w", lo, hi, err)
	}
	return nil
}

// CheckTradesByAccountOverlap refuses [lo,hi] when it overlaps a recorded window, unless replace is set; replace in turn needs every
// overlapping window inside [lo,hi], since the clear deletes only [lo,hi]'s rows and a window reaching past it would keep claiming them.
func CheckTradesByAccountOverlap(recorded []TradesByAccountRun, lo, hi uint32, replace bool) error {
	for _, w := range recorded {
		if w.ToLedger < lo || w.FromLedger > hi {
			continue
		}
		if !replace {
			return fmt.Errorf("clickhouse: trades_by_account window [%d,%d] overlaps recorded window [%d,%d]; pass -replace to clear and rewrite it", lo, hi, w.FromLedger, w.ToLedger)
		}
		if w.FromLedger < lo || w.ToLedger > hi {
			return fmt.Errorf("clickhouse: trades_by_account -replace [%d,%d] cuts recorded window [%d,%d]; widen the range to cover it", lo, hi, w.FromLedger, w.ToLedger)
		}
	}
	return nil
}

// PrepareTradesByAccountWindow refuses an overlapping window without replace; with replace it deletes the overlapping window records
// first (so a failed run leaves coverage absent, never stale) and then [lo,hi]'s rows.
func PrepareTradesByAccountWindow(ctx context.Context, addr string, lo, hi uint32, replace bool) error {
	conn, err := openAccountMovementsWrite(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	recorded, err := scanTradesByAccountWindows(ctx, conn, `SELECT from_ledger, to_ledger, from_ts, to_ts, sources
		FROM stellar.trades_by_account_windows FINAL WHERE from_ledger <= ? AND to_ledger >= ?`, hi, lo)
	if err != nil {
		return err
	}
	if err := CheckTradesByAccountOverlap(recorded, lo, hi, replace); err != nil {
		return err
	}
	if len(recorded) == 0 {
		return nil
	}
	for _, stmt := range []string{
		`ALTER TABLE stellar.trades_by_account_windows DELETE WHERE from_ledger >= ? AND to_ledger <= ? SETTINGS mutations_sync = 2`,
		`ALTER TABLE stellar.trades_by_account DELETE WHERE ledger >= ? AND ledger <= ? SETTINGS mutations_sync = 2`,
	} {
		if err := conn.Exec(ctx, stmt, lo, hi); err != nil {
			return fmt.Errorf("clickhouse: trades_by_account -replace [%d,%d]: %w", lo, hi, err)
		}
	}
	return nil
}

func scanTradesByAccountWindows(ctx context.Context, conn driver.Conn, q string, args ...any) ([]TradesByAccountRun, error) {
	rows, err := conn.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: trades_by_account windows: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ws []TradesByAccountRun
	for rows.Next() {
		var w TradesByAccountRun
		if err := rows.Scan(&w.FromLedger, &w.ToLedger, &w.FromTs, &w.ToTs, &w.Sources); err != nil {
			return nil, fmt.Errorf("clickhouse: trades_by_account windows scan: %w", err)
		}
		ws = append(ws, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("clickhouse: trades_by_account windows rows: %w", err)
	}
	return ws, nil
}

// TradesByAccountRun is a contiguous ledger range of recorded windows and the close times of its end ledgers.
type TradesByAccountRun struct {
	FromLedger, ToLedger uint32
	FromTs, ToTs         time.Time
	// Sources is the window's TradesSourcesKey; a merged run carries the key it was filtered on.
	Sources string
}

// MergeTradesByAccountWindows folds the windows recorded for sourcesKey into contiguous runs, ordered by FromLedger. A window recorded
// for another source set is not coverage: it lacks a source's trades, or predates the key column.
func MergeTradesByAccountWindows(ws []TradesByAccountRun, sourcesKey string) []TradesByAccountRun {
	var sorted []TradesByAccountRun
	for _, w := range ws {
		if w.Sources == sourcesKey {
			sorted = append(sorted, w)
		}
	}
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

// TradesByAccountCoverage returns the merged coverage runs for the current TradesByAccountSources (nil when the table does not exist yet).
func (r *ExplorerReader) TradesByAccountCoverage(ctx context.Context) ([]TradesByAccountRun, error) {
	ws, err := scanTradesByAccountWindows(ctx, r.conn, `SELECT from_ledger, to_ledger, from_ts, to_ts, sources
		FROM stellar.trades_by_account_windows FINAL`)
	if err != nil {
		if isSchemaAbsent(err) {
			return nil, nil
		}
		return nil, err
	}
	return MergeTradesByAccountWindows(ws, TradesSourcesKey(TradesByAccountSources)), nil
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
