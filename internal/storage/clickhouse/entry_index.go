package clickhouse

import (
	"context"
	"fmt"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Entry index roles: the relation that ties an account to a ledger it appeared in.
const (
	EntryIndexRoleOwner    = "owner"
	EntryIndexRoleClaimant = "claimant"
	EntryIndexRoleSponsor  = "sponsor"
)

// EntryIndexOwnerRow is one (account, ledger) pair from the lake's account_id column: the owner relation of a classic entry.
type EntryIndexOwnerRow struct {
	AccountID string // G-strkey as the lake stores it
	Ledger    uint32
}

// EntryIndexAssetRow is one ledger's trustline activity for one asset, as stellar.asset_entry_ledgers stores it.
type EntryIndexAssetRow struct {
	Asset     string
	EntryType string
	Ledger    uint32
	Created   uint32
	Updated   uint32
	Removed   uint32
}

// EntryIndexDecodeRow is an entry whose XDR the builder decodes for claimants or a sponsor.
type EntryIndexDecodeRow struct {
	Ledger    uint32
	EntryType string
	EntryXDR  string
}

// EntryIndexAccountRow is one row of stellar.account_entry_ledgers; Account holds the raw 32-byte key.
type EntryIndexAccountRow struct {
	Account string
	Role    string
	Ledger  uint32
}

// Only tx-scoped rows (non-empty tx_hash) count: seed rows carry no ledger activity.
const entryIndexOwnerQuery = `
SELECT account_id, ledger_seq
  FROM stellar.ledger_entry_changes
 WHERE ledger_seq BETWEEN ? AND ?
   AND tx_hash != ''
   AND account_id != ''
   AND entry_type IN ('account', 'trustline', 'offer', 'data')
 GROUP BY ledger_seq, account_id
 ORDER BY ledger_seq`

const entryIndexAssetQuery = `
SELECT asset, entry_type, ledger_seq,
       toUInt32(uniqExactIf((tx_hash, op_index, change_index), change_type = 'created')) AS n_created,
       toUInt32(uniqExactIf((tx_hash, op_index, change_index), change_type = 'updated')) AS n_updated,
       toUInt32(uniqExactIf((tx_hash, op_index, change_index), change_type = 'removed')) AS n_removed
  FROM stellar.ledger_entry_changes
 WHERE ledger_seq BETWEEN ? AND ?
   AND tx_hash != ''
   AND asset != ''
   AND entry_type = 'trustline'
 GROUP BY asset, entry_type, ledger_seq
 ORDER BY ledger_seq`

// A sponsored LedgerEntry ends in a fixed 48-byte tail: ext v1, sponsor present, ed25519 key type, 32 key bytes, v1 ext v0.
// Base64 groups are 4-aligned from the right, so the last 68 characters decode to at least the last 48 bytes. The predicate
// over-approximates (a data entry whose value ends that way also matches); Go re-checks SponsoringID on every decoded row.
const entryIndexDecodeQuery = `
SELECT ledger_seq, entry_type, entry_xdr
  FROM stellar.ledger_entry_changes
 WHERE ledger_seq BETWEEN ? AND ?
   AND tx_hash != ''
   AND entry_xdr != ''
   AND (entry_type = 'claimable_balance'
        OR (entry_type IN ('account', 'trustline', 'offer', 'data', 'liquidity_pool')
            AND substring(right(tryBase64Decode(right(entry_xdr, 68)), 48), 1, 12) = unhex('000000010000000100000000')
            AND substring(right(tryBase64Decode(right(entry_xdr, 68)), 48), 45, 4) = unhex('00000000')))
 ORDER BY ledger_seq`

func streamEntryIndex(ctx context.Context, addr, label, query string, from, to uint32, scan func(driver.Rows) error) error {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	rows, err := conn.Query(ctx, query, from, to)
	if err != nil {
		return fmt.Errorf("clickhouse: query entry-index %s [%d,%d]: %w", label, from, to, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// StreamEntryIndexOwners yields the distinct (account, ledger) owner pairs of one window, ascending by ledger.
func StreamEntryIndexOwners(ctx context.Context, addr string, from, to uint32, fn func(EntryIndexOwnerRow) error) error {
	return streamEntryIndex(ctx, addr, "owners", entryIndexOwnerQuery, from, to, func(rows driver.Rows) error {
		var r EntryIndexOwnerRow
		if err := rows.Scan(&r.AccountID, &r.Ledger); err != nil {
			return fmt.Errorf("clickhouse: scan entry-index owner: %w", err)
		}
		return fn(r)
	})
}

// StreamEntryIndexAssets yields per-(asset, ledger) trustline change counts of one window, ascending by ledger.
func StreamEntryIndexAssets(ctx context.Context, addr string, from, to uint32, fn func(EntryIndexAssetRow) error) error {
	return streamEntryIndex(ctx, addr, "assets", entryIndexAssetQuery, from, to, func(rows driver.Rows) error {
		var r EntryIndexAssetRow
		if err := rows.Scan(&r.Asset, &r.EntryType, &r.Ledger, &r.Created, &r.Updated, &r.Removed); err != nil {
			return fmt.Errorf("clickhouse: scan entry-index asset: %w", err)
		}
		return fn(r)
	})
}

// StreamEntryIndexDecodeRows yields the claimable-balance and sponsored-looking entries of one window, ascending by ledger.
func StreamEntryIndexDecodeRows(ctx context.Context, addr string, from, to uint32, fn func(EntryIndexDecodeRow) error) error {
	return streamEntryIndex(ctx, addr, "decode rows", entryIndexDecodeQuery, from, to, func(rows driver.Rows) error {
		var r EntryIndexDecodeRow
		if err := rows.Scan(&r.Ledger, &r.EntryType, &r.EntryXDR); err != nil {
			return fmt.Errorf("clickhouse: scan entry-index decode row: %w", err)
		}
		return fn(r)
	})
}

// entryIndexInsertChunk bounds one native INSERT batch; rows are narrow, so one part per chunk stays small.
const entryIndexInsertChunk = 500_000

// InsertEntryIndex writes one batch of both index tables; retry-safe since both ReplacingMergeTree tables collapse a re-sent batch on merge.
func InsertEntryIndex(ctx context.Context, addr string, accounts []EntryIndexAccountRow, assets []EntryIndexAssetRow) (int64, error) {
	if len(accounts) == 0 && len(assets) == 0 {
		return 0, nil
	}
	conn, err := openAccountMovementsWrite(ctx, addr)
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Close() }()

	var written int64
	for i := 0; i < len(accounts); i += entryIndexInsertChunk {
		end := min(i+entryIndexInsertChunk, len(accounts))
		if err := insertEntryIndexAccounts(ctx, conn, accounts[i:end]); err != nil {
			return written, err
		}
		written += int64(end - i)
	}
	for i := 0; i < len(assets); i += entryIndexInsertChunk {
		end := min(i+entryIndexInsertChunk, len(assets))
		if err := insertEntryIndexAssets(ctx, conn, assets[i:end]); err != nil {
			return written, err
		}
		written += int64(end - i)
	}
	return written, nil
}

func insertEntryIndexAccounts(ctx context.Context, conn driver.Conn, rows []EntryIndexAccountRow) error {
	batch, err := conn.PrepareBatch(ctx, `INSERT INTO stellar.account_entry_ledgers (account, role, ledger)`)
	if err != nil {
		return fmt.Errorf("clickhouse: prepare account_entry_ledgers batch: %w", err)
	}
	for _, r := range rows {
		if err := batch.Append(r.Account, r.Role, r.Ledger); err != nil {
			return fmt.Errorf("clickhouse: append account_entry_ledgers row: %w", err)
		}
	}
	return wrapSend(batch.Send(), "account_entry_ledgers")
}

func insertEntryIndexAssets(ctx context.Context, conn driver.Conn, rows []EntryIndexAssetRow) error {
	batch, err := conn.PrepareBatch(ctx, `INSERT INTO stellar.asset_entry_ledgers (asset, entry_type, ledger, n_created, n_updated, n_removed)`)
	if err != nil {
		return fmt.Errorf("clickhouse: prepare asset_entry_ledgers batch: %w", err)
	}
	for _, r := range rows {
		if err := batch.Append(r.Asset, r.EntryType, r.Ledger, r.Created, r.Updated, r.Removed); err != nil {
			return fmt.Errorf("clickhouse: append asset_entry_ledgers row: %w", err)
		}
	}
	return wrapSend(batch.Send(), "asset_entry_ledgers")
}

// EntryIndexWatermark returns the highest ledger ch-entry-index has built through (0 = never run, or watermark table not provisioned).
func EntryIndexWatermark(ctx context.Context, addr string) (uint32, error) {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Close() }()
	return entryWatermarkOn(ctx, conn, entryIndexWatermarkName)
}

// SetEntryIndexWatermark records completion through `thru` under the same proofs as SetEntryHistoryWatermark; a window at or below the watermark records nothing.
func SetEntryIndexWatermark(ctx context.Context, addr string, from, thru uint32, readTxLedgers uint64) error {
	return advanceEntryWatermark(ctx, addr, entryIndexWatermarkName, from, thru, readTxLedgers)
}

const entryIndexWatermarkName = "entry_index"
