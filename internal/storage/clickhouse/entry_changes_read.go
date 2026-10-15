package clickhouse

import (
	"context"
	"fmt"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/xdrjson"
)

// EntryHistoryWatermark is the ledger ch-entry-history has derived the sponsorship lifecycle through (0 = never
// derived, or the watermark table is absent).
func (r *ExplorerReader) EntryHistoryWatermark(ctx context.Context) (uint32, error) {
	return entryWatermarkOn(ctx, r.conn, sponsorshipWatermarkName)
}

// EntryIndexWatermark is the ledger ch-entry-index has built the account and asset entry indexes through (0 = never
// built, or the watermark table not provisioned); the entry-changes routes answer 503 below it.
func (r *ExplorerReader) EntryIndexWatermark(ctx context.Context) (uint32, error) {
	return entryWatermarkOn(ctx, r.conn, entryIndexWatermarkName)
}

// AccountEntryLedgers lists, newest first, the ledgers at or below maxLedger in which the account (raw 32-byte key)
// had an entry change in one of the roles; an absent index table reads as no ledgers.
func (r *ExplorerReader) AccountEntryLedgers(ctx context.Context, account string, roles []string, maxLedger uint32, limit int) ([]uint32, error) {
	const q = `SELECT DISTINCT ledger FROM stellar.account_entry_ledgers
		WHERE account = ? AND role IN (?) AND ledger <= ?
		ORDER BY ledger DESC LIMIT ?` + explorerScanSettings
	return r.entryLedgers(ctx, "account entry ledgers", q, account, roles, maxLedger, limit)
}

// AssetEntryLedgers lists, newest first, the ledgers at or below maxLedger in which an entry of one of the types held
// the asset changed.
func (r *ExplorerReader) AssetEntryLedgers(ctx context.Context, asset string, entryTypes []string, maxLedger uint32, limit int) ([]uint32, error) {
	const q = `SELECT DISTINCT ledger FROM stellar.asset_entry_ledgers
		WHERE asset = ? AND entry_type IN (?) AND ledger <= ?
		ORDER BY ledger DESC LIMIT ?` + explorerScanSettings
	return r.entryLedgers(ctx, "asset entry ledgers", q, asset, entryTypes, maxLedger, limit)
}

func (r *ExplorerReader) entryLedgers(ctx context.Context, label, q, key string, in []string, maxLedger uint32, limit int) ([]uint32, error) {
	if len(in) == 0 || limit <= 0 {
		return nil, nil
	}
	rows, err := r.conn.Query(ctx, q, key, in, maxLedger, limit)
	if err != nil {
		if isSchemaAbsent(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("clickhouse: %s: %w", label, err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]uint32, 0, limit)
	for rows.Next() {
		var l uint32
		if err := rows.Scan(&l); err != nil {
			return nil, fmt.Errorf("clickhouse: %s scan: %w", label, err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// entryChangesByLedgerQuery is the derive's source query over a ledger list instead of a range; the projector needs
// every tx-scoped classic row of a ledger in sort-key order to pair state pre-images with their changes.
const entryChangesByLedgerQuery = `SELECT ledger_seq, close_time, tx_hash, op_index, change_index, intra_ledger_seq,
		change_type, entry_type, key_xdr, entry_xdr
	FROM stellar.ledger_entry_changes
	WHERE ledger_seq IN (?)
	  AND entry_type IN ('account', 'trustline', 'offer', 'data', 'claimable_balance', 'liquidity_pool')
	ORDER BY ledger_seq, tx_hash, op_index, change_index` + explorerScanSettings

// StreamEntryChangesForLedgers streams the entry-history source rows of the listed ledgers in the projector's order.
func (r *ExplorerReader) StreamEntryChangesForLedgers(ctx context.Context, ledgers []uint32, fn func(EntryHistorySourceRow) error) error {
	if len(ledgers) == 0 {
		return nil
	}
	rows, err := r.conn.Query(ctx, entryChangesByLedgerQuery, ledgers)
	if err != nil {
		return fmt.Errorf("clickhouse: query entry changes for %d ledgers: %w", len(ledgers), err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var s EntryHistorySourceRow
		if err := rows.Scan(&s.Ledger, &s.CloseTime, &s.TxHash, &s.OpIndex, &s.ChangeIndex, &s.IntraLedgerSeq,
			&s.ChangeType, &s.EntryType, &s.KeyXDR, &s.EntryXDR); err != nil {
			return fmt.Errorf("clickhouse: scan entry change: %w", err)
		}
		s.CloseTime = s.CloseTime.UTC()
		if err := fn(s); err != nil {
			return err
		}
	}
	return rows.Err()
}

// AssetTrustlineDay is one UTC day of an asset's trustline activity from the entry index.
type AssetTrustlineDay struct {
	Day     time.Time
	Created uint64
	Removed uint64
}

// assetTrustlineDailyQuery buckets the asset's indexed ledgers into UTC days by ASOF-joining each ledger to its day's
// first ledger; a hash join on every ledger's close time is about 4x slower on the busiest asset. The index is a
// ReplacingMergeTree whose duplicates are identical, so any() per ledger collapses them.
const assetTrustlineDailyQuery = `WITH
	days AS (SELECT toDate(close_time) AS day, min(ledger_seq) AS first, toUInt8(0) AS k
		FROM stellar.ledgers WHERE ledger_seq <= ? GROUP BY day),
	led AS (SELECT ledger, any(n_created) AS c, any(n_removed) AS r, toUInt8(0) AS k
		FROM stellar.asset_entry_ledgers WHERE asset = ? AND entry_type = 'trustline' AND ledger <= ? GROUP BY ledger)
	SELECT days.day, toUInt64(sum(led.c)), toUInt64(sum(led.r))
	FROM led ASOF INNER JOIN days ON led.k = days.k AND led.ledger >= days.first
	GROUP BY days.day ORDER BY days.day` + explorerScanSettings

// AssetTrustlineDaily returns, oldest first, each UTC day through maxLedger on which the asset's trustlines were
// created or removed; an absent index table reads as no days.
func (r *ExplorerReader) AssetTrustlineDaily(ctx context.Context, asset string, maxLedger uint32) ([]AssetTrustlineDay, error) {
	rows, err := r.conn.Query(ctx, assetTrustlineDailyQuery, maxLedger, asset, maxLedger)
	if err != nil {
		if isSchemaAbsent(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("clickhouse: asset trustline daily: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []AssetTrustlineDay
	for rows.Next() {
		var d AssetTrustlineDay
		if err := rows.Scan(&d.Day, &d.Created, &d.Removed); err != nil {
			return nil, fmt.Errorf("clickhouse: asset trustline daily scan: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// LiveClaimableBalance is a claimable balance still on the ledger, with the ledger that last modified it.
type LiveClaimableBalance struct {
	Ledger    uint32
	CloseTime time.Time
	// Claimants are the claimant destinations' strkeys; Fields is the entry as xdrjson.LedgerEntryFields decodes it.
	Claimants []string
	Fields    map[string]any
}

// One tuple argMax over the RMT version stands in for FINAL (2.6x faster on r1's 1.2B-row claimable set) and keeps
// all three columns from the winning change. Removals (empty entry_xdr) win it and are dropped in Go, after counting
// toward maxKeys. The position() prefilter only narrows: the handler re-checks the decoded claimants.
const claimableKeysQuery = `SELECT key_xdr FROM stellar.ledger_entry_changes
	WHERE ledger_seq IN (?) AND entry_type = 'claimable_balance' AND entry_xdr != ''
	  AND position(tryBase64Decode(entry_xdr), ?) > 0
	GROUP BY key_xdr ORDER BY max(ledger_seq) DESC LIMIT ?` + explorerScanSettings

const claimableCurrentQuery = `SELECT w.1, w.2, w.3 FROM (
	SELECT key_xdr, argMax((ledger_seq, close_time, entry_xdr), version) AS w
	FROM stellar.ledger_entries_current
	WHERE entry_type = 'claimable_balance' AND key_xdr IN (?)
	GROUP BY key_xdr)
	ORDER BY w.1 DESC` + explorerScanSettings

// ClaimableScan is one capped read of an account's claimable balances.
type ClaimableScan struct {
	Live []LiveClaimableBalance
	// Keys is how many balances were examined (maxKeys = capped); Unresolved is how many of them have no row at all in
	// ledger_entries_current, so their state is unknown and they are left out.
	Keys, Unresolved int
}

// LiveClaimableBalancesInLedgers returns, newest first, the claimable balances naming the raw account that changed in
// one of the ledgers (at most maxKeys of them) and are still unclaimed now.
func (r *ExplorerReader) LiveClaimableBalancesInLedgers(ctx context.Context, account string, ledgers []uint32, maxKeys int) (ClaimableScan, error) {
	var scan ClaimableScan
	if len(ledgers) == 0 {
		return scan, nil
	}
	keys, err := r.claimableKeys(ctx, account, ledgers, maxKeys)
	if err != nil || len(keys) == 0 {
		return scan, err
	}
	scan.Keys = len(keys)
	rows, err := r.conn.Query(ctx, claimableCurrentQuery, keys)
	if err != nil {
		return scan, fmt.Errorf("clickhouse: current state of %d claimable balances: %w", len(keys), err)
	}
	defer func() { _ = rows.Close() }()
	resolved := 0
	for rows.Next() {
		var (
			c   LiveClaimableBalance
			b64 string
			e   xdr.LedgerEntry
		)
		if err := rows.Scan(&c.Ledger, &c.CloseTime, &b64); err != nil {
			return scan, fmt.Errorf("clickhouse: scan live claimable balance: %w", err)
		}
		resolved++
		if b64 == "" {
			continue
		}
		if err := xdr.SafeUnmarshalBase64(b64, &e); err != nil {
			return scan, fmt.Errorf("clickhouse: decode claimable balance at ledger %d: %w", c.Ledger, err)
		}
		cb, ok := e.Data.GetClaimableBalance()
		if !ok {
			return scan, fmt.Errorf("clickhouse: claimable_balance row at ledger %d holds a %s entry", c.Ledger, e.Data.Type)
		}
		for _, cl := range cb.Claimants {
			if v0, ok := cl.GetV0(); ok {
				c.Claimants = append(c.Claimants, v0.Destination.Address())
			}
		}
		c.Fields, _ = xdrjson.LedgerEntryFields(e)
		c.CloseTime = c.CloseTime.UTC()
		scan.Live = append(scan.Live, c)
	}
	scan.Unresolved = scan.Keys - resolved
	return scan, rows.Err()
}

func (r *ExplorerReader) claimableKeys(ctx context.Context, account string, ledgers []uint32, maxKeys int) ([]string, error) {
	rows, err := r.conn.Query(ctx, claimableKeysQuery, ledgers, account, maxKeys)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: claimable balance keys in %d ledgers: %w", len(ledgers), err)
	}
	defer func() { _ = rows.Close() }()
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, fmt.Errorf("clickhouse: scan claimable balance key: %w", err)
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}
