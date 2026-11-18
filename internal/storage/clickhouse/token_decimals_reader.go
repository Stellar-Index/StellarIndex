package clickhouse

import (
	"context"
	"fmt"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// maxSaneTokenDecimals bounds the accepted on-chain `decimal`: a token self-declares a u32 and unit math divides by 10^decimals, so
// absurd scales would zero out amounts. 38 mirrors the NUMERIC(38) ceiling; above it is "no usable metadata".
const maxSaneTokenDecimals = 38

// tokenDecimalsKeys are the METADATA map keys that carry the scale, in preference order: `decimal` (soroban-token-sdk, SACs) and
// `decimals` (hand-written tokens). A missed reading publishes a wrong figure (a 5-decimal fund read at the default 7 shows one hundredth
// of its capitalisation). Never blended: see [decimalsFromInstanceEntry].
var tokenDecimalsKeys = []string{"decimal", "decimals"}

// tokenMetadataMapKeys are the instance-storage keys whose map may carry the scale, in preference order: `METADATA` (soroban-token-sdk)
// and `Config` (private-credit deal tokens). Both are read and never blended: differing scales are refused by [decimalsFromInstance].
var tokenMetadataMapKeys = []string{"METADATA", "Config"}

// TokenDecimals resolves a token contract's decimals() from its instance METADATA map without executing WASM; found=false (nil error) when unavailable, and callers serve the scale as unknown, never a default.
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

// tokenDecimalsBatchMax bounds the contracts in one [ExplorerReader.TokenDecimalsBatch] IN-list (two instance keys each).
const tokenDecimalsBatchMax = 500

// TokenDecimalsBatch is [ExplorerReader.TokenDecimals] for many contracts, one read per tokenDecimalsBatchMax ids. A contract absent
// from the map has no usable declaration; any error fails the whole call, so no partial answer can pass as "declares none".
func (r *ExplorerReader) TokenDecimalsBatch(ctx context.Context, contractIDs []string) (map[string]uint32, error) {
	out := make(map[string]uint32, len(contractIDs))
	for start := 0; start < len(contractIDs); start += tokenDecimalsBatchMax {
		if err := r.tokenDecimalsChunk(ctx, contractIDs[start:min(start+tokenDecimalsBatchMax, len(contractIDs))], out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (r *ExplorerReader) tokenDecimalsChunk(ctx context.Context, contractIDs []string, out map[string]uint32) error {
	byKey := make(map[string]string, 2*len(contractIDs))
	keys := make([]string, 0, 2*len(contractIDs))
	for _, id := range contractIDs {
		raw, err := strkey.Decode(strkey.VersionByteContract, id)
		if err != nil {
			return fmt.Errorf("clickhouse: TokenDecimalsBatch: bad contract id %q: %w", id, err)
		}
		var cid xdr.Hash
		copy(cid[:], raw)
		ks, err := instanceKeyXDR(cid)
		if err != nil {
			return err
		}
		for _, k := range ks {
			byKey[k] = id
			keys = append(keys, k)
		}
	}
	// Same table and PK-prefix lookup as TokenDecimals; the newest of a contract's two instance keys wins, as its LIMIT 1 does.
	const q = `SELECT key_xdr, entry_xdr, toUInt32(ledger_seq) FROM stellar.ledger_entries_current FINAL
		WHERE entry_type = 'contract_data' AND key_xdr IN (?) AND entry_xdr != ''`
	rows, err := r.conn.Query(ctx, q, keys)
	if err != nil {
		return fmt.Errorf("clickhouse: token decimals batch scan[%d contracts]: %w", len(contractIDs), err)
	}
	defer func() { _ = rows.Close() }()
	type newest struct {
		seq uint32
		b64 string
	}
	latest := make(map[string]newest, len(contractIDs))
	for rows.Next() {
		var (
			key, b64 string
			seq      uint32
		)
		if err := rows.Scan(&key, &b64, &seq); err != nil {
			return fmt.Errorf("clickhouse: scan token decimals batch: %w", err)
		}
		id, ok := byKey[key]
		if !ok {
			continue
		}
		if cur, seen := latest[id]; !seen || seq > cur.seq {
			latest[id] = newest{seq: seq, b64: b64}
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("clickhouse: token decimals batch rows: %w", err)
	}
	for id, n := range latest {
		if d, ok := decimalsFromInstanceEntry(n.b64); ok {
			out[id] = d
		}
	}
	return nil
}

// decimalsFromInstanceEntry decodes one contract-instance LedgerEntry and returns its declared scale as read by [decimalsFromInstance].
// ok=false when it isn't an instance or no usable scale exists (none, out of bounds, or two that DISAGREE: refused, not resolved by
// preference, since picking one would invent an exponent for a money figure).
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

// decimalsFromInstance reads the declared scale from a decoded instance under either map spelling in [tokenMetadataMapKeys] and either
// field spelling in [tokenDecimalsKeys]. ok=false when none is stored, one is out of bounds, or two DISAGREE (choosing would invent an
// exponent).
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
