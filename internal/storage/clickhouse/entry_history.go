package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Entry-change history: one decode pass of stellar.ledger_entry_changes
// (`stellarindex-ops ch-entry-history`) writes two serving projections of the
// same rows — stellar.account_entry_changes keyed by account and
// stellar.asset_entry_changes keyed by asset (deploy/clickhouse/entry_history.sql).
// The raw table is ORDER BY ledger, so neither read shape can use it directly.

// EntryHistorySourceRow is one stellar.ledger_entry_changes row as the
// entry-history derive reads it.
type EntryHistorySourceRow struct {
	Ledger         uint32
	CloseTime      time.Time
	TxHash         string
	OpIndex        int32
	ChangeIndex    uint32
	IntraLedgerSeq uint32
	ChangeType     string
	EntryType      string
	KeyXDR         string
	EntryXDR       string
}

// EntryChangeRef is the change a history row records: its position in the
// lake (the source row's primary key plus the canonical in-ledger order), what
// happened, which decoded fields differ from the pre-image, and the decoded
// entry as JSON. Fields carries the pre-image for a removal.
type EntryChangeRef struct {
	Ledger         uint32
	CloseTime      time.Time
	TxHash         string
	OpIndex        int32
	ChangeIndex    uint32
	IntraLedgerSeq uint32
	EntryType      string
	ChangeType     string
	Changed        []string
	Fields         string
}

// AccountEntryChange is one stellar.account_entry_changes row: the change as
// seen by one account in one role (owner / sponsor / claimant). Asset and
// Balance are set for the owner of an account or trustline and for a
// claimable balance's claimants; Balance is the post-change amount (0 once
// the entry is removed).
type AccountEntryChange struct {
	EntryChangeRef
	Account string
	Role    string
	Asset   string
	Balance *big.Int
}

// AssetEntryChange is one stellar.asset_entry_changes row: the change as seen
// by one asset in one role (holder / selling / buying / claimable / reserve_a /
// reserve_b / pool). Account is the entry's holder, seller or sponsor, empty
// for a liquidity pool.
type AssetEntryChange struct {
	EntryChangeRef
	Asset   string
	Role    string
	Account string
	Balance *big.Int
}

// entryHistorySourceQuery streams the window in the source's sort-key order,
// which puts each 'state' pre-image directly before the change it precedes.
// No FINAL: an unmerged duplicate re-derives a byte-identical target row, and
// both targets are ReplacingMergeTrees on the same identity.
const entryHistorySourceQuery = `
	SELECT ledger_seq, close_time, tx_hash, op_index, change_index, intra_ledger_seq,
	       change_type, entry_type, key_xdr, entry_xdr
	FROM stellar.ledger_entry_changes
	WHERE ledger_seq BETWEEN ? AND ?
	  AND entry_type IN ('account', 'trustline', 'offer', 'data', 'claimable_balance', 'liquidity_pool')
	ORDER BY ledger_seq, tx_hash, op_index, change_index
`

// StreamEntryHistorySource calls fn for every classic entry change in
// [from, to], in (ledger_seq, tx_hash, op_index, change_index) order.
func StreamEntryHistorySource(ctx context.Context, addr string, from, to uint32, fn func(EntryHistorySourceRow) error) error {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	rows, err := conn.Query(ctx, entryHistorySourceQuery, from, to)
	if err != nil {
		return fmt.Errorf("clickhouse: query entry-history source [%d,%d]: %w", from, to, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var r EntryHistorySourceRow
		if err := rows.Scan(&r.Ledger, &r.CloseTime, &r.TxHash, &r.OpIndex, &r.ChangeIndex, &r.IntraLedgerSeq,
			&r.ChangeType, &r.EntryType, &r.KeyXDR, &r.EntryXDR); err != nil {
			return fmt.Errorf("clickhouse: scan entry-history source: %w", err)
		}
		r.CloseTime = r.CloseTime.UTC()
		if err := fn(r); err != nil {
			return err
		}
	}
	return rows.Err()
}

// entryHistoryInsertChunk bounds one native INSERT batch.
const entryHistoryInsertChunk = 20_000

// InsertEntryHistory writes one batch of both projections. Retry-safe: both
// tables are ReplacingMergeTrees keyed on the source row's identity plus the
// account/asset and role, so a re-sent batch collapses on merge.
func InsertEntryHistory(ctx context.Context, addr string, accounts []AccountEntryChange, assets []AssetEntryChange) (int64, error) {
	if len(accounts) == 0 && len(assets) == 0 {
		return 0, nil
	}
	conn, err := openAccountMovementsWrite(ctx, addr)
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Close() }()

	var written int64
	for i := 0; i < len(accounts); i += entryHistoryInsertChunk {
		end := min(i+entryHistoryInsertChunk, len(accounts))
		if err := insertAccountEntryChanges(ctx, conn, accounts[i:end]); err != nil {
			return written, err
		}
		written += int64(end - i)
	}
	for i := 0; i < len(assets); i += entryHistoryInsertChunk {
		end := min(i+entryHistoryInsertChunk, len(assets))
		if err := insertAssetEntryChanges(ctx, conn, assets[i:end]); err != nil {
			return written, err
		}
		written += int64(end - i)
	}
	return written, nil
}

func insertAccountEntryChanges(ctx context.Context, conn driver.Conn, rows []AccountEntryChange) error {
	batch, err := conn.PrepareBatch(ctx, `INSERT INTO stellar.account_entry_changes
		(account, ledger, close_time, tx_hash, op_index, change_index, role, intra_ledger_seq,
		 entry_type, change_type, changed, asset, balance, fields)`)
	if err != nil {
		return fmt.Errorf("prepare account_entry_changes batch: %w", err)
	}
	for _, r := range rows {
		if err := batch.Append(r.Account, r.Ledger, r.CloseTime, r.TxHash, r.OpIndex, r.ChangeIndex, r.Role,
			r.IntraLedgerSeq, r.EntryType, r.ChangeType, nonNilStrings(r.Changed), r.Asset, nonNilAmount(r.Balance), r.Fields); err != nil {
			return fmt.Errorf("append account_entry_changes %s/%d/%s/%d/%d: %w", r.Account, r.Ledger, r.TxHash, r.OpIndex, r.ChangeIndex, err)
		}
	}
	return wrapSend(batch.Send(), "account_entry_changes")
}

func insertAssetEntryChanges(ctx context.Context, conn driver.Conn, rows []AssetEntryChange) error {
	batch, err := conn.PrepareBatch(ctx, `INSERT INTO stellar.asset_entry_changes
		(asset, ledger, close_time, tx_hash, op_index, change_index, role, intra_ledger_seq,
		 entry_type, change_type, changed, account, balance, fields)`)
	if err != nil {
		return fmt.Errorf("prepare asset_entry_changes batch: %w", err)
	}
	for _, r := range rows {
		if err := batch.Append(r.Asset, r.Ledger, r.CloseTime, r.TxHash, r.OpIndex, r.ChangeIndex, r.Role,
			r.IntraLedgerSeq, r.EntryType, r.ChangeType, nonNilStrings(r.Changed), r.Account, nonNilAmount(r.Balance), r.Fields); err != nil {
			return fmt.Errorf("append asset_entry_changes %s/%d/%s/%d/%d: %w", r.Asset, r.Ledger, r.TxHash, r.OpIndex, r.ChangeIndex, err)
		}
	}
	return wrapSend(batch.Send(), "asset_entry_changes")
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func nonNilAmount(a *big.Int) *big.Int {
	if a == nil {
		return big.NewInt(0)
	}
	return a
}

// ErrEntryHistoryHole reports a refused watermark advance over a window the
// lake does not hold in full: delay, not failure — the next run re-derives it.
var ErrEntryHistoryHole = errors.New("clickhouse: entry-history window is not contiguous in the lake")

// ErrEntryHistorySkippedPrefix reports a refused advance whose window starts
// above watermark+1, which would claim never-derived ledgers as done.
var ErrEntryHistorySkippedPrefix = errors.New("clickhouse: entry-history window skips ledgers below it")

// ErrEntryHistoryEntryChangeShortfall reports a refused advance over a window
// in which the derive read transaction-scoped entry changes for fewer ledgers
// than stellar.ledgers declares tx-bearing: it would record the rest as having
// no history.
var ErrEntryHistoryEntryChangeShortfall = errors.New("clickhouse: entry-history window lacks ledger_entry_changes coverage")

// entryHistoryWindowCovered compares the tx-bearing ledgers the derive READ
// (non-empty tx_hash; snapshot seed rows carry none) with stellar.ledgers. A
// lake-state check after the read would pass once a concurrent ch-backfill
// filled the window, though the derive saw none of those rows. Only
// tx-bearing ledgers hold tx-scoped rows, so the counts compare as sets.
func entryHistoryWindowCovered(ctx context.Context, conn driver.Conn, from, thru uint32, readTxLedgers uint64) error {
	const q = `SELECT uniqExact(ledger_seq) FROM stellar.ledgers WHERE ledger_seq BETWEEN ? AND ? AND tx_count > 0`
	cov := ECWindowCoverage{From: from, To: thru, ECCoveredTxLedgers: readTxLedgers}
	if err := conn.QueryRow(ctx, q, from, thru).Scan(&cov.TxLedgers); err != nil {
		return fmt.Errorf("clickhouse: entry-history window [%d,%d] tx-bearing ledgers: %w", from, thru, err)
	}
	if missing := cov.Missing(); missing > 0 {
		return fmt.Errorf("%w: [%d,%d] read entry changes for %d of %d tx-bearing ledgers",
			ErrEntryHistoryEntryChangeShortfall, from, thru, readTxLedgers, cov.TxLedgers)
	}
	return nil
}

// EntryHistoryWatermark returns the highest ledger ch-entry-history has
// derived through (0 = never run, or the watermark table is not provisioned).
func EntryHistoryWatermark(ctx context.Context, addr string) (uint32, error) {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Close() }()
	return entryHistoryWatermarkOn(ctx, conn)
}

func entryHistoryWatermarkOn(ctx context.Context, conn driver.Conn) (uint32, error) {
	// max() collapses unmerged duplicates; the watermark only ever advances.
	const q = `SELECT max(thru_ledger) FROM stellar.entry_history_watermark WHERE name = 'entry_history'`
	var wm uint32
	if err := conn.QueryRow(ctx, q).Scan(&wm); err != nil {
		if isSchemaAbsent(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("clickhouse: entry-history watermark: %w", err)
	}
	return wm, nil
}

// SetEntryHistoryWatermark records completion through `thru` for the derive
// window [from, thru], only when the window continues the derived prefix, the
// lake holds every ledger in it and the derive read transaction-scoped entry
// changes for readTxLedgers distinct ledgers, covering every tx-bearing one:
// the derive resumes at watermark+1, so an unproven advance would lose those
// ledgers' history for good. A window at or
// below the watermark is an idempotent re-derive and records nothing.
func SetEntryHistoryWatermark(ctx context.Context, addr string, from, thru uint32, readTxLedgers uint64) error {
	if from == 0 || thru < from {
		return fmt.Errorf("clickhouse: entry-history watermark window [%d,%d] is not a ledger range (genesis is ledger 1)", from, thru)
	}
	conn, err := openRead(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	wm, err := entryHistoryWatermarkOn(ctx, conn)
	if err != nil {
		return err
	}
	if thru <= wm {
		return nil
	}
	if wm > 0 && from > wm+1 {
		return fmt.Errorf("%w: window [%d,%d] starts above watermark+1 (%d)", ErrEntryHistorySkippedPrefix, from, thru, wm+1)
	}
	present, err := windowLedgersPresent(ctx, conn, from, thru)
	if err != nil {
		return fmt.Errorf("clickhouse: entry-history window [%d,%d] contiguity: %w", from, thru, err)
	}
	if want := uint64(thru-from) + 1; present != want {
		return fmt.Errorf("%w: [%d,%d] holds %d of %d ledgers", ErrEntryHistoryHole, from, thru, present, want)
	}
	if err := entryHistoryWindowCovered(ctx, conn, from, thru, readTxLedgers); err != nil {
		return err
	}
	const q = `INSERT INTO stellar.entry_history_watermark (name, thru_ledger) VALUES ('entry_history', ?)`
	if err := conn.Exec(ctx, q, thru); err != nil {
		return fmt.Errorf("clickhouse: set entry-history watermark %d: %w", thru, err)
	}
	return nil
}
