package clickhouse

import (
	"context"
	"fmt"
	"slices"

	"github.com/stellar/go-stellar-sdk/xdr"
)

// AuthFlagsSource records HOW an [AccountAuthFlags] value was obtained, so a
// consumer can tell a CURRENT reading from a historical one. Persisted
// verbatim into `issuers.auth_flags_source` (migration 0153) and surfaced on
// /v1/issuers/{g_strkey}.
//
// This exists because ~10.2k of the ~59.2k known issuers (r1, 2026-09-02)
// have merged their account away: their flags are knowable, but only as of
// the ledger that removed them. Serving those with no provenance would assert
// "this is the issuer's current authorisation policy" about an account that no
// longer exists.
type AuthFlagsSource string

const (
	// AuthFlagsSourceLive — decoded from the account's CURRENT AccountEntry
	// in stellar.ledger_entries_current. The account exists on-chain.
	AuthFlagsSourceLive AuthFlagsSource = "live"
	// AuthFlagsSourceLastKnownBeforeRemoval — the account has been merged
	// away. Decoded from the last non-`removed` change to the key in the
	// ledger that removed it. True AS OF AsOfLedger, NOT current, and never
	// to be presented as current.
	AuthFlagsSourceLastKnownBeforeRemoval AuthFlagsSource = "last_known_before_removal"
)

// AccountAuthFlags is one account's decoded AccountEntry auth flags.
//
// Stellar packs these in a single uint32 bitmask:
//
//	AUTH_REQUIRED = 1, AUTH_REVOCABLE = 2, AUTH_IMMUTABLE = 4, AUTH_CLAWBACK = 8
//
// Decoded here rather than shipped as the raw mask so callers cannot get
// the bit positions wrong independently — the API's read-time enrichment
// (Server.enrichIssuerFromAccountState) already decodes them this way,
// and two decoders drifting would put different answers on the detail
// page and in the durable column.
type AccountAuthFlags struct {
	Required  bool
	Revocable bool
	Immutable bool
	Clawback  bool
	// HomeDomain is carried along because the same AccountEntry holds it
	// and the issuers table wants it — fetching it separately would mean
	// a second pass over the same rows.
	//
	// ALWAYS EMPTY when Source is AuthFlagsSourceLastKnownBeforeRemoval.
	// home_domain is an IDENTITY claim the account makes about itself, and
	// a merged account can no longer be checked against SEP-1 (the toml's
	// [[CURRENCIES]] back-reference is what makes the claim bidirectional).
	// Persisting a dead account's self-declared domain would create an
	// impersonation surface on exactly the accounts that can no longer be
	// verified on-chain — one sampled residue account (GD37LIDE…) was
	// merged claiming `lobstr.co`. The auth FLAGS are objective account
	// state and are kept; the domain is not.
	HomeDomain string
	// Source is how this reading was obtained — see [AuthFlagsSource].
	Source AuthFlagsSource
	// AsOfLedger is the ledger the reading is true as of: the entry's
	// last-modified ledger for a live account, the removal ledger for a
	// last-known one.
	AsOfLedger uint32
}

// accountLedgerKeys maps the caller's G-strkeys to their LedgerKey base64,
// returning both the reverse index (key_xdr → G-strkey) and the deduplicated
// key list to bind into a query.
//
// A malformed G-strkey is the caller's data problem, not a reason to fail the
// whole batch — it is skipped.
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

// decodeAccountAuthFlags decodes a base64 LedgerEntry into its auth flags +
// home domain. ok=false for an empty, undecodable or non-account entry — one
// bad entry must not cost the other ~59k, and a decode failure here is also
// the signature of being behind a protocol upgrade, which the ledger-meta
// decode probe alerts on separately.
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

// BulkAccountAuthFlags returns decoded auth flags for the given account
// G-strkeys, omitting any account that has no live entry in the captured
// window (absence is a measurement, not an error). Every returned value
// carries Source = [AuthFlagsSourceLive].
//
// A MERGED account is deliberately NOT resolved here — see
// [ExplorerReader.RemovedAccountsLastKnownAuthFlags], which is a separate call
// precisely because its answer is historical and must be persisted with its
// provenance rather than blended into a "current state" map.
//
// WHY key_xdr AND NOT account_id
//
// stellar.ledger_entries_current is ORDER BY (entry_type, key_xdr), so
// key_xdr is a sort-key prefix and account_id is not. The same table's
// point-lookup path records the measured difference: 5.18s via the
// account_id skip-index versus 0.069s by key_xdr. At ~59k issuers the
// wrong choice here is the difference between a job that finishes and one
// that does not, so the caller's G-strkeys are converted to their
// LedgerKey base64 and matched on the sort key.
//
// Caller batches. This issues ONE query for whatever slice it is given;
// it does not chunk internally, because the right chunk size depends on
// the caller's memory budget and ClickHouse's max_query_size, neither of
// which this function can see.
func (r *ExplorerReader) BulkAccountAuthFlags(ctx context.Context, gStrkeys []string) (map[string]AccountAuthFlags, error) {
	out := make(map[string]AccountAuthFlags, len(gStrkeys))
	if len(gStrkeys) == 0 {
		return out, nil
	}

	// key_xdr -> G-strkey, so a returned row maps back to its account
	// without re-deriving the address from the entry.
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

// RemovedAccountsLastKnownAuthFlags resolves the auth flags of accounts that
// have been MERGED AWAY, from the account's last state before removal. Every
// returned value carries Source = [AuthFlagsSourceLastKnownBeforeRemoval],
// AsOfLedger = the removal ledger, and an EMPTY HomeDomain.
//
// Keys that are live, or that have no `removed` row in the current-state
// projection at all, are simply absent from the result — absence is a
// measurement, not an error. Callers pass their whole unresolved set and read
// what comes back.
//
// # WHY A SCOPED SINGLE-LEDGER READ, NOT A KEY SCAN
//
// The obvious shape — `WHERE key_xdr IN (?)` over stellar.ledger_entry_changes
// with no ledger predicate — leans on idx_lec_key_xdr, a bloom skip-index over
// a 150B-row / 6+ TiB table whose false-positive rate is a hard pruning FLOOR
// (see the index's own comment in deploy/clickhouse/tier1_schema.sql). It is
// not needed: the account's removal ledger is already in
// ledger_entries_current, and the lake records the PRE-IMAGE IN THAT SAME
// LEDGER — an account_merge leaves `state` → `updated` pairs for the fee phase
// and every operation, ending `state` → `removed`. So the second read is
// PARTITION-PRUNED to the removal ledgers only. Measured on r1 2026-09-02 over
// 296 real residue issuers spanning 277 removal ledgers: 296/296 resolved, 0
// empty pre-images, 0.388s for the whole batch.
//
// # WHY change_type != 'removed' RATHER THAN A `state` FILTER
//
// Admitting `state` rows is the whole point: the `state` row immediately
// before the `removed` one IS the pre-image. `updated` rows are admitted for
// the same reason and are ORDERED against it, not preferred over it.
//
// # WHY THE (ledger_seq, intra_ledger_seq, change_index) ARGMAX KEY
//
// change_index restarts per TRANSACTION (deploy/clickhouse/tier1_schema.sql),
// so it alone is only monotonic within one tx. intra_ledger_seq is the
// canonical ledger-wide walk position and is the correct primary tiebreak —
// but it is 0 on every row written before the ADR-0038 walk fix and on legacy
// rows until a re-derive repopulates it (verified on r1: the whole
// 38,137,083 removal is all-zero), so change_index is the necessary
// fallback, and it is correct there because a merged account's same-ledger
// changes are all in its own merge transaction. Residual, stated plainly: an
// account that submits set_options and account_merge in TWO transactions in
// the same ledger, on a range whose intra_ledger_seq has not been re-derived,
// can resolve to the pre-set_options flags.
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

	// One partition-pruned read over the removal ledgers. Batching the
	// ledgers rather than issuing one query per key is safe because each
	// key's returned `as_of` is checked against that key's OWN removal
	// ledger below: a key that was merely ACTIVE in some other key's
	// removal ledger cannot contribute its state to this answer.
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
		// The pre-image must come from the ledger that removed THIS key.
		// Anything else is another key's ledger leaking through the
		// batched IN-list, and is refused rather than mis-attributed.
		if seq, ok := removedAt[keyXDR]; !ok || seq != asOf {
			continue
		}
		f, ok := decodeAccountAuthFlags(entryXDR)
		if !ok {
			continue
		}
		f.Source = AuthFlagsSourceLastKnownBeforeRemoval
		f.AsOfLedger = asOf
		// A merged account's self-declared identity is not servable —
		// see the HomeDomain field comment.
		f.HomeDomain = ""
		out[g] = f
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("clickhouse: RemovedAccountsLastKnownAuthFlags rows: %w", err)
	}
	return out, nil
}

// accountRemovalLedgers returns key_xdr → the ledger in which the account was
// merged away, for whichever of the given LedgerKeys the current-state
// projection records as `removed`. Live keys and keys the projection has
// never seen are absent.
//
// Note the projection holds no `removed` row below its floor (r1: ledger
// 38,000,000), so an account merged before that is absent here and stays
// unresolved. That is a projection-coverage gap, not a reader defect, and it
// is ~1.5% of the residue (r1 2026-09-02: 4 of a 300-key sample).
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
