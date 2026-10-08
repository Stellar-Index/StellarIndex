package clickhouse

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
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

// SACBalanceSeed is one current SAC / SEP-41 Balance(Address) entry from the
// current-state projection, shaped for seeding sac_balance_observations. The live
// observer only writes on change, so dormant contract-held balances would
// otherwise drop out of Algorithm-2 supply. This reads authoritative current state.
type SACBalanceSeed struct {
	ContractID string    // SAC-wrapper contract C-strkey
	AssetKey   string    // operator-mapped classic asset_key (CODE:ISSUER)
	Holder     string    // balance owner strkey (G… / C… / …)
	Balance    *big.Int  // current balance, stroops (i128 — never truncated, ADR-0003); zero when IsRemoval
	LedgerSeq  uint32    // the entry's last-modified ledger; for a tombstone, the removal or archival ledger
	CloseTime  time.Time // close time of that ledger (UTC)

	// IsRemoval marks a tombstone: the entry was removed or TTL-archived. Same meaning
	// as the live observer's Observation.IsRemoval, so a served row retracts identically.
	IsRemoval bool

	// keyXDR is the base64 LedgerKey, kept so TTL liveness can be resolved before emission.
	keyXDR string
}

// StreamSACBalanceSeeds invokes fn once per live Balance(Address) entry of a WATCHED SAC wrapper.
// Liveness-filtered: ledger_entries_current keeps archived (TTL-lapsed) values
// forever, so matched keys are batched and resolved through ClassifyTTLLiveness.
// The table has no contract_id column, so the watched filter runs in Go after
// decoding key_xdr; the scan covers every contract_data row (FINAL) and must run
// under run-heavy-job.sh. Removed and archived entries emit tombstones
// (IsRemoval, Balance=0). A corrupt XDR on a watched Balance entry is a hard
// error: dropping it would read as "holder holds nothing".
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

	// Liveness is judged at the lake's tip: this reader reconstructs current state.
	_, asOfLedger, err := entryChangeLedgerBounds(ctx, conn)
	if err != nil {
		return err
	}

	const q = `SELECT key_xdr, entry_xdr, change_type, ledger_seq, close_time
		FROM stellar.ledger_entries_current FINAL
		WHERE entry_type = 'contract_data'`
	rows, err := conn.Query(ctx, q)
	if err != nil {
		return fmt.Errorf("clickhouse: scan contract_data current-state: %w", err)
	}
	defer func() { _ = rows.Close() }()

	// Matched seeds are buffered in bounded batches so liveness can be resolved before
	// emission; only watched Balance keys are held, keeping memory bounded.
	return streamCurrentStateSeeds(ctx, conn, rows, watched, asOfLedger, fn)
}

// streamCurrentStateSeeds folds the scan into bounded batches, resolving liveness per batch.
func streamCurrentStateSeeds(
	ctx context.Context,
	conn driver.Conn,
	rows driver.Rows,
	watched map[string]string,
	asOfLedger uint32,
	fn func(SACBalanceSeed) error,
) error {
	pending := make([]SACBalanceSeed, 0, ttlLivenessBatchSize)
	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		if err := emitLiveSeeds(ctx, conn, pending, asOfLedger, fn); err != nil {
			return err
		}
		pending = pending[:0]
		return nil
	}

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
		seed.keyXDR = keyXDR
		pending = append(pending, seed)
		if len(pending) >= ttlLivenessBatchSize {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return flush()
}

// emitLiveSeeds resolves each buffered seed's liveness and passes it to fn,
// retracting archived ones as tombstones at their archival ledger (a row served
// while live would otherwise stay the latest observation). A key with no TTL row
// fails the pass; see resolveSACArchivals.
func emitLiveSeeds(
	ctx context.Context,
	conn driver.Conn,
	seeds []SACBalanceSeed,
	asOfLedger uint32,
	fn func(SACBalanceSeed) error,
) error {
	lastWrite := make(map[string]uint32, len(seeds))
	for _, s := range seeds {
		if !s.IsRemoval {
			lastWrite[s.keyXDR] = s.LedgerSeq
		}
	}
	archivals, err := resolveSACArchivals(ctx, conn, lastWrite, asOfLedger)
	if err != nil {
		return err
	}
	for _, s := range seeds {
		if a, archived := archivals[s.keyXDR]; archived {
			s.IsRemoval = true
			s.Balance = big.NewInt(0)
			s.LedgerSeq = a.ledger
			s.CloseTime = a.closeTime.UTC()
		}
		if err := fn(s); err != nil {
			return err
		}
	}
	return nil
}

// sacArchival is the ledger an archived entry left live state (liveUntil+1) and its close time.
type sacArchival struct {
	ledger    uint32
	closeTime time.Time
}

// resolveSACArchivals maps each key of lastWrite whose TTL lapsed before asOfLedger to its archival ledger.
// The tombstone goes at the ARCHIVAL ledger, never the last write: the seed is written
// at timescale.SeedIntraLedgerSeq, so a last-write tombstone would overwrite the real
// observation and zero the holder across [lastWrite, archival). Fails closed on a
// key with no TTL row. A live_until below the entry's last write is stale TTL data, so the key stays live.
func resolveSACArchivals(ctx context.Context, conn driver.Conn, lastWrite map[string]uint32, asOfLedger uint32) (map[string]sacArchival, error) {
	out := make(map[string]sacArchival)
	if len(lastWrite) == 0 {
		return out, nil
	}
	keys := make([]string, 0, len(lastWrite))
	for k := range lastWrite {
		keys = append(keys, k)
	}
	liveUntil, err := resolveTTLLiveUntil(ctx, conn, keys)
	if err != nil {
		return nil, err
	}
	archivedAt, err := sacArchivalLedgers(lastWrite, liveUntil, asOfLedger)
	if err != nil {
		return nil, err
	}
	ledgers := make([]uint32, 0, len(archivedAt))
	for _, at := range archivedAt {
		ledgers = append(ledgers, at)
	}
	closeTimes, err := ledgerCloseTimes(ctx, conn, ledgers)
	if err != nil {
		return nil, err
	}
	for k, at := range archivedAt {
		ct, ok := closeTimes[at]
		if !ok {
			// Archival ledgers lie below the contiguous lake tip (ADR-0034); a missing row is a
			// lake hole, and inventing an observed_at would be worse than stopping.
			return nil, fmt.Errorf("clickhouse: sac seed: archival ledger %d has no stellar.ledgers row", at)
		}
		out[k] = sacArchival{ledger: at, closeTime: ct}
	}
	return out, nil
}

// errSACSeedTTLUnresolved: a watched Balance key has no stellar.ttl_live_until row.
var errSACSeedTTLUnresolved = errors.New("clickhouse: sac seed: watched Balance entries have no stellar.ttl_live_until row")

// sacArchivalLedgers maps each lapsed key to its archival ledger (live_until+1).
// Every contract_data entry has a TTL, so a missing row means the projection does
// not cover it (e.g. skipped ttl_live_until.sql backfill). Either guess would
// misstate supply, so the seed refuses.
func sacArchivalLedgers(lastWrite, liveUntil map[string]uint32, asOfLedger uint32) (map[string]uint32, error) {
	archivedAt := make(map[string]uint32)
	var unresolved []string
	for k, written := range lastWrite {
		lu, ok := liveUntil[k]
		if !ok {
			unresolved = append(unresolved, k)
			continue
		}
		if TTLVerdictAt(lu, asOfLedger) != TTLArchived || lu < written {
			continue
		}
		archivedAt[k] = lu + 1 // lu < asOfLedger, so no overflow
	}
	if len(unresolved) > 0 {
		sort.Strings(unresolved)
		return nil, fmt.Errorf("%w: %d of %d key(s), first %s — complete the Step-2 backfill in deploy/clickhouse/ttl_live_until.sql and re-run",
			errSACSeedTTLUnresolved, len(unresolved), len(lastWrite), unresolved[0])
	}
	return archivedAt, nil
}

// ledgerCloseTimes reads close times for seqs from stellar.ledgers in ttlLivenessBatchSize chunks; absent ledgers are omitted.
func ledgerCloseTimes(ctx context.Context, conn driver.Conn, seqs []uint32) (map[uint32]time.Time, error) {
	out := make(map[uint32]time.Time, len(seqs))
	for start := 0; start < len(seqs); start += ttlLivenessBatchSize {
		end := min(start+ttlLivenessBatchSize, len(seqs))
		if err := ledgerCloseTimesBatch(ctx, conn, seqs[start:end], out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func ledgerCloseTimesBatch(ctx context.Context, conn driver.Conn, seqs []uint32, out map[uint32]time.Time) error {
	const q = `SELECT ledger_seq, any(close_time)
		FROM stellar.ledgers
		WHERE ledger_seq IN (?)
		GROUP BY ledger_seq`
	rows, err := conn.Query(ctx, q, seqs)
	if err != nil {
		return fmt.Errorf("clickhouse: ledger close times: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			seq uint32
			ct  time.Time
		)
		if err := rows.Scan(&seq, &ct); err != nil {
			return fmt.Errorf("clickhouse: ledger close times scan: %w", err)
		}
		out[seq] = ct
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("clickhouse: ledger close times stream: %w", err)
	}
	return nil
}

// StreamSACBalanceSeedsFullHistory is the raw-history counterpart to StreamSACBalanceSeeds.
// It reads the stellar.ledger_entry_changes append-log, not ledger_entries_current:
// the current-state MV only sees rows inserted after it existed, so a dormant
// Balance entry last written before that floor is invisible there while the raw
// substrate (ADR-0034) is complete. Reduces to the latest write per
// (entry_type, key_xdr): server-side within each ledger window, in Go across windows.
// Heavy: run under run-heavy-job.sh, for the small watched set only, never scheduled.
// Memory: one unbounded query's footprint grows with the span it covers (aggregate
// states plus the wide entry_xdr read), so no ceiling is ever enough. Scan in
// ledger WINDOWS (sacSeedLedgerWindow); windows are primary-key ranges. Splitting
// per contract does not help: one wrapper owns ~98% of the GROUP BY cardinality.
// The SeedEvidence returned records the reduced range and, under walk.VerifyLake,
// the ledger through which the lake was proven intact before anything was emitted.
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
	// Liveness is judged at the walk's upper bound: the seed reconstructs state as of that ledger.
	retracted, err := red.retractArchived(ctx, conn, ev.ToLedger)
	if err != nil {
		return SeedEvidence{}, err
	}
	if retracted > 0 {
		slog.InfoContext(ctx, "sac seed: retracted archived contract_data entries",
			"retracted", retracted, "distinct_keys", len(red.best), "as_of_ledger", ev.ToLedger)
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
	// sacSeedLedgerWindow is the span of one scan step. It divides the table's
	// PARTITION BY intDiv(ledger_seq, 1000000) evenly, and ledger_seq leads the ORDER
	// BY, so a window is a primary-key range within one partition. 250,000 was measured:
	// 1.5-1.8 GiB peak in the densest Soroban stretches vs died at 1,000,000.
	sacSeedLedgerWindow = 250_000
	// sacSeedMinLedgerWindow is the bisection floor (250k >> 4); if a window this small
	// does not fit, the size is not the problem and the error should surface.
	sacSeedMinLedgerWindow = sacSeedLedgerWindow >> 4
	// sacSeedWidenAfter: consecutive clean windows before doubling back up (capped at sacSeedLedgerWindow).
	sacSeedWidenAfter = 4

	// chMemoryLimitExceeded is ClickHouse's MEMORY_LIMIT_EXCEEDED.
	chMemoryLimitExceeded = 241
)

// sacWatchedContractNeedles renders the watched set as raw-byte literals for the
// multiSearchAny prefilter pushed into the SQL: the table has no contract_id
// column, and reducing over every contract_data key exceeded the query budget.
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

// entryChangeLedgerBounds reads the append-log's [min, max] ledger from part metadata (ledger_seq leads ORDER BY).
// Not floored at a Soroban-activation constant: integration fixtures write contract_data below it.
// NOT hole-safe: max(ledger_seq) says nothing about ledgers below it, so a reader
// that persists a cursor must bound itself with ledgerContiguityFrom instead.
func entryChangeLedgerBounds(ctx context.Context, conn driver.Conn) (uint32, uint32, error) {
	var lo, hi uint32
	const q = `SELECT min(ledger_seq), max(ledger_seq) FROM stellar.ledger_entry_changes`
	if err := conn.QueryRow(ctx, q).Scan(&lo, &hi); err != nil {
		return 0, 0, fmt.Errorf("clickhouse: read ledger_entry_changes ledger bounds: %w", err)
	}
	return lo, hi, nil
}

// ledgerContiguity is the lake's completeness picture at and above one ledger, read
// off stellar.ledgers (the commit marker Sink.Flush writes last; see
// ContiguousWatermark). Zero means "none" in every field.
type ledgerContiguity struct {
	lakeMax    uint32 // highest ledger present anywhere in the lake
	firstGap   uint32 // lowest MISSING ledger between two present ledgers >= from
	minPresent uint32 // lowest PRESENT ledger >= from (> from ⟹ from itself is a hole)
}

// ledgerContiguityFrom is ContiguousWatermark's read on a caller-owned connection,
// with the same SQL and normalisation. The DISTINCT scan is bounded below by `from`,
// so pass a ledger near the tip, never the lake floor (the sort exceeds the CH memory cap).
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

// scanSACSeedWindow reduces one window server-side to at most one row per storage key and offers each to red.
// One argMax over a TUPLE of every projected column, keyed on the full within-ledger
// identity (ledger_seq, intra_ledger_seq, tx_hash, op_index, change_index): a ledger
// can hold several changes to one key, and per-column argMax(col, ledger_seq) could
// mix a present entry_xdr with a later 'removed' change_type, resurrecting a deleted balance.
// intra_ledger_seq leads the tuple so the winner matches ledger_entries_current's
// version tie-break; legacy rows with 0 fall through to (tx_hash, op_index, change_index).
// Output aliases must not shadow source columns (ILLEGAL_AGGREGATION), hence win_ prefixes.
// SETTINGS: the ceiling is deliberately below 8 GB; a window that does not fit
// should bisect, not consume the host's memory (galexie's captive core shares it).
// GROUP-BY SPILL IS OFF: the threshold is compared to the whole query's memory
// tracker, dominated by the wide entry_xdr read, so spilling flushed near-empty
// tables (116,753 temp parts) and made it worse. max_bytes_ratio_before_external_group_by
// is unset: it does not exist on the 24.8 server the integration harness runs.
// PREWHERE on entry_type, as in claimable_balance_seed.go: WHERE does not stop
// ClickHouse running base64Decode on other entry types' key_xdr first.
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

// lakeEntryChangeOrder is the within-ledger identity tuple of one entry change
// (ledger_seq, intra_ledger_seq, tx_hash, op_index, change_index), compared
// lexicographically. Every windowed latest-write-wins reduction over
// ledger_entry_changes must share it (this seed and StreamClaimableBalanceSeeds);
// a per-seed copy lets a same-ledger removal resurrect a deleted entry in one reader.
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

// sacSeedReducer finishes the latest-write-wins reduction across windows in Go.
// Memory is bounded by distinct watched Balance keys: other keys the byte-match
// prefilter admits (allowances, third-party storage) are rejected on first sight,
// and a removed winner is stored without its entry_xdr.
type sacSeedReducer struct {
	watched map[string]string
	best    map[string]sacSeedWinner // key_xdr → latest change seen
}

func newSACSeedReducer(watched map[string]string) *sacSeedReducer {
	return &sacSeedReducer{watched: watched, best: make(map[string]sacSeedWinner)}
}

// offer folds one window-winning row into the per-key reduction. Idempotent and
// order-independent (it keeps the max under lakeEntryChangeOrder.after), so a
// bisected retry re-reading a window changes nothing. Removals are tracked like
// any change so a removal in window N suppresses a live balance from window N-1,
// and emit turns them into tombstones.
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

// retractArchived replaces the winner of every key whose entry was ARCHIVED (TTL
// lapsed before asOfLedger) with a removal at the archival ledger and returns the count.
// Call after the window walk and before emit. The lake keeps an archived entry's
// last value forever, so without this a balance that left live state is seeded as
// current; the tombstone also clears a row served earlier. Only a positively
// resolved, lapsed TTL retracts: an unresolved key fails the pass (see
// resolveSACArchivals), since retracting a dormant-but-live balance would understate supply.
func (r *sacSeedReducer) retractArchived(ctx context.Context, conn driver.Conn, asOfLedger uint32) (int, error) {
	lastWrite := make(map[string]uint32, len(r.best))
	for k, w := range r.best {
		if w.changeType != "removed" {
			lastWrite[k] = w.order.ledgerSeq
		}
	}
	archivals, err := resolveSACArchivals(ctx, conn, lastWrite, asOfLedger)
	if err != nil {
		return 0, err
	}
	for k, a := range archivals {
		r.best[k] = sacSeedWinner{
			order:      lakeEntryChangeOrder{ledgerSeq: a.ledger},
			changeType: "removed",
			closeTime:  a.closeTime,
		}
	}
	return len(archivals), nil
}

// emit decodes each key's final winner and hands survivors to fn in ascending key_xdr
// order (reproducible output). Only the final winner is decoded: corrupt XDR on a
// superseded change is not an error, on a live one it is.
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

// sacWatchedBalanceKey reports whether keyXDR is a Balance(Address) key of a WATCHED
// wrapper: a key-only prefilter that keeps sacSeedReducer from retaining the
// allowance and third-party keys multiSearchAny also matches.
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

// watchedKeyDecodeErr classifies a key_xdr that failed to decode. Only a key
// carrying a WATCHED contract id (same raw byte match as sacWatchedContractNeedles)
// is lake corruption; any other undecodable key is skipped so one bad row elsewhere cannot abort the seed.
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

// sacBalanceSeedFromRow decodes one ledger_entries_current row into a SACBalanceSeed.
// matched=false (no error) for non-Balance keys and keys of non-watched contracts.
// Errors only for a WATCHED contract's undecodable live key or Balance value.
// A removed entry on a watched Balance key is a tombstone (IsRemoval, Balance=0).
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
