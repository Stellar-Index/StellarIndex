package clickhouse

import (
	"context"
	"fmt"
	"math/big"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// BackstopBalance is one backstop PoolBalance entry: shares issued, tokens held and shares queued for withdrawal.
type BackstopBalance struct {
	Shares *big.Int
	Tokens *big.Int
	Q4W    *big.Int
}

// BackstopTierState is one backstop tier's balance; Balance is nil when it cannot be read (unavailable, never a guessed zero).
type BackstopTierState struct {
	Tier    string // "" for Blend's single, unnamed tier
	Balance *BackstopBalance
	// Asset, Token and TakeRateWeight are a BLNT tier's PoolBackstopConfig entry; zero when the configuration is unread.
	Asset          string // BackstopAsset: BlntXlm | BlntUsdc | Usdc | Xlm
	Token          string // that asset's token, from the backstop instance
	TakeRateWeight *uint32
}

// LendingBackstopState is a lending pool's backstop as contract storage holds it.
type LendingBackstopState struct {
	Instance *PoolInstance // nil when the pool instance is missing, undecodable or archived; then nothing else is read
	Token    string        // Blend backstop instance "BToken"; "" when unread, and always for BLNT, whose tiers carry their own
	Tiers    []BackstopTierState
}

// blntBackstopTiers are BLNT's loss tiers in waterfall order, which is also their index in PoolBackstopConfig. SecondLoss lives
// under the pre-tier PoolBalance(pool) key and the others under TierPoolBalance(PoolTierKey{pool, tier}) (backstop storage.rs
// get_pool_balance_for_tier), so no key holds a total.
var blntBackstopTiers = []string{"FirstLoss", "SecondLoss", "ThirdLoss"}

// blntAssetTokenKey is the backstop instance key holding each BackstopAsset's token (backstop pool.rs asset_token).
var blntAssetTokenKey = map[string]string{"BlntXlm": "BXLMTkn", "BlntUsdc": "BToken", "Usdc": "USDCTkn", "Xlm": "XLMTkn"}

const (
	backstopKindBalance  = "PoolBalance"
	backstopKindInstance = "Instance"
	backstopKindConfig   = "PoolBackstopConfig"
)

// LendingBackstop reads a pool's current backstop balances from the backstop contract its instance names: Blend's one
// PoolBalance(pool) and backstop token, or BLNT's three tiers when tiered.
func (r *ExplorerReader) LendingBackstop(ctx context.Context, pool string, tiered bool) (LendingBackstopState, error) {
	poolID, err := contractIDFromStrkey(pool)
	if err != nil {
		return LendingBackstopState{}, fmt.Errorf("clickhouse: lending pool id %q: %w", pool, err)
	}
	instKeys, err := instanceKeyXDR(xdr.Hash(poolID))
	if err != nil {
		return LendingBackstopState{}, fmt.Errorf("clickhouse: lending pool instance key: %w", err)
	}
	vals, err := r.liveContractData(ctx, instKeys)
	if err != nil {
		return LendingBackstopState{}, err
	}
	var out LendingBackstopState
	for _, v := range vals {
		if pi, ok := poolInstanceFromStorage(v); ok {
			out.Instance = &pi
		}
	}
	if out.Instance == nil || out.Instance.Backstop == "" {
		return out, nil
	}
	backstopID, err := contractIDFromStrkey(out.Instance.Backstop)
	if err != nil {
		return LendingBackstopState{}, fmt.Errorf("clickhouse: backstop id %q: %w", out.Instance.Backstop, err)
	}
	keys, refByKey, err := backstopKeys(backstopID, poolID, tiered)
	if err != nil {
		return LendingBackstopState{}, err
	}
	vals, archived, err := r.contractData(ctx, keys)
	if err != nil {
		return LendingBackstopState{}, err
	}
	out.Token, out.Tiers = assembleBackstop(vals, archived, refByKey, tiered)
	return out, nil
}

// backstopRead is the decoded backstop entries of one read.
type backstopRead struct {
	instance *xdr.ScVal
	config   []blntTierConfig // nil when unread
	balances map[string]*BackstopBalance
	unusable map[string]bool // tier's entry exists but is archived or undecodable
}

type blntTierConfig struct {
	asset  string
	weight uint32
}

// assembleBackstop decodes the read entries into tiers in waterfall order. A BLNT pool with a configuration serves only its
// configured tiers, and a configured tier with no entry as zero: the contract reads a missing balance as zero
// (get_persistent_default). An archived or undecodable entry is unavailable, never zero.
func assembleBackstop(vals map[string]xdr.ScVal, archived map[string]bool, refByKey map[string]keyRef, tiered bool) (string, []BackstopTierState) {
	rd := backstopRead{balances: map[string]*BackstopBalance{}, unusable: map[string]bool{}}
	for k, v := range vals {
		rd.add(refByKey[k], v, archived[k])
	}
	for k := range archived {
		if _, ok := vals[k]; !ok {
			rd.add(refByKey[k], xdr.ScVal{}, true)
		}
	}
	if !tiered {
		token := ""
		if rd.instance != nil {
			token = instanceAddress(*rd.instance, "BToken")
		}
		return token, []BackstopTierState{{Balance: rd.balances[""]}}
	}
	if rd.config == nil {
		out := make([]BackstopTierState, 0, len(blntBackstopTiers))
		for _, t := range blntBackstopTiers {
			out = append(out, BackstopTierState{Tier: t, Balance: rd.balances[t]})
		}
		return "", out
	}
	out := make([]BackstopTierState, 0, len(rd.config))
	for i, c := range rd.config {
		t := blntBackstopTiers[i]
		st := BackstopTierState{Tier: t, Balance: rd.balances[t], Asset: c.asset, TakeRateWeight: &c.weight}
		if st.Balance == nil && !rd.unusable[t] {
			st.Balance = &BackstopBalance{Shares: new(big.Int), Tokens: new(big.Int), Q4W: new(big.Int)}
		}
		if rd.instance != nil {
			st.Token = instanceAddress(*rd.instance, blntAssetTokenKey[c.asset])
		}
		out = append(out, st)
	}
	return "", out
}

func (rd *backstopRead) add(ref keyRef, v xdr.ScVal, archived bool) {
	switch ref.kind {
	case backstopKindInstance:
		if !archived {
			rd.instance = &v
		}
	case backstopKindConfig:
		// Immutable once cached (storage.rs get_pool_backstop_config), so an archived entry's value is still the configuration.
		if cfg, err := decodeBLNTBackstopConfig(v); err == nil {
			rd.config = cfg
		}
	default:
		b, err := decodeBackstopBalance(v)
		if archived || err != nil {
			rd.unusable[ref.asset] = true
			return
		}
		rd.balances[ref.asset] = &b
	}
}

// decodeBLNTBackstopConfig decodes Vec<BackstopTierConfig{asset: BackstopAsset, take_rate_weight: u32}>, indexed by tier.
func decodeBLNTBackstopConfig(v xdr.ScVal) ([]blntTierConfig, error) {
	vec, err := scval.AsVec(v)
	if err != nil {
		return nil, fmt.Errorf("PoolBackstopConfig shape: %w", err)
	}
	if len(vec) == 0 || len(vec) > len(blntBackstopTiers) {
		return nil, fmt.Errorf("PoolBackstopConfig has %d tiers", len(vec))
	}
	out := make([]blntTierConfig, 0, len(vec))
	for _, tv := range vec {
		entries, err := scval.AsMap(tv)
		if err != nil {
			return nil, fmt.Errorf("BackstopTierConfig shape: %w", err)
		}
		av, okA := scval.MapField(entries, "asset")
		wv, okW := scval.MapField(entries, "take_rate_weight")
		if !okA || !okW {
			return nil, fmt.Errorf("BackstopTierConfig missing a field")
		}
		asset, err := unitVariant(av)
		if err != nil {
			return nil, err
		}
		if _, known := blntAssetTokenKey[asset]; !known {
			return nil, fmt.Errorf("BackstopAsset %q unknown", asset)
		}
		w, err := scval.AsU32(wv)
		if err != nil {
			return nil, fmt.Errorf("take_rate_weight: %w", err)
		}
		out = append(out, blntTierConfig{asset: asset, weight: w})
	}
	return out, nil
}

// unitVariant decodes a contracttype unit enum variant, encoded as Vec[Symbol].
func unitVariant(v xdr.ScVal) (string, error) {
	vec, err := scval.AsVec(v)
	if err != nil {
		return "", fmt.Errorf("enum variant shape: %w", err)
	}
	if len(vec) != 1 {
		return "", fmt.Errorf("enum variant shape: %d elements, want 1", len(vec))
	}
	return scval.AsSymbol(vec[0])
}

// backstopKeys builds the backstop entries to fetch; keyRef.asset carries the tier name.
func backstopKeys(backstopID, poolID xdr.ContractId, tiered bool) ([]string, map[string]keyRef, error) {
	type spec struct {
		variant string
		arg     xdr.ScVal
		ref     keyRef
	}
	specs := []spec{{"PoolBalance", contractAddressVal(poolID), keyRef{kind: backstopKindBalance}}}
	if tiered {
		specs = []spec{{"PoolBackstopConfig", contractAddressVal(poolID), keyRef{kind: backstopKindConfig}}}
		for _, tier := range blntBackstopTiers {
			s := spec{"TierPoolBalance", poolTierKeyVal(poolID, tier), keyRef{asset: tier, kind: backstopKindBalance}}
			if tier == "SecondLoss" {
				s.variant, s.arg = "PoolBalance", contractAddressVal(poolID)
			}
			specs = append(specs, s)
		}
	}
	refByKey := map[string]keyRef{}
	for _, sp := range specs {
		k, err := contractDataKeyXDR(backstopID, sp.variant, sp.arg)
		if err != nil {
			return nil, nil, fmt.Errorf("clickhouse: backstop %s key: %w", sp.variant, err)
		}
		refByKey[k] = sp.ref
	}
	instKeys, err := instanceKeyXDR(xdr.Hash(backstopID))
	if err != nil {
		return nil, nil, fmt.Errorf("clickhouse: backstop instance key: %w", err)
	}
	for _, k := range instKeys {
		refByKey[k] = keyRef{kind: backstopKindInstance}
	}
	keys := make([]string, 0, len(refByKey))
	for k := range refByKey {
		keys = append(keys, k)
	}
	return keys, refByKey, nil
}

// poolTierKeyVal is the contracttype struct PoolTierKey{pool, tier}: a Map with symbol keys in sorted order, the unit-variant tier
// enum as Vec[Symbol].
func poolTierKeyVal(poolID xdr.ContractId, tier string) xdr.ScVal {
	poolSym, tierSym, tierName := xdr.ScSymbol("pool"), xdr.ScSymbol("tier"), xdr.ScSymbol(tier)
	tierVec := &xdr.ScVec{{Type: xdr.ScValTypeScvSymbol, Sym: &tierName}}
	m := &xdr.ScMap{
		{Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &poolSym}, Val: contractAddressVal(poolID)},
		{Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &tierSym}, Val: xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &tierVec}},
	}
	return xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &m}
}

// decodeBackstopBalance decodes a PoolBalance struct {q4w, shares, tokens: i128} with the full 128 bits.
func decodeBackstopBalance(v xdr.ScVal) (BackstopBalance, error) {
	entries, err := scval.AsMap(v)
	if err != nil {
		return BackstopBalance{}, fmt.Errorf("PoolBalance shape: %w", err)
	}
	var out BackstopBalance
	for _, f := range []struct {
		name string
		dst  **big.Int
	}{{"shares", &out.Shares}, {"tokens", &out.Tokens}, {"q4w", &out.Q4W}} {
		sv, ok := scval.MapField(entries, f.name)
		if !ok {
			return BackstopBalance{}, fmt.Errorf("PoolBalance missing %q", f.name)
		}
		a, err := scval.AsAmountFromI128(sv)
		if err != nil {
			return BackstopBalance{}, fmt.Errorf("PoolBalance %s: %w", f.name, err)
		}
		*f.dst = a.BigInt()
	}
	return out, nil
}

// instanceAddress returns the address under a symbol key of an instance entry's storage, "" when absent.
func instanceAddress(v xdr.ScVal, key string) string {
	inst, ok := v.GetInstance()
	if !ok || inst.Storage == nil {
		return ""
	}
	for _, e := range *inst.Storage {
		if e.Key.Type == xdr.ScValTypeScvSymbol && e.Key.Sym != nil && string(*e.Key.Sym) == key {
			if a, err := scval.AsAddressStrkey(e.Val); err == nil {
				return a
			}
		}
	}
	return ""
}

// liveContractData is contractData without the TTL-archived entries.
func (r *ExplorerReader) liveContractData(ctx context.Context, keys []string) (map[string]xdr.ScVal, error) {
	vals, archived, err := r.contractData(ctx, keys)
	for k := range archived {
		delete(vals, k)
	}
	return vals, err
}

// contractDataVersionQuery is blendReserveStateQuery that also returns a removed key's winning row (empty entry_xdr), so the
// removal ledger is known; the empty-entry filter moves to the caller, still after the FINAL collapse.
const contractDataVersionQuery = `SELECT key_xdr, entry_xdr, ledger_seq
	FROM stellar.ledger_entries_current FINAL
	WHERE entry_type = 'contract_data' AND key_xdr IN (?)
	SETTINGS max_threads = 4, max_memory_usage = 8000000000, optimize_move_to_prewhere_if_final = 0`

// contractEntries is one batched contract_data read. archived marks a TTL-archived entry, or an undecodable one (no value);
// either is unreadable, never zero. ledger is a present entry's last-modified ledger or a removed entry's removal ledger; a
// key the lake never saw is in no map.
type contractEntries struct {
	vals     map[string]xdr.ScVal
	archived map[string]bool
	ledger   map[string]uint32
}

// contractData reads current contract_data values by key and reports the TTL-archived or undecodable ones (the projection keeps
// a lapsed entry's last value); a key with no row is absent from both.
func (r *ExplorerReader) contractData(ctx context.Context, keys []string) (map[string]xdr.ScVal, map[string]bool, error) {
	ce, err := r.readContractData(ctx, keys)
	return ce.vals, ce.archived, err
}

// readContractData is contractData with each key's last-modified or removal ledger.
func (r *ExplorerReader) readContractData(ctx context.Context, keys []string) (contractEntries, error) {
	out, _, err := r.queryContractData(ctx, contractDataVersionQuery, keys)
	return out, err
}

// queryContractData runs a (key_xdr, entry_xdr, ledger_seq) contract_data read and resolves the TTL liveness of every present
// entry; n is the number of rows read, removed keys included.
func (r *ExplorerReader) queryContractData(ctx context.Context, query string, args ...any) (out contractEntries, n int, err error) {
	rows, err := r.conn.Query(ctx, query, args...)
	if err != nil {
		return contractEntries{}, 0, fmt.Errorf("clickhouse: contract data lookup: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out = contractEntries{vals: map[string]xdr.ScVal{}, archived: map[string]bool{}, ledger: map[string]uint32{}}
	var matched []string
	for rows.Next() {
		var keyXDR, b64 string
		var ledger uint32
		if err := rows.Scan(&keyXDR, &b64, &ledger); err != nil {
			return contractEntries{}, 0, fmt.Errorf("clickhouse: scan contract data: %w", err)
		}
		n++
		if b64 == "" {
			out.ledger[keyXDR] = ledger
			continue
		}
		v, modified, ok := contractDataEntry(b64)
		if !ok {
			out.archived[keyXDR], out.ledger[keyXDR] = true, ledger
			continue
		}
		out.vals[keyXDR], out.ledger[keyXDR] = v, modified
		matched = append(matched, keyXDR)
	}
	if err := rows.Err(); err != nil {
		return contractEntries{}, 0, fmt.Errorf("clickhouse: contract data rows: %w", err)
	}
	if len(matched) == 0 {
		return out, n, nil
	}
	liveness, err := r.ttlVerdicts.resolve(ctx, matched)
	if err != nil {
		return contractEntries{}, 0, fmt.Errorf("clickhouse: contract data ttl liveness: %w", err)
	}
	for _, k := range matched {
		if liveness[k] == TTLArchived {
			out.archived[k] = true
		}
	}
	return out, n, nil
}
