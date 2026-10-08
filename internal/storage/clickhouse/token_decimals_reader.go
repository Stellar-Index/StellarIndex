package clickhouse

import (
	"context"
	"fmt"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// maxSaneTokenDecimals bounds the accepted on-chain `decimal`. A token self-declares a u32,
// and unit math divides by 10^decimals, so absurd scales would zero out market-cap and display
// amounts. 38 mirrors the NUMERIC(38) ceiling; above it is "no usable metadata" (default kept).
const maxSaneTokenDecimals = 38

// tokenDecimalsKeys are the METADATA map keys that carry the scale, in preference order.
//
// `decimal` is the soroban-token-sdk's name (SACs, token-sdk builds); hand-written tokens
// use `decimals`, including tokenized Treasury funds, so both are read. A missed reading
// publishes a wrong figure on the RWA surface: a 5-decimal fund read at the default 7 shows one
// hundredth of its capitalisation. Never blended: see [decimalsFromInstanceEntry] for
// disagreement.
var tokenDecimalsKeys = []string{"decimal", "decimals"}

// tokenMetadataMapKeys are the instance-storage keys whose value is a map that may carry the
// scale, in preference order.
//
// `METADATA` is the soroban-token-sdk convention. The private-credit deal tokens have no
// METADATA key; their instance storage holds `Config`, a map whose `decimals` is the scale.
// Reading it keeps a nine-figure published supply on our own measurement, not a third-party
// seed file. As with the field spellings, both maps are read and never blended: a contract
// declaring different scales in each is refused by [decimalsFromInstance].
var tokenMetadataMapKeys = []string{"METADATA", "Config"}

// TokenDecimals resolves a token contract's `decimals()` from the lake. The soroban-token-sdk
// persists TokenMetadata in the contract INSTANCE storage under Symbol "METADATA" as
// Map{decimal: U32, name, symbol}; hand-written tokens use `decimals` in the same map. This is
// the value `decimals()` would return, without executing WASM.
//
// found=false (nil error) when the instance isn't in the lake, no METADATA map exists, or the
// declaration is out of bounds; callers keep their default (7).
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
	// Same table choice as contractWasmHash: ledger_entries_current is merge-loss immune and
	// (entry_type, key_xdr) is a PK-prefix lookup, cheap for the cached asset-detail path.
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

// decimalsFromInstanceEntry decodes one contract-instance LedgerEntry and returns its declared
// scale as read by [decimalsFromInstance]. ok=false when it isn't an instance or no usable
// scale exists: none, out of bounds, or two that DISAGREE. Disagreement is refused, not
// resolved by preference: picking one of two contradictory self-declarations would invent an
// exponent for a money figure, which maxSaneTokenDecimals exists to prevent.
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
	if !ok {
		return 0, false
	}
	return decimalsFromInstance(inst)
}

// decimalsFromInstance reads the declared scale from a decoded instance, under either map
// spelling in [tokenMetadataMapKeys] and either field spelling in [tokenDecimalsKeys].
// ok=false when no scale is stored, one is out of bounds, or two DISAGREE (across map names
// for the same reason as across field names: choosing would invent an exponent).
func decimalsFromInstance(inst xdr.ScContractInstance) (uint32, bool) {
	if inst.Storage == nil {
		return 0, false
	}
	var (
		found bool
		value uint32
	)
	for _, want := range tokenMetadataMapKeys {
		for _, kv := range *inst.Storage {
			// Both key encodings: a bare Symbol (soroban-token-sdk) and the single-element Vec a Rust
			// enum variant derives to. See [instanceStorageKeyName].
			name, ok := instanceStorageKeyName(kv.Key)
			if !ok || name != want || kv.Val.Type != xdr.ScValTypeScvMap || kv.Val.Map == nil {
				continue
			}
			d, decl := decimalsFromMetadataMap(**kv.Val.Map)
			if decl == scaleAbsent {
				// A map with no scale field (an admin Config struct) is
				// not a declaration; the other spelling may still carry one.
				break
			}
			if decl == scaleUnusable {
				// A present-but-unusable declaration is a refusal for the
				// whole instance, matching how it refuses the whole map.
				return 0, false
			}
			if found && value != d {
				return 0, false
			}
			found, value = true, d
			break
		}
	}
	return value, found
}

// scaleDecl is what one instance map says about the token's scale.
type scaleDecl int

const (
	scaleAbsent   scaleDecl = iota // no `decimal`/`decimals` field
	scaleDeclared                  // one usable, self-consistent scale
	scaleUnusable                  // a field mistyped, out of bounds, or two that disagree
)

// decimalsFromMetadataMap reads the scale out of one decoded METADATA map.
func decimalsFromMetadataMap(entries []xdr.ScMapEntry) (uint32, scaleDecl) {
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
				// A present-but-unusable declaration refuses the whole entry, not a reason to try the
				// other spelling: the contract answered, and the answer was not a scale.
				return 0, scaleUnusable
			}
			if found && value != uint32(u) {
				return 0, scaleUnusable
			}
			found, value = true, uint32(u)
			break
		}
	}
	if !found {
		return 0, scaleAbsent
	}
	return value, scaleDeclared
}
