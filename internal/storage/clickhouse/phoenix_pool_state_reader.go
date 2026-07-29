package clickhouse

import (
	"context"
	"fmt"
	"math/big"
	"sort"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// Phoenix pool PERSISTENT-storage layout, derived from the protocol's
// public Rust source (Phoenix-Protocol-Group/phoenix-contracts,
// contracts/pool/src/storage.rs @ main, read 2026-07-29):
//
//	DataKey is a #[repr(u32)] enum with a manual `TryFromVal<Env,
//	DataKey> for Val` impl of `(*v as u32).into()` — so its storage
//	keys are plain ScvU32 values (same convention the Soroswap pair
//	contract uses):
//
//	  U32(0) = TotalShares (i128)
//	  U32(1) = ReserveA    (i128)   ← read
//	  U32(2) = ReserveB    (i128)   ← read
//	  U32(3) = Admin
//	  U32(4) = Initialized
//
//	plus `const CONFIG: Symbol = symbol_short!("CONFIG")` → a
//	#[contracttype] Config struct (ScvMap keyed by field-name
//	Symbols) whose `token_a` / `token_b` Address fields carry the
//	pool's token identities.                                  ← read
//
// All three live in PERSISTENT durability (env.storage().persistent()
// throughout storage.rs), i.e. standalone contract_data entries under
// the pool contract — NOT the contract-instance entry.
//
// VALIDATED ON R1 2026-07-29: all 6 storage keys for two curated pools
// (CBHCRSVX…, CBCZGGNO…) matched real ledger_entries_current rows and
// decoded cleanly — reserves at stroop scale + CONFIG token pairs
// (PHO/USDC-class addresses) consistent with known pools. The layout
// below is confirmed against the DEPLOYED WASM, not just source. Re-run
// the query below after any Phoenix contract upgrade.
//
// VALIDATE-ON-R1 (original derivation note): this layout is source-derived, NOT yet validated
// against real lake entries (no r1 access from the implementing
// session), and Phoenix pools upgrade in place — two pool-WASM
// generations are already curated (phoenix.MainnetPools vs
// phoenix.MainnetMapPools), so the deployed storage shape may differ
// from main. An operator should confirm with (HTTP port 8123):
//
//	SELECT key_xdr, ledger_seq, base64Decode(entry_xdr) IS NOT NULL
//	FROM stellar.ledger_entries_current FINAL
//	WHERE entry_type = 'contract_data'
//	  AND key_xdr IN (<output of phoenixPoolKeys for one curated pool,
//	                   e.g. CBHCRSVX3ZZ7EGTSYMKPEFGZNWRVCSESQR3UABET4MIW52N4EVU6BIZX>)
//
// (three rows expected: two ScvU32-keyed i128s + the CONFIG map), then
// cross-check the decoded reserves against the pool's latest
// phoenix_trades post-state fields. A mismatch shows up as pools in
// the undecodable list — fail-to-absent, never a misread number.
const (
	phoenixKeyReserveA = 1 // DataKey::ReserveA
	phoenixKeyReserveB = 2 // DataKey::ReserveB

	phoenixConfigKeySymbol = "CONFIG"
	phoenixFieldTokenA     = "token_a"
	phoenixFieldTokenB     = "token_b"
)

// PhoenixPoolState is one Phoenix pool contract's decoded CURRENT
// state: post-interaction reserves + token identities straight from
// the pool's persistent storage in the certified lake. Reserves are
// full-precision i128 (*big.Int, ADR-0003) in token base units.
type PhoenixPoolState struct {
	Pool     string // pool contract C-strkey
	TokenA   string // token contract C-strkey, from the CONFIG entry
	TokenB   string
	ReserveA *big.Int
	ReserveB *big.Int
	// Ledger is the highest ledger_seq across the pool's decoded
	// entries — its last interaction; the reserves are current as of
	// this ledger (and unchanged since).
	Ledger uint32
}

// phoenixPoolStateQuery is the batched current-state lookup — a
// PK-prefix probe on (entry_type, key_xdr), bounded by construction to
// 3 keys per curated pool. The SETTINGS pins are guard rails (see
// ttlLivenessBatchQuery's rationale): the read is cheap, but a planner
// or layout shift must fail THIS query loudly rather than fan out on
// the shared host.
const phoenixPoolStateQuery = `SELECT key_xdr, ledger_seq, entry_xdr
	FROM stellar.ledger_entries_current FINAL
	WHERE entry_type = 'contract_data' AND key_xdr IN (?) AND entry_xdr != ''
	SETTINGS max_threads = 4, max_memory_usage = 8000000000`

// phoenixKeyKind discriminates which of a pool's three storage entries
// a fetched row is.
type phoenixKeyKind int

const (
	phoenixKindReserveA phoenixKeyKind = iota
	phoenixKindReserveB
	phoenixKindConfig
)

// phoenixKeyRef maps a built storage key back to (pool, kind).
type phoenixKeyRef struct {
	pool string
	kind phoenixKeyKind
}

// phoenixPoolParts accumulates one pool's fetched entries until the
// full (token_a, token_b, reserve_a, reserve_b) set is known.
type phoenixPoolParts struct {
	tokenA, tokenB     string
	reserveA, reserveB *big.Int
	ledger             uint32
	shapeBad           bool
}

// complete reports whether every field the TVL surface needs decoded.
func (p *phoenixPoolParts) complete() bool {
	return !p.shapeBad && p.tokenA != "" && p.tokenB != "" && p.reserveA != nil && p.reserveB != nil
}

// PhoenixPoolReserves reads the CURRENT reserve state for the given
// Phoenix pool contracts from the lake in a single batched
// `key_xdr IN (...)` lookup (3 persistent keys per pool). Semantics
// mirror SoroswapPairReserves:
//
//   - Pools with NO captured entries are absent from both returns —
//     absence is "reserves unavailable", never zero.
//   - Pools whose entries have been TTL-ARCHIVED are absent too (a
//     dead pool's last-known reserves are not current liquidity;
//     only a positively-resolved lapsed TTL drops a pool).
//   - Pools with captured entries that do NOT decode to the verified
//     shape — or with a partial entry set — come back in the second
//     return (undecodable, sorted): PRESENT on-chain but unreadable,
//     so callers can count them honestly instead of fabricating or
//     silently dropping. Never partially decoded.
func (r *ExplorerReader) PhoenixPoolReserves(ctx context.Context, pools []string) (map[string]PhoenixPoolState, []string, error) {
	keys, refByKey, err := phoenixPoolKeys(pools)
	if err != nil {
		return nil, nil, err
	}
	if len(keys) == 0 {
		return map[string]PhoenixPoolState{}, nil, nil
	}

	rows, err := r.conn.Query(ctx, phoenixPoolStateQuery, keys)
	if err != nil {
		return nil, nil, fmt.Errorf("clickhouse: phoenix pool reserves lookup: %w", err)
	}
	defer func() { _ = rows.Close() }()

	parts, err := scanPhoenixPoolParts(rows, refByKey)
	if err != nil {
		return nil, nil, err
	}

	out := make(map[string]PhoenixPoolState, len(parts))
	var undecodable []string
	for pool, p := range parts {
		if !p.complete() {
			undecodable = append(undecodable, pool)
			continue
		}
		out[pool] = PhoenixPoolState{
			Pool:     pool,
			TokenA:   p.tokenA,
			TokenB:   p.tokenB,
			ReserveA: p.reserveA,
			ReserveB: p.reserveB,
			Ledger:   p.ledger,
		}
	}
	sort.Strings(undecodable)

	if err := dropArchivedPhoenixPools(ctx, r.conn, out, refByKey); err != nil {
		return nil, nil, err
	}
	return out, undecodable, nil
}

// phoenixPoolKeys builds the 3 persistent storage keys per pool and
// the reverse index from key to (pool, kind).
func phoenixPoolKeys(pools []string) ([]string, map[string]phoenixKeyRef, error) {
	keys := make([]string, 0, len(pools)*3)
	refByKey := make(map[string]phoenixKeyRef, len(pools)*3)
	for _, pool := range pools {
		cid, err := contractIDFromStrkey(pool)
		if err != nil {
			return nil, nil, fmt.Errorf("clickhouse: phoenix pool id %q: %w", pool, err)
		}
		for _, spec := range []struct {
			kind phoenixKeyKind
			val  xdr.ScVal
		}{
			{phoenixKindReserveA, u32ScVal(phoenixKeyReserveA)},
			{phoenixKindReserveB, u32ScVal(phoenixKeyReserveB)},
			{phoenixKindConfig, symbolScVal(phoenixConfigKeySymbol)},
		} {
			k, err := persistentContractDataKeyXDR(cid, spec.val)
			if err != nil {
				return nil, nil, err
			}
			refByKey[k] = phoenixKeyRef{pool: pool, kind: spec.kind}
			keys = append(keys, k)
		}
	}
	return keys, refByKey, nil
}

// scanPhoenixPoolParts folds fetched rows into per-pool parts.
func scanPhoenixPoolParts(rows driver.Rows, refByKey map[string]phoenixKeyRef) (map[string]*phoenixPoolParts, error) {
	parts := make(map[string]*phoenixPoolParts)
	for rows.Next() {
		var keyXDR, b64 string
		var ledgerSeq uint32
		if err := rows.Scan(&keyXDR, &ledgerSeq, &b64); err != nil {
			return nil, fmt.Errorf("clickhouse: scan phoenix pool state: %w", err)
		}
		ref, ok := refByKey[keyXDR]
		if !ok {
			continue
		}
		p := parts[ref.pool]
		if p == nil {
			p = &phoenixPoolParts{}
			parts[ref.pool] = p
		}
		if ledgerSeq > p.ledger {
			p.ledger = ledgerSeq
		}
		applyPhoenixEntry(p, ref.kind, b64)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("clickhouse: phoenix pool reserves rows: %w", err)
	}
	return parts, nil
}

// applyPhoenixEntry decodes one fetched entry onto the pool's parts,
// marking the pool shape-bad (→ undecodable, never misread) when the
// entry doesn't match the source-derived layout.
func applyPhoenixEntry(p *phoenixPoolParts, kind phoenixKeyKind, b64 string) {
	switch kind {
	case phoenixKindReserveA, phoenixKindReserveB:
		amt, ok := phoenixReserveFromEntry(b64)
		if !ok {
			p.shapeBad = true
			return
		}
		if kind == phoenixKindReserveA {
			p.reserveA = amt
		} else {
			p.reserveB = amt
		}
	case phoenixKindConfig:
		tokenA, tokenB, ok := phoenixTokensFromConfigEntry(b64)
		if !ok {
			p.shapeBad = true
			return
		}
		p.tokenA, p.tokenB = tokenA, tokenB
	}
}

// phoenixReserveFromEntry decodes a ReserveA/ReserveB entry. The value
// must be a bare i128 (fail-to-absent on anything else — a schema
// change or a non-Phoenix contract, never a guess).
func phoenixReserveFromEntry(b64 string) (*big.Int, bool) {
	val, ok := contractDataValue(b64)
	if !ok {
		return nil, false
	}
	amt, err := scval.AsAmountFromI128(val)
	if err != nil {
		return nil, false
	}
	return amt.BigInt(), true
}

// phoenixTokensFromConfigEntry decodes the CONFIG entry's token_a /
// token_b Address fields, by field name (contract-schema-evolution:
// never by position). Extra/unknown fields are ignored; a missing or
// mis-typed token field fails the whole entry.
func phoenixTokensFromConfigEntry(b64 string) (tokenA, tokenB string, ok bool) {
	val, okVal := contractDataValue(b64)
	if !okVal || val.Type != xdr.ScValTypeScvMap || val.Map == nil || *val.Map == nil {
		return "", "", false
	}
	for _, e := range **val.Map {
		sym, isSym := e.Key.GetSym()
		if !isSym {
			continue
		}
		switch string(sym) {
		case phoenixFieldTokenA, phoenixFieldTokenB:
			addr, err := scval.AsAddressStrkey(e.Val)
			if err != nil {
				return "", "", false
			}
			if string(sym) == phoenixFieldTokenA {
				tokenA = addr
			} else {
				tokenB = addr
			}
		}
	}
	if tokenA == "" || tokenB == "" {
		return "", "", false
	}
	return tokenA, tokenB, true
}

// dropArchivedPhoenixPools removes pools with ANY TTL-archived entry
// among their three keys — a lapsed pool's last-known reserves must
// not be reported as current (same contract as dropArchivedPairs;
// only a positively-resolved lapsed TTL drops a pool).
func dropArchivedPhoenixPools(ctx context.Context, conn driver.Conn, out map[string]PhoenixPoolState, refByKey map[string]phoenixKeyRef) error {
	if len(out) == 0 {
		return nil
	}
	_, asOfLedger, err := entryChangeLedgerBounds(ctx, conn)
	if err != nil {
		return err
	}
	keys := make([]string, 0, len(out)*3)
	for k, ref := range refByKey {
		if _, live := out[ref.pool]; live {
			keys = append(keys, k)
		}
	}
	liveness, err := ClassifyTTLLiveness(ctx, conn, keys, asOfLedger)
	if err != nil {
		return err
	}
	for k, ref := range refByKey {
		if liveness[k] == TTLArchived {
			delete(out, ref.pool)
		}
	}
	return nil
}

// u32ScVal / symbolScVal are tiny ScVal constructors for storage keys.
func u32ScVal(v uint32) xdr.ScVal {
	u := xdr.Uint32(v)
	return xdr.ScVal{Type: xdr.ScValTypeScvU32, U32: &u}
}

func symbolScVal(s string) xdr.ScVal {
	sym := xdr.ScSymbol(s)
	return xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym}
}

// persistentContractDataKeyXDR builds the base64 LedgerKey for a
// PERSISTENT contract_data entry under `cid` with storage key `key` —
// matching the lake's key_xdr column verbatim.
func persistentContractDataKeyXDR(cid xdr.ContractId, key xdr.ScVal) (string, error) {
	id := cid
	lk := xdr.LedgerKey{
		Type: xdr.LedgerEntryTypeContractData,
		ContractData: &xdr.LedgerKeyContractData{
			Contract:   xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &id},
			Key:        key,
			Durability: xdr.ContractDataDurabilityPersistent,
		},
	}
	b64, err := xdr.MarshalBase64(lk)
	if err != nil {
		return "", fmt.Errorf("clickhouse: marshal contract_data key: %w", err)
	}
	return b64, nil
}
