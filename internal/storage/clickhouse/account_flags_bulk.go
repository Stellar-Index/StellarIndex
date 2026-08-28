package clickhouse

import (
	"context"
	"fmt"

	"github.com/stellar/go-stellar-sdk/xdr"
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
	HomeDomain string
}

// BulkAccountAuthFlags returns decoded auth flags for the given account
// G-strkeys, omitting any account that has no live entry in the captured
// window (absence is a measurement, not an error).
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
	byKey := make(map[string]string, len(gStrkeys))
	keys := make([]string, 0, len(gStrkeys))
	for _, g := range gStrkeys {
		k, err := accountKeyXDR(g)
		if err != nil {
			// A malformed G-strkey is the caller's data problem, not a
			// reason to fail the whole batch — skip it.
			continue
		}
		if _, dup := byKey[k]; dup {
			continue
		}
		byKey[k] = g
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return out, nil
	}

	const q = `SELECT key_xdr, entry_xdr
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
		var keyXDR, entryXDR string
		if err := rows.Scan(&keyXDR, &entryXDR); err != nil {
			return nil, fmt.Errorf("clickhouse: BulkAccountAuthFlags scan: %w", err)
		}
		g, ok := byKey[keyXDR]
		if !ok || entryXDR == "" {
			continue
		}
		var le xdr.LedgerEntry
		if err := xdr.SafeUnmarshalBase64(entryXDR, &le); err != nil {
			// One undecodable entry must not cost the other ~59k. A
			// decode failure here is also the signature of being behind
			// a protocol upgrade, which the ledger-meta decode probe
			// alerts on separately.
			continue
		}
		acc, ok := le.Data.GetAccount()
		if !ok {
			continue
		}
		f := uint32(acc.Flags)
		out[g] = AccountAuthFlags{
			Required:   f&0x1 != 0,
			Revocable:  f&0x2 != 0,
			Immutable:  f&0x4 != 0,
			Clawback:   f&0x8 != 0,
			HomeDomain: string(acc.HomeDomain),
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("clickhouse: BulkAccountAuthFlags rows: %w", err)
	}
	return out, nil
}
