package clickhouse

import (
	"context"
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

// SACBalanceSeed is one current SAC / SEP-41 `Balance(Address)` entry
// read from the certified lake's current-state projection
// (stellar.ledger_entries_current, ADR-0034), shaped for seeding the
// served tier's sac_balance_observations hypertable (ADR-0022 /
// migration 0014).
//
// Motivation. The live SAC balance observer (internal/sources/
// sac_balances) writes a row only when a `Balance(Address)`
// contract_data entry CHANGES after the observer's window opened. A
// Balance entry created before that window and idle since never emits a
// LedgerEntryChange, so its balance is invisible to Algorithm-2 classic
// supply — dormant contract-held (C-address) SAC balances silently drop
// out of the SAC component. Incident 2026-07-06: ~98% of PHO sits in a
// handful of dormant Phoenix contracts, dragging PHO's Algorithm-2 total
// 156.9% under true supply (BLND 12.4% under). This is the SAC analogue
// of the dormant-reserve-account bootstrap that `supply
// seed-observations` closes for account_observations (ADR-0021).
//
// Unlike the SEP-41 pre-Soroban genesis baseline (which sums
// replay-derived flows below the Soroban activation ledger), this seed
// reads AUTHORITATIVE current on-chain state — the live ContractData
// Balance entry itself — so it is always correct to run.
type SACBalanceSeed struct {
	ContractID string    // SAC-wrapper contract C-strkey
	AssetKey   string    // operator-mapped classic asset_key (CODE:ISSUER)
	Holder     string    // balance owner strkey (G… / C… / …)
	Balance    *big.Int  // current balance, stroops (i128 — never truncated, ADR-0003)
	LedgerSeq  uint32    // the current-state row's ledger (the entry's last-modified ledger)
	CloseTime  time.Time // close time of that ledger (UTC)
}

// StreamSACBalanceSeeds scans the current-state projection for every
// live SAC / SEP-41 `Balance(Address)` contract_data entry belonging to
// a WATCHED SAC-wrapper contract, invoking fn once per decoded entry.
//
// ledger_entries_current carries NO contract_id column — the contract
// id lives inside key_xdr (the LedgerKey) — so the watched-set filter
// runs in Go after decoding, not in SQL. The scan is therefore over
// EVERY contract_data entry network-wide (bounded to the contract_data
// range by the entry_type sort-key prefix, then FINAL-deduped to the
// latest per key). It is read-heavy and MUST run under
// run-heavy-job.sh. Per row, key_xdr is decoded first (cheap) to reject
// non-watched contracts and non-Balance keys before the value-bearing
// entry_xdr is decoded at all.
//
// Removed entries (change_type='removed') are skipped — a deleted
// Balance entry holds nothing. Matches the account-seed reader's
// posture: a corrupt XDR on a WATCHED Balance entry is a hard error
// (the caller is about to persist into the served tier; silently
// dropping it would masquerade as "holder holds nothing" — the exact
// under-count this seed exists to fix).
func StreamSACBalanceSeeds(ctx context.Context, addr string, watched map[string]string, fn func(SACBalanceSeed) error) error {
	if len(watched) == 0 {
		return errors.New("clickhouse: StreamSACBalanceSeeds: empty watched SAC-wrapper set")
	}
	// The heavy-FINAL streaming read class (openRead): unlimited
	// max_execution_time + a per-query memory ceiling, so a full-range
	// FINAL scan isn't aborted mid-stream (G12-04). The 30s-capped
	// ExplorerReader/SupplyReader connections would trip on this scan.
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

// StreamSACBalanceSeedsFullHistory is the full-raw-history counterpart to
// [StreamSACBalanceSeeds]. It scans stellar.ledger_entry_changes — the
// certified append-log (ADR-0034), not the ledger_entries_current
// current-state projection — and reduces to the latest write per
// (entry_type, key_xdr), reproducing exactly what ledger_entries_current's
// ReplacingMergeTree would hold if it had been backfilled for this key. The
// reduction is server-side WITHIN each ledger window and finished in Go across
// them (see the Memory note below).
//
// Why this exists (PHO/BLND VERDICT, incident 2026-07-06 — see
// docs/architecture/supply-pipeline.md "Dormant contract-held SAC balances").
// ledger_entries_current is fed by a ClickHouse MATERIALIZED VIEW
// (`stellar.ledger_entries_current_mv`) that only processes rows INSERTed
// into ledger_entry_changes AFTER the MV was created — standard ClickHouse
// MV semantics, not a bug in the MV itself. Rows that were ch-backfilled
// into ledger_entry_changes for ledgers before the MV existed (~ledger
// 62,000,000) never triggered the MV, so ledger_entries_current has a
// coverage FLOOR: a Balance(Address) entry whose last write predates that
// floor and hasn't changed since (dormant) is invisible to
// [StreamSACBalanceSeeds] even though ledger_entry_changes has always had
// it — the raw substrate is complete (ADR-0034 "100% coverage"; ledgers
// contiguous + hash-chained to genesis), only the current-state PROJECTION
// of it is incomplete below the floor.
//
// The final 2026-07-06 investigation confirmed this is EXACTLY the PHO/BLND
// (and, per the 2026-07-09 residual set, EURC/KALE) gap: their biggest
// holders are Phoenix/Blend POOL CONTRACTS that acquired the SAC-wrapped
// token via an ordinary SEP-41 `transfer` years before the current-state MV
// existed and have been dormant (no further Balance-key writes) since — a
// ContractData `Vec(Symbol("Balance"), Address(pool))` entry on the SAC's
// OWN storage, the identical shape [StreamSACBalanceSeeds] already handles
// for every other holder. An earlier hypothesis in this repo's operational
// notes guessed the balances instead lived in Phoenix/Blend's PRIVATE
// internal-accounting contract_data keys (candidate mechanism (b) — reading
// pool-internal storage, which would need protocol-specific, upgrade-brittle
// decoders); that hypothesis was superseded by the final verdict once
// rollup-vs-lake reconciliation proved Algorithm-3 (SAC lifetime
// Σmint−burn−clawback) correct to the stroop and traced the Algorithm-2 gap
// to this current-state-floor bootstrap issue instead. No new observer, no
// pool-internal reader — mechanism (a) (the SAC's own Balance(Address)
// entries) was already the right one; this function only widens WHERE it
// reads them from.
//
// Cost. ledger_entry_changes holds every historical write, not just the
// latest per key — this scan is substantially heavier than
// [StreamSACBalanceSeeds]'s current-state read and MUST run under
// run-heavy-job.sh on r1 (CLAUDE.md heavy-job doctrine), same discipline as
// the existing seed. It is intended for the small `[supply.sac_wrappers]`
// watched-set (a handful of contracts), never a routine/scheduled job.
//
// Memory (incident 2026-07-27 — the THIRD 241 on this query, and the one that
// changed its shape). Prefiltering to the watched set was not enough. A single
// unbounded query over the append-log carries a per-query footprint that grows
// with the SPAN it covers: the aggregate states (one latest-write state per
// distinct storage key, and entry_xdr is KB-scale) plus the read pipeline for
// that same wide entry_xdr column, which is the larger of the two. Neither is
// bounded by anything except how much history the WHERE admits, so no ceiling
// is ever the right answer — this is the third one it outgrew. Measured on r1
// the day it died: 380 s, 110.3 BILLION rows / 601 GiB read (~70% of the
// table) before hitting its own 8 GB limit in AggregatingTransform.
//
// The fix is to bound the span: scan in ledger WINDOWS ([sacSeedLedgerWindow],
// sized from measurement) and finish the latest-write-wins reduction across
// them in Go ([sacSeedReducer]). Windows are primary-key ranges, so a window
// reads its own slice and nothing else; the granule-level over-read makes the
// windowed total roughly 2x a single pass's rows through the Soroban-dense
// stretch, which is the price of the fix — call it ~1 h wall on r1 for the
// whole chain, still one heavy job under run-heavy-job.sh.
//
// Note what was deliberately NOT done: one query per watched contract. The r1
// skew makes it useless here — over ledgers 63.0–63.2M the USDC wrapper alone
// owns 914,993 of ~1.3M matched rows and 28,287 of ~28,800 matched distinct
// keys (98% of the GROUP BY's cardinality), so a per-contract split leaves
// essentially the whole aggregation intact for USDC while multiplying the
// scan by 38 — hours of saturated I/O on the host that also runs galexie's
// captive core (CLAUDE.md heavy-job doctrine). Windowing bounds the footprint
// on the axis it actually grows along; splitting by contract does not.
func StreamSACBalanceSeedsFullHistory(ctx context.Context, addr string, watched map[string]string, fn func(SACBalanceSeed) error) error {
	if len(watched) == 0 {
		return errors.New("clickhouse: StreamSACBalanceSeedsFullHistory: empty watched SAC-wrapper set")
	}
	conn, err := openRead(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	needles, err := sacWatchedContractNeedles(watched)
	if err != nil {
		return err
	}
	minLedger, maxLedger, err := entryChangeLedgerBounds(ctx, conn)
	if err != nil {
		return err
	}

	// Windows are walked in ascending ledger order and never overlap, so the
	// per-window server-side reduction plus the Go reduction across windows is
	// exactly the reduction the single unbounded GROUP BY performed.
	red := newSACSeedReducer(watched)
	window := uint32(sacSeedLedgerWindow)
	for start := minLedger; ; {
		end := start + window - 1
		if end < start || end > maxLedger { // uint32 overflow guard + clamp
			end = maxLedger
		}
		switch err := scanSACSeedWindow(ctx, conn, needles, start, end, red); {
		case err == nil:
		case isMemoryLimitExceeded(err) && window > sacSeedMinLedgerWindow:
			// Bisect and retry the SAME start: a window whose key space
			// still doesn't fit is a signal to narrow, never to raise the
			// ceiling (chasing the ceiling is what failed three times).
			// Monotonic — a narrowed window is never widened again, so one
			// dense stretch can't be re-hit window after window.
			window /= 2
			continue
		default:
			return err
		}
		if end >= maxLedger {
			break
		}
		start = end + 1
	}
	return red.emit(fn)
}

const (
	// sacSeedLedgerWindow is the ledger span of one full-history seed scan
	// step. It divides ledger_entry_changes' PARTITION BY
	// intDiv(ledger_seq, 1000000) evenly, so a window never straddles a
	// partition, and ledger_seq leads the table's ORDER BY, so a window is a
	// primary-key range — the windows partition the ledger range and their
	// reads sum to roughly the one pass the unbounded query made.
	//
	// 250,000 is measured, not guessed. On r1 (2026-07-27, 38 watched
	// wrappers, ClickHouse 26.5.1) against the two densest stretches of
	// Soroban history:
	//
	//	window     ledgers            peak mem   spills   wall
	//	1,000,000  63.00M–64.00M      >3.73 GiB  died     —
	//	  250,000  63.00M–63.25M      1.75 GiB   0        70 s
	//	  250,000  63.40M–63.65M      1.48 GiB   0        59 s
	//	  250,000  40.00M–40.25M      32 MiB     0        1 s   (pre-Soroban)
	//
	// — roughly 2.5x headroom under the per-query ceiling in the worst
	// stretch measured, and pre-Soroban windows cost almost nothing because
	// entry_type prunes them outright.
	sacSeedLedgerWindow = 250_000
	// sacSeedMinLedgerWindow is the bisection floor (250k >> 4). Below this a
	// window holds a few thousand keys and tens of MiB — if THAT doesn't fit,
	// the window size is not the problem and the error should surface.
	sacSeedMinLedgerWindow = sacSeedLedgerWindow >> 4

	// chMemoryLimitExceeded is ClickHouse's MEMORY_LIMIT_EXCEEDED.
	chMemoryLimitExceeded = 241
)

// sacWatchedContractNeedles renders the watched SAC-wrapper set as raw-byte
// ClickHouse literals for the multiSearchAny prefilter.
//
// The watched-contract filter is pushed INTO the SQL as a raw-byte match on
// the strkey-decoded contract IDs embedded in key_xdr (multiSearchAny over the
// base64-decoded key — the same byte-match technique StreamContractCallOps
// uses on body_xdr): ledger_entry_changes carries no contract_id column.
// Without it the reduction ran over EVERY contract_data key in the
// multi-billion-row append-log and exceeded the CH query budget twice on r1
// (2026-07-11): first in the sort, then — with spill settings — in the
// wide-column read pipeline itself.
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

// entryChangeLedgerBounds reads the append-log's [min, max] ledger. ledger_seq
// leads ledger_entry_changes' ORDER BY, so both come from part metadata — no
// scan. Deliberately NOT floored at a hard-coded Soroban-activation constant:
// the seed's semantics are "whatever contract_data the lake holds", and the
// integration fixtures write contract_data far below mainnet activation.
func entryChangeLedgerBounds(ctx context.Context, conn driver.Conn) (uint32, uint32, error) {
	var lo, hi uint32
	const q = `SELECT min(ledger_seq), max(ledger_seq) FROM stellar.ledger_entry_changes`
	if err := conn.QueryRow(ctx, q).Scan(&lo, &hi); err != nil {
		return 0, 0, fmt.Errorf("clickhouse: read ledger_entry_changes ledger bounds: %w", err)
	}
	return lo, hi, nil
}

// scanSACSeedWindow reduces one ledger window server-side to at most one row
// per storage key and offers each to red.
//
// A SINGLE argMax over a TUPLE of every projected column, keyed on the full
// within-ledger identity tuple (ledger_seq, intra_ledger_seq, tx_hash,
// op_index, change_index) — NOT ledger_seq alone, and not one argMax per
// column. audit-2026-07-16 C2-4: ledger_seq is not unique per key within a
// ledger (change_index is only a per-TRANSACTION counter — see
// extract_entry_changes.go — so a single ledger can hold several changes to
// the same storage key), so `argMax(col, ledger_seq)` computed INDEPENDENTLY
// per column let ClickHouse resolve the tie differently for each column:
// entry_xdr from a still-present change and change_type from a later 'removed'
// change in the same ledger → a Frankenstein current-state row. When
// change_type was mis-read as present, the removed-entry skip never fired and
// a deleted balance was RESURRECTED (before-image) into the SAC supply seed — a
// direct contributor to the supply cross-check divergence. Projecting all
// columns through ONE aggregate makes column coherence structural rather than
// a property of tie-impossibility, and costs one aggregate state per key
// instead of four (the ordering tuple embeds a 64-char tx_hash, so the four
// separate argMax states carried four copies of it).
//
// intra_ledger_seq LEADS the within-ledger part of the tuple (C2-4c): it is the
// per-LEDGER canonical walk position (apply order across all txs), so it
// resolves same-ledger cross-tx writes by TRUE apply order — and it is the
// exact same tie-break folded into ledger_entries_current's version, so this
// full-history reduction and the FINAL projection agree on the winner. Rows
// written before the C2-4c fix (and legacy rows until a re-derive) carry
// intra_ledger_seq = 0, so among them the tuple falls through to
// (tx_hash, op_index, change_index) — the prior lexical-but-deterministic
// canonical order — a strict superset, never a regression.
//
// Output aliases must NOT shadow the source column names: ClickHouse resolves
// a shadowing alias back into sibling aggregate arguments (ILLEGAL_AGGREGATION
// — caught live 2026-07-11), hence the win_ prefixes and the tupleElement
// unpack in an outer SELECT.
//
// SETTINGS: the per-query ceiling is LOWER than the 8 GB the pre-windowing
// query asked for, on purpose. With the key space bounded to one window a
// healthy step needs a fraction of it (1.5–1.8 GiB measured), and a step that
// doesn't fit should bisect — bounded extra time — rather than consume the
// host's memory, whose blast radius is unbounded and includes galexie's
// captive core.
//
// GROUP-BY SPILL IS OFF, which reverses the earlier belt-and-braces posture.
// ClickHouse compares max_bytes_before_external_group_by against the WHOLE
// QUERY's memory tracker, not the hash table alone — and this query's tracker
// is dominated by the wide entry_xdr read, not by the aggregate states (only
// ~32k–54k groups per window). With the threshold at 2 GB / 1 GB the read
// alone sat above it, so the aggregator flushed a near-empty hash table on
// every block: 116,753 temporary parts on one r1 window (2026-07-27), and
// merging that many spilled parts is itself what exhausted the budget. Spilling
// here made the query strictly worse; narrowing the window is what actually
// bounds it. (max_bytes_ratio_before_external_group_by, which would clamp the
// CH-25+ ratio-based path too, is deliberately NOT set: it does not exist on
// the 24.8 server the integration harness runs, and an unknown setting is a
// hard error. On a newer server the ratio can still spill near the ceiling —
// at which point the bisection below takes over.)
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
		    WHERE entry_type = 'contract_data'
		      AND ledger_seq BETWEEN ? AND ?
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

// isMemoryLimitExceeded reports whether err is ClickHouse refusing a query for
// memory (241). It is the ONE error class the window walk answers by bisecting
// rather than failing — every other exception is surfaced unchanged, so a real
// fault can never be masked as "just narrow the window".
func isMemoryLimitExceeded(err error) bool {
	var chErr *clickhouse.Exception
	return errors.As(err, &chErr) && chErr.Code == chMemoryLimitExceeded
}

// lakeEntryChangeOrder is the full within-ledger identity tuple of one entry
// change — the ordering key the server-side argMax uses, carried into Go so the
// cross-window reduction compares winners on exactly the same terms
// (audit-2026-07-16 C2-4 / C2-4c). Compared lexicographically:
// ledger_seq, intra_ledger_seq, tx_hash, op_index, change_index.
//
// It is deliberately NOT named for one seed: every windowed
// latest-write-wins reduction over stellar.ledger_entry_changes must use
// this exact tuple, so the SAC-balance seed
// ([StreamSACBalanceSeedsFullHistory]) and the claimable-balance seed
// ([StreamClaimableBalanceSeeds]) share ONE definition of it. Duplicating
// [lakeEntryChangeOrder.after] per seed is how a same-ledger removal starts
// resurrecting a deleted entry in one reader but not the other.
type lakeEntryChangeOrder struct {
	ledgerSeq      uint32
	intraLedgerSeq uint32
	txHash         string
	opIndex        int32 // -1 for fee-meta / tx-level changes
	changeIndex    uint32
}

// after reports whether a sorts STRICTLY after b, matching ClickHouse's
// lexicographic tuple comparison element for element. tx_hash comparison is
// byte-wise in both (CH String compare is memcmp; Go string compare is
// byte-wise), and op_index is signed in both (Int32 / int32), so the -1
// fee-meta sentinel orders below op 0 identically on either side.
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

// sacSeedReducer finishes, in Go, the latest-write-wins reduction that the
// per-window queries can only complete WITHIN their window.
//
// Memory is bounded by the number of distinct WATCHED Balance keys — the seed's
// own output cardinality — not by the append-log's key space: a key that is not
// a watched wrapper's Balance(Address) entry is rejected on first sight and
// never stored (the byte-match prefilter admits any contract_data key that
// merely MENTIONS a watched contract — allowances, third-party pool storage —
// and those are the bulk of the matched rows). A removed winner is stored
// without its entry_xdr: the emit path never reads the value of a removal, and
// dropping it keeps a churn-heavy key cheap.
type sacSeedReducer struct {
	watched map[string]string
	best    map[string]sacSeedWinner // key_xdr → latest change seen
}

func newSACSeedReducer(watched map[string]string) *sacSeedReducer {
	return &sacSeedReducer{watched: watched, best: make(map[string]sacSeedWinner)}
}

// offer folds one window-winning row into the running per-key reduction.
//
// Idempotent and order-independent: it keeps the maximum under
// [lakeEntryChangeOrder.after], so re-offering rows (a bisected retry re-reads a window
// whose stream already delivered part of its output) can never change the
// outcome, and windows may be walked in any order.
//
// A REMOVAL must be able to win. The pre-windowing reader could short-circuit
// on change_type before decoding anything because the server had already picked
// the single global winner per key; here a removal seen in window N must still
// suppress a live balance seen in window N-1, so removals are tracked like any
// other change and dropped at emit time.
func (r *sacSeedReducer) offer(keyXDR, entryXDR, changeType string, closeTime time.Time, ord lakeEntryChangeOrder) error {
	if prev, seen := r.best[keyXDR]; seen {
		if !ord.after(prev.order) {
			return nil
		}
	} else {
		watchedBalance, err := sacWatchedBalanceKey(keyXDR, r.watched)
		if err != nil {
			// An undecodable key on a REMOVED change identifies no holder and
			// held nothing; the pre-windowing reader never decoded it either
			// (it returned on change_type first). Preserve that exactly — only
			// a live entry's corrupt key is a hard error.
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

// emit decodes each key's final winner and hands the survivors to fn, in
// ascending key_xdr order so a run's output (and any log tail of it) is
// reproducible. Decoding only the FINAL winner keeps the reader's error
// contract identical to the pre-windowing version: corrupt XDR on a superseded
// change is not the seed's problem, corrupt XDR on a live one is.
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

// sacWatchedBalanceKey reports whether keyXDR is a `Balance(Address)` storage
// key belonging to a WATCHED SAC wrapper — the cheap key-only prefilter that
// keeps [sacSeedReducer] from retaining the allowance / third-party keys the
// raw-byte multiSearchAny necessarily also matches. Mirrors the key half of
// [sacBalanceSeedFromRow]; the value is not touched.
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

// sacBalanceSeedFromRow decodes one ledger_entries_current contract_data
// row into a SACBalanceSeed. Split from the query for testability
// (mirrors accountSeedFromRow).
//
// Returns matched=false (no error) for rows the seed intentionally
// skips: a removed entry, a non-Balance contract-storage key, or a
// Balance entry belonging to a contract outside the watched set. Returns
// an error only for a WATCHED Balance entry whose key/value fails to
// decode — that is real lake corruption worth failing the seed for.
func sacBalanceSeedFromRow(keyXDR, entryXDR, changeType string, ledgerSeq uint32, closeTime time.Time, watched map[string]string) (SACBalanceSeed, bool, error) {
	if changeType == "removed" {
		return SACBalanceSeed{}, false, nil
	}

	// Decode the LedgerKey first (cheap): it carries the contract id +
	// the storage key, enough to reject non-watched contracts and
	// non-Balance keys before touching the value-bearing entry_xdr.
	var lk xdr.LedgerKey
	if err := xdr.SafeUnmarshalBase64(keyXDR, &lk); err != nil {
		return SACBalanceSeed{}, false, fmt.Errorf("clickhouse: decode contract_data key_xdr: %w", err)
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

	// The amount lives only in entry_xdr (the LedgerKey has no Val). A
	// non-removed current-state row always carries it; an empty value
	// here would be a lake inconsistency — skip rather than fabricate.
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
