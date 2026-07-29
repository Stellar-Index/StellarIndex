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

// Comet pool (Balancer-v1 Soroban port) PERSISTENT-storage layout,
// derived from the protocol's public Rust source
// (CometDEX/comet-contracts-v1, contracts/src/c_pool/storage_types.rs
// + metadata.rs @ main, read 2026-07-29):
//
//	DataKey is a #[contracttype] enum of unit variants (NO explicit
//	u32 discriminants), so per the Soroban custom-types spec each
//	variant encodes as a single-element vector:
//	ScvVec[ScvSymbol("<Variant>")].
//
//	DataKey::AllRecordData → Map<Address, Record>, PERSISTENT —
//	the per-token balance records this reader consumes:
//
//	  Record { balance: i128, weight: i128, scalar: i128, index: u32 }
//
//	(#[contracttype] struct → ScvMap keyed by field-name Symbols.)
//	Earlier WASM generations carried a different Record field set
//	(bound/index/denorm/balance); decode is by field name, so only
//	the `balance` i128 is required and either generation reads.
//
// VALIDATED ON R1 2026-07-29: of the two candidate key encodings
// probed, the Vec[Symbol] form matched the real AllRecordData entry for
// the sole mainnet pool (CAS3FL6T…); decode yielded 2 legs —
// ~748k USDC + ~71.7M BLND — plausible Blend-backstop magnitudes. The
// dual-probe stays (cheap, robust to upgrades).
//
// VALIDATE-ON-R1 (original derivation note): layout is source-derived, NOT yet validated against
// real lake entries (no r1 access from the implementing session), and
// the single curated pool (Blend's BLND/USDC backstop, WASM
// 8abc28913035c07411ed5d134e6bfeab4723d97ddd4d1a22a0605d35c94d1a36)
// predates current main — the deployed key encoding could differ. The
// reader therefore probes BOTH plausible encodings of the
// AllRecordData key (Vec[Symbol] per the spec, bare Symbol
// defensively) and decodes whichever the lake holds. An operator
// should confirm with (HTTP port 8123):
//
//	SELECT key_xdr, ledger_seq, entry_xdr != '' AS present
//	FROM stellar.ledger_entries_current FINAL
//	WHERE entry_type = 'contract_data'
//	  AND key_xdr IN (<output of cometPoolKeys for
//	                   comet.MainnetBackstopPool — both candidate
//	                   encodings>)
//
// (exactly one row expected), then cross-check the decoded BLND/USDC
// balances against Blend's public backstop figures. A mismatch shows
// up as the pool landing in the undecodable list — fail-to-absent,
// never a misread number.
const (
	cometRecordDataKeySymbol = "AllRecordData"
	cometFieldBalance        = "balance"
)

// CometPoolLeg is one token's current balance record inside a Comet
// pool. Balance is full-precision i128 (*big.Int, ADR-0003) in token
// base units.
type CometPoolLeg struct {
	Token   string // token contract C-strkey (the record map's key)
	Balance *big.Int
}

// CometPoolState is one Comet pool contract's decoded CURRENT state:
// every bound token's balance from the pool's AllRecordData persistent
// entry in the certified lake. Legs are sorted by token strkey for
// deterministic output.
type CometPoolState struct {
	Pool string // pool contract C-strkey
	Legs []CometPoolLeg
	// Ledger is the ledger_seq at which the record entry last changed
	// — the pool's last interaction; balances are current as of this
	// ledger (and unchanged since).
	Ledger uint32
}

// cometPoolStateQuery is the batched current-state lookup — a
// PK-prefix probe on (entry_type, key_xdr), bounded by construction to
// 2 candidate keys per curated pool (today: one pool). SETTINGS pins
// per the shared guard-rail rationale (ttlLivenessBatchQuery).
const cometPoolStateQuery = `SELECT key_xdr, ledger_seq, entry_xdr
	FROM stellar.ledger_entries_current FINAL
	WHERE entry_type = 'contract_data' AND key_xdr IN (?) AND entry_xdr != ''
	SETTINGS max_threads = 4, max_memory_usage = 8000000000`

// CometPoolReserves reads the CURRENT per-token balance records for
// the given Comet pool contracts from the lake in a single batched
// `key_xdr IN (...)` lookup. Semantics mirror SoroswapPairReserves /
// PhoenixPoolReserves:
//
//   - Pools with NO captured record entry are absent from both
//     returns — absence is "reserves unavailable", never zero.
//   - Pools whose record entry has been TTL-ARCHIVED are absent too.
//   - Pools whose captured entry does NOT decode to the recognised
//     Map<Address, Record{balance: i128, …}> shape come back in the
//     second return (undecodable, sorted) so callers can count them
//     honestly. The whole pool fails on ANY malformed record — a
//     partially-read balance set would understate TVL silently.
func (r *ExplorerReader) CometPoolReserves(ctx context.Context, pools []string) (map[string]CometPoolState, []string, error) {
	keys := make([]string, 0, len(pools)*2)
	poolByKey := make(map[string]string, len(pools)*2)
	for _, pool := range pools {
		poolKeys, err := cometPoolKeys(pool)
		if err != nil {
			return nil, nil, err
		}
		for _, k := range poolKeys {
			poolByKey[k] = pool
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return map[string]CometPoolState{}, nil, nil
	}

	rows, err := r.conn.Query(ctx, cometPoolStateQuery, keys)
	if err != nil {
		return nil, nil, fmt.Errorf("clickhouse: comet pool reserves lookup: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out, keyByPool, badPools, err := scanCometPoolStates(rows, poolByKey)
	if err != nil {
		return nil, nil, err
	}

	undecodable := make([]string, 0, len(badPools))
	for pool := range badPools {
		if _, decoded := out[pool]; !decoded {
			undecodable = append(undecodable, pool)
		}
	}
	sort.Strings(undecodable)

	if err := dropArchivedCometPools(ctx, r.conn, out, keyByPool); err != nil {
		return nil, nil, err
	}
	return out, undecodable, nil
}

// cometPoolKeys builds the candidate persistent LedgerKeys for a
// pool's DataKey::AllRecordData entry — the spec'd Vec[Symbol]
// encoding first, then the defensive bare-Symbol form (see the
// VALIDATE-ON-R1 note above). At most one exists on-chain.
func cometPoolKeys(pool string) ([]string, error) {
	cid, err := contractIDFromStrkey(pool)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: comet pool id %q: %w", pool, err)
	}
	sym := xdr.ScSymbol(cometRecordDataKeySymbol)
	vec := &xdr.ScVec{{Type: xdr.ScValTypeScvSymbol, Sym: &sym}}
	candidates := []xdr.ScVal{
		{Type: xdr.ScValTypeScvVec, Vec: &vec},
		symbolScVal(cometRecordDataKeySymbol),
	}
	keys := make([]string, 0, len(candidates))
	for _, val := range candidates {
		k, err := persistentContractDataKeyXDR(cid, val)
		if err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, nil
}

// scanCometPoolStates folds fetched rows into per-pool winning state.
// badPools collects pools that had a present-but-undecodable entry;
// a pool appears there AND in out only if one candidate key decoded
// while another (theoretically impossible on-chain) did not — the
// caller resolves that in favour of the decoded state.
func scanCometPoolStates(rows driver.Rows, poolByKey map[string]string) (map[string]CometPoolState, map[string]string, map[string]bool, error) {
	out := make(map[string]CometPoolState)
	keyByPool := make(map[string]string)
	badPools := make(map[string]bool)
	for rows.Next() {
		var keyXDR, b64 string
		var ledgerSeq uint32
		if err := rows.Scan(&keyXDR, &ledgerSeq, &b64); err != nil {
			return nil, nil, nil, fmt.Errorf("clickhouse: scan comet pool state: %w", err)
		}
		pool, ok := poolByKey[keyXDR]
		if !ok {
			continue
		}
		legs, ok := cometLegsFromRecordEntry(b64)
		if !ok {
			badPools[pool] = true
			continue
		}
		if prev, dup := out[pool]; dup && prev.Ledger >= ledgerSeq {
			continue
		}
		out[pool] = CometPoolState{Pool: pool, Legs: legs, Ledger: ledgerSeq}
		keyByPool[pool] = keyXDR
	}
	if err := rows.Err(); err != nil {
		return nil, nil, nil, fmt.Errorf("clickhouse: comet pool reserves rows: %w", err)
	}
	return out, keyByPool, badPools, nil
}

// cometLegsFromRecordEntry decodes an AllRecordData entry into
// per-token balance legs. ok=false when the entry isn't the
// recognised Map<Address, Record{balance: i128, …}> shape — ANY
// malformed record fails the whole entry (refuse to partially read).
func cometLegsFromRecordEntry(b64 string) ([]CometPoolLeg, bool) {
	val, ok := contractDataValue(b64)
	if !ok || val.Type != xdr.ScValTypeScvMap || val.Map == nil || *val.Map == nil {
		return nil, false
	}
	legs := make([]CometPoolLeg, 0, len(**val.Map))
	for _, e := range **val.Map {
		token, err := scval.AsAddressStrkey(e.Key)
		if err != nil {
			return nil, false
		}
		balance, ok := cometBalanceFromRecord(e.Val)
		if !ok {
			return nil, false
		}
		legs = append(legs, CometPoolLeg{Token: token, Balance: balance})
	}
	sort.Slice(legs, func(i, j int) bool { return legs[i].Token < legs[j].Token })
	return legs, true
}

// cometBalanceFromRecord pulls the `balance` i128 out of a Record map
// by field name (works across Record field-set generations; the
// SEP-41 map-or-i128 trap class — always type-test, never assume).
func cometBalanceFromRecord(val xdr.ScVal) (*big.Int, bool) {
	if val.Type != xdr.ScValTypeScvMap || val.Map == nil || *val.Map == nil {
		return nil, false
	}
	for _, e := range **val.Map {
		sym, isSym := e.Key.GetSym()
		if !isSym || string(sym) != cometFieldBalance {
			continue
		}
		amt, err := scval.AsAmountFromI128(e.Val)
		if err != nil {
			return nil, false
		}
		return amt.BigInt(), true
	}
	return nil, false
}

// dropArchivedCometPools removes pools whose winning record entry has
// been TTL-archived (same contract as dropArchivedPairs).
func dropArchivedCometPools(ctx context.Context, conn driver.Conn, out map[string]CometPoolState, keyByPool map[string]string) error {
	if len(out) == 0 {
		return nil
	}
	_, asOfLedger, err := entryChangeLedgerBounds(ctx, conn)
	if err != nil {
		return err
	}
	keys := make([]string, 0, len(out))
	for pool := range out {
		if k, ok := keyByPool[pool]; ok {
			keys = append(keys, k)
		}
	}
	liveness, err := ClassifyTTLLiveness(ctx, conn, keys, asOfLedger)
	if err != nil {
		return err
	}
	for pool, k := range keyByPool {
		if liveness[k] == TTLArchived {
			delete(out, pool)
		}
	}
	return nil
}
