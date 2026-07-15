package clickhouse

import (
	"context"
	"fmt"
	"math/big"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// Soroswap pair instance-storage layout (empirically verified against
// the r1 lake, 2026-07-05, two pairs cross-checked against the
// soroswap_pairs registry). The pair contract's DataKey enum is a
// u32-repr enum, so instance-storage keys are plain ScvU32 values:
//
//	U32(0) = Token0   (Address)
//	U32(1) = Token1   (Address)
//	U32(2) = Reserve0 (i128)
//	U32(3) = Reserve1 (i128)
//	U32(4) = FactoryAddress (Address)
//
// plus the LP-share token's METADATA map + Vec[Symbol(TotalSupply)].
const (
	soroswapKeyToken0   = 0
	soroswapKeyToken1   = 1
	soroswapKeyReserve0 = 2
	soroswapKeyReserve1 = 3
)

// SoroswapPairState is one Soroswap pair contract's decoded CURRENT
// state (ADR-0039): post-interaction reserves straight from the pair's
// instance storage in the certified lake. Reserves are full-precision
// i128 (*big.Int, ADR-0003) in token base units.
type SoroswapPairState struct {
	Pair     string // pair contract C-strkey
	Token0   string // token contract C-strkey, from instance storage
	Token1   string
	Reserve0 *big.Int
	Reserve1 *big.Int
	// Ledger is the ledger_seq at which the instance entry last
	// changed — i.e. the pool's last interaction; the reserves are
	// current as of this ledger (and unchanged since).
	Ledger uint32
}

// TokenDisplayMeta is a token contract's display metadata read from
// its instance METADATA entry (soroban-token-sdk convention, same
// entry TokenDecimals reads). Best-effort: HasMeta=false when the
// token stores no readable METADATA map — callers keep their own
// defaults (decimals 7, contract-id label).
type TokenDisplayMeta struct {
	Symbol   string
	Name     string
	Decimals uint32
	HasMeta  bool
}

// SoroswapPairReserves reads the CURRENT reserve state for the given
// Soroswap pair contracts from the lake in a single batched
// `key_xdr IN (...)` lookup against ledger_entries_current (PK-prefix
// on (entry_type, key_xdr) — cheap even for every pair at once).
// Pairs whose instance entry isn't captured, or whose storage doesn't
// match the verified u32-keyed layout, are absent from the result —
// callers treat absence as "reserves unavailable", never as zero.
func (r *ExplorerReader) SoroswapPairReserves(ctx context.Context, pairs []string) (map[string]SoroswapPairState, error) {
	keys := make([]string, 0, len(pairs)*2)
	pairByKey := make(map[string]string, len(pairs)*2)
	for _, p := range pairs {
		poolID, err := contractIDFromStrkey(p)
		if err != nil {
			return nil, fmt.Errorf("clickhouse: soroswap pair id %q: %w", p, err)
		}
		instKeys, err := instanceKeyXDR(xdr.Hash(poolID))
		if err != nil {
			return nil, err
		}
		for _, k := range instKeys {
			pairByKey[k] = p
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return map[string]SoroswapPairState{}, nil
	}

	const q = `SELECT key_xdr, ledger_seq, entry_xdr
		FROM stellar.ledger_entries_current FINAL
		WHERE entry_type = 'contract_data' AND key_xdr IN (?) AND entry_xdr != ''`
	rows, err := r.conn.Query(ctx, q, keys)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: soroswap pair reserves lookup: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]SoroswapPairState, len(pairs))
	for rows.Next() {
		var keyXDR, b64 string
		var ledgerSeq uint32
		if err := rows.Scan(&keyXDR, &ledgerSeq, &b64); err != nil {
			return nil, fmt.Errorf("clickhouse: scan soroswap pair state: %w", err)
		}
		pair, ok := pairByKey[keyXDR]
		if !ok {
			continue
		}
		st, ok := soroswapStateFromInstanceEntry(b64)
		if !ok {
			continue
		}
		st.Pair = pair
		st.Ledger = ledgerSeq
		// Two durabilities are keyed per pair; only one entry exists
		// on-chain, but keep the higher-ledger state defensively.
		if prev, dup := out[pair]; dup && prev.Ledger >= st.Ledger {
			continue
		}
		out[pair] = st
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("clickhouse: soroswap pair reserves rows: %w", err)
	}
	return out, nil
}

// TokenDisplays batch-reads display metadata (symbol/name/decimals)
// for the given token contracts from their instance METADATA entries.
// Same single-query batching as SoroswapPairReserves. Tokens without
// readable metadata are present with HasMeta=false.
func (r *ExplorerReader) TokenDisplays(ctx context.Context, tokens []string) (map[string]TokenDisplayMeta, error) {
	keys := make([]string, 0, len(tokens)*2)
	tokByKey := make(map[string]string, len(tokens)*2)
	for _, t := range tokens {
		cid, err := contractIDFromStrkey(t)
		if err != nil {
			continue // skip malformed ids — best-effort display surface
		}
		instKeys, err := instanceKeyXDR(xdr.Hash(cid))
		if err != nil {
			continue
		}
		for _, k := range instKeys {
			tokByKey[k] = t
			keys = append(keys, k)
		}
	}
	out := make(map[string]TokenDisplayMeta, len(tokens))
	for _, t := range tokens {
		out[t] = TokenDisplayMeta{}
	}
	if len(keys) == 0 {
		return out, nil
	}

	const q = `SELECT key_xdr, entry_xdr
		FROM stellar.ledger_entries_current FINAL
		WHERE entry_type = 'contract_data' AND key_xdr IN (?) AND entry_xdr != ''`
	rows, err := r.conn.Query(ctx, q, keys)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: token displays lookup: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var keyXDR, b64 string
		if err := rows.Scan(&keyXDR, &b64); err != nil {
			return nil, fmt.Errorf("clickhouse: scan token display: %w", err)
		}
		tok, ok := tokByKey[keyXDR]
		if !ok {
			continue
		}
		if meta, ok := displayMetaFromInstanceEntry(b64); ok {
			out[tok] = meta
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("clickhouse: token displays rows: %w", err)
	}
	return out, nil
}

// soroswapStateFromInstanceEntry decodes one pair-contract instance
// LedgerEntry into token addresses + reserves. ok=false when the entry
// isn't an instance or any of the four u32-keyed fields is missing or
// mis-typed (a non-Soroswap contract, or a future layout change —
// refuse to guess rather than misreport reserves).
func soroswapStateFromInstanceEntry(b64 string) (SoroswapPairState, bool) {
	var entry xdr.LedgerEntry
	if xdr.SafeUnmarshalBase64(b64, &entry) != nil {
		return SoroswapPairState{}, false
	}
	cd, ok := entry.Data.GetContractData()
	if !ok {
		return SoroswapPairState{}, false
	}
	inst, ok := cd.Val.GetInstance()
	if !ok || inst.Storage == nil {
		return SoroswapPairState{}, false
	}

	var st SoroswapPairState
	var have int
	for _, kv := range *inst.Storage {
		u, ok := kv.Key.GetU32()
		if !ok {
			continue
		}
		matched, err := applySoroswapField(&st, uint32(u), kv.Val)
		if err != nil {
			return SoroswapPairState{}, false
		}
		if matched {
			have++
		}
	}
	if have != 4 || st.Token0 == "" || st.Token1 == "" || st.Reserve0 == nil || st.Reserve1 == nil {
		return SoroswapPairState{}, false
	}
	return st, true
}

// applySoroswapField assigns one u32-keyed instance-storage field onto
// the state. matched=false for keys outside the four we read (e.g. the
// factory address); err != nil when a known key holds the wrong type.
func applySoroswapField(st *SoroswapPairState, key uint32, val xdr.ScVal) (matched bool, err error) {
	switch key {
	case soroswapKeyToken0, soroswapKeyToken1:
		addr, err := scval.AsAddressStrkey(val)
		if err != nil {
			return false, err
		}
		if key == soroswapKeyToken0 {
			st.Token0 = addr
		} else {
			st.Token1 = addr
		}
		return true, nil
	case soroswapKeyReserve0, soroswapKeyReserve1:
		amt, err := scval.AsAmountFromI128(val)
		if err != nil {
			return false, err
		}
		if key == soroswapKeyReserve0 {
			st.Reserve0 = amt.BigInt()
		} else {
			st.Reserve1 = amt.BigInt()
		}
		return true, nil
	}
	return false, nil
}

// displayMetaFromInstanceEntry decodes a token instance entry's
// METADATA map into TokenDisplayMeta. ok=false when no usable
// METADATA is present. Decimals above maxSaneTokenDecimals are
// rejected wholesale (same bound + rationale as TokenDecimals).
func displayMetaFromInstanceEntry(b64 string) (TokenDisplayMeta, bool) {
	var entry xdr.LedgerEntry
	if xdr.SafeUnmarshalBase64(b64, &entry) != nil {
		return TokenDisplayMeta{}, false
	}
	cd, ok := entry.Data.GetContractData()
	if !ok {
		return TokenDisplayMeta{}, false
	}
	inst, ok := cd.Val.GetInstance()
	if !ok || inst.Storage == nil {
		return TokenDisplayMeta{}, false
	}
	for _, kv := range *inst.Storage {
		sym, ok := kv.Key.GetSym()
		if !ok || string(sym) != "METADATA" || kv.Val.Type != xdr.ScValTypeScvMap || kv.Val.Map == nil {
			continue
		}
		return parseTokenMetadataMap(**kv.Val.Map)
	}
	return TokenDisplayMeta{}, false
}

// parseTokenMetadataMap reads decimal/symbol/name out of a token-sdk
// METADATA map. ok=false when the decimal declaration is absent or
// out of sane bounds (the whole entry is then treated as unusable).
func parseTokenMetadataMap(m xdr.ScMap) (TokenDisplayMeta, bool) {
	var meta TokenDisplayMeta
	for _, e := range m {
		ksym, ok := e.Key.GetSym()
		if !ok {
			continue
		}
		switch string(ksym) {
		case "decimal":
			u, ok := e.Val.GetU32()
			if !ok || uint32(u) > maxSaneTokenDecimals {
				return TokenDisplayMeta{}, false
			}
			meta.Decimals = uint32(u)
			meta.HasMeta = true
		case "symbol":
			if s, ok := e.Val.GetStr(); ok {
				meta.Symbol = string(s)
			}
		case "name":
			if s, ok := e.Val.GetStr(); ok {
				meta.Name = string(s)
			}
		}
	}
	return meta, meta.HasMeta
}
