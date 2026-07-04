package clickhouse

import (
	"context"
	"fmt"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// maxSaneTokenDecimals bounds the accepted on-chain `decimal` declaration.
// The value is self-declared by the token contract (a u32 on the wire), so a
// hostile or broken token can claim anything up to 2^32−1; downstream unit
// math divides by 10^decimals, and absurd scales would zero out market-cap /
// display amounts. 38 mirrors the NUMERIC(38) precision ceiling used across
// the served tier — no legitimate token exceeds 18 in practice. Declarations
// above the bound are treated as "no usable metadata" (callers keep their
// default).
const maxSaneTokenDecimals = 38

// TokenDecimals resolves a token contract's `decimals()` value from the
// certified lake: the soroban-token-sdk convention — followed by SACs (always
// 7) and virtually every SEP-41 WASM token — persists TokenMetadata in the
// contract INSTANCE storage under Symbol "METADATA" as
// Map{decimal: U32, name: String, symbol: String}. Reading the instance entry
// is exactly the `decimals()` a caller would get from the contract for
// token-sdk-shaped tokens, without executing WASM.
//
// found=false (nil error) when the instance isn't captured in the lake, the
// contract stores no METADATA map (a non-standard token — its decimals are
// simply not derivable from storage), or the declaration is out of sane
// bounds. Callers keep their default (7) in that case.
func (r *ExplorerReader) TokenDecimals(ctx context.Context, contractID string) (uint32, bool, error) {
	raw, err := strkey.Decode(strkey.VersionByteContract, contractID)
	if err != nil {
		return 0, false, fmt.Errorf("clickhouse: TokenDecimals: bad contract id %q: %w", contractID, err)
	}
	var cid xdr.Hash
	copy(cid[:], raw)
	keys, err := instanceKeyXDR(cid)
	if err != nil {
		return 0, false, err
	}
	// Same table choice + rationale as contractWasmHash: ledger_entries_current
	// is merge-loss immune and (entry_type, key_xdr) is a PK-prefix lookup —
	// cheap enough for the (response-cached) asset-detail path.
	const q = `SELECT entry_xdr FROM stellar.ledger_entries_current FINAL
		WHERE entry_type = 'contract_data' AND key_xdr IN (?) AND entry_xdr != ''
		ORDER BY ledger_seq DESC LIMIT 1`
	rows, err := r.conn.Query(ctx, q, keys)
	if err != nil {
		return 0, false, fmt.Errorf("clickhouse: token decimals scan: %w", err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return 0, false, rows.Err()
	}
	var b64 string
	if err := rows.Scan(&b64); err != nil {
		return 0, false, fmt.Errorf("clickhouse: scan token decimals: %w", err)
	}
	d, ok := decimalsFromInstanceEntry(b64)
	return d, ok, rows.Err()
}

// decimalsFromInstanceEntry decodes one contract-instance LedgerEntry and
// returns the token-sdk METADATA map's `decimal` u32. ok=false when the entry
// isn't an instance, carries no METADATA map, the map has no u32 `decimal`,
// or the declared value exceeds maxSaneTokenDecimals.
func decimalsFromInstanceEntry(b64 string) (uint32, bool) {
	var entry xdr.LedgerEntry
	if xdr.SafeUnmarshalBase64(b64, &entry) != nil {
		return 0, false
	}
	cd, ok := entry.Data.GetContractData()
	if !ok {
		return 0, false
	}
	inst, ok := cd.Val.GetInstance()
	if !ok || inst.Storage == nil {
		return 0, false
	}
	for _, kv := range *inst.Storage {
		sym, ok := kv.Key.GetSym()
		if !ok || string(sym) != "METADATA" || kv.Val.Type != xdr.ScValTypeScvMap || kv.Val.Map == nil {
			continue
		}
		for _, e := range **kv.Val.Map {
			ksym, ok := e.Key.GetSym()
			if !ok || string(ksym) != "decimal" {
				continue
			}
			if u, ok := e.Val.GetU32(); ok && uint32(u) <= maxSaneTokenDecimals {
				return uint32(u), true
			}
			return 0, false
		}
	}
	return 0, false
}
