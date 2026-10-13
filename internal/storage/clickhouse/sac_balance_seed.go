package clickhouse

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/scval"

	"github.com/stellar/go-stellar-sdk/strkey"
)

// SACBalanceSeed is one current SAC / SEP-41 Balance(Address) entry from the current-state projection, seeding sac_balance_observations so dormant contract-held balances do not drop out of Algorithm-2 supply.
type SACBalanceSeed struct {
	ContractID string    // SAC-wrapper contract C-strkey
	AssetKey   string    // operator-mapped classic asset_key (CODE:ISSUER)
	Holder     string    // balance owner strkey (G… / C… / …)
	Balance    *big.Int  // current balance, stroops (i128 — never truncated, ADR-0003); zero when IsRemoval
	LedgerSeq  uint32    // the entry's last-modified ledger; for a tombstone, the removal ledger
	CloseTime  time.Time // close time of that ledger (UTC)

	// IsRemoval marks a tombstone: the entry was removed. Same meaning as the live observer's
	// Observation.IsRemoval, so a served row retracts identically. An archived (TTL-lapsed)
	// persistent entry is NOT a tombstone: it is restorable and stays served, as the lake keeps it.
	IsRemoval bool
}

// StreamSACBalanceSeeds invokes fn once per current Balance(Address) entry of a WATCHED SAC wrapper (tombstones for removed entries, archived ones stay live, corrupt XDR is a hard error); the FINAL scan is heavy, run under run-heavy-job.sh.
func StreamSACBalanceSeeds(ctx context.Context, addr string, watched map[string]string, fn func(SACBalanceSeed) error) error {
	if len(watched) == 0 {
		return errors.New("clickhouse: StreamSACBalanceSeeds: empty watched SAC-wrapper set")
	}
	// Heavy-FINAL streaming read class (openRead): no max_execution_time, so the
	// full-range scan is not aborted mid-stream.
	conn, err := openRead(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	const q = `SELECT key_xdr, entry_xdr, change_type, ledger_seq, close_time
		FROM stellar.ledger_entries_current FINAL
		WHERE entry_type = 'contract_data'`
	rows, err := conn.Query(ctx, q)
	if err != nil {
		return fmt.Errorf("clickhouse: scan contract_data current-state: %w", err)
	}
	defer func() { _ = rows.Close() }()

	return streamCurrentStateSeeds(rows, watched, fn)
}

// streamCurrentStateSeeds emits each watched Balance row as it is scanned.
func streamCurrentStateSeeds(rows driver.Rows, watched map[string]string, fn func(SACBalanceSeed) error) error {
	for rows.Next() {
		var (
			keyXDR, entryXDR, changeType string
			ledgerSeq                    uint32
			closeTime                    time.Time
		)
		if err := rows.Scan(&keyXDR, &entryXDR, &changeType, &ledgerSeq, &closeTime); err != nil {
			return fmt.Errorf("clickhouse: scan contract_data row: %w", err)
		}
		seed, matched, err := sacBalanceSeedFromRow(keyXDR, entryXDR, changeType, ledgerSeq, closeTime, watched)
		if err != nil {
			return err
		}
		if !matched {
			continue
		}
		if err := fn(seed); err != nil {
			return err
		}
	}
	return rows.Err()
}

// StreamSACBalanceSeedsFullHistory is StreamSACBalanceSeeds over the raw ledger_entry_changes log, so dormant Balance entries written before the MV existed are seen; heavy (windowed by ledger), run under run-heavy-job.sh, never scheduled.
func StreamSACBalanceSeedsFullHistory(ctx context.Context, addr string, watched map[string]string, walk SeedWalk, fn func(SACBalanceSeed) error) (SeedEvidence, error) {
	if len(watched) == 0 {
		return SeedEvidence{}, errors.New("clickhouse: StreamSACBalanceSeedsFullHistory: empty watched SAC-wrapper set")
	}
	conn, err := openRead(ctx, addr)
	if err != nil {
		return SeedEvidence{}, err
	}
	defer func() { _ = conn.Close() }()

	needles, err := sacWatchedContractNeedles(watched)
	if err != nil {
		return SeedEvidence{}, err
	}
	ev, err := resolveSeedWalk(ctx, conn, addr, walk)
	if err != nil {
		return SeedEvidence{}, err
	}

	// Windows are walked ascending and never overlap, so per-window plus Go reduction
	// equals the single unbounded GROUP BY.
	red := newSACSeedReducer(watched)
	err = walkSACSeedWindows(ev.FromLedger, ev.ToLedger, walk.progressScan(func(from, to uint32) error {
		return scanSACSeedWindow(ctx, conn, needles, from, to, red)
	}))
	if err != nil {
		return SeedEvidence{}, err
	}
	return ev, red.emit(fn)
}

// walkSACSeedWindows walks the ledger windows. A memory-limit error bisects and
// retries the same start rather than raising the ceiling; sustained success widens back.
func walkSACSeedWindows(minLedger, maxLedger uint32, scan func(from, to uint32) error) error {
	win := newAdaptiveLedgerWindow(sacSeedLedgerWindow, sacSeedMinLedgerWindow, sacSeedWidenAfter)
	return walkLedgerWindows(minLedger, maxLedger, win, scan)
}

const (
	// sacSeedLedgerWindow is one scan step: it divides the PARTITION BY intDiv(ledger_seq, 1000000) evenly, so a window is a primary-key
	// range within one partition. 250,000 was measured: 1.5-1.8 GiB peak in the densest stretches vs died at 1,000,000.
	sacSeedLedgerWindow = 250_000
	// sacSeedMinLedgerWindow is the bisection floor (250k >> 4); if a window this small
	// does not fit, the size is not the problem and the error should surface.
	sacSeedMinLedgerWindow = sacSeedLedgerWindow >> 4
	// sacSeedWidenAfter: consecutive clean windows before doubling back up (capped at sacSeedLedgerWindow).
	sacSeedWidenAfter = 4

	// chMemoryLimitExceeded is ClickHouse's MEMORY_LIMIT_EXCEEDED.
	chMemoryLimitExceeded = 241
)

// sacWatchedContractNeedles renders the watched set as raw-byte literals for the multiSearchAny prefilter pushed into the SQL: the table
// has no contract_id column, and reducing over every contract_data key exceeded the query budget.
func sacWatchedContractNeedles(watched map[string]string) ([]string, error) {
	needles := make([]string, 0, len(watched))
	for strk := range watched {
		raw, err := strkey.Decode(strkey.VersionByteContract, strk)
		if err != nil {
			return nil, fmt.Errorf("clickhouse: full-history seed: decode watched strkey %s: %w", strk, err)
		}
		needles = append(needles, "unhex('"+hex.EncodeToString(raw)+"')")
	}
	sort.Strings(needles) // deterministic SQL for tests/logs
	return needles, nil
}

// entryChangeLedgerBounds reads the append-log's [min, max] ledger. An aggregate over the table scans every part (tens of
// billions of rows) and blows the TVL refresh deadline; reading in primary-key order with LIMIT 1 from each end touches one
// granule. Not floored at a Soroban-activation constant (integration fixtures write below it). NOT hole-safe: a reader that
// persists a cursor must bound itself with ledgerContiguityFrom.
func entryChangeLedgerBounds(ctx context.Context, conn driver.Conn) (uint32, uint32, error) {
	var lo, hi uint32
	const q = `SELECT min(ledger_seq), max(ledger_seq) FROM (
		SELECT ledger_seq FROM stellar.ledger_entry_changes ORDER BY ledger_seq ASC LIMIT 1
		UNION ALL
		SELECT ledger_seq FROM stellar.ledger_entry_changes ORDER BY ledger_seq DESC LIMIT 1)`
	if err := conn.QueryRow(ctx, q).Scan(&lo, &hi); err != nil {
		return 0, 0, fmt.Errorf("clickhouse: read ledger_entry_changes ledger bounds: %w", err)
	}
	return lo, hi, nil
}

// ledgerContiguity is the lake's completeness picture at and above one ledger, read off stellar.ledgers (the commit marker Sink.Flush
// writes last; see ContiguousWatermark). Zero means "none" in every field.
type ledgerContiguity struct {
	lakeMax    uint32 // highest ledger present anywhere in the lake
	firstGap   uint32 // lowest MISSING ledger between two present ledgers >= from
	minPresent uint32 // lowest PRESENT ledger >= from (> from ⟹ from itself is a hole)
}

// ledgerContiguityFrom is ContiguousWatermark's read on a caller-owned connection, with the same SQL and normalisation. The DISTINCT scan
// is bounded below by `from`: pass a ledger near the tip, never the lake floor (the sort exceeds the CH memory cap).
func ledgerContiguityFrom(ctx context.Context, conn driver.Conn, from uint32) (ledgerContiguity, error) {
	const q = `
		SELECT
			toUInt64(ifNull((SELECT max(ledger_seq) FROM stellar.ledgers), 0)) AS ch_max,
			toUInt64(ifNull((SELECT min(gap_start) FROM (
				SELECT ledger_seq + 1 AS gap_start
				FROM (
					SELECT ledger_seq,
					       leadInFrame(ledger_seq) OVER (
					           ORDER BY ledger_seq ROWS BETWEEN CURRENT ROW AND 1 FOLLOWING
					       ) AS nxt
					FROM (SELECT DISTINCT ledger_seq FROM stellar.ledgers WHERE ledger_seq >= ?)
				)
				WHERE nxt > ledger_seq + 1
			)), 0)) AS first_gap_start,
			toUInt64(ifNull((SELECT min(ledger_seq) FROM stellar.ledgers WHERE ledger_seq >= ?), 0)) AS min_present`
	var chMax, firstGap, minPresent uint64
	if err := conn.QueryRow(ctx, q, from, from).Scan(&chMax, &firstGap, &minPresent); err != nil {
		return ledgerContiguity{}, fmt.Errorf("clickhouse: ledger contiguity from %d: %w", from, err)
	}
	// Ledger sequences are always well within uint32.
	return ledgerContiguity{lakeMax: uint32(chMax), firstGap: uint32(firstGap), minPresent: uint32(minPresent)}, nil
}

// scanSACSeedWindow reduces one window server-side to at most one row per storage key and offers each to red. One argMax over a TUPLE of
// every projected column keyed on the full within-ledger identity: per-column argMax(col, ledger_seq) could mix a present entry_xdr with
// a later 'removed' change_type and resurrect a deleted balance. Output aliases must not shadow source columns (ILLEGAL_AGGREGATION),
// hence win_ prefixes. The memory ceiling is deliberately below 8 GB so a window that does not fit bisects rather than starving the host.
// GROUP-BY SPILL IS OFF: its threshold tracks the whole query, dominated by the wide entry_xdr, so it flushed near-empty tables. PREWHERE
// on entry_type, as in claimable_balance_seed.go: WHERE does not stop ClickHouse running base64Decode on other entry types' key_xdr
// first.
func scanSACSeedWindow(ctx context.Context, conn driver.Conn, needles []string, from, to uint32, red *sacSeedReducer) error {
	q := `SELECT key_xdr,
		       tupleElement(win, 1) AS win_ledger_seq,
		       tupleElement(win, 2) AS win_intra_ledger_seq,
		       tupleElement(win, 3) AS win_tx_hash,
		       tupleElement(win, 4) AS win_op_index,
		       tupleElement(win, 5) AS win_change_index,
		       tupleElement(win, 6) AS win_entry_xdr,
		       tupleElement(win, 7) AS win_change_type,
		       tupleElement(win, 8) AS win_close_time
		FROM (
		    SELECT key_xdr,
		           argMax((ledger_seq, intra_ledger_seq, tx_hash, op_index, change_index,
		                   entry_xdr, toString(change_type), close_time),
		                  (ledger_seq, intra_ledger_seq, tx_hash, op_index, change_index)) AS win
		    FROM stellar.ledger_entry_changes
		    PREWHERE entry_type = 'contract_data'
		    WHERE ledger_seq BETWEEN ? AND ?
		      AND multiSearchAny(base64Decode(key_xdr), [` + strings.Join(needles, ", ") + `])
		    GROUP BY key_xdr
		)
		SETTINGS max_memory_usage = 4000000000,
		         max_bytes_before_external_group_by = 0,
		         max_threads = 4`
	rows, err := conn.Query(ctx, q, from, to)
	if err != nil {
		return fmt.Errorf("clickhouse: scan contract_data full history [%d, %d]: %w", from, to, err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			keyXDR, txHash, entryXDR, changeType string
			ord                                  lakeEntryChangeOrder
			closeTime                            time.Time
		)
		if err := rows.Scan(&keyXDR, &ord.ledgerSeq, &ord.intraLedgerSeq, &txHash,
			&ord.opIndex, &ord.changeIndex, &entryXDR, &changeType, &closeTime); err != nil {
			return fmt.Errorf("clickhouse: scan contract_data full-history row [%d, %d]: %w", from, to, err)
		}
		ord.txHash = txHash
		if err := red.offer(keyXDR, entryXDR, changeType, closeTime, ord); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("clickhouse: stream contract_data full history [%d, %d]: %w", from, to, err)
	}
	return nil
}

// isMemoryLimitExceeded reports ClickHouse code 241, the one error the window walk
// answers by bisecting; every other exception surfaces unchanged.
func isMemoryLimitExceeded(err error) bool {
	var chErr *clickhouse.Exception
	return errors.As(err, &chErr) && chErr.Code == chMemoryLimitExceeded
}

// lakeEntryChangeOrder is the within-ledger identity tuple of one entry change (ledger_seq, intra_ledger_seq, tx_hash, op_index,
// change_index). Every windowed latest-write-wins reduction over ledger_entry_changes must share it (this seed and
// StreamClaimableBalanceSeeds), or a same-ledger removal can resurrect a deleted entry.
type lakeEntryChangeOrder struct {
	ledgerSeq      uint32
	intraLedgerSeq uint32
	txHash         string
	opIndex        int32 // -1 for fee-meta / tx-level changes
	changeIndex    uint32
}

// after reports whether a sorts strictly after b, matching ClickHouse's tuple compare:
// byte-wise tx_hash, signed op_index (the -1 fee-meta sentinel sorts below op 0 on both sides).
func (a lakeEntryChangeOrder) after(b lakeEntryChangeOrder) bool {
	switch {
	case a.ledgerSeq != b.ledgerSeq:
		return a.ledgerSeq > b.ledgerSeq
	case a.intraLedgerSeq != b.intraLedgerSeq:
		return a.intraLedgerSeq > b.intraLedgerSeq
	case a.txHash != b.txHash:
		return a.txHash > b.txHash
	case a.opIndex != b.opIndex:
		return a.opIndex > b.opIndex
	default:
		return a.changeIndex > b.changeIndex
	}
}

// sacSeedWinner is the latest change seen so far for one storage key.
type sacSeedWinner struct {
	order      lakeEntryChangeOrder
	changeType string
	entryXDR   string
	closeTime  time.Time
}

// sacSeedReducer finishes the latest-write-wins reduction across windows in Go. Memory is bounded by distinct watched Balance keys: other
// prefilter hits (allowances, third-party storage) are rejected on first sight.
type sacSeedReducer struct {
	watched map[string]string
	best    map[string]sacSeedWinner // key_xdr → latest change seen
}

func newSACSeedReducer(watched map[string]string) *sacSeedReducer {
	return &sacSeedReducer{watched: watched, best: make(map[string]sacSeedWinner)}
}

// offer folds one window-winning row into the per-key reduction; idempotent and order-independent, so a bisected retry changes nothing.
// Removals are tracked like any change so a removal in window N suppresses a live balance from window N-1.
func (r *sacSeedReducer) offer(keyXDR, entryXDR, changeType string, closeTime time.Time, ord lakeEntryChangeOrder) error {
	if prev, seen := r.best[keyXDR]; seen {
		if !ord.after(prev.order) {
			return nil
		}
	} else {
		watchedBalance, err := sacWatchedBalanceKey(keyXDR, r.watched)
		if err != nil {
			// An undecodable key on a REMOVED change identifies no holder; only a live entry's
			// corrupt key is a hard error.
			if changeType == "removed" {
				return nil
			}
			return err
		}
		if !watchedBalance {
			return nil
		}
	}
	if changeType == "removed" {
		entryXDR = "" // never read for a removal — don't pay to keep it
	}
	r.best[keyXDR] = sacSeedWinner{order: ord, changeType: changeType, entryXDR: entryXDR, closeTime: closeTime}
	return nil
}

// emit decodes each key's final winner and hands survivors to fn in ascending key_xdr order (reproducible output). Only the final winner
// is decoded: corrupt XDR on a superseded change is not an error, on a live one it is.
func (r *sacSeedReducer) emit(fn func(SACBalanceSeed) error) error {
	keys := make([]string, 0, len(r.best))
	for k := range r.best {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		w := r.best[k]
		seed, matched, err := sacBalanceSeedFromRow(k, w.entryXDR, w.changeType, w.order.ledgerSeq, w.closeTime, r.watched)
		if err != nil {
			return err
		}
		if !matched {
			continue
		}
		if err := fn(seed); err != nil {
			return err
		}
	}
	return nil
}

// sacWatchedBalanceKey reports whether keyXDR is a Balance(Address) key of a WATCHED wrapper: a key-only prefilter that keeps
// sacSeedReducer from retaining the allowance and third-party keys multiSearchAny also matches.
func sacWatchedBalanceKey(keyXDR string, watched map[string]string) (bool, error) {
	var lk xdr.LedgerKey
	if err := xdr.SafeUnmarshalBase64(keyXDR, &lk); err != nil {
		return false, fmt.Errorf("clickhouse: decode contract_data key_xdr: %w", err)
	}
	if lk.Type != xdr.LedgerEntryTypeContractData || lk.ContractData == nil {
		return false, nil // defensive: SQL already scopes to contract_data
	}
	contractID, ok := scval.ContractIDFromScAddress(lk.ContractData.Contract)
	if !ok {
		return false, nil
	}
	if _, isWatched := watched[contractID]; !isWatched {
		return false, nil
	}
	return scval.IsSEP41BalanceKey(lk.ContractData.Key), nil
}

// watchedKeyDecodeErr classifies a key_xdr that failed to decode. Only a key carrying a WATCHED contract id (same raw byte match as
// sacWatchedContractNeedles) is lake corruption; any other undecodable key is skipped so one bad row elsewhere cannot abort the seed.
func watchedKeyDecodeErr(keyXDR string, watched map[string]string, decodeErr error) error {
	raw, err := base64.StdEncoding.DecodeString(keyXDR)
	if err != nil {
		return nil //nolint:nilerr // non-base64 bytes cannot carry a watched contract id, so the row is skipped like any non-watched row
	}
	for strk := range watched {
		id, err := strkey.Decode(strkey.VersionByteContract, strk)
		if err == nil && bytes.Contains(raw, id) {
			return fmt.Errorf("clickhouse: decode contract_data key_xdr (watched contract %s): %w", strk, decodeErr)
		}
	}
	return nil
}

// sacBalanceSeedFromRow decodes one ledger_entries_current row into a SACBalanceSeed. matched=false (no error) for non-Balance keys and
// non-watched contracts; errors only for a WATCHED contract's undecodable key or value. A removed entry on a watched Balance key is a
// tombstone (IsRemoval, Balance=0).
func sacBalanceSeedFromRow(keyXDR, entryXDR, changeType string, ledgerSeq uint32, closeTime time.Time, watched map[string]string) (SACBalanceSeed, bool, error) {
	// Decode the LedgerKey first (cheap) to reject non-watched contracts and non-Balance
	// keys before touching the value-bearing entry_xdr.
	var lk xdr.LedgerKey
	if err := xdr.SafeUnmarshalBase64(keyXDR, &lk); err != nil {
		if changeType == "removed" {
			// Same as the full-history reducer's offer: no holder to retract.
			return SACBalanceSeed{}, false, nil
		}
		return SACBalanceSeed{}, false, watchedKeyDecodeErr(keyXDR, watched, err)
	}
	if lk.Type != xdr.LedgerEntryTypeContractData || lk.ContractData == nil {
		return SACBalanceSeed{}, false, nil // defensive: SQL already scopes to contract_data
	}
	contractID, ok := scval.ContractIDFromScAddress(lk.ContractData.Contract)
	if !ok {
		return SACBalanceSeed{}, false, nil
	}
	assetKey, isWatched := watched[contractID]
	if !isWatched {
		return SACBalanceSeed{}, false, nil
	}
	if !scval.IsSEP41BalanceKey(lk.ContractData.Key) {
		return SACBalanceSeed{}, false, nil
	}
	holder, err := scval.HolderFromBalanceKey(lk.ContractData.Key)
	if err != nil {
		return SACBalanceSeed{}, false, fmt.Errorf("clickhouse: sac balance holder (contract %s ledger %d): %w", contractID, ledgerSeq, err)
	}

	if changeType == "removed" {
		return SACBalanceSeed{
			ContractID: contractID,
			AssetKey:   assetKey,
			Holder:     holder,
			Balance:    big.NewInt(0),
			LedgerSeq:  ledgerSeq,
			CloseTime:  closeTime.UTC(),
			IsRemoval:  true,
		}, true, nil
	}

	// The amount lives only in entry_xdr; a non-removed row always carries it, so an
	// empty value is a lake inconsistency: skip rather than fabricate.
	if entryXDR == "" {
		return SACBalanceSeed{}, false, nil
	}
	var le xdr.LedgerEntry
	if err := xdr.SafeUnmarshalBase64(entryXDR, &le); err != nil {
		return SACBalanceSeed{}, false, fmt.Errorf("clickhouse: decode contract_data entry_xdr (contract %s holder %s ledger %d): %w", contractID, holder, ledgerSeq, err)
	}
	if le.Data.Type != xdr.LedgerEntryTypeContractData || le.Data.ContractData == nil {
		return SACBalanceSeed{}, false, fmt.Errorf("clickhouse: entry_xdr for %s/%s at ledger %d is %s, not ContractData", contractID, holder, ledgerSeq, le.Data.Type.String())
	}
	balance, err := scval.SEP41BalanceAmount(le.Data.ContractData.Val)
	if err != nil {
		return SACBalanceSeed{}, false, fmt.Errorf("clickhouse: sac balance amount (contract %s holder %s ledger %d): %w", contractID, holder, ledgerSeq, err)
	}

	return SACBalanceSeed{
		ContractID: contractID,
		AssetKey:   assetKey,
		Holder:     holder,
		Balance:    balance,
		LedgerSeq:  ledgerSeq,
		CloseTime:  closeTime.UTC(),
	}, true, nil
}
