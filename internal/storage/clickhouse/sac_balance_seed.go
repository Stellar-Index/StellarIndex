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

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/StellarIndex/stellar-index/internal/scval"

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
// (entry_type, key_xdr) with a server-side `ORDER BY … LIMIT 1 BY key_xdr`,
// reproducing exactly what ledger_entries_current's ReplacingMergeTree
// would hold if it had been backfilled for this key.
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
func StreamSACBalanceSeedsFullHistory(ctx context.Context, addr string, watched map[string]string, fn func(SACBalanceSeed) error) error {
	if len(watched) == 0 {
		return errors.New("clickhouse: StreamSACBalanceSeedsFullHistory: empty watched SAC-wrapper set")
	}
	conn, err := openRead(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	// LIMIT 1 BY key_xdr, ordered by ledger_seq DESC within each key group,
	// keeps only the highest-ledger row per unique storage key — the same
	// "latest write wins" reduction ledger_entries_current's
	// ReplacingMergeTree(ledger_seq) performs, computed directly over the
	// full append-log instead of the (floor-limited) projection.
	// The watched-contract filter is pushed INTO the SQL as a raw-byte
	// match on the strkey-decoded contract IDs embedded in key_xdr
	// (multiSearchAny over the base64-decoded key — the same byte-match
	// technique StreamContractCallOps uses on body_xdr). Without it, the
	// LIMIT 1 BY reduction ran over EVERY contract_data key in the
	// multi-billion-row append-log and exceeded the CH query budget
	// twice on r1 (2026-07-11): first in the sort, then — with spill
	// settings — in the wide-column read pipeline itself. Prefiltered
	// to the watched handful of contracts, the surviving row set is
	// tiny and the reduction is trivial. Spill settings retained as a
	// belt-and-suspenders bound.
	needles := make([]string, 0, len(watched))
	for strk := range watched {
		raw, derr := strkey.Decode(strkey.VersionByteContract, strk)
		if derr != nil {
			return fmt.Errorf("clickhouse: full-history seed: decode watched strkey %s: %w", strk, derr)
		}
		needles = append(needles, "unhex('"+hex.EncodeToString(raw)+"')")
	}
	sort.Strings(needles) // deterministic SQL for tests/logs
	q := `SELECT key_xdr, entry_xdr, change_type, ledger_seq, close_time
		FROM stellar.ledger_entry_changes
		WHERE entry_type = 'contract_data'
		  AND multiSearchAny(base64Decode(key_xdr), [` + strings.Join(needles, ", ") + `])
		ORDER BY key_xdr, ledger_seq DESC
		LIMIT 1 BY key_xdr
		SETTINGS max_memory_usage = 8000000000,
		         max_bytes_before_external_sort = 2000000000,
		         max_bytes_before_external_group_by = 2000000000,
		         max_threads = 4`
	rows, err := conn.Query(ctx, q)
	if err != nil {
		return fmt.Errorf("clickhouse: scan contract_data full history: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			keyXDR, entryXDR, changeType string
			ledgerSeq                    uint32
			closeTime                    time.Time
		)
		if err := rows.Scan(&keyXDR, &entryXDR, &changeType, &ledgerSeq, &closeTime); err != nil {
			return fmt.Errorf("clickhouse: scan contract_data full-history row: %w", err)
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
