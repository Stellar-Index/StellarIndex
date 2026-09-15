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

// tokenDecimalsKeys are the METADATA map keys that carry the scale, in
// preference order.
//
// `decimal` is the soroban-token-sdk's own field name, and reading only
// it was a MEASURED defect rather than a theoretical gap. Of the 17
// Soroban contract addresses a public listing platform names on Stellar
// (2026-09-15), SEVEN spell it `decimal` and TEN spell it `decimals`:
// the SACs and token-sdk builds take the first, and every hand-written
// token in that sample takes the second — including two tokenized
// Treasury funds holding nine figures of supply.
//
// On a display surface a missed reading costs a wrong amount. On the
// RWA surface it is the published figure: a 5-decimal fund read at the
// caller's default of 7 publishes one HUNDREDTH of its capitalisation,
// and an 18-decimal one read at 7 publishes eleven orders of magnitude
// too much. "Virtually every SEP-41 token follows the SDK" was the
// assumption this list replaces, and the measurement says it was wrong
// for the majority of the population that matters.
//
// Both are read, never blended: see [decimalsFromInstanceEntry] for what
// happens when a contract declares both and they disagree.
var tokenDecimalsKeys = []string{"decimal", "decimals"}

// TokenDecimals resolves a token contract's `decimals()` value from the
// certified lake: the soroban-token-sdk convention — followed by SACs (always
// 7) and by token-sdk-shaped SEP-41 WASM tokens — persists TokenMetadata in
// the contract INSTANCE storage under Symbol "METADATA" as
// Map{decimal: U32, name: String, symbol: String}. Hand-written tokens use
// the same map under the key `decimals`, and both are read. Reading the
// instance entry is exactly the `decimals()` a caller would get from the
// contract, without executing WASM.
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
// returns the METADATA map's declared scale, under either spelling in
// [tokenDecimalsKeys].
//
// ok=false when the entry isn't an instance, carries no METADATA map, the map
// declares no u32 scale under either key, the declared value exceeds
// maxSaneTokenDecimals, or the contract declares BOTH keys with DIFFERENT
// values.
//
// The last case is refused rather than resolved by preference. A contract
// claiming two different scales for itself has not told us its scale, and the
// caller's documented response to ok=false — keep the default — is at least a
// stated convention. Picking one of two contradictory self-declarations would
// be this layer inventing an exponent for a money figure, which is the one
// thing the bound above exists to prevent.
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
		return decimalsFromMetadataMap(**kv.Val.Map)
	}
	return 0, false
}

// decimalsFromMetadataMap reads the scale out of one decoded METADATA map.
func decimalsFromMetadataMap(entries []xdr.ScMapEntry) (uint32, bool) {
	var (
		found bool
		value uint32
	)
	for _, want := range tokenDecimalsKeys {
		for _, e := range entries {
			ksym, ok := e.Key.GetSym()
			if !ok || string(ksym) != want {
				continue
			}
			u, ok := e.Val.GetU32()
			if !ok || uint32(u) > maxSaneTokenDecimals {
				// A present-but-unusable declaration is a refusal for
				// the whole entry, not a reason to try the other
				// spelling: the contract answered, and the answer was
				// not a scale.
				return 0, false
			}
			if found && value != uint32(u) {
				return 0, false
			}
			found, value = true, uint32(u)
			break
		}
	}
	return value, found
}
