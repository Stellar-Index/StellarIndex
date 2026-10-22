package clickhouse

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"
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

// SignerIndexWatermark is the ledger the entry index's signer role is complete through (0 = not yet built from the
// lake floor); the signer-on route answers 503 below it.
func (r *ExplorerReader) SignerIndexWatermark(ctx context.Context) (uint32, error) {
	return entryWatermarkOn(ctx, r.conn, signerIndexWatermarkName)
}

// ClaimableIndexWatermark is the ledger the claimable index is complete through (0 = not yet built from the lake
// floor); below it the claimable-balances route keeps its ledger scan.
func (r *ExplorerReader) ClaimableIndexWatermark(ctx context.Context) (uint32, error) {
	return entryWatermarkOn(ctx, r.conn, claimableIndexWatermarkName)
}

// AccountEntryLedgers lists, newest first, the ledgers at or below maxLedger in which the account (raw 32-byte key)
// had an entry change in one of the roles; an absent index table reads as no ledgers.
func (r *ExplorerReader) AccountEntryLedgers(ctx context.Context, account string, roles []string, maxLedger uint32, limit int) ([]uint32, error) {
	return r.entryLedgers(ctx, "account_entry_ledgers", "account", "role", account, roles, maxLedger, limit)
}

// AssetEntryLedgers lists, newest first, the ledgers at or below maxLedger in which an entry of one of the types held
// the asset changed.
func (r *ExplorerReader) AssetEntryLedgers(ctx context.Context, asset string, entryTypes []string, maxLedger uint32, limit int) ([]uint32, error) {
	return r.entryLedgers(ctx, "asset_entry_ledgers", "asset", "entry_type", asset, entryTypes, maxLedger, limit)
}

// entryLedgers reads each (key, kind) prefix of the index newest-first on its own and merges them: an IN-list over the second
// ORDER BY column cannot read in order, so it sorted every row the key has (32.8M rows for one busy asset, against 0.95M).
func (r *ExplorerReader) entryLedgers(ctx context.Context, table, keyCol, kindCol, key string, kinds []string, maxLedger uint32, limit int) ([]uint32, error) {
	if len(kinds) == 0 || limit <= 0 {
		return nil, nil
	}
	arms := make([]string, len(kinds))
	args := make([]any, 0, 4*len(kinds)+1)
	for i, kind := range kinds {
		arms[i] = `SELECT ledger FROM stellar.` + table + ` WHERE ` + keyCol + ` = ? AND ` + kindCol + ` = ? AND ledger <= ?
			ORDER BY ledger DESC LIMIT ?`
		args = append(args, key, kind, maxLedger, limit)
	}
	q := `SELECT DISTINCT ledger FROM (` + strings.Join(arms, " UNION ALL ") + `) ORDER BY ledger DESC LIMIT ?` + explorerScanSettings
	rows, err := r.conn.Query(ctx, q, append(args, limit)...)
	if err != nil {
		if absentReadsEmpty(table, err) {
			return nil, nil
		}
		return nil, fmt.Errorf("clickhouse: %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]uint32, 0, limit)
	for rows.Next() {
		var l uint32
		if err := rows.Scan(&l); err != nil {
			return nil, fmt.Errorf("clickhouse: %s scan: %w", table, err)
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
	return r.streamEntryChanges(ctx, ledgers, fn, entryChangesByLedgerQuery, ledgers)
}

// accountEntryChangesByLedgerQuery keeps only the (op, entry key) groups in which some row carries the account's raw
// key: a row's projected parties come from its key, its entry and its group's state pre-image, so the projection
// for the account is unchanged while the busiest accounts stream about a tenth of the rows.
const accountEntryChangesByLedgerQuery = `SELECT ledger_seq, close_time, tx_hash, op_index, change_index, intra_ledger_seq,
		change_type, entry_type, key_xdr, entry_xdr
	FROM stellar.ledger_entry_changes
	WHERE ledger_seq IN (?)
	  AND entry_type IN ('account', 'trustline', 'offer', 'data', 'claimable_balance', 'liquidity_pool')
	  AND (ledger_seq, tx_hash, op_index, key_xdr) IN (
		SELECT ledger_seq, tx_hash, op_index, key_xdr FROM stellar.ledger_entry_changes
		WHERE ledger_seq IN (?)
		  AND entry_type IN ('account', 'trustline', 'offer', 'data', 'claimable_balance', 'liquidity_pool')
		  AND (position(tryBase64Decode(key_xdr), ?) > 0 OR position(tryBase64Decode(entry_xdr), ?) > 0))
	ORDER BY ledger_seq, tx_hash, op_index, change_index` + explorerScanSettings

// StreamAccountEntryChangesForLedgers streams, in the projector's order, the listed ledgers' source rows that can
// project to the account (raw 32-byte key).
func (r *ExplorerReader) StreamAccountEntryChangesForLedgers(ctx context.Context, account string, ledgers []uint32, fn func(EntryHistorySourceRow) error) error {
	return r.streamEntryChanges(ctx, ledgers, fn, accountEntryChangesByLedgerQuery, ledgers, ledgers, account, account)
}

func (r *ExplorerReader) streamEntryChanges(ctx context.Context, ledgers []uint32, fn func(EntryHistorySourceRow) error, q string, args ...any) error {
	if len(ledgers) == 0 {
		return nil
	}
	rows, err := r.conn.Query(ctx, q, args...)
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
		if absentReadsEmpty("asset_entry_ledgers", err) {
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

// LIMIT 1 BY takes each balance's newest version in place of FINAL. Removals are looked up by key in the account's own
// key set, so neither table is scanned beyond the account.
const indexedClaimablesQuery = `SELECT ledger, close_time, entry_xdr FROM stellar.account_claimables
	WHERE account = ? AND key_xdr NOT IN (
		SELECT key_xdr FROM stellar.claimable_removals
		WHERE key_xdr IN (SELECT key_xdr FROM stellar.account_claimables WHERE account = ?))
	ORDER BY ledger DESC LIMIT 1 BY key_xdr LIMIT ?` + explorerScanSettings

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
			ledger uint32
			at     time.Time
			b64    string
		)
		if err := rows.Scan(&ledger, &at, &b64); err != nil {
			return scan, fmt.Errorf("clickhouse: scan live claimable balance: %w", err)
		}
		resolved++
		if b64 == "" {
			continue
		}
		c, err := decodeLiveClaimable(ledger, at, b64)
		if err != nil {
			return scan, err
		}
		scan.Live = append(scan.Live, c)
	}
	scan.Unresolved = scan.Keys - resolved
	return scan, rows.Err()
}

// IndexedClaimableBalances returns, newest first, at most maxKeys unclaimed balances naming the raw account as a
// claimant, read from the claimable index; Keys counts them, so maxKeys means capped.
func (r *ExplorerReader) IndexedClaimableBalances(ctx context.Context, account string, maxKeys int) (ClaimableScan, error) {
	var scan ClaimableScan
	rows, err := r.conn.Query(ctx, indexedClaimablesQuery, account, account, maxKeys)
	if err != nil {
		return scan, fmt.Errorf("clickhouse: indexed claimable balances: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			ledger uint32
			at     time.Time
			b64    string
		)
		if err := rows.Scan(&ledger, &at, &b64); err != nil {
			return scan, fmt.Errorf("clickhouse: scan indexed claimable balance: %w", err)
		}
		c, err := decodeLiveClaimable(ledger, at, b64)
		if err != nil {
			return scan, err
		}
		scan.Live = append(scan.Live, c)
	}
	scan.Keys = len(scan.Live)
	return scan, rows.Err()
}

func decodeLiveClaimable(ledger uint32, at time.Time, b64 string) (LiveClaimableBalance, error) {
	c := LiveClaimableBalance{Ledger: ledger, CloseTime: at.UTC()}
	var e xdr.LedgerEntry
	if err := xdr.SafeUnmarshalBase64(b64, &e); err != nil {
		return c, fmt.Errorf("clickhouse: decode claimable balance at ledger %d: %w", ledger, err)
	}
	cb, ok := e.Data.GetClaimableBalance()
	if !ok {
		return c, fmt.Errorf("clickhouse: claimable_balance row at ledger %d holds a %s entry", ledger, e.Data.Type)
	}
	for _, cl := range cb.Claimants {
		if v0, ok := cl.GetV0(); ok {
			c.Claimants = append(c.Claimants, v0.Destination.Address())
		}
	}
	c.Fields, _ = xdrjson.LedgerEntryFields(e)
	return c, nil
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

// SignedAccount is one account whose current signer list holds a key.
type SignedAccount struct {
	Account string
	Weight  uint32
	Ledger  uint32
}

// SignerScan is one capped read of the accounts a key signs for.
type SignerScan struct {
	Signed []SignedAccount
	// Candidates is how many accounts the change rows named (maxAccounts = capped).
	Candidates int
}

const signerCandidatesQuery = `SELECT account_id FROM stellar.ledger_entry_changes
	WHERE ledger_seq IN (?) AND entry_type = 'account' AND entry_xdr != '' AND account_id != ?
	  AND position(tryBase64Decode(entry_xdr), ?) > 0
	GROUP BY account_id ORDER BY max(ledger_seq) DESC LIMIT ?` + explorerScanSettings

const signerCurrentQuery = `SELECT w.1, w.2 FROM (
	SELECT key_xdr, argMax((ledger_seq, entry_xdr), version) AS w
	FROM stellar.ledger_entries_current
	WHERE entry_type = 'account' AND key_xdr IN (?)
	GROUP BY key_xdr)
	ORDER BY w.1 DESC` + explorerScanSettings

// AccountsSignedBy returns, newest change first, the accounts changed in one of the ledgers (at most maxAccounts of
// them) whose current signer list holds the raw ed25519 key; a merged account has an empty current entry and drops out.
func (r *ExplorerReader) AccountsSignedBy(ctx context.Context, key string, ledgers []uint32, maxAccounts int) (SignerScan, error) {
	var scan SignerScan
	if len(ledgers) == 0 || len(key) != 32 {
		return scan, nil
	}
	self, err := strkey.Encode(strkey.VersionByteAccountID, []byte(key))
	if err != nil {
		return scan, fmt.Errorf("clickhouse: encode signer key: %w", err)
	}
	candidates, err := r.signerCandidates(ctx, self, key, ledgers, maxAccounts)
	if err != nil || len(candidates) == 0 {
		return scan, err
	}
	scan.Candidates = len(candidates)
	_, keys := accountLedgerKeys(candidates)
	if len(keys) == 0 {
		return scan, nil
	}
	rows, err := r.conn.Query(ctx, signerCurrentQuery, keys)
	if err != nil {
		return scan, fmt.Errorf("clickhouse: current state of %d signer candidates: %w", len(candidates), err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			ledger uint32
			b64    string
		)
		if err := rows.Scan(&ledger, &b64); err != nil {
			return scan, fmt.Errorf("clickhouse: scan signer candidate: %w", err)
		}
		signed, ok, err := signedAccountIn(b64, key, ledger)
		if err != nil {
			return scan, err
		}
		if ok {
			scan.Signed = append(scan.Signed, signed)
		}
	}
	return scan, rows.Err()
}

// signedAccountIn decodes one current account entry and reports the raw ed25519 key's signer weight on it; an empty entry is a merged account.
func signedAccountIn(b64, key string, ledger uint32) (SignedAccount, bool, error) {
	if b64 == "" {
		return SignedAccount{}, false, nil
	}
	var e xdr.LedgerEntry
	if err := xdr.SafeUnmarshalBase64(b64, &e); err != nil {
		return SignedAccount{}, false, fmt.Errorf("clickhouse: decode account entry at ledger %d: %w", ledger, err)
	}
	acc, ok := e.Data.GetAccount()
	if !ok {
		return SignedAccount{}, false, fmt.Errorf("clickhouse: account row at ledger %d holds a %s entry", ledger, e.Data.Type)
	}
	for _, sg := range acc.Signers {
		if k, ok := sg.Key.GetEd25519(); ok && string(k[:]) == key {
			return SignedAccount{Account: acc.AccountId.Address(), Weight: uint32(sg.Weight), Ledger: ledger}, true, nil
		}
	}
	return SignedAccount{}, false, nil
}

func (r *ExplorerReader) signerCandidates(ctx context.Context, self, key string, ledgers []uint32, maxAccounts int) ([]string, error) {
	rows, err := r.conn.Query(ctx, signerCandidatesQuery, ledgers, self, key, maxAccounts)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: signer candidates in %d ledgers: %w", len(ledgers), err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, fmt.Errorf("clickhouse: scan signer candidate account: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
