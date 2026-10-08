package clickhouse

import (
	"context"
	"fmt"
	"slices"

	"github.com/stellar/go-stellar-sdk/xdr"
)

// AuthFlagsSource records HOW an [AccountAuthFlags] value was obtained, so a consumer
// can tell a CURRENT reading from a historical one. Persisted verbatim into
// `issuers.auth_flags_source` and served on /v1/issuers/{g_strkey}. Merged-away
// issuers' flags are knowable only as of their removal ledger; serving them without
// provenance would assert a current policy for an account that no longer exists.
type AuthFlagsSource string

const (
	// AuthFlagsSourceLive: decoded from the CURRENT AccountEntry in
	// ledger_entries_current.
	AuthFlagsSourceLive AuthFlagsSource = "live"
	// AuthFlagsSourceLastKnownBeforeRemoval: the account was merged away; decoded from
	// the last non-`removed` change in its removal ledger. True AS OF AsOfLedger, never
	// to be presented as current.
	AuthFlagsSourceLastKnownBeforeRemoval AuthFlagsSource = "last_known_before_removal"
)

// AccountAuthFlags is one account's decoded AccountEntry auth flags.
// Bitmask: AUTH_REQUIRED=1, AUTH_REVOCABLE=2, AUTH_IMMUTABLE=4, AUTH_CLAWBACK=8.
// Decoded here so bit positions cannot drift from Server.enrichIssuerFromAccountState,
// which decodes the same way.
type AccountAuthFlags struct {
	Required  bool
	Revocable bool
	Immutable bool
	Clawback  bool
	// HomeDomain rides along because the same AccountEntry holds it.
	//
	// ALWAYS EMPTY when Source is AuthFlagsSourceLastKnownBeforeRemoval: home_domain is
	// a self-asserted identity claim that a merged account can no longer back with SEP-1
	// (the toml's [[CURRENCIES]] back-reference), so persisting it would create an
	// impersonation surface. The flags are objective state and are kept.
	HomeDomain string
	// Source is how this reading was obtained — see [AuthFlagsSource].
	Source AuthFlagsSource
	// AsOfLedger is the entry's last-modified ledger for a live account, the removal
	// ledger for a last-known one.
	AsOfLedger uint32
}

// accountLedgerKeys maps G-strkeys to LedgerKey base64, returning the reverse index
// (key_xdr to G-strkey) and the deduplicated key list. A malformed G-strkey is
// skipped, not a batch failure.
func accountLedgerKeys(gStrkeys []string) (map[string]string, []string) {
	byKey := make(map[string]string, len(gStrkeys))
	keys := make([]string, 0, len(gStrkeys))
	for _, g := range gStrkeys {
		k, err := accountKeyXDR(g)
		if err != nil {
			continue
		}
		if _, dup := byKey[k]; dup {
			continue
		}
		byKey[k] = g
		keys = append(keys, k)
	}
	return byKey, keys
}

// decodeAccountAuthFlags decodes a base64 LedgerEntry. ok=false for an empty,
// undecodable or non-account entry, so one bad entry cannot cost the batch; a decode
// failure is also the signature of being behind a protocol upgrade.
func decodeAccountAuthFlags(entryXDR string) (AccountAuthFlags, bool) {
	if entryXDR == "" {
		return AccountAuthFlags{}, false
	}
	var le xdr.LedgerEntry
	if err := xdr.SafeUnmarshalBase64(entryXDR, &le); err != nil {
		return AccountAuthFlags{}, false
	}
	acc, ok := le.Data.GetAccount()
	if !ok {
		return AccountAuthFlags{}, false
	}
	f := uint32(acc.Flags)
	return AccountAuthFlags{
		Required:   f&0x1 != 0,
		Revocable:  f&0x2 != 0,
		Immutable:  f&0x4 != 0,
		Clawback:   f&0x8 != 0,
		HomeDomain: string(acc.HomeDomain),
	}, true
}

// BulkAccountAuthFlags returns auth flags for the G-strkeys, omitting accounts with
// no live entry (absence is a measurement). Every value has Source =
// [AuthFlagsSourceLive]. Merged accounts are NOT resolved here; see
// [ExplorerReader.RemovedAccountsLastKnownAuthFlags], whose historical answer must
// keep its provenance.
//
// Matches on key_xdr, not account_id: ledger_entries_current is ORDER BY
// (entry_type, key_xdr), so key_xdr is a sort-key prefix (0.069 s vs 5.18 s via the
// account_id skip-index; decisive at ~59k issuers).
//
// One query per call: the caller batches, since chunk size depends on its memory
// budget and max_query_size.
func (r *ExplorerReader) BulkAccountAuthFlags(ctx context.Context, gStrkeys []string) (map[string]AccountAuthFlags, error) {
	out := make(map[string]AccountAuthFlags, len(gStrkeys))
	if len(gStrkeys) == 0 {
		return out, nil
	}

	byKey, keys := accountLedgerKeys(gStrkeys)
	if len(keys) == 0 {
		return out, nil
	}

	const q = `SELECT key_xdr, entry_xdr, ledger_seq
		FROM stellar.ledger_entries_current FINAL
		WHERE entry_type = 'account'
		  AND change_type != 'removed'
		  AND key_xdr IN (?)`
	rows, err := r.conn.Query(ctx, q, keys)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: BulkAccountAuthFlags[%d keys]: %w", len(keys), err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			keyXDR, entryXDR string
			ledgerSeq        uint32
		)
		if err := rows.Scan(&keyXDR, &entryXDR, &ledgerSeq); err != nil {
			return nil, fmt.Errorf("clickhouse: BulkAccountAuthFlags scan: %w", err)
		}
		g, ok := byKey[keyXDR]
		if !ok {
			continue
		}
		f, ok := decodeAccountAuthFlags(entryXDR)
		if !ok {
			continue
		}
		f.Source = AuthFlagsSourceLive
		f.AsOfLedger = ledgerSeq
		out[g] = f
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("clickhouse: BulkAccountAuthFlags rows: %w", err)
	}
	return out, nil
}

// RemovedAccountsLastKnownAuthFlags resolves flags of MERGED accounts from their last
// state before removal. Values carry Source = [AuthFlagsSourceLastKnownBeforeRemoval],
// AsOfLedger = the removal ledger, and an EMPTY HomeDomain. Live keys and keys with
// no `removed` row are absent.
//
// Scoped single-ledger read, not a key scan: an unbounded `key_xdr IN (?)` over
// ledger_entry_changes leans on idx_lec_key_xdr, a bloom skip-index over a 150B-row
// table whose false-positive rate is a pruning floor. The removal ledger is already
// in ledger_entries_current and the lake records the PRE-IMAGE in that same ledger
// (a merge leaves `state` then `updated` rows ending in `removed`), so the read is
// partition-pruned to the removal ledgers.
//
// change_type != 'removed' admits `state` rows on purpose: the `state` row before
// the `removed` one IS the pre-image; `updated` rows are ordered against it.
//
// ARGMAX key (ledger_seq, intra_ledger_seq, change_index): change_index restarts per
// TRANSACTION, intra_ledger_seq is the ledger-wide tiebreak but is 0 on rows not
// re-derived after the ADR-0038 walk fix, so change_index is the fallback (valid
// because a merge's same-ledger changes share its tx). Residual: set_options and
// account_merge in TWO txs of one ledger on a non-re-derived range can resolve to
// the pre-set_options flags.
func (r *ExplorerReader) RemovedAccountsLastKnownAuthFlags(ctx context.Context, gStrkeys []string) (map[string]AccountAuthFlags, error) {
	out := make(map[string]AccountAuthFlags, len(gStrkeys))
	if len(gStrkeys) == 0 {
		return out, nil
	}
	byKey, keys := accountLedgerKeys(gStrkeys)
	if len(keys) == 0 {
		return out, nil
	}

	removedAt, err := r.accountRemovalLedgers(ctx, keys)
	if err != nil {
		return nil, err
	}
	if len(removedAt) == 0 {
		return out, nil
	}

	removedKeys := make([]string, 0, len(removedAt))
	ledgers := make([]uint32, 0, len(removedAt))
	for k, seq := range removedAt {
		removedKeys = append(removedKeys, k)
		ledgers = append(ledgers, seq)
	}
	slices.Sort(removedKeys)
	slices.Sort(ledgers)
	ledgers = slices.Compact(ledgers)

	// One partition-pruned read over the removal ledgers. Batching is safe because each
	// key's as_of is checked against its OWN removal ledger below.
	const q = `SELECT key_xdr,
		       argMax(entry_xdr, (ledger_seq, intra_ledger_seq, change_index)) AS last_entry,
		       max(ledger_seq) AS as_of
		FROM stellar.ledger_entry_changes
		WHERE ledger_seq IN (?)
		  AND entry_type = 'account'
		  AND change_type != 'removed'
		  AND key_xdr IN (?)
		GROUP BY key_xdr`
	rows, err := r.conn.Query(ctx, q, ledgers, removedKeys)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: RemovedAccountsLastKnownAuthFlags[%d keys]: %w", len(removedKeys), err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			keyXDR, entryXDR string
			asOf             uint32
		)
		if err := rows.Scan(&keyXDR, &entryXDR, &asOf); err != nil {
			return nil, fmt.Errorf("clickhouse: RemovedAccountsLastKnownAuthFlags scan: %w", err)
		}
		g, ok := byKey[keyXDR]
		if !ok {
			continue
		}
		// The pre-image must come from THIS key's removal ledger; another key's ledger
		// leaking through the batched IN-list is refused.
		if seq, ok := removedAt[keyXDR]; !ok || seq != asOf {
			continue
		}
		f, ok := decodeAccountAuthFlags(entryXDR)
		if !ok {
			continue
		}
		f.Source = AuthFlagsSourceLastKnownBeforeRemoval
		f.AsOfLedger = asOf
		// A merged account's self-declared identity is not servable (see HomeDomain).
		f.HomeDomain = ""
		out[g] = f
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("clickhouse: RemovedAccountsLastKnownAuthFlags rows: %w", err)
	}
	return out, nil
}

// accountRemovalLedgers returns key_xdr to the ledger that merged the account away,
// for keys the current-state projection records as `removed`. The projection holds
// no `removed` row below its floor, so older merges stay unresolved: a coverage gap,
// not a reader defect.
func (r *ExplorerReader) accountRemovalLedgers(ctx context.Context, keys []string) (map[string]uint32, error) {
	const q = `SELECT key_xdr, ledger_seq
		FROM stellar.ledger_entries_current FINAL
		WHERE entry_type = 'account'
		  AND change_type = 'removed'
		  AND key_xdr IN (?)`
	rows, err := r.conn.Query(ctx, q, keys)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: accountRemovalLedgers[%d keys]: %w", len(keys), err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]uint32, len(keys))
	for rows.Next() {
		var (
			keyXDR    string
			ledgerSeq uint32
		)
		if err := rows.Scan(&keyXDR, &ledgerSeq); err != nil {
			return nil, fmt.Errorf("clickhouse: accountRemovalLedgers scan: %w", err)
		}
		out[keyXDR] = ledgerSeq
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("clickhouse: accountRemovalLedgers rows: %w", err)
	}
	return out, nil
}
